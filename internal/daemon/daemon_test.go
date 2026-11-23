package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/hookapi"
	"github.com/kylepantall/runner-warden/internal/scheduler"
	"github.com/kylepantall/runner-warden/internal/store"
)

const (
	tPrefix = "my-ci"
	tHost   = "ab12cd34"
	rGood   = "my-ci-ab12cd34-0000000a"
	rGood2  = "my-ci-ab12cd34-0000000b"
)

type fakeBackend struct {
	mu       sync.Mutex
	list     []core.Handle
	listErr  error
	statsErr error
	alive    bool
	adopted  []string
	stopped  []string
	cleaned  []string
}

func (f *fakeBackend) Caps() core.Caps               { return core.Caps{OS: core.Linux} }
func (f *fakeBackend) Prepare(context.Context) error { return nil }
func (f *fakeBackend) Start(context.Context, core.RunnerSpec) (core.Handle, error) {
	return core.Handle{}, errors.New("unused")
}
func (f *fakeBackend) Stats(context.Context, core.Handle) (core.Usage, error) {
	return core.Usage{Alive: f.alive}, f.statsErr
}
func (f *fakeBackend) Stop(_ context.Context, h core.Handle, _ time.Duration) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, h.RunnerName)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Cleanup(_ context.Context, n string) error {
	f.mu.Lock()
	f.cleaned = append(f.cleaned, n)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) List(context.Context) ([]core.Handle, error) { return f.list, f.listErr }
func (f *fakeBackend) Adopt(_ context.Context, h core.Handle) error {
	f.mu.Lock()
	f.adopted = append(f.adopted, h.RunnerName)
	f.mu.Unlock()
	return nil
}

type fakeAPI struct {
	mu        sync.Mutex
	runners   []gh.Runner
	run       gh.Run
	branch    bool
	tag       bool
	cancelled int
	forced    int
	rerun     int
}

func (f *fakeAPI) ListRunners(context.Context, core.Target) ([]gh.Runner, error) {
	return f.runners, nil
}
func (f *fakeAPI) DeleteRunner(context.Context, core.Target, int64) error        { return nil }
func (f *fakeAPI) GetRun(context.Context, string, string, int64) (gh.Run, error) { return f.run, nil }
func (f *fakeAPI) CancelRun(context.Context, string, string, int64) error {
	f.mu.Lock()
	f.cancelled++
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) ForceCancelRun(context.Context, string, string, int64) error {
	f.mu.Lock()
	f.forced++
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) RerunJob(context.Context, string, string, int64, string) error {
	f.mu.Lock()
	f.rerun++
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) PRsForCommit(context.Context, string, string, string, string) ([]gh.PRState, error) {
	return nil, nil
}
func (f *fakeAPI) BranchExists(context.Context, string, string, string) (bool, error) {
	return f.branch, nil
}
func (f *fakeAPI) TagExists(context.Context, string, string, string) (bool, error) { return f.tag, nil }

func testDaemon(t *testing.T, b *fakeBackend, api *fakeAPI) *Daemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Prefix: tPrefix, Instance: "t"}
	cfg.Hang.MaxDuration = config.Duration(time.Hour)
	cfg.Hang.Idle = config.Duration(time.Hour)
	d := &Daemon{
		cfg: cfg, log: slog.New(slog.DiscardHandler), store: st, hostID: tHost, rest: api,
		sets:      []*gh.ScaleSets{gh.NewScaleSets("", "t")},
		backends:  map[core.OS]core.Backend{core.Linux: b},
		listeners: map[scheduler.LaneKey]*gh.Listener{},
		capacity:  map[scheduler.LaneKey]int{},
		runners:   map[string]*runnerRec{},
		starting:  map[string]bool{},
		events:    map[int64]eventRec{},
	}
	d.sched = scheduler.New(scheduler.Options{Runners: d, Capacity: d, Log: d.log, Observer: d})
	d.hooks = hookapi.NewRunnerHooks(filepath.Join(dir, "hooks"), os.Getuid(), d.handlers(), 0)
	t.Cleanup(d.hooks.Close)
	return d
}

func addRec(t *testing.T, d *Daemon, name string, mod func(*store.Runner)) {
	t.Helper()
	r := store.Runner{Name: name, Owner: "acme", Repo: "app", OS: "linux", Lane: 1, State: store.RunnerIdle, Created: time.Now().Add(-time.Hour)}
	if mod != nil {
		mod(&r)
	}
	if err := d.store.UpsertRunner(r); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreAbortsOnBackendListError(t *testing.T) {
	b := &fakeBackend{listErr: errors.New("boom"), alive: true}
	d := testDaemon(t, b, &fakeAPI{})
	addRec(t, d, rGood, nil)
	if err := d.restore(context.Background()); err == nil {
		t.Fatal("restore must abort")
	}
	if len(b.cleaned)+len(b.stopped) != 0 {
		t.Fatal("restore touched runners with an incomplete view")
	}
	if _, ok, _ := d.store.GetRunner(rGood); !ok {
		t.Fatal("record dropped")
	}
}

func TestRestoreAdoptsOnStatsErrorAndRestoresJob(t *testing.T) {
	b := &fakeBackend{list: []core.Handle{{RunnerName: rGood, BackendID: "c1"}}, statsErr: errors.New("transient")}
	d := testDaemon(t, b, &fakeAPI{})
	addRec(t, d, rGood, func(r *store.Runner) {
		r.JobRunID, r.JobName, r.JobRepo, r.JobID, r.JobEvent = 9, "build", "acme/app", "jid-1", "push"
	})
	if err := d.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.cleaned)+len(b.stopped) != 0 {
		t.Fatal("a stats error must not remove a live runner")
	}
	if len(b.adopted) != 1 || b.adopted[0] != rGood {
		t.Fatalf("adopted %v", b.adopted)
	}
	d.mu.Lock()
	rr := d.runners[rGood]
	ev := d.events[9]
	d.mu.Unlock()
	if rr == nil || rr.runID != 9 || rr.jobAt.IsZero() || ev.event != "push" {
		t.Fatalf("%+v %+v", rr, ev)
	}
	if _, err := os.Stat(d.hooks.SocketPath(rGood)); err != nil {
		t.Fatalf("hook socket for adopted runner: %v", err)
	}
}

func TestRestoreMarksBusyRunnerWithoutRecordedJob(t *testing.T) {
	b := &fakeBackend{list: []core.Handle{{RunnerName: rGood, BackendID: "c1"}, {RunnerName: rGood2, BackendID: "c2"}}, alive: true}
	api := &fakeAPI{runners: []gh.Runner{{Name: rGood, Busy: true}, {Name: rGood2}}}
	d := testDaemon(t, b, api)
	addRec(t, d, rGood, nil)
	addRec(t, d, rGood2, nil)
	if err := d.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.runners[rGood].jobAt.IsZero() || !d.runners[rGood2].jobAt.IsZero() {
		t.Fatal("busy flag not honoured")
	}
}

func TestRestoreRefusesInvalidNames(t *testing.T) {
	bad := "../../etc"
	b := &fakeBackend{list: []core.Handle{{RunnerName: bad, BackendID: "x"}}, alive: true}
	d := testDaemon(t, b, &fakeAPI{})
	addRec(t, d, "other-ab12cd34-0000000a", nil)
	if err := d.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.cleaned)+len(b.stopped) != 0 {
		t.Fatalf("invalid names reached the backend: %v %v", b.cleaned, b.stopped)
	}
	if rs, _ := d.store.ListRunners(); len(rs) != 0 {
		t.Fatal("invalid stored record kept")
	}
	if err := d.StopRunner(context.Background(), bad); err == nil || len(b.cleaned) != 0 {
		t.Fatal("StopRunner accepted an invalid name")
	}
}

func TestReconcileSkipsStartingAndRemovesOrphans(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	b := &fakeBackend{list: []core.Handle{
		{RunnerName: rGood, StartedAt: old},  // starting: slow start
		{RunnerName: rGood2, StartedAt: old}, // orphan
		{RunnerName: "../x", StartedAt: old}, // invalid
	}, alive: true}
	d := testDaemon(t, b, &fakeAPI{})
	d.starting[rGood] = true
	d.reconcile(context.Background())
	if len(b.cleaned) != 1 || b.cleaned[0] != rGood2 {
		t.Fatalf("cleaned %v", b.cleaned)
	}
}

func TestKillHungStopsRunnerWithoutCancellingRun(t *testing.T) {
	b := &fakeBackend{alive: true}
	api := &fakeAPI{}
	d := testDaemon(t, b, api)
	rr := &runnerRec{name: rGood, lane: scheduler.LaneKey{ScaleSetKey: core.ScaleSetKey{OS: core.Linux}, Lane: 1}, runID: 5, owner: "acme", repo: "app", jobAt: time.Now()}
	d.runners[rGood] = rr
	d.killHung(context.Background(), rr, "idle")
	if len(b.stopped) != 1 {
		t.Fatal("runner not stopped")
	}
	if api.cancelled+api.forced+api.rerun != 0 {
		t.Fatal("hang handling must not touch the run")
	}
	if !rr.killed || rr.goneAt.IsZero() {
		t.Fatal("not marked gone")
	}
}

func TestStaleBranchDeleteSparesTags(t *testing.T) {
	api := &fakeAPI{run: gh.Run{HeadBranch: "v1.2.3"}}
	d := testDaemon(t, &fakeBackend{}, api)
	d.cfg.Stale.CancelDeletedBranch = true
	ctx := context.Background()
	if r := d.staleReason(ctx, "o", "r", "push", 1); r == "" {
		t.Fatal("deleted branch with no tag must be stale")
	}
	api.tag = true
	if r := d.staleReason(ctx, "o", "r", "push", 1); r != "" {
		t.Fatal("tag push must not be cancelled")
	}
	api.tag, api.branch = false, true
	if r := d.staleReason(ctx, "o", "r", "push", 1); r != "" {
		t.Fatal("existing branch is not stale")
	}
}

func TestEventsOutliveFirstJobAndExpire(t *testing.T) {
	d := testDaemon(t, &fakeBackend{}, &fakeAPI{})
	d.mu.Lock()
	d.noteEvent(7, "pull_request")
	d.mu.Unlock()
	d.JobFinished(scheduler.JobResult{Job: core.Job{RunID: 7}, Finished: time.Now()})
	d.mu.Lock()
	if d.events[7].event != "pull_request" {
		t.Fatal("event dropped when the first job finished")
	}
	d.events[7] = eventRec{event: "push", seen: time.Now().Add(-2 * eventTTL)}
	d.evPruned = time.Time{}
	d.noteEvent(8, "push")
	_, stale := d.events[7]
	d.mu.Unlock()
	if stale {
		t.Fatal("expired event kept")
	}
}

type persistentBackend struct {
	*fakeBackend
	cs  []core.PersistentContainer
	err error
}

func (p *persistentBackend) Persistent(context.Context) ([]core.PersistentContainer, error) {
	return p.cs, p.err
}

func TestPersistentMemoryIsReservedExternally(t *testing.T) {
	pb := &persistentBackend{fakeBackend: &fakeBackend{alive: true}, cs: []core.PersistentContainer{{Name: "dev-1", Mem: 300}, {Name: "dev-2", Mem: 200}}}
	d := testDaemon(t, pb.fakeBackend, &fakeAPI{})
	d.backends[core.Linux] = pb
	d.cfg.Limits.ExternalReserved = 1000

	d.reservePersistent(context.Background())
	if got := d.sched.Snapshot().Ledger.External; got != 1500 {
		t.Fatalf("external %d", got)
	}
	if got := len(d.status(context.Background()).Persistent); got != 2 {
		t.Fatalf("status persistent %d", got)
	}

	// A container going away frees its share; a failed sample changes nothing.
	pb.cs = pb.cs[:1]
	d.reservePersistent(context.Background())
	if got := d.sched.Snapshot().Ledger.External; got != 1300 {
		t.Fatalf("external %d", got)
	}
	pb.err = errors.New("boom")
	d.reservePersistent(context.Background())
	if got := d.sched.Snapshot().Ledger.External; got != 1300 {
		t.Fatalf("external after failed sample %d", got)
	}
}

func TestAutoCacheMountsAndEnv(t *testing.T) {
	cfg := &config.Config{Instance: "main", Linux: config.Linux{Enabled: true}, AutoCache: config.AutoCache{Scope: "org"},
		Caches: []config.Cache{{Name: "pub", Env: "PUB_CACHE"}}}
	d := &Daemon{cfg: cfg, log: slog.New(slog.DiscardHandler)}
	lane := scheduler.LaneKey{ScaleSetKey: core.ScaleSetKey{Target: core.Target{Owner: "org"}, OS: core.Linux}, Lane: 1}
	spec := core.RunnerSpec{Env: map[string]string{}}
	if s := d.autoCache(&spec, lane, scheduler.Grant{Host: "pc", Job: core.Job{Owner: "org", Repo: "app"}}); s != "org" {
		t.Fatalf("scope %q", s)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].ContainerPath != "/opt/rw-cache" || !strings.HasSuffix(spec.Mounts[0].HostPath, "/main/auto/org") {
		t.Fatalf("%+v", spec.Mounts)
	}
	if _, ok := spec.Env["PUB_CACHE"]; ok || spec.Env["NUGET_PACKAGES"] == "" {
		t.Fatalf("[[cache]] env must win: %v", spec.Env)
	}
	win := lane
	win.OS = core.Windows
	spec2 := core.RunnerSpec{Env: map[string]string{}}
	cfg.AutoCache.Scope = "off"
	if d.autoCache(&spec2, lane, scheduler.Grant{}); len(spec2.Mounts) != 0 {
		t.Fatal("off means off")
	}
	cfg.AutoCache.Scope = "repo"
	if d.autoCache(&spec2, win, scheduler.Grant{}); len(spec2.Mounts) != 0 {
		t.Fatal("Linux only")
	}
}
