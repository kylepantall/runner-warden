// Package daemon wires the scale-set listeners, the scheduler, the backends
// and the control/hook servers into the long-running service.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/hookapi"
	"github.com/kylepantall/runner-warden/internal/naming"
	"github.com/kylepantall/runner-warden/internal/scheduler"
	"github.com/kylepantall/runner-warden/internal/store"
)

// Version is set by main for the user agent.
var Version = "dev"

// Daemon is one running instance.
type Daemon struct {
	cfg    *config.Config
	log    *slog.Logger
	store  *store.Store
	hostID string

	sets     []*gh.ScaleSets // one per lane index: a ScaleSets keeps one set per key
	rest     *gh.REST
	backends map[core.OS]core.Backend
	win      WindowsPool // nil unless windows is enabled
	sched    *scheduler.Scheduler
	lanes    []scheduler.LaneKey
	loops    map[string]func(context.Context) error

	mu        sync.Mutex
	listeners map[scheduler.LaneKey]*gh.Listener
	capacity  map[scheduler.LaneKey]int
	runners   map[string]*runnerRec
	events    map[int64]string // run id -> event name, for stale-PR checks
}

type runnerRec struct {
	name     string
	lane     scheduler.LaneKey
	ghID     int
	handle   core.Handle
	started  time.Time
	vm       string
	lastCPU  float64
	lastLog  int64
	lastIO   int64
	activeAt time.Time
	jobAt    time.Time // job start, for the wall-clock cap
	runID    int64
	owner    string
	repo     string
	killed   bool
}

// Options are the inputs main provides.
type Options struct {
	Config *config.Config
	Log    *slog.Logger
	Token  string
	// MakeBackends builds the backends once the host id is known.
	MakeBackends func(hostID string) (map[core.OS]core.Backend, WindowsPool, error)
	// Loops are extra components run alongside the daemon (e.g. image GC).
	Loops map[string]func(context.Context) error
}

// New opens state and prepares everything that does not touch GitHub yet.
func New(o Options) (*Daemon, error) {
	c := o.Config
	if err := os.MkdirAll(c.StateDir, 0o750); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(c.StateDir, "state.db"))
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	meta, err := st.GetMeta()
	switch {
	case errors.Is(err, store.ErrNoMeta):
		meta = store.Meta{HostID: naming.NewHostID(), Prefix: c.Prefix}
		if err := st.SetMeta(meta); err != nil {
			st.Close()
			return nil, err
		}
	case err != nil:
		st.Close()
		return nil, err
	case meta.Prefix != c.Prefix:
		st.Close()
		return nil, fmt.Errorf("config prefix %q differs from the recorded prefix %q; use `runner-warden prefix set`", c.Prefix, meta.Prefix)
	}
	_ = os.WriteFile(HostIDFile(c.StateDir), []byte(meta.HostID), 0o644)
	rest, err := gh.NewREST(o.Token)
	if err != nil {
		st.Close()
		return nil, err
	}
	d := &Daemon{
		cfg:       c,
		log:       o.Log,
		store:     st,
		hostID:    meta.HostID,
		rest:      rest,
		loops:     o.Loops,
		listeners: map[scheduler.LaneKey]*gh.Listener{},
		capacity:  map[scheduler.LaneKey]int{},
		runners:   map[string]*runnerRec{},
		events:    map[int64]string{},
	}
	d.backends, d.win, err = o.MakeBackends(meta.HostID)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("backends: %w", err)
	}
	lanes := c.Limits.Lanes
	if lanes <= 0 {
		lanes = 4
	}
	for i := 0; i < lanes; i++ {
		d.sets = append(d.sets, gh.NewScaleSets(o.Token, Version))
	}
	for _, t := range c.Targets {
		for _, os := range t.OSes() {
			for n := 1; n <= lanes; n++ {
				d.lanes = append(d.lanes, scheduler.LaneKey{ScaleSetKey: core.ScaleSetKey{Target: t.Core(), OS: os}, Lane: n})
			}
		}
	}
	so := scheduler.OptionsFromConfig(c, d.lanes)
	so.Runners = d
	so.Capacity = d
	so.Log = o.Log
	so.Observer = d
	if d.win != nil {
		so.VMs = d.win
	}
	if cores, err := admission.ReadTopology(""); err == nil {
		so.Cores = cores
	} else {
		o.Log.Warn("cpu topology unavailable; pinning disabled", "error", err.Error())
	}
	d.sched = scheduler.New(so)
	return d, nil
}

// HostID is this instance's host id.
func (d *Daemon) HostID() string { return d.hostID }

// Close releases state.
func (d *Daemon) Close() error { return d.store.Close() }

// Run starts everything and blocks until ctx is cancelled. Cancelling drains:
// running jobs keep going in their containers and are adopted next start.
func (d *Daemon) Run(ctx context.Context) error {
	for os, b := range d.backends {
		if err := b.Prepare(ctx); err != nil {
			return fmt.Errorf("prepare %s backend: %w", os, err)
		}
	}
	if err := d.ensureScaleSets(ctx); err != nil {
		return err
	}
	d.restore(ctx)

	ctrl, err := hookapi.ListenControl(config.SocketPath(d.cfg.Instance), d.cfg.ControlGroup)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	hookLn, _, err := hookapi.ListenHook(HookDir(d.cfg.Instance))
	if err != nil {
		return fmt.Errorf("hook socket: %w", err)
	}
	h := d.handlers()

	var wg sync.WaitGroup
	run := func(name string, f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.log.Error("component stopped", "component", name, "error", err.Error())
			}
		}()
	}
	run("control", func(ctx context.Context) error { return hookapi.Serve(ctx, hookapi.NewControlServer(h), ctrl) })
	run("hook", func(ctx context.Context) error { return hookapi.Serve(ctx, hookapi.NewHookServer(h), hookLn) })
	run("scheduler", d.sched.Run)
	d.mu.Lock()
	for _, l := range d.lanes {
		ln := d.listeners[l]
		run("listener", ln.Run)
	}
	d.mu.Unlock()
	run("monitor", d.monitor)
	run("reconcile", d.reconcileLoop)
	run("stale", d.staleLoop)
	if d.win != nil {
		run("windows-idle", d.windowsIdleLoop)
	}
	for name, f := range d.loops {
		run(name, f)
	}
	d.log.Info("daemon started", "instance", d.cfg.Instance, "host_id", d.hostID, "lanes", len(d.lanes))

	<-ctx.Done()
	d.sched.Drain(true)
	wg.Wait()
	d.sched.Wait()
	d.log.Info("daemon stopped")
	return nil
}

// HostIDFile records the host id for CLI commands that run beside the daemon.
func HostIDFile(stateDir string) string { return filepath.Join(stateDir, "host-id") }

// HookDir is the directory holding hook.sock, mounted into runners.
func HookDir(instance string) string { return filepath.Join(config.RunDir, instance, "hook") }

func (d *Daemon) ensureScaleSets(ctx context.Context) error {
	for _, l := range d.lanes {
		set := d.sets[l.Lane-1]
		name := naming.LaneName(d.cfg.Prefix, l.OS, l.Lane)
		label := naming.Label(d.cfg.Prefix, l.OS)
		if _, err := set.Ensure(ctx, l.ScaleSetKey, name, []string{label}, l.Target.RunnerGroup); err != nil {
			return fmt.Errorf("scale set %s for %s: %w", name, l.Target, err)
		}
		lane := l
		ln, err := set.NewListener(l.ScaleSetKey, laneEvents{d: d, lane: lane}, d.log.With("lane", name, "target", l.Target.String()), 1)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.listeners[l] = ln
		if n, ok := d.capacity[l]; ok {
			ln.SetCapacity(n)
		}
		d.mu.Unlock()
	}
	return nil
}

// laneEvents adapts gh.Events (one listener per lane) to the scheduler.
type laneEvents struct {
	d    *Daemon
	lane scheduler.LaneKey
}

func (e laneEvents) Assigned(k core.ScaleSetKey, j core.Job) {
	e.d.mu.Lock()
	e.d.events[j.RunID] = j.Event
	e.d.mu.Unlock()
	e.d.sched.Assigned(k, e.lane.Lane, j)
}
func (e laneEvents) Reassigned(k core.ScaleSetKey, j core.Job) {
	e.d.sched.Reassigned(k, e.lane.Lane, j)
}
func (e laneEvents) Started(k core.ScaleSetKey, j core.Job) {
	e.d.mu.Lock()
	if rr := e.d.runners[j.RunnerName]; rr != nil {
		rr.jobAt, rr.runID, rr.owner, rr.repo = time.Now(), j.RunID, j.Owner, j.Repo
		rr.activeAt = time.Now()
	}
	e.d.mu.Unlock()
	_, _ = e.d.store.SetRunnerJob(j.RunnerName, j.Owner+"/"+j.Repo, j.RunID, j.Name)
	_, _ = e.d.store.SetRunnerState(j.RunnerName, "running")
	e.d.sched.Started(k, e.lane.Lane, j)
}
func (e laneEvents) Completed(k core.ScaleSetKey, j core.Job) { e.d.sched.Completed(k, e.lane.Lane, j) }
func (e laneEvents) Stats(k core.ScaleSetKey, s gh.Stats) {
	e.d.sched.Stats(k, e.lane.Lane, scheduler.LaneStats{Assigned: s.Assigned, Running: s.Running, Registered: s.Registered, Busy: s.Busy, Idle: s.Idle})
}

// SetCapacity implements scheduler.Capacity. Called under the scheduler
// lock: must not block.
func (d *Daemon) SetCapacity(l scheduler.LaneKey, n int) {
	d.mu.Lock()
	d.capacity[l] = n
	ln := d.listeners[l]
	d.mu.Unlock()
	if ln != nil {
		ln.SetCapacity(n)
	}
}

// StartRunner implements scheduler.Runners.
func (d *Daemon) StartRunner(ctx context.Context, l scheduler.LaneKey, g scheduler.Grant) (string, error) {
	b := d.backends[l.OS]
	if b == nil {
		return "", fmt.Errorf("no backend for %s", l.OS)
	}
	name := naming.RunnerName(d.cfg.Prefix, d.hostID)
	spec := core.RunnerSpec{
		Name:     name,
		ScaleSet: l.ScaleSetKey,
		MemBytes: g.MemBytes,
		CPUSet:   g.CPUSet,
		Env:      map[string]string{},
	}
	var vm string
	if l.OS == core.Windows {
		if d.win == nil {
			return "", errors.New("windows is not enabled")
		}
		var ok bool
		if vm, ok = d.win.Pick(l.Target); !ok {
			return "", errors.New("no idle windows vm serves this target")
		}
		if err := d.win.EnsureUp(ctx, vm); err != nil {
			d.win.Release(vm)
			return "", fmt.Errorf("windows vm %s: %w", vm, err)
		}
		spec.Env[WindowsVMEnv] = vm
	}
	var workFolder []string
	if vm != "" {
		workFolder = []string{d.win.WorkFolder(vm, name)}
	}
	jit, ghID, err := d.sets[l.Lane-1].GenerateJIT(ctx, l.ScaleSetKey, name, workFolder...)
	if err != nil {
		if vm != "" {
			d.win.Release(vm)
		}
		return "", fmt.Errorf("jit config: %w", err)
	}
	spec.JITConfig = jit
	now := time.Now()
	rec := store.Runner{
		Name: name, Owner: l.Target.Owner, Repo: l.Target.Repo, OS: string(l.OS), Lane: l.Lane,
		Backend: string(l.OS), GitHubRunnerID: ghID, State: "starting", Created: now,
	}
	if err := d.store.UpsertRunner(rec); err != nil {
		d.log.Warn("store runner failed", "runner", name, "error", err.Error())
	}
	h, err := b.Start(ctx, spec)
	if err != nil {
		_ = d.sets[l.Lane-1].RemoveRunner(context.WithoutCancel(ctx), l.ScaleSetKey, ghID)
		_ = b.Cleanup(context.WithoutCancel(ctx), name)
		_, _ = d.store.DeleteRunner(name)
		if vm != "" {
			d.win.Release(vm)
		}
		return "", fmt.Errorf("start runner: %w", err)
	}
	rec.BackendID, rec.State = h.BackendID, "idle"
	_ = d.store.UpsertRunner(rec)
	d.mu.Lock()
	d.runners[name] = &runnerRec{name: name, lane: l, ghID: ghID, handle: h, started: now, vm: vm, activeAt: now}
	d.mu.Unlock()
	d.log.Info("runner started", "runner", name, "lane", l.Lane, "target", l.Target.String(), "os", string(l.OS), "mem", g.MemBytes, "parked", g.Parked, "cpuset", g.CPUSet)
	return name, nil
}

// StopRunner implements scheduler.Runners: stop, clean up, deregister.
func (d *Daemon) StopRunner(ctx context.Context, name string) error {
	d.mu.Lock()
	rr := d.runners[name]
	delete(d.runners, name)
	d.mu.Unlock()
	var os core.OS
	var lane scheduler.LaneKey
	ghID := 0
	if rr != nil {
		os, lane, ghID = rr.lane.OS, rr.lane, rr.ghID
	} else if r, ok, _ := d.store.GetRunner(name); ok {
		os = core.OS(r.OS)
		lane = scheduler.LaneKey{ScaleSetKey: core.ScaleSetKey{Target: core.Target{Owner: r.Owner, Repo: r.Repo}, OS: os}, Lane: r.Lane}
		ghID = r.GitHubRunnerID
	}
	b := d.backends[os]
	var errs []error
	if b != nil {
		if rr != nil {
			if err := b.Stop(ctx, rr.handle, 30*time.Second); err != nil {
				errs = append(errs, err)
			}
		}
		if err := b.Cleanup(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}
	if ghID != 0 && lane.Lane > 0 && lane.Lane <= len(d.sets) {
		if err := d.sets[lane.Lane-1].RemoveRunner(ctx, lane.ScaleSetKey, ghID); err != nil {
			d.log.Debug("deregister runner failed", "runner", name, "error", err.Error())
		}
	}
	_, _ = d.store.DeleteRunner(name)
	d.log.Info("runner removed", "runner", name)
	return errors.Join(errs...)
}

// ForceCancelRun implements scheduler.Runners.
func (d *Daemon) ForceCancelRun(ctx context.Context, owner, repo string, runID int64) error {
	d.log.Warn("force-cancelling run", "owner", owner, "repo", repo, "run_id", runID)
	return d.rest.ForceCancelRun(ctx, owner, repo, runID)
}

// RerunJob implements scheduler.Runners.
func (d *Daemon) RerunJob(ctx context.Context, owner, repo string, runID int64, job string) error {
	d.log.Info("re-running job after runner loss", "owner", owner, "repo", repo, "run_id", runID, "job", job)
	return d.rest.RerunJob(ctx, owner, repo, runID, job)
}

// JobFinished implements scheduler.Observer.
func (d *Daemon) JobFinished(r scheduler.JobResult) {
	dur := time.Duration(0)
	if !r.Started.IsZero() {
		dur = r.Finished.Sub(r.Started)
	}
	var peak int64
	d.mu.Lock()
	delete(d.events, r.Job.RunID)
	d.mu.Unlock()
	if err := d.store.AddJob(store.JobRecord{
		Owner: r.Job.Owner, Repo: r.Job.Repo, Workflow: admission.WorkflowFile(r.Job.WorkflowRef), Job: r.Job.Name,
		Cost: r.Cost, PeakMem: peak, Duration: dur, Result: r.Result, Finished: r.Finished,
	}); err != nil {
		d.log.Warn("record job failed", "error", err.Error())
	}
	d.log.Info("job finished", "owner", r.Job.Owner, "repo", r.Job.Repo, "job", r.Job.Name, "run_id", r.Job.RunID, "result", r.Result, "runner", r.Runner, "duration", dur.String())
}

// restore adopts runners that survived a restart and removes the rest.
func (d *Daemon) restore(ctx context.Context) {
	recs, err := d.store.ListRunners()
	if err != nil {
		d.log.Error("list stored runners failed", "error", err.Error())
	}
	byName := map[string]store.Runner{}
	for _, r := range recs {
		byName[r.Name] = r
	}
	live := map[string]core.Handle{}
	for os, b := range d.backends {
		hs, err := b.List(ctx)
		if err != nil {
			d.log.Error("list backend runners failed", "os", string(os), "error", err.Error())
			continue
		}
		for _, h := range hs {
			if _, ok := byName[h.RunnerName]; !ok {
				d.log.Warn("removing unrecorded runner", "runner", h.RunnerName)
				_ = b.Stop(ctx, h, 10*time.Second)
				_ = b.Cleanup(ctx, h.RunnerName)
				continue
			}
			live[h.RunnerName] = h
		}
	}
	var restored []scheduler.RestoredRunner
	for _, r := range recs {
		lane := scheduler.LaneKey{ScaleSetKey: core.ScaleSetKey{Target: core.Target{Owner: r.Owner, Repo: r.Repo, RunnerGroup: d.groupFor(r.Owner, r.Repo)}, OS: core.OS(r.OS)}, Lane: r.Lane}
		h, ok := live[r.Name]
		if !ok || lane.Lane < 1 || lane.Lane > len(d.sets) {
			_ = d.StopRunner(ctx, r.Name)
			continue
		}
		rr := &runnerRec{name: r.Name, lane: lane, ghID: r.GitHubRunnerID, handle: h, started: r.Created, activeAt: time.Now()}
		var job *core.Job
		if r.JobRunID != 0 {
			owner, repo, _ := strings.Cut(r.JobRepo, "/")
			job = &core.Job{RunID: r.JobRunID, Name: r.JobName, Owner: owner, Repo: repo, RunnerName: r.Name, ScaleSet: lane.ScaleSetKey}
			rr.jobAt, rr.runID, rr.owner, rr.repo = time.Now(), r.JobRunID, owner, repo
		}
		d.mu.Lock()
		d.runners[r.Name] = rr
		d.mu.Unlock()
		restored = append(restored, scheduler.RestoredRunner{Name: r.Name, Lane: lane, Created: r.Created, Job: job, Started: job != nil})
	}
	if len(restored) > 0 {
		d.log.Info("adopted runners after restart", "count", len(restored))
	}
	d.sched.Restore(restored)
}

func (d *Daemon) groupFor(owner, repo string) string {
	for _, t := range d.cfg.Targets {
		ct := t.Core()
		if ct.Owner == owner && ct.Repo == repo {
			return ct.RunnerGroup
		}
	}
	return ""
}

// WindowsVMEnv carries the chosen VM name to the Windows backend.
const WindowsVMEnv = "RUNNER_WARDEN_VM"
