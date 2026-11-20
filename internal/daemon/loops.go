package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/hookapi"
	"github.com/kylepantall/runner-warden/internal/scheduler"
)

// WindowsPool is the extra surface of the Windows VM backend.
type WindowsPool interface {
	core.Backend
	scheduler.VMProbe
	Pick(t core.Target) (vm string, ok bool)
	Release(vm string)
	EnsureUp(ctx context.Context, vm string) error
	WorkFolder(vm, runner string) string
	StopIdle(ctx context.Context, now time.Time) []string
}

const monitorEvery = 15 * time.Second

// monitor samples every runner: death, OOM, wall-clock cap, idleness.
func (d *Daemon) monitor(ctx context.Context) error {
	t := time.NewTicker(monitorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		d.mu.Lock()
		rs := make([]*runnerRec, 0, len(d.runners))
		for _, r := range d.runners {
			rs = append(rs, r)
		}
		d.mu.Unlock()
		for _, rr := range rs {
			d.check(ctx, rr)
		}
	}
}

func (d *Daemon) check(ctx context.Context, rr *runnerRec) {
	b := d.backends[rr.lane.OS]
	if b == nil || rr.killed {
		return
	}
	u, err := b.Stats(ctx, rr.handle)
	if err != nil {
		d.log.Debug("runner stats failed", "runner", rr.name, "error", err.Error())
		return
	}
	now := time.Now()
	if !u.Alive {
		rr.killed = true
		if u.OOMKills > 0 {
			d.log.Warn("runner exited after oom kill", "runner", rr.name, "oom_kills", u.OOMKills, "run_id", rr.runID)
		}
		d.sched.RunnerDied(rr.name, u.OOMKills > 0)
		return
	}
	if u.CPUSeconds-rr.lastCPU > 0.15*monitorEvery.Seconds() || u.LogBytes != rr.lastLog || u.IOBytes != rr.lastIO {
		rr.activeAt = now
	}
	rr.lastCPU, rr.lastLog, rr.lastIO = u.CPUSeconds, u.LogBytes, u.IOBytes
	if rr.jobAt.IsZero() {
		return
	}
	reason := ""
	switch {
	case now.Sub(rr.jobAt) > d.cfg.Hang.MaxDuration.D():
		reason = "max_duration"
	case now.Sub(rr.activeAt) > d.cfg.Hang.Idle.D():
		reason = "idle"
	}
	if reason != "" {
		d.killHung(ctx, rr, reason)
	}
}

// killHung cancels the run, then stops the runner. GitHub's Completed event
// lets the scheduler reap it; no re-run is attempted.
func (d *Daemon) killHung(ctx context.Context, rr *runnerRec, reason string) {
	rr.killed = true
	d.log.Warn("killing hung job", "runner", rr.name, "reason", reason, "run_id", rr.runID, "owner", rr.owner, "repo", rr.repo,
		"job_age", time.Since(rr.jobAt).String(), "idle_for", time.Since(rr.activeAt).String())
	if rr.runID != 0 {
		if err := d.rest.CancelRun(ctx, rr.owner, rr.repo, rr.runID); err != nil {
			d.log.Warn("cancel run failed", "run_id", rr.runID, "error", err.Error())
		}
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
			}
			if run, err := d.rest.GetRun(ctx, rr.owner, rr.repo, rr.runID); err == nil && run.Status != "completed" {
				_ = d.rest.ForceCancelRun(ctx, rr.owner, rr.repo, rr.runID)
			}
		}()
	}
	if b := d.backends[rr.lane.OS]; b != nil {
		if err := b.Stop(ctx, rr.handle, 15*time.Second); err != nil {
			d.log.Warn("stop hung runner failed", "runner", rr.name, "error", err.Error())
		}
	}
}

const reconcileEvery = 5 * time.Minute

// reconcileLoop removes our orphans on both sides: offline GitHub
// registrations carrying our host id that we no longer track, and backend
// resources we no longer track.
func (d *Daemon) reconcileLoop(ctx context.Context) error {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		d.reconcile(ctx)
	}
}

func (d *Daemon) reconcile(ctx context.Context) {
	ours := d.cfg.Prefix + "-" + d.hostID + "-"
	seen := map[core.Target]bool{}
	for _, l := range d.lanes {
		if seen[l.Target] {
			continue
		}
		seen[l.Target] = true
		rs, err := d.rest.ListRunners(ctx, l.Target)
		if err != nil {
			d.log.Warn("reconcile list runners failed", "target", l.Target.String(), "error", err.Error())
			continue
		}
		for _, r := range rs {
			if !strings.HasPrefix(r.Name, ours) || r.Busy || r.Status != "offline" {
				continue
			}
			d.mu.Lock()
			_, tracked := d.runners[r.Name]
			d.mu.Unlock()
			if tracked {
				continue
			}
			if _, ok, _ := d.store.GetRunner(r.Name); ok {
				continue
			}
			if err := d.rest.DeleteRunner(ctx, l.Target, r.ID); err != nil {
				d.log.Debug("reconcile delete runner failed", "runner", r.Name, "error", err.Error())
			} else {
				d.log.Info("removed orphaned registration", "runner", r.Name, "target", l.Target.String())
			}
		}
	}
	for os, b := range d.backends {
		hs, err := b.List(ctx)
		if err != nil {
			continue
		}
		for _, h := range hs {
			d.mu.Lock()
			_, tracked := d.runners[h.RunnerName]
			d.mu.Unlock()
			if tracked {
				continue
			}
			if time.Since(h.StartedAt) < 2*time.Minute { // starting right now
				continue
			}
			d.log.Warn("removing untracked runner", "runner", h.RunnerName, "os", string(os))
			_ = b.Stop(ctx, h, 10*time.Second)
			_ = b.Cleanup(ctx, h.RunnerName)
		}
	}
}

// staleLoop cancels runs whose pull request was merged or closed (or whose
// pushed branch was deleted) after the run was created.
func (d *Daemon) staleLoop(ctx context.Context) error {
	if !d.cfg.Stale.CancelClosedPRs && !d.cfg.Stale.CancelDeletedBranch {
		<-ctx.Done()
		return ctx.Err()
	}
	t := time.NewTicker(d.cfg.Stale.Interval.D())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		d.staleCheck(ctx)
	}
}

func (d *Daemon) staleCheck(ctx context.Context) {
	type run struct {
		owner, repo, event string
		id                 int64
	}
	runs := map[int64]run{}
	snap := d.sched.Snapshot()
	d.mu.Lock()
	for _, l := range snap.Lanes {
		for _, j := range l.Jobs {
			if j.RunID != 0 {
				runs[j.RunID] = run{owner: j.Owner, repo: j.Repo, id: j.RunID, event: d.events[j.RunID]}
			}
		}
	}
	d.mu.Unlock()
	for _, r := range runs {
		reason := ""
		switch r.event {
		case "pull_request", "pull_request_target":
			if !d.cfg.Stale.CancelClosedPRs {
				continue
			}
			info, err := d.rest.GetRun(ctx, r.owner, r.repo, r.id)
			if err != nil {
				continue
			}
			prs, err := d.rest.PRsForCommit(ctx, r.owner, r.repo, info.HeadSHA, info.HeadBranch)
			if err != nil {
				continue
			}
			for _, pr := range prs {
				if !pr.Closed {
					continue
				}
				// Runs created after the close are deliberate (types: [closed]).
				if !pr.ClosedAt.IsZero() && info.CreatedAt.After(pr.ClosedAt) {
					continue
				}
				reason = "pr_closed"
				if pr.Merged {
					reason = "pr_merged"
				}
			}
		case "push":
			if !d.cfg.Stale.CancelDeletedBranch {
				continue
			}
			info, err := d.rest.GetRun(ctx, r.owner, r.repo, r.id)
			if err != nil || info.HeadBranch == "" {
				continue
			}
			exists, err := d.rest.BranchExists(ctx, r.owner, r.repo, info.HeadBranch)
			if err == nil && !exists {
				reason = "branch_deleted"
			}
		}
		if reason == "" {
			continue
		}
		d.log.Info("cancelling stale run", "owner", r.owner, "repo", r.repo, "run_id", r.id, "reason", reason)
		if err := d.rest.CancelRun(ctx, r.owner, r.repo, r.id); err != nil {
			d.log.Warn("cancel stale run failed", "run_id", r.id, "error", err.Error())
		}
	}
}

func (d *Daemon) windowsIdleLoop(ctx context.Context) error {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-t.C:
			for _, vm := range d.win.StopIdle(ctx, now) {
				d.log.Info("windows vm stopped after idle", "vm", vm)
			}
		}
	}
}

// RunnerStatus is one runner in the status output.
type RunnerStatus struct {
	Name    string    `json:"name"`
	Target  string    `json:"target"`
	OS      string    `json:"os"`
	Lane    int       `json:"lane"`
	VM      string    `json:"vm,omitempty"`
	Started time.Time `json:"started"`
	RunID   int64     `json:"run_id,omitempty"`
	Mem     int64     `json:"mem_current"`
	MemPeak int64     `json:"mem_peak"`
	OOM     int       `json:"oom_kills"`
	Alive   bool      `json:"alive"`
}

// StatusReport is what `runner-warden status` shows.
type StatusReport struct {
	Instance  string           `json:"instance"`
	Prefix    string           `json:"prefix"`
	HostID    string           `json:"host_id"`
	Scheduler scheduler.Status `json:"scheduler"`
	Runners   []RunnerStatus   `json:"runners"`
	VMsUp     map[string]int64 `json:"vms_up,omitempty"`
}

func (d *Daemon) handlers() hookapi.Handlers {
	return hookapi.Handlers{
		Hook: d.sched.Hook,
		HookCompleted: func(_ context.Context, req scheduler.HookRequest) error {
			d.sched.HookCompleted(req)
			return nil
		},
		Status: func(ctx context.Context) (any, error) { return d.status(ctx), nil },
		Drain: func(on bool) error {
			d.sched.Drain(on)
			d.log.Info("drain changed", "draining", on)
			return nil
		},
		Kill: func(ctx context.Context, name string) error {
			d.mu.Lock()
			rr := d.runners[name]
			d.mu.Unlock()
			if rr == nil {
				return hookapi.ErrNotFound
			}
			d.killHung(ctx, rr, "manual")
			return nil
		},
	}
}

func (d *Daemon) status(ctx context.Context) StatusReport {
	rep := StatusReport{Instance: d.cfg.Instance, Prefix: d.cfg.Prefix, HostID: d.hostID, Scheduler: d.sched.Snapshot()}
	d.mu.Lock()
	rs := make([]*runnerRec, 0, len(d.runners))
	for _, r := range d.runners {
		rs = append(rs, r)
	}
	d.mu.Unlock()
	for _, rr := range rs {
		s := RunnerStatus{Name: rr.name, Target: rr.lane.Target.String(), OS: string(rr.lane.OS), Lane: rr.lane.Lane, VM: rr.vm, Started: rr.started, RunID: rr.runID}
		if b := d.backends[rr.lane.OS]; b != nil {
			if u, err := b.Stats(ctx, rr.handle); err == nil {
				s.Mem, s.MemPeak, s.OOM, s.Alive = u.MemCurrent, u.MemPeak, u.OOMKills, u.Alive
			}
		}
		rep.Runners = append(rep.Runners, s)
	}
	if d.win != nil {
		rep.VMsUp = d.win.VMsUp()
	}
	return rep
}
