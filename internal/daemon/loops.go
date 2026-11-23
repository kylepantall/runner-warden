package daemon

import (
	"context"
	"os"
	"slices"
	"time"

	"github.com/kylepantall/runner-warden/internal/autocache"
	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/hookapi"
	"github.com/kylepantall/runner-warden/internal/scheduler"
)

// WindowsPool is the extra surface of the Windows VM backend.
type WindowsPool interface {
	core.Backend
	scheduler.VMProbe
	Pick(t core.Target) (vm string, ok bool)
	PickNamed(vm string, t core.Target) (string, bool)
	PeekNamed(vm string) (mem int64, up, remote, idle bool)
	PackagesPending() []string
	NeedsPackages(vm string) bool
	EnsurePackages(ctx context.Context, vm string) ([]string, error)
	Peek(t core.Target) (vm string, mem int64, up bool)
	Release(vm string)
	EnsureUp(ctx context.Context, vm string) error
	WorkFolder(vm, runner string) string
	StopIdle(ctx context.Context, now time.Time) []string
	IsRemote(vm string) bool
	HostsUp() map[string]bool // remote hosts: reachable or not
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
		d.reservePersistent(ctx)
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

// reservePersistent adds the memory of persistent containers to what admission
// treats as used outside the ledger.
func (d *Daemon) reservePersistent(ctx context.Context) {
	pr, ok := d.backends[core.Linux].(persistentReporter)
	if !ok {
		return
	}
	cs, err := pr.Persistent(ctx)
	if err != nil {
		d.log.Debug("persistent containers sample failed", "error", err.Error())
		return
	}
	sum := int64(d.cfg.Limits.ExternalReserved)
	for _, c := range cs {
		sum += c.Mem
	}
	d.mu.Lock()
	d.persist = cs
	d.mu.Unlock()
	d.sched.SetExternalReserved(sum)
}

// reapAfter is how long a dead runner may stay tracked before the daemon
// reaps it itself (the scheduler normally does, on the job's Completed event).
const reapAfter = 5 * time.Minute

func (d *Daemon) check(ctx context.Context, rr *runnerRec) {
	b := d.backends[rr.lane.OS]
	if b == nil {
		return
	}
	d.mu.Lock()
	killed, goneAt := rr.killed, rr.goneAt
	d.mu.Unlock()
	if killed {
		if !goneAt.IsZero() && time.Since(goneAt) > reapAfter {
			d.log.Warn("reaping runner the scheduler never reaped", "runner", rr.name)
			_ = d.StopRunner(ctx, rr.name)
		}
		return
	}
	u, err := b.Stats(ctx, rr.handle)
	if err != nil {
		d.log.Debug("runner stats failed", "runner", rr.name, "error", err.Error())
		return
	}
	now := time.Now()
	if !u.Alive {
		d.mu.Lock()
		rr.killed, rr.goneAt = true, now
		runID := rr.runID
		d.mu.Unlock()
		if u.OOMKills > 0 {
			d.log.Warn("runner exited after oom kill", "runner", rr.name, "oom_kills", u.OOMKills, "run_id", runID)
		}
		d.sched.RunnerDied(rr.name, u.OOMKills > 0)
		return
	}
	d.mu.Lock()
	if u.CPUSeconds-rr.lastCPU > 0.15*monitorEvery.Seconds() || u.LogBytes != rr.lastLog || u.IOBytes != rr.lastIO {
		rr.activeAt = now
	}
	rr.lastCPU, rr.lastLog, rr.lastIO = u.CPUSeconds, u.LogBytes, u.IOBytes
	jobAt, activeAt := rr.jobAt, rr.activeAt
	d.mu.Unlock()
	if jobAt.IsZero() {
		return
	}
	reason := ""
	switch {
	case now.Sub(jobAt) > d.cfg.Hang.MaxDuration.D():
		reason = "max_duration"
	case now.Sub(activeAt) > d.cfg.Hang.Idle.D():
		reason = "idle"
	}
	if reason != "" {
		d.killHung(ctx, rr, reason)
	}
}

// killHung stops the runner (and its job containers) only. It does not cancel
// the run: that would take down the job's matrix siblings, and GitHub fails
// just this job once its runner is gone. The scheduler reaps on Completed.
func (d *Daemon) killHung(ctx context.Context, rr *runnerRec, reason string) {
	d.mu.Lock()
	rr.killed, rr.goneAt = true, time.Now()
	runID, owner, repo, jobAt, activeAt := rr.runID, rr.owner, rr.repo, rr.jobAt, rr.activeAt
	d.mu.Unlock()
	d.log.Warn("killing hung job", "runner", rr.name, "reason", reason, "run_id", runID, "owner", owner, "repo", repo,
		"job_age", time.Since(jobAt).String(), "idle_for", time.Since(activeAt).String())
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
			if !d.validRunner(r.Name) || r.Busy || r.Status != "offline" {
				continue
			}
			d.mu.Lock()
			_, tracked := d.runners[r.Name]
			starting := d.starting[r.Name]
			d.mu.Unlock()
			if tracked || starting {
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
			if !d.validRunner(h.RunnerName) {
				continue
			}
			d.mu.Lock()
			_, tracked := d.runners[h.RunnerName]
			starting := d.starting[h.RunnerName]
			d.mu.Unlock()
			if tracked || starting {
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
				runs[j.RunID] = run{owner: j.Owner, repo: j.Repo, id: j.RunID, event: d.events[j.RunID].event}
			}
		}
	}
	d.mu.Unlock()
	for _, r := range runs {
		reason := d.staleReason(ctx, r.owner, r.repo, r.event, r.id)
		if reason == "" {
			continue
		}
		d.log.Info("cancelling stale run", "owner", r.owner, "repo", r.repo, "run_id", r.id, "reason", reason)
		if err := d.rest.CancelRun(ctx, r.owner, r.repo, r.id); err != nil {
			d.log.Warn("cancel stale run failed", "run_id", r.id, "error", err.Error())
		}
	}
}

const (
	finishedRunsEvery = 2 * time.Minute
	finishedRunsAfter = 2 * time.Minute // younger waiting jobs are normal
)

// finishedRunsLoop drops waiting jobs whose workflow run has already
// finished (see scheduler.DropRun).
func (d *Daemon) finishedRunsLoop(ctx context.Context) error {
	t := time.NewTicker(finishedRunsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		d.dropFinishedRuns(ctx)
	}
}

func (d *Daemon) dropFinishedRuns(ctx context.Context) {
	type run struct {
		owner, repo string
		id          int64
	}
	runs := map[run]bool{}
	for _, l := range d.sched.Snapshot().Lanes {
		for _, j := range l.Jobs {
			switch j.State {
			case scheduler.StateHeld, scheduler.StateStarting, scheduler.StateUnbound:
			default:
				continue
			}
			if j.RunID != 0 && !j.Phantom && j.Age >= finishedRunsAfter {
				runs[run{j.Owner, j.Repo, j.RunID}] = true
			}
		}
	}
	for r := range runs {
		info, err := d.rest.GetRun(ctx, r.owner, r.repo, r.id)
		if err != nil || info.Status != "completed" {
			continue
		}
		if n := d.sched.DropRun(r.owner, r.repo, r.id); n > 0 {
			d.log.Info("dropped jobs of a finished run", "owner", r.owner, "repo", r.repo, "run_id", r.id, "jobs", n)
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

// autoCacheEvery is how often automatic caches are measured and trimmed.
const autoCacheEvery = 30 * time.Minute

// hostCacheTrimmer is a Linux backend whose remote hosts keep caches too.
type hostCacheTrimmer interface {
	TrimCaches(ctx context.Context, host string, limit int64, inUse []string) (kept, removed []autocache.Usage, ok bool, err error)
}

// autoCacheLoop measures the automatic caches of this host and of every
// [[linux.host]] for status, and removes each machine's least recently used
// scopes not in use once they outgrow auto_cache.max_size.
func (d *Daemon) autoCacheLoop(ctx context.Context) error {
	t := time.NewTicker(autoCacheEvery)
	defer t.Stop()
	for {
		d.trimAutoCaches(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (d *Daemon) trimAutoCaches(ctx context.Context) {
	limit := int64(d.cfg.AutoCache.MaxSize)
	inUse := map[string][]string{} // machine ("" = here) -> scopes
	d.mu.Lock()
	for _, r := range d.runners {
		if r.cache != "" && r.lane.OS == core.Linux {
			inUse[r.vm] = append(inUse[r.vm], r.cache)
		}
	}
	d.mu.Unlock()
	report := func(machine string, gone []autocache.Usage, err error) {
		for _, g := range gone {
			d.log.Info("automatic cache removed", "machine", machine, "scope", g.Scope, "bytes", g.Bytes, "last_used", g.LastUsed)
		}
		if err != nil {
			d.log.Warn("automatic cache trim failed", "machine", machine, "error", err.Error())
		}
	}
	var all []autocache.Usage
	base := AutoCacheDir(d.cfg.Instance)
	if us, err := autocache.Scan(base); err != nil {
		d.log.Warn("automatic cache scan failed", "error", err.Error())
	} else {
		busy := map[string]bool{}
		for _, s := range inUse[""] {
			busy[s] = true
		}
		gone, err := autocache.Evict(base, us, limit, busy)
		report("", gone, err)
		all = append(all, slices.DeleteFunc(us, func(u autocache.Usage) bool {
			return slices.ContainsFunc(gone, func(g autocache.Usage) bool { return g.Scope == u.Scope })
		})...)
	}
	if tr, ok := d.backends[core.Linux].(hostCacheTrimmer); ok {
		for _, h := range d.cfg.Linux.Hosts {
			kept, gone, ok, err := tr.TrimCaches(ctx, h.Name, limit, inUse[h.Name])
			if !ok {
				continue
			}
			report(h.Name, gone, err)
			for _, u := range kept {
				u.Machine = h.Name
				all = append(all, u)
			}
		}
	}
	slices.SortFunc(all, func(a, b autocache.Usage) int { return b.LastUsed.Compare(a.LastUsed) })
	d.mu.Lock()
	d.caches = all
	d.mu.Unlock()
}

// packagesEvery is how often windows.package is re-checked on members that
// are up (a VM just booted, a host back online).
const packagesEvery = 2 * time.Minute

// windowsPackagesLoop keeps windows.package installed on every VM and host.
func (d *Daemon) windowsPackagesLoop(ctx context.Context) error {
	t := time.NewTicker(packagesEvery)
	defer t.Stop()
	for {
		for _, vm := range d.win.PackagesPending() {
			got, err := d.win.EnsurePackages(ctx, vm)
			if err != nil {
				d.log.Warn("windows packages failed", "vm", vm, "error", err.Error())
				continue
			}
			if len(got) > 0 {
				d.log.Info("windows packages installed", "vm", vm, "packages", got)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
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
	Hostname  string           `json:"hostname,omitempty"` // this machine, where Linux runners run
	Scheduler scheduler.Status `json:"scheduler"`
	Runners   []RunnerStatus   `json:"runners"`
	VMsUp     map[string]int64 `json:"vms_up,omitempty"`
	// WindowsHosts are remote Windows hosts and whether they answer.
	WindowsHosts map[string]bool `json:"windows_hosts,omitempty"`
	// AutoCaches are this host's automatic caches, most recently used first,
	// as last measured; AutoCacheMax is their budget.
	AutoCaches   []autocache.Usage `json:"auto_caches,omitempty"`
	AutoCacheMax int64             `json:"auto_cache_max,omitempty"`
	// LinuxHosts maps every [[linux.host]] to reachable or not, in
	// LinuxHostNames order.
	LinuxHosts     map[string]bool `json:"linux_hosts,omitempty"`
	LinuxHostNames []string        `json:"linux_host_names,omitempty"`
	// MacHosts maps every [[macos.host]] to reachable or not, in
	// MacHostNames order.
	MacHosts     map[string]bool `json:"mac_hosts,omitempty"`
	MacHostNames []string        `json:"mac_host_names,omitempty"`
	// WindowsVMs lists every configured VM, up or not.
	WindowsVMs []string `json:"windows_vms,omitempty"`
	// Persistent are containers that outlive runners; their memory counts as
	// external reservation.
	Persistent []core.PersistentContainer `json:"persistent,omitempty"`
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
	rep.Hostname, _ = os.Hostname()
	d.mu.Lock()
	rs := make([]runnerRec, 0, len(d.runners))
	for _, r := range d.runners {
		rs = append(rs, *r)
	}
	rep.Persistent = append(rep.Persistent, d.persist...)
	rep.AutoCaches = append(rep.AutoCaches, d.caches...)
	rep.AutoCacheMax = int64(d.cfg.AutoCache.MaxSize)
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
	if lh, ok := d.backends[core.Linux].(linuxHosts); ok {
		rep.LinuxHosts = lh.HostsUp()
		for _, h := range d.cfg.Linux.Hosts {
			rep.LinuxHostNames = append(rep.LinuxHostNames, h.Name)
		}
	}
	if mh, ok := d.backends[core.MacOS].(linuxHosts); ok {
		rep.MacHosts = mh.HostsUp()
		for _, h := range d.cfg.MacOS.Hosts {
			rep.MacHostNames = append(rep.MacHostNames, h.Name)
		}
	}
	if d.win != nil {
		rep.VMsUp = d.win.VMsUp()
		rep.WindowsHosts = d.win.HostsUp()
		for _, v := range d.cfg.Windows.VMs {
			rep.WindowsVMs = append(rep.WindowsVMs, v.Name)
		}
	}
	return rep
}

// staleReason says why a run is stale ("" = not stale or unknown).
func (d *Daemon) staleReason(ctx context.Context, owner, repo, event string, id int64) string {
	switch event {
	case "pull_request", "pull_request_target":
		if !d.cfg.Stale.CancelClosedPRs {
			return ""
		}
		info, err := d.rest.GetRun(ctx, owner, repo, id)
		if err != nil {
			return ""
		}
		prs, err := d.rest.PRsForCommit(ctx, owner, repo, info.HeadSHA, info.HeadBranch)
		if err != nil {
			return ""
		}
		reason := ""
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
		return reason
	case "push":
		if !d.cfg.Stale.CancelDeletedBranch {
			return ""
		}
		info, err := d.rest.GetRun(ctx, owner, repo, id)
		if err != nil || info.HeadBranch == "" {
			return ""
		}
		exists, err := d.rest.BranchExists(ctx, owner, repo, info.HeadBranch)
		if err != nil || exists {
			return ""
		}
		// A tag push has a tag, not a branch, as its head ref.
		if tag, err := d.rest.TagExists(ctx, owner, repo, info.HeadBranch); err != nil || tag {
			return ""
		}
		return "branch_deleted"
	}
	return ""
}
