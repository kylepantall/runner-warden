package winvm

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

type notFoundErr struct{}

func (notFoundErr) Error() string { return "not found" }
func (notFoundErr) NotFound()     {}

type fakeCtr struct {
	id      string
	cfg     *container.Config
	hc      *container.HostConfig
	running bool
	mounts  []container.MountPoint
}

type fakeDocker struct {
	mu        sync.Mutex
	ctrs      map[string]*fakeCtr
	created   []string
	started   []string
	stopped   []string
	restarted []string
	removed   []string
	statsCPU  uint64
	stopErr   error
}

func newFakeDocker() *fakeDocker { return &fakeDocker{ctrs: map[string]*fakeCtr{}} }

func (f *fakeDocker) find(id string) *fakeCtr {
	for n, c := range f.ctrs {
		if n == id || c.id == id {
			return c
		}
	}
	return nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctrs[name] = &fakeCtr{id: "id-" + name, cfg: c, hc: hc}
	f.created = append(f.created, name)
	return container.CreateResponse{ID: "id-" + name}, nil
}
func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(id); c != nil {
		c.running = true
	}
	f.started = append(f.started, id)
	return nil
}
func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return container.InspectResponse{}, notFoundErr{}
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: c.id, Name: "/" + id, State: &container.State{Running: c.running}},
		Config:            c.cfg, Mounts: c.mounts,
	}, nil
}
func (f *fakeDocker) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return nil, nil
}
func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	if c := f.find(id); c != nil {
		c.running = false
	}
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeDocker) ContainerRestart(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, id)
	return nil
}
func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, c := range f.ctrs {
		if c.id == id {
			delete(f.ctrs, n)
		}
	}
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeDocker) ContainerStatsOneShot(context.Context, string) (container.StatsResponseReader, error) {
	return container.StatsResponseReader{}, io.EOF
}
func (f *fakeDocker) ImageInspect(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error) {
	return image.InspectResponse{}, nil
}
func (f *fakeDocker) ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

type call struct{ script, stdin string }

type fakeExec struct {
	mu      sync.Mutex
	calls   []call
	handler func(script, stdin string) (string, string, int, error)
}

func (f *fakeExec) Run(ctx context.Context, s string) (string, string, int, error) {
	return f.RunStdin(ctx, s, "")
}
func (f *fakeExec) RunStdin(_ context.Context, s, in string) (string, string, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{s, in})
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		return h(s, in)
	}
	return defaultGuest(s)
}

func defaultGuest(s string) (string, string, int, error) {
	switch {
	case strings.Contains(s, "rw-ready"):
		return "rw-ready", "", 0, nil
	case strings.Contains(s, "state=$state"):
		return "state=Running procs=2 log=100 diag=50", "", 0, nil
	case strings.Contains(s, "leftover="):
		return "leftover=0", "", 0, nil
	case strings.Contains(s, "rw-install.done"):
		return "rw-done", "", 0, nil
	case strings.Contains(s, "actions.runner.*"):
		return "service=actions.runner.org.old\n", "", 0, nil
	}
	return "ok", "", 0, nil
}

func (f *fakeExec) scripts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.script)
	}
	return out
}

func (f *fakeExec) find(sub string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.script, sub) {
			return c, true
		}
	}
	return call{}, false
}

type fixture struct {
	t    *testing.T
	pool *Pool
	api  *fakeDocker
	ex   *fakeExec
	dir  string
}

func newFixture(t *testing.T, maxConc int, vms ...config.WindowsVM) *fixture {
	t.Helper()
	dir := t.TempDir()
	if len(vms) == 0 {
		vms = []config.WindowsVM{{Name: "a", Targets: []string{"*"}}, {Name: "b", Targets: []string{"*"}}, {Name: "c", Targets: []string{"*"}}}
	}
	for i := range vms {
		v := &vms[i]
		v.StorageDir = filepath.Join(dir, "vm", v.Name, "storage")
		v.OEMDir = filepath.Join(dir, "vm", v.Name, "oem")
		v.Mem, v.CPUs, v.Version, v.DiskSize, v.SSHPort = 8<<30, 4, "11", "96G", 2222+i
	}
	api := newFakeDocker()
	ex := &fakeExec{}
	p, err := newPool(Config{
		Instance: "main", HostID: "h1", StateDir: filepath.Join(dir, "state"), Slice: "runner-warden-main.slice",
		SSHReadyTimeout: 50 * time.Millisecond, StopSSHTimeout: 20 * time.Millisecond, SSHPoll: time.Millisecond,
		Windows: config.Windows{
			Enabled: true, MaxConcurrent: maxConc, Image: "dockurr/windows", SSHUser: "runner",
			SSHKeyFile: filepath.Join(dir, "key"), RunnerDir: `C:\actions-runner`, WorkDir: `C:\rw`,
			RunnerVersion: "2.999.0", IdleTTL: config.Duration(15 * time.Minute), VMs: vms,
		},
	}, api, func(config.WindowsVM) (Exec, error) { return ex, nil })
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, pool: p, api: api, ex: ex, dir: dir}
}

func (f *fixture) setUp(vm string, idleSince time.Time) {
	f.pool.mu.Lock()
	f.pool.up[vm], f.pool.ready[vm], f.pool.lastActive[vm] = true, true, idleSince
	f.pool.mu.Unlock()
}

func TestCapsAndPick(t *testing.T) {
	f := newFixture(t, 2)
	c := f.pool.Caps()
	if c.OS != core.Windows || c.MaxConcurrent != 2 || c.FixedMemBytes != 8<<30 || c.DockerInJob {
		t.Fatalf("%+v", c)
	}
	tgt := core.Target{Owner: "o"}
	f.setUp("b", time.Now())
	if vm, ok := f.pool.Pick(tgt); !ok || vm != "b" {
		t.Fatalf("want running VM b, got %q %v", vm, ok)
	}
	// b is claimed now: next pick boots a cold VM (second licence).
	vm2, ok := f.pool.Pick(tgt)
	if !ok || vm2 == "b" {
		t.Fatalf("got %q %v", vm2, ok)
	}
	// Both licences used: a third pick must fail.
	if vm, ok := f.pool.Pick(tgt); ok {
		t.Fatalf("exceeded max_concurrent with %q", vm)
	}
	f.pool.Release("b")
	if vm, ok := f.pool.Pick(tgt); !ok || vm != "b" {
		t.Fatalf("released VM should be pickable: %q %v", vm, ok)
	}
}

func TestPickRespectsTargets(t *testing.T) {
	f := newFixture(t, 3, config.WindowsVM{Name: "a", Targets: []string{"acme/app"}}, config.WindowsVM{Name: "b", Targets: []string{"other"}})
	vm, ok := f.pool.Pick(core.Target{Owner: "acme", Repo: "app"})
	if !ok || vm != "a" {
		t.Fatalf("%q %v", vm, ok)
	}
	if _, ok := f.pool.Pick(core.Target{Owner: "acme", Repo: "app"}); ok {
		t.Fatal("only VM a serves acme/app and it is claimed")
	}
	if _, ok := f.pool.Pick(core.Target{Owner: "nobody"}); ok {
		t.Fatal("no VM serves nobody")
	}
}

func TestEnsureUpCreatesThenStarts(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	if err := f.pool.EnsureUp(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(f.api.created) != 1 || f.api.created[0] != "main-win-a" {
		t.Fatalf("created %v", f.api.created)
	}
	c := f.api.ctrs["main-win-a"]
	l := c.cfg.Labels
	if l[naming.MetaManaged] != "1" || l[naming.MetaInstance] != "main" || l[naming.MetaRole] != "vm" || l[naming.MetaRunner] != "" {
		t.Fatalf("labels %v", l)
	}
	if _, has := l[naming.MetaRunner]; !has {
		t.Fatal("runner label must exist (empty)")
	}
	if c.hc.Resources.CgroupParent != "runner-warden-main.slice" {
		t.Fatal("cgroup parent")
	}
	if len(c.hc.Binds) != 2 || !strings.HasSuffix(c.hc.Binds[0], ":/storage") || !strings.HasSuffix(c.hc.Binds[1], ":/oem") {
		t.Fatalf("binds %v", c.hc.Binds)
	}
	var pb string
	for _, v := range c.hc.PortBindings {
		pb = v[0].HostIP + ":" + v[0].HostPort
	}
	if pb != "127.0.0.1:2222" {
		t.Fatalf("port %s", pb)
	}
	if !f.pool.Up("a") {
		t.Fatal("not up")
	}
	// Already up and ready: no second SSH probe, no second create.
	n := len(f.ex.calls)
	if err := f.pool.EnsureUp(ctx, "a"); err != nil || len(f.ex.calls) != n {
		t.Fatal("repeat EnsureUp should be a no-op")
	}
	// Stop it, then EnsureUp starts the existing container, no create.
	if err := f.pool.StopNow(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.EnsureUp(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(f.api.created) != 1 || len(f.api.removed) != 0 || len(f.api.started) != 2 {
		t.Fatalf("created=%v removed=%v started=%v", f.api.created, f.api.removed, f.api.started)
	}
}

func TestEnsureUpRecreatesOnConfigDrift(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	if err := f.pool.EnsureUp(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.StopNow(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	v := f.pool.vms["a"]
	v.Mem = 16 << 30
	f.pool.vms["a"] = v
	if err := f.pool.EnsureUp(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(f.api.created) != 2 || len(f.api.removed) != 1 {
		t.Fatalf("created=%v removed=%v", f.api.created, f.api.removed)
	}
}

func TestEnsureUpRefusesForeignAndLicenceCap(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	f.api.ctrs["main-win-a"] = &fakeCtr{id: "x", cfg: &container.Config{Labels: map[string]string{}}, running: true}
	if err := f.pool.EnsureUp(ctx, "a"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("got %v", err)
	}
	if len(f.api.started)+len(f.api.stopped)+len(f.api.removed) != 0 {
		t.Fatal("touched a container without our labels")
	}
	if err := f.pool.StopNow(ctx, "a"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("got %v", err)
	}
	f.setUp("b", time.Now())
	if err := f.pool.EnsureUp(ctx, "c"); !errors.Is(err, ErrLicenceCap) {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureUpSSHTimeoutAndExit(t *testing.T) {
	f := newFixture(t, 3)
	f.ex.handler = func(string, string) (string, string, int, error) { return "", "", -1, errors.New("refused") }
	if err := f.pool.EnsureUp(context.Background(), "a"); !errors.Is(err, ErrSSHTimeout) {
		t.Fatalf("got %v", err)
	}
	// Container dies while we wait: fail fast, not after the timeout.
	f.pool.cfg.SSHReadyTimeout = time.Hour
	f.ex.handler = func(string, string) (string, string, int, error) {
		f.api.mu.Lock()
		f.api.ctrs["main-win-b"].running = false
		f.api.mu.Unlock()
		return "", "", -1, errors.New("refused")
	}
	if err := f.pool.EnsureUp(context.Background(), "b"); !errors.Is(err, ErrVMExited) {
		t.Fatalf("got %v", err)
	}
	if f.pool.Up("b") {
		t.Fatal("exited VM must not count as up")
	}
}

func TestStopIdleAndStopNow(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		if err := f.pool.EnsureUp(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Add(time.Hour)
	f.pool.mu.Lock()
	f.pool.runners["r1"] = &runnerRec{Name: "r1", VM: "b"}
	f.pool.mu.Unlock()

	if got := f.pool.StopIdle(ctx, time.Now()); len(got) != 0 {
		t.Fatalf("not idle long enough: %v", got)
	}
	got := f.pool.StopIdle(ctx, now)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("stopped %v (b has a runner)", got)
	}
	if f.pool.Up("a") || !f.pool.Up("b") {
		t.Fatal("wrong up state")
	}
	if err := f.pool.StopNow(ctx, "b"); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	if err := f.pool.StopNow(ctx, "zzz"); !errors.Is(err, ErrUnknownVM) {
		t.Fatalf("got %v", err)
	}
}

func spec(name, vm string) core.RunnerSpec {
	return core.RunnerSpec{Name: name, JITConfig: "SECRET-JIT-BLOB", Env: map[string]string{EnvVM: vm},
		ScaleSet: core.ScaleSetKey{Target: core.Target{Owner: "o"}, OS: core.Windows}}
}

func TestStartCommands(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	h, err := f.pool.Start(ctx, spec("rw-h1-abcd", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if h.BackendID != "a" || h.RunnerName != "rw-h1-abcd" {
		t.Fatalf("%+v", h)
	}
	for _, c := range f.ex.calls {
		if strings.Contains(c.script, "SECRET-JIT-BLOB") {
			t.Fatal("JIT config leaked into a script")
		}
	}
	rs, ok := f.ex.find("actions-runner-win-x64")
	if !ok || !strings.Contains(rs.script, "https://github.com/actions/runner/releases/download/v2.999.0/actions-runner-win-x64-2.999.0.zip") ||
		!strings.Contains(rs.script, `C:\actions-runner`) {
		t.Fatal("runner install script")
	}
	js, ok := f.ex.find("Register-ScheduledTask")
	if !ok {
		t.Fatal("no task registration")
	}
	if js.stdin != "SECRET-JIT-BLOB" {
		t.Fatal("JIT config must arrive on stdin")
	}
	for _, want := range []string{
		`'rw-rw-h1-abcd'`, `C:\rw\rw-h1-abcd\jit.cfg`, `C:\rw\rw-h1-abcd\run-task.cmd`, `C:\rw\rw-h1-abcd\runner.log`,
		`C:\rw\rw-h1-abcd\_work`, `C:\actions-runner\run.cmd`, `-RunLevel Highest`, `-LogonType Interactive`,
		`ACTIONS_RUNNER_INPUT_JITCONFIG`, `Start-ScheduledTask`,
	} {
		if !strings.Contains(js.script, want) {
			t.Errorf("start script lacks %s", want)
		}
	}
	if strings.Contains(js.script, "--jitconfig") {
		t.Error("config must not be on a command line")
	}
	if f.pool.WorkFolder("a", "rw-h1-abcd") != `C:\rw\rw-h1-abcd\_work` {
		t.Fatal("work folder")
	}
	if vm, ok := f.pool.VMFor("rw-h1-abcd"); !ok || vm != "a" {
		t.Fatal("VMFor")
	}
	// State persisted for adoption after a restart.
	hs, _ := f.pool.List(ctx)
	if len(hs) != 1 {
		t.Fatalf("list %v", hs)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "state", "windows", "a", "runners.json")); err != nil {
		t.Fatal(err)
	}
	// A second runner on the busy VM is refused.
	if _, err := f.pool.Start(ctx, spec("other", "a")); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	// Without the env, Start picks another VM.
	s2 := spec("picked", "")
	s2.Env = nil
	h2, err := f.pool.Start(ctx, s2)
	if err != nil || h2.BackendID == "a" {
		t.Fatalf("%+v %v", h2, err)
	}
	if _, err := f.pool.Start(ctx, spec("bad name;rm", "c")); !errors.Is(err, ErrBadRunner) {
		t.Fatalf("got %v", err)
	}
}

func TestStartFailureCleansUp(t *testing.T) {
	f := newFixture(t, 3)
	f.ex.handler = func(s, in string) (string, string, int, error) {
		if strings.Contains(s, "Register-ScheduledTask") {
			return "", "boom", 1, nil
		}
		return defaultGuest(s)
	}
	if _, err := f.pool.Start(context.Background(), spec("r1", "a")); err == nil {
		t.Fatal("expected failure")
	}
	if _, ok := f.ex.find("leftover="); !ok {
		t.Fatal("cleanup script not run")
	}
	if hs, _ := f.pool.List(context.Background()); len(hs) != 0 {
		t.Fatal("runner still recorded")
	}
}

func TestCleanupIdempotent(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	if err := f.pool.Cleanup(ctx, "never-started"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Start(ctx, spec("r1", "a")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := f.pool.Cleanup(ctx, "r1"); err != nil {
			t.Fatal(err)
		}
	}
	cs, ok := f.ex.find("leftover=")
	if !ok {
		t.Fatal("no cleanup")
	}
	for _, want := range []string{"Unregister-ScheduledTask", `C:\rw\r1`, "Remove-Item", "rd /s /q", "$env:TEMP", "_diag", "AddDays(-7)", "taskkill.exe /T /F"} {
		if !strings.Contains(cs.script, want) {
			t.Errorf("cleanup lacks %s", want)
		}
	}
	// Leftover reported: error, runner kept for retry.
	if _, err := f.pool.Start(ctx, spec("r2", "a")); err != nil {
		t.Fatal(err)
	}
	f.ex.handler = func(string, string) (string, string, int, error) { return "leftover=1", "", 0, nil }
	if err := f.pool.Cleanup(ctx, "r2"); err == nil {
		t.Fatal("expected error while the work dir is locked")
	}
	if hs, _ := f.pool.List(ctx); len(hs) != 1 {
		t.Fatal("runner must stay listed until cleaned")
	}
}

func TestStopEscalatesToRestart(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	h, err := f.pool.Start(ctx, spec("r1", "a"))
	if err != nil {
		t.Fatal(err)
	}
	// Healthy guest: no restart.
	if err := f.pool.Stop(ctx, h, 5*time.Second); err != nil || len(f.api.restarted) != 0 {
		t.Fatalf("%v %v", err, f.api.restarted)
	}
	f.ex.handler = func(string, string) (string, string, int, error) { return "", "", -1, errors.New("ssh down") }
	if err := f.pool.Stop(ctx, h, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.api.restarted) != 1 || f.api.restarted[0] != "id-main-win-a" {
		t.Fatalf("restarted %v", f.api.restarted)
	}
	if f.pool.ready["a"] {
		t.Fatal("ready flag must reset after a restart")
	}
	// Cleanup after a restart needs no guest access.
	n := len(f.ex.calls)
	if err := f.pool.Cleanup(ctx, "r1"); err != nil || len(f.ex.calls) != n {
		t.Fatalf("%v", err)
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	h, err := f.pool.Start(ctx, spec("r1", "a"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.pool.Stats(ctx, h)
	if err != nil || !u.Alive || u.LogBytes != 150 {
		t.Fatalf("%+v %v", u, err)
	}
	f.ex.handler = func(string, string) (string, string, int, error) {
		return "state=Ready procs=0 log=1 diag=1", "", 0, nil
	}
	if u, _ := f.pool.Stats(ctx, h); u.Alive {
		t.Fatal("runner finished: not alive")
	}
	f.ex.handler = func(string, string) (string, string, int, error) { return "", "", -1, errors.New("slow") }
	u, err = f.pool.Stats(ctx, h)
	if err != nil || !u.Alive || u.LogBytes != 2 {
		t.Fatalf("guest down: fall back to container state and last log size: %+v %v", u, err)
	}
}

func TestPrepareLoadsState(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	if _, err := f.pool.Start(ctx, spec("r1", "a")); err != nil {
		t.Fatal(err)
	}
	p2, err := newPool(f.pool.cfg, f.api, func(config.WindowsVM) (Exec, error) { return f.ex, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	hs, _ := p2.List(ctx)
	if len(hs) != 1 || hs[0].RunnerName != "r1" || !p2.Up("a") {
		t.Fatalf("%v up=%v", hs, p2.Up("a"))
	}
	if _, ok := p2.Pick(core.Target{Owner: "o"}); !ok {
		t.Fatal("other VMs stay pickable")
	}
	if vm, _ := p2.Pick(core.Target{Owner: "o"}); vm == "a" {
		t.Fatal("busy VM picked")
	}
}

func TestRunnerVersionResolved(t *testing.T) {
	f := newFixture(t, 3)
	f.pool.version = ""
	srv := httptestServer(`{"tag_name":"v2.400.1"}`)
	defer srv.Close()
	f.pool.cfg.ReleaseAPI = srv.URL
	v, err := f.pool.runnerVersion(context.Background())
	if err != nil || v != "2.400.1" {
		t.Fatalf("%q %v", v, err)
	}
	srv.Close()
	if v2, err := f.pool.runnerVersion(context.Background()); err != nil || v2 != v {
		t.Fatal("version must be cached")
	}
}

func TestParseGuestStats(t *testing.T) {
	g, err := parseGuestStats("state=Running procs=0 log=5 diag=7\n")
	if err != nil || !g.Alive() || g.Log != 5 || g.Diag != 7 {
		t.Fatalf("%+v %v", g, err)
	}
	if g, _ := parseGuestStats("state=Ready procs=0 log=0 diag=0"); g.Alive() {
		t.Fatal("ready & no procs is dead")
	}
	if _, err := parseGuestStats("garbage"); err == nil {
		t.Fatal("expected error")
	}
}

func TestPSQuoting(t *testing.T) {
	if psq("a'b") != "'a''b'" {
		t.Fatal(psq("a'b"))
	}
	if winJoin(`C:\rw\`, "x", `\y`) != `C:\rw\x\y` {
		t.Fatal(winJoin(`C:\rw\`, "x", `\y`))
	}
}
