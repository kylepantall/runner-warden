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
	"github.com/kylepantall/runner-warden/internal/autocache"
	"github.com/kylepantall/runner-warden/internal/backend/hostpool"
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
	rest     githubAPI
	confs    repoConfs
	backends map[core.OS]core.Backend
	win      WindowsPool // nil unless windows is enabled
	sched    *scheduler.Scheduler
	hooks    *hookapi.RunnerHooks // per-runner hook sockets (Linux runners)
	lanes    []scheduler.LaneKey
	loops    map[string]func(context.Context) error

	mu        sync.Mutex
	listeners map[scheduler.LaneKey]*gh.Listener
	capacity  map[scheduler.LaneKey]int
	runners   map[string]*runnerRec
	starting  map[string]bool    // runner names being started: not orphans yet
	events    map[int64]eventRec // run id -> event name, for stale-PR checks
	evPruned  time.Time
	persist   []core.PersistentContainer // last sample, for status
	caches    []autocache.Usage          // last automatic-cache scan, for status
}

// eventRec remembers a run's event; entries expire (a run's first job
// finishing must not drop the event for its sibling jobs).
type eventRec struct {
	event string
	seen  time.Time
}

const eventTTL = 6 * time.Hour

// githubAPI is the REST surface the daemon uses.
type githubAPI interface {
	ListRunners(ctx context.Context, t core.Target) ([]gh.Runner, error)
	DeleteRunner(ctx context.Context, t core.Target, id int64) error
	GetRun(ctx context.Context, owner, repo string, runID int64) (gh.Run, error)
	CancelRun(ctx context.Context, owner, repo string, runID int64) error
	ForceCancelRun(ctx context.Context, owner, repo string, runID int64) error
	RerunJob(ctx context.Context, owner, repo string, runID int64, jobName string) error
	PRsForCommit(ctx context.Context, owner, repo, sha, branch string) ([]gh.PRState, error)
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
	TagExists(ctx context.Context, owner, repo, tag string) (bool, error)
}

// adopter is implemented by backends whose per-runner plumbing (the Docker
// proxy) lives in the daemon process and must be rebuilt for adopted runners.
type adopter interface {
	Adopt(ctx context.Context, h core.Handle) error
}

// linuxHosts is implemented by a backend with remote hosts ([[linux.host]],
// [[macos.host]]).
type linuxHosts interface {
	IsUp(name string) bool // never blocks
	HostsUp() map[string]bool
}

// persistentReporter is implemented by backends whose jobs can leave
// containers behind on purpose (outside the ledger, but inside the slice).
type persistentReporter interface {
	Persistent(ctx context.Context) ([]core.PersistentContainer, error)
}

type runnerRec struct {
	name     string
	lane     scheduler.LaneKey
	ghID     int
	handle   core.Handle
	started  time.Time
	vm       string
	cache    string // automatic-cache scope on this host, "" for none
	lastCPU  float64
	lastLog  int64
	lastIO   int64
	activeAt time.Time
	jobAt    time.Time // job start, for the wall-clock cap
	runID    int64
	owner    string
	repo     string
	killed   bool
	goneAt   time.Time // when the runner died or was stopped by us
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
			_ = st.Close()
			return nil, err
		}
	case err != nil:
		_ = st.Close()
		return nil, err
	case meta.Prefix != c.Prefix:
		_ = st.Close()
		return nil, fmt.Errorf("config prefix %q differs from the recorded prefix %q; use `runner-warden prefix set`", c.Prefix, meta.Prefix)
	}
	_ = os.WriteFile(HostIDFile(c.StateDir), []byte(meta.HostID), 0o644) //nolint:gosec // host id is not secret; readable by the runner hooks
	rest, err := gh.NewREST(o.Token)
	if err != nil {
		_ = st.Close()
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
		starting:  map[string]bool{},
		events:    map[int64]eventRec{},
	}
	d.backends, d.win, err = o.MakeBackends(meta.HostID)
	if err != nil {
		_ = st.Close()
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
		so.VMCost = func(l scheduler.LaneKey) (int64, bool) {
			_, mem, up := d.win.Peek(l.Target)
			return mem, up
		}
		so.VMNamed = d.win.PeekNamed
		so.RemoteVM = func(l scheduler.LaneKey) bool {
			vm, _, up := d.win.Peek(l.Target)
			return up && d.win.IsRemote(vm)
		}
	}
	var pools []linuxHosts
	if lh, ok := d.backends[core.Linux].(linuxHosts); ok {
		pools = append(pools, lh)
		for _, h := range c.Linux.Hosts {
			so.LinuxHosts = append(so.LinuxHosts, scheduler.RemoteHost{Name: h.Name, OS: core.Linux, Slots: h.MaxConcurrent, Serves: h.Serves})
		}
	}
	if mh, ok := d.backends[core.MacOS].(linuxHosts); ok {
		pools = append(pools, mh)
		for _, h := range c.MacOS.Hosts {
			so.LinuxHosts = append(so.LinuxHosts, scheduler.RemoteHost{Name: h.Name, OS: core.MacOS, Slots: 1, Serves: h.Serves})
		}
		so.HostsOnly = map[core.OS]bool{core.MacOS: true}
	}
	if len(pools) > 0 {
		so.HostUp = func(name string) bool {
			for _, p := range pools {
				if p.IsUp(name) {
					return true
				}
			}
			return false
		}
	}
	if cores, err := admission.ReadTopology(""); err == nil {
		so.Cores = cores
	} else {
		o.Log.Warn("cpu topology unavailable; pinning disabled", "error", err.Error())
	}
	so.CPUBudget = admission.CPUBudget(c.Limits.CPUBudget, so.Cores, so.ReservedCPUs)
	d.sched = scheduler.New(so)
	uid := c.Linux.RunnerUID
	if uid <= 0 {
		uid = 1001
	}
	d.hooks = hookapi.NewRunnerHooks(HookDir(c.Instance), uid, d.handlers(), 0)
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
	if err := d.restore(ctx); err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	ctrl, err := hookapi.ListenControl(config.SocketPath(d.cfg.Instance), d.cfg.ControlGroup)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
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
	run("scheduler", d.sched.Run)
	d.mu.Lock()
	for _, l := range d.lanes {
		ln := d.listeners[l]
		run("listener", ln.Run)
	}
	d.mu.Unlock()
	run("monitor", d.monitor)
	if d.cfg.Linux.Enabled && d.cfg.AutoCache.On() {
		run("auto-cache", d.autoCacheLoop)
	}
	run("reconcile", d.reconcileLoop)
	run("stale", d.staleLoop)
	run("finished-runs", d.finishedRunsLoop)
	if d.win != nil {
		run("windows-idle", d.windowsIdleLoop)
		run("windows-packages", d.windowsPackagesLoop)
	}
	for name, f := range d.loops {
		run(name, f)
	}
	d.log.Info("daemon started", "instance", d.cfg.Instance, "host_id", d.hostID, "lanes", len(d.lanes))

	<-ctx.Done()
	d.sched.Drain(true)
	d.hooks.Close()
	wg.Wait()
	d.sched.Wait()
	d.log.Info("daemon stopped")
	return nil
}

// HostIDFile records the host id for CLI commands that run beside the daemon.
func HostIDFile(stateDir string) string { return filepath.Join(stateDir, "host-id") }

// HookDir is the base of the per-runner hook dirs (<HookDir>/<runner>/hook.sock);
// only a runner's own dir is mounted into it.
func HookDir(instance string) string { return filepath.Join(config.RunDir, instance, "hook") }

// validRunner reports whether name is one this daemon could have generated.
// Names read back from container labels, the store or a backend listing must
// pass before they reach a filesystem path or a GitHub call.
func (d *Daemon) validRunner(name string) bool {
	return naming.ValidRunnerName(d.cfg.Prefix, d.hostID, name)
}

// startHookSocket creates the runner's private hook socket (Linux runners only).
func (d *Daemon) startHookSocket(os core.OS, name string) error {
	if os != core.Linux {
		return nil
	}
	_, err := d.hooks.Start(name)
	return err
}

func (d *Daemon) ensureScaleSets(ctx context.Context) error {
	for _, l := range d.lanes {
		set := d.sets[l.Lane-1]
		name := naming.LaneName(d.cfg.Prefix, l.OS, l.Lane)
		if _, err := set.Ensure(ctx, l.ScaleSetKey, name, naming.LaneLabels(d.cfg.Prefix, l.OS), l.Target.RunnerGroup); err != nil {
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

// noteEvent records a run's event and drops expired ones. d.mu is held.
func (d *Daemon) noteEvent(runID int64, event string) {
	now := time.Now()
	d.events[runID] = eventRec{event: event, seen: now}
	if now.Sub(d.evPruned) > time.Minute {
		d.evPruned = now
		for id, e := range d.events {
			if now.Sub(e.seen) > eventTTL {
				delete(d.events, id)
			}
		}
	}
}

func (e laneEvents) Assigned(k core.ScaleSetKey, j core.Job) {
	e.d.mu.Lock()
	e.d.noteEvent(j.RunID, j.Event)
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
	_, _ = e.d.store.SetRunnerJob(j.RunnerName, store.RunnerJob{
		Repo: j.Owner + "/" + j.Repo, RunID: j.RunID, Name: j.Name, JobID: j.JobID, WorkflowRef: j.WorkflowRef, Event: j.Event,
	})
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
	// Slow starts (a Windows VM coming up) must not look like orphans.
	d.mu.Lock()
	d.starting[name] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.starting, name)
		d.mu.Unlock()
	}()
	spec := core.RunnerSpec{
		Name:     name,
		ScaleSet: l.ScaleSetKey,
		MemBytes: g.MemBytes,
		CPUSet:   g.CPUSet,
		Devices:  g.Devices,
		Env:      map[string]string{},
	}
	if g.Host != "" {
		spec.Env[hostpool.HostEnv] = g.Host
	}
	scope := d.autoCache(&spec, l, g)
	if l.OS == core.Linux {
		if img := d.cfg.Linux.ResolveImage(d.runnerImage(ctx, g)); img != "" && g.Host != "" {
			spec.Image = img // the host pulls it, falling back to the default
		} else if img != "" {
			if ie, ok := b.(imageEnsurer); ok {
				if err := ie.EnsureImage(ctx, img); err != nil {
					d.log.Warn("job image unavailable, using default", "image", img, "error", err.Error())
				} else {
					spec.Image = img
				}
			}
		}
	}
	var vm string
	if l.OS == core.Windows {
		if d.win == nil {
			return "", errors.New("windows is not enabled")
		}
		var ok bool
		if g.Machine != "" {
			vm, ok = d.win.PickNamed(g.Machine, l.Target)
		} else {
			vm, ok = d.win.Pick(l.Target)
		}
		if !ok {
			return "", errors.New("no idle windows vm serves this target")
		}
		if err := d.win.EnsureUp(ctx, vm); err != nil {
			d.win.Release(vm)
			return "", fmt.Errorf("windows vm %s: %w", vm, err)
		}
		// The job may need them: install windows.package before it starts.
		if d.win.NeedsPackages(vm) {
			if got, err := d.win.EnsurePackages(ctx, vm); err != nil {
				d.log.Warn("windows packages failed before a job", "vm", vm, "error", err.Error())
			} else if len(got) > 0 {
				d.log.Info("windows packages installed", "vm", vm, "packages", got)
			}
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
	if g.Host == "" { // remote runners run without hooks
		err = d.startHookSocket(l.OS, name)
	}
	var h core.Handle
	if err == nil {
		h, err = b.Start(ctx, spec)
	}
	if err != nil {
		d.hooks.Stop(name)
		_ = d.sets[l.Lane-1].RemoveRunner(context.WithoutCancel(ctx), l.ScaleSetKey, ghID)
		_ = b.Cleanup(context.WithoutCancel(ctx), name)
		_, _ = d.store.DeleteRunner(name)
		if vm != "" {
			d.win.Release(vm)
		}
		return "", fmt.Errorf("start runner: %w", err)
	}
	_, _ = d.store.SetRunnerBackendID(name, h.BackendID)
	if g.Host != "" {
		vm = g.Host // shown as the runner's machine
	}
	rr := &runnerRec{name: name, lane: l, ghID: ghID, handle: h, started: now, vm: vm, activeAt: now, cache: scope}
	// A job may have started before we registered the runner (the Started
	// event only updated the store then).
	if sr, ok, _ := d.store.GetRunner(name); ok && sr.JobRunID != 0 {
		owner, repo, _ := strings.Cut(sr.JobRepo, "/")
		rr.jobAt, rr.runID, rr.owner, rr.repo = time.Now(), sr.JobRunID, owner, repo
	}
	d.mu.Lock()
	d.runners[name] = rr
	d.mu.Unlock()
	d.log.Info("runner started", "runner", name, "lane", l.Lane, "target", l.Target.String(), "os", string(l.OS), "mem", g.MemBytes, "parked", g.Parked, "cpuset", g.CPUSet, "image", spec.Image, "host", g.Host)
	return name, nil
}

// StopRunner implements scheduler.Runners: stop, clean up, deregister.
func (d *Daemon) StopRunner(ctx context.Context, name string) error {
	if !d.validRunner(name) {
		d.log.Warn("refusing to stop runner with an invalid name")
		_, _ = d.store.DeleteRunner(name)
		return errors.New("invalid runner name")
	}
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
	d.hooks.Stop(name)
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
	if err := d.store.AddJob(store.JobRecord{
		Owner: r.Job.Owner, Repo: r.Job.Repo, Workflow: admission.WorkflowFile(r.Job.WorkflowRef), Job: r.Job.Name,
		Cost: r.Cost, Duration: dur, Result: r.Result, Finished: r.Finished,
	}); err != nil {
		d.log.Warn("record job failed", "error", err.Error())
	}
	d.log.Info("job finished", "owner", r.Job.Owner, "repo", r.Job.Repo, "job", r.Job.Name, "run_id", r.Job.RunID, "result", r.Result, "runner", r.Runner, "duration", dur.String())
}

// restore adopts runners that survived a restart and removes the rest. Any
// failure to read the store or a backend aborts it: acting on a partial view
// would force-remove live runners.
func (d *Daemon) restore(ctx context.Context) error {
	recs, err := d.store.ListRunners()
	if err != nil {
		return fmt.Errorf("list stored runners: %w", err)
	}
	byName := map[string]store.Runner{}
	valid := recs[:0:0]
	for _, r := range recs {
		if !d.validRunner(r.Name) {
			d.log.Warn("dropping stored runner with an invalid name")
			_, _ = d.store.DeleteRunner(r.Name)
			continue
		}
		byName[r.Name] = r
		valid = append(valid, r)
	}
	recs = valid
	live := map[string]core.Handle{}
	for os, b := range d.backends {
		hs, err := b.List(ctx)
		if err != nil {
			return fmt.Errorf("list %s runners: %w", os, err)
		}
		for _, h := range hs {
			if !d.validRunner(h.RunnerName) {
				d.log.Warn("ignoring backend runner with an invalid name", "os", string(os))
				continue
			}
			if _, ok := byName[h.RunnerName]; !ok {
				d.log.Warn("removing unrecorded runner", "runner", h.RunnerName)
				_ = b.Stop(ctx, h, 10*time.Second)
				_ = b.Cleanup(ctx, h.RunnerName)
				continue
			}
			// Only adopt runners still running; exited ones finished (or died)
			// while we were down and GitHub has already settled their job. A
			// stats error says nothing about liveness: adopt, the monitor
			// rechecks.
			if u, err := b.Stats(ctx, h); err == nil && !u.Alive {
				continue
			}
			live[h.RunnerName] = h
		}
	}
	busy := d.busyRunners(ctx, recs, live)
	var restored []scheduler.RestoredRunner
	for _, r := range recs {
		lane := scheduler.LaneKey{ScaleSetKey: core.ScaleSetKey{Target: core.Target{Owner: r.Owner, Repo: r.Repo, RunnerGroup: d.groupFor(r.Owner, r.Repo)}, OS: core.OS(r.OS)}, Lane: r.Lane}
		h, ok := live[r.Name]
		if !ok || lane.Lane < 1 || lane.Lane > len(d.sets) {
			_ = d.StopRunner(ctx, r.Name)
			continue
		}
		if a, ok := d.backends[lane.OS].(adopter); ok {
			if err := a.Adopt(ctx, h); err != nil {
				d.log.Error("adopting runner failed", "runner", r.Name, "error", err.Error())
			}
		}
		host := ""
		if lane.OS != core.Windows {
			host, _ = hostpool.HostOf(h.BackendID)
		}
		if host == "" {
			if err := d.startHookSocket(lane.OS, r.Name); err != nil {
				d.log.Error("hook socket for adopted runner failed", "runner", r.Name, "error", err.Error())
			}
		}
		rr := &runnerRec{name: r.Name, lane: lane, ghID: r.GitHubRunnerID, handle: h, started: r.Created, activeAt: time.Now(), vm: host}
		if lane.OS == core.Windows {
			rr.vm = h.BackendID // the VM or host the runner sits on
		}
		var job *core.Job
		if r.JobRunID != 0 {
			owner, repo, _ := strings.Cut(r.JobRepo, "/")
			job = &core.Job{
				JobID: r.JobID, RunID: r.JobRunID, Name: r.JobName, Owner: owner, Repo: repo,
				WorkflowRef: r.JobWorkflowRef, Event: r.JobEvent, RunnerName: r.Name, ScaleSet: lane.ScaleSetKey,
			}
			rr.jobAt, rr.runID, rr.owner, rr.repo = time.Now(), r.JobRunID, owner, repo
		} else if busy[r.Name] {
			// Took a job while we were down; the scheduler binds it from the hook.
			rr.jobAt = time.Now()
		}
		d.mu.Lock()
		d.runners[r.Name] = rr
		if job != nil && job.Event != "" {
			d.noteEvent(job.RunID, job.Event)
		}
		d.mu.Unlock()
		restored = append(restored, scheduler.RestoredRunner{Name: r.Name, Lane: lane, Created: r.Created, Job: job, Started: job != nil || busy[r.Name], Host: host,
			Remote: lane.OS == core.Windows && d.win != nil && d.win.IsRemote(h.BackendID)})
	}
	if len(restored) > 0 {
		d.log.Info("adopted runners after restart", "count", len(restored))
	}
	d.sched.Restore(restored)
	return nil
}

// busyRunners asks GitHub which adopted runners with no recorded job are
// busy (they took a job while the daemon was down). Lookup failures count as
// not busy.
func (d *Daemon) busyRunners(ctx context.Context, recs []store.Runner, live map[string]core.Handle) map[string]bool {
	out := map[string]bool{}
	targets := map[core.Target]bool{}
	for _, r := range recs {
		if _, ok := live[r.Name]; ok && r.JobRunID == 0 {
			targets[core.Target{Owner: r.Owner, Repo: r.Repo}] = true
		}
	}
	for t := range targets {
		rs, err := d.rest.ListRunners(ctx, t)
		if err != nil {
			d.log.Warn("busy check failed", "target", t.String(), "error", err.Error())
			continue
		}
		for _, x := range rs {
			if x.Busy {
				out[x.Name] = true
			}
		}
	}
	return out
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

// AutoCacheDir holds the instance's automatic caches, on this host and on
// every [[linux.host]] (the same path there).
func AutoCacheDir(instance string) string { return filepath.Join(config.CacheDir, instance, "auto") }

// autoCache mounts the job's automatic cache into a Linux runner and points
// the tools at it. It returns the scope when the cache is on this host.
// [[cache]] entries setting the same variables win.
func (d *Daemon) autoCache(spec *core.RunnerSpec, l scheduler.LaneKey, g scheduler.Grant) string {
	if l.OS != core.Linux || !d.cfg.AutoCache.On() {
		return ""
	}
	scope := autocache.Scope(d.cfg.AutoCache.Scope, l.Target, g.Job)
	if scope == "" {
		return ""
	}
	base := AutoCacheDir(d.cfg.Instance)
	keep := map[string]bool{}
	for _, c := range d.cfg.Caches {
		if c.Env != "" {
			keep[c.Env] = true
		}
	}
	spec.Mounts = append(spec.Mounts, autocache.Mount(base, scope))
	for k, v := range autocache.EnvFor(keep) {
		spec.Env[k] = v
	}
	if g.Host != "" {
		return "" // the host's backend creates the dir; its usage stays there
	}
	if err := autocache.Touch(base, scope, d.cfg.Linux.RunnerUID); err != nil {
		d.log.Warn("automatic cache unavailable", "scope", scope, "error", err.Error())
	}
	return scope
}
