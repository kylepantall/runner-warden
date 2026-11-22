package docker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

type fakeAPI struct {
	builds       []string
	buildErr     string
	pulls        []string
	pullErr      error
	missing      map[string]bool // image refs ImageInspect reports absent
	inspectBinds []string
	mu           sync.Mutex
	created      []*container.Config
	hostCfgs     []*container.HostConfig
	names        []string
	started      []string
	removed      []string
	stopped      []string
	nets         []network.Summary
	vols         []volume.Volume
	netRemove    []string
	volRemove    []string
	conts        []container.Summary // returned by every ContainerList
	running      bool
	lastList     []container.ListOptions
}

func (f *fakeAPI) ContainerCreate(_ context.Context, c *container.Config, hc *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created, f.hostCfgs, f.names = append(f.created, c), append(f.hostCfgs, hc), append(f.names, name)
	return container.CreateResponse{ID: "cid" + name}, nil
}
func (f *fakeAPI) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	f.started = append(f.started, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
		State: &container.State{Running: f.running}, HostConfig: &container.HostConfig{Binds: f.inspectBinds},
	}}, nil
}
func (f *fakeAPI) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastList = append(f.lastList, o)
	return f.conts, nil
}
func (f *fakeAPI) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) ContainerKill(context.Context, string, string) error { return nil }
func (f *fakeAPI) ContainerRemove(_ context.Context, id string, o container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !o.Force || !o.RemoveVolumes {
		return io.ErrUnexpectedEOF
	}
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeAPI) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	c := make(chan container.WaitResponse, 1)
	c <- container.WaitResponse{}
	return c, make(chan error)
}
func (f *fakeAPI) ContainerStatsOneShot(context.Context, string) (container.StatsResponseReader, error) {
	return container.StatsResponseReader{}, io.EOF
}
func (f *fakeAPI) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return f.nets, nil
}
func (f *fakeAPI) NetworkRemove(_ context.Context, id string) error {
	f.netRemove = append(f.netRemove, id)
	return nil
}
func (f *fakeAPI) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	vs := make([]*volume.Volume, len(f.vols))
	for i := range f.vols {
		vs[i] = &f.vols[i]
	}
	return volume.ListResponse{Volumes: vs}, nil
}
func (f *fakeAPI) VolumeRemove(_ context.Context, id string, _ bool) error {
	f.volRemove = append(f.volRemove, id)
	return nil
}
func (f *fakeAPI) ImageBuild(_ context.Context, ctx io.Reader, o build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(ctx)
	f.builds = append(f.builds, o.Tags[0]+"\n"+string(b))
	if f.buildErr != "" {
		return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"error":"` + f.buildErr + `"}`))}, nil
	}
	delete(f.missing, o.Tags[0])
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"stream":"done"}`))}, nil
}

func (f *fakeAPI) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, ref)
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeAPI) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	if f.missing[ref] {
		return image.InspectResponse{}, errors.New("no such image")
	}
	return image.InspectResponse{}, nil
}

const testRunner = "my-ci-h1-0000abcd"

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "rwb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func testBackend(t *testing.T, api dockerAPI) *Backend {
	t.Helper()
	hookBase := shortDir(t)
	b, err := newBackend(Config{
		Instance: "main", Prefix: "my-ci", HostID: "h1", StateDir: shortDir(t),
		Image: "runner:img", DockerSocket: filepath.Join(shortDir(t), "docker.sock"),
		Slice: "runner-warden-main.slice", RunnerUID: 1001, TmpfsSize: 1 << 30, OOMScoreAdj: 500,
		HookBinary: "/usr/bin/rw", HookDir: hookBase,
		CacheBinds: []string{"/cache/go:/go/pkg"}, CacheEnv: map[string]string{"GOMODCACHE": "/go/pkg"},
	}, api)
	if err != nil {
		t.Fatal(err)
	}
	b.statGID = func(string) (int, error) { return 998, nil }
	b.cgroupRoot = shortDir(t)
	return b
}

func TestCaps(t *testing.T) {
	c := testBackend(t, &fakeAPI{}).Caps()
	if c.OS != core.Linux || c.MemLimits || !c.DockerInJob {
		t.Fatalf("%+v", c)
	}
}

func TestBuildRunnerContainer(t *testing.T) {
	b := testBackend(t, &fakeAPI{})
	spec := core.RunnerSpec{
		Name: testRunner, JITConfig: "JIT", CPUSet: "2-3",
		Env:     map[string]string{"FOO": "bar"},
		Mounts:  []core.Mount{{HostPath: "/data", ContainerPath: "/data", ReadOnly: true}},
		Devices: []string{"/dev/kvm"},
	}
	dir, sock := b.runnerDir(spec.Name), b.proxySock(spec.Name)
	cfg, hc := b.buildRunnerContainer(spec, dir, sock, 998)

	wantLabels := map[string]string{
		naming.MetaManaged: "1", naming.MetaInstance: "main", naming.MetaHostID: "h1",
		naming.MetaRunner: spec.Name, naming.MetaRole: "runner",
	}
	for k, v := range wantLabels {
		if cfg.Labels[k] != v {
			t.Errorf("label %s=%s", k, cfg.Labels[k])
		}
	}
	env := map[string]string{}
	for _, e := range cfg.Env {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	checks := map[string]string{
		"DOCKER_HOST":                       "unix://" + sock,
		"ACTIONS_RUNNER_HOOK_JOB_STARTED":   filepath.Join(dir, hookDir, "job-started.sh"),
		"ACTIONS_RUNNER_HOOK_JOB_COMPLETED": filepath.Join(dir, hookDir, "job-completed.sh"),
		"RUNNER_WARDEN_SOCKET":              filepath.Join(b.cfg.HookDir, testRunner, "hook.sock"),
		"RUNNER_WARDEN_RUNNER":              spec.Name,
		"FOO":                               "bar",
		"GOMODCACHE":                        "/go/pkg",
	}
	for k, v := range checks {
		if env[k] != v {
			t.Errorf("env %s=%q want %q", k, env[k], v)
		}
	}
	for _, e := range cfg.Env {
		if strings.Contains(e, "JIT") && strings.HasPrefix(e, "ACTIONS_RUNNER_INPUT_JITCONFIG") {
			t.Error("JIT config must not be in the container env")
		}
	}
	binds := map[string]bool{}
	for _, s := range hc.Binds {
		binds[s] = true
	}
	for _, want := range []string{
		dir + ":" + dir,
		"/usr/bin/rw:/usr/bin/rw:ro", b.hookDir(testRunner) + ":" + b.hookDir(testRunner),
		"/data:/data:ro", "/cache/go:/go/pkg",
	} {
		if !binds[want] {
			t.Errorf("missing bind %s in %v", want, hc.Binds)
		}
	}
	if binds[b.cfg.HookDir+":"+b.cfg.HookDir] {
		t.Error("the shared hook base must not be mounted")
	}
	if hc.CgroupParent != "runner-warden-main.slice" || hc.OomScoreAdj != 500 || hc.CpusetCpus != "2-3" {
		t.Errorf("hostconfig %+v", hc.Resources)
	}
	if hc.Memory != 0 || hc.MemorySwap != 0 {
		t.Error("runner must not get a memory limit")
	}
	// PID 1 is root; the runner drops to uid 1001 via setpriv.
	if len(hc.GroupAdd) != 1 || hc.GroupAdd[0] != "998" || cfg.User != "0" ||
		len(cfg.Entrypoint) < 2 || cfg.Entrypoint[0] != "/usr/bin/setpriv" || cfg.Entrypoint[1] != "--reuid=1001" {
		t.Errorf("group/user %v %s %v", hc.GroupAdd, cfg.User, cfg.Entrypoint)
	}
	if len(hc.Devices) != 1 || hc.Devices[0].PathOnHost != "/dev/kvm" {
		t.Errorf("devices %v", hc.Devices)
	}
	if opts, ok := hc.Tmpfs["/tmp"]; !ok || !strings.Contains(opts, "exec") || strings.Contains(opts, "noexec") {
		t.Error("no /tmp tmpfs")
	}
	if cfg.WorkingDir != dir || cfg.Entrypoint[len(cfg.Entrypoint)-1] != dir {
		t.Error("runner dir must be the same path in the container")
	}
}

func TestNoHooksRunner(t *testing.T) {
	b := testBackend(t, &fakeAPI{})
	b.cfg.NoHooks = true
	spec := core.RunnerSpec{Name: testRunner, JITConfig: "JIT"}
	cfg, hc := b.buildRunnerContainer(spec, b.runnerDir(spec.Name), b.proxySock(spec.Name), 0)
	for _, e := range cfg.Env {
		if strings.HasPrefix(e, "ACTIONS_RUNNER_HOOK_") || strings.HasPrefix(e, "RUNNER_WARDEN_SOCKET=") {
			t.Errorf("hook env %s", e)
		}
	}
	for _, bd := range hc.Binds {
		if strings.HasPrefix(bd, b.cfg.HookBinary+":") || strings.HasPrefix(bd, b.hookDir(testRunner)+":") {
			t.Errorf("hook bind %s", bd)
		}
	}
	if _, err := b.Start(context.Background(), spec); err != nil {
		t.Fatalf("no hook dir is needed without hooks: %v", err)
	}
}

func TestHookWrapperQuoting(t *testing.T) {
	s := hookWrapper("/opt/it's here/rw", "job-started")
	if !strings.HasPrefix(s, "#!/bin/sh\n") || !strings.Contains(s, `'/opt/it'\''s here/rw' hook job-started`) {
		t.Fatalf("%q", s)
	}
}

func TestStartAndCleanup(t *testing.T) {
	api := &fakeAPI{}
	b := testBackend(t, api)
	spec := core.RunnerSpec{Name: testRunner, JITConfig: "J"}
	if _, err := b.Start(context.Background(), spec); err == nil {
		t.Fatal("start without the daemon-made hook dir must fail")
	}
	if err := os.MkdirAll(b.hookDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.BackendID != "cid"+spec.Name || len(api.started) != 1 || api.names[0] != spec.Name {
		t.Fatalf("handle %+v started %v", h, api.started)
	}
	jit, err := os.ReadFile(filepath.Join(b.runnerDir(spec.Name), ".jit"))
	if err != nil || string(jit) != "J" {
		t.Errorf("jit file: %v", err)
	}
	for _, f := range []string{"job-started.sh", "job-completed.sh"} {
		st, err := os.Stat(filepath.Join(b.runnerDir(spec.Name), hookDir, f))
		if err != nil || st.Mode()&0o111 == 0 {
			t.Errorf("wrapper %s: %v", f, err)
		}
	}
	if _, err := os.Stat(b.proxySock(spec.Name)); err != nil {
		t.Errorf("proxy socket: %v", err)
	}

	api.conts = []container.Summary{{ID: "c1"}, {ID: "c2"}}
	api.nets = []network.Summary{{ID: "n1"}}
	api.vols = []volume.Volume{{Name: "v1"}}
	for i := 0; i < 2; i++ { // idempotent
		if err := b.Cleanup(context.Background(), spec.Name); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.removed) != 4 || len(api.netRemove) != 2 || len(api.volRemove) != 2 {
		t.Errorf("removed %v nets %v vols %v", api.removed, api.netRemove, api.volRemove)
	}
	if _, err := os.Stat(b.runnerDir(spec.Name)); !os.IsNotExist(err) {
		t.Error("runner dir not removed")
	}
	if _, err := os.Stat(b.proxySock(spec.Name)); !os.IsNotExist(err) {
		t.Error("socket not removed")
	}
	// Cleanup of a runner that never started must not fail.
	if err := b.Cleanup(context.Background(), "my-ci-h1-0000beef"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidRunnerNamesRefused(t *testing.T) {
	api := &fakeAPI{}
	b := testBackend(t, api)
	victim := filepath.Join(b.runnersDir(), "victim")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", "..", "../victim", "victim", "my-ci-h1-ZZZZZZZZ"} {
		if err := b.Cleanup(context.Background(), n); err == nil {
			t.Errorf("cleanup accepted %q", n)
		}
		if _, err := b.Start(context.Background(), core.RunnerSpec{Name: n, JITConfig: "J"}); err == nil && n != "" {
			t.Errorf("start accepted %q", n)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("cleanup touched a directory for an invalid name")
	}
	if len(api.removed)+len(api.started) != 0 {
		t.Fatal("docker calls made for invalid names")
	}
}

func TestListFiltersByLabel(t *testing.T) {
	api := &fakeAPI{conts: []container.Summary{{ID: "x", Created: 100, Labels: map[string]string{naming.MetaRunner: "my-ci-h1-0000000a"}}, {ID: "evil", Labels: map[string]string{naming.MetaRunner: "../../etc"}}}}
	b := testBackend(t, api)
	hs, err := b.List(context.Background())
	if err != nil || len(hs) != 1 || hs[0].RunnerName != "my-ci-h1-0000000a" || hs[0].BackendID != "x" {
		t.Fatalf("%v %v", hs, err)
	}
	f := api.lastList[0].Filters
	for _, want := range []string{naming.MetaInstance + "=main", naming.MetaRole + "=runner", naming.MetaManaged + "=1"} {
		if !f.ExactMatch("label", want) {
			t.Errorf("filter missing %s", want)
		}
	}
	if !api.lastList[0].All {
		t.Error("List must include stopped runners")
	}
}

func TestStopStopsRunnerThenJobs(t *testing.T) {
	api := &fakeAPI{conts: []container.Summary{{ID: "job1"}}}
	b := testBackend(t, api)
	if err := b.Stop(context.Background(), core.Handle{RunnerName: "r", BackendID: "run1"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(api.stopped) != 2 || api.stopped[0] != "run1" || api.stopped[1] != "job1" {
		t.Fatalf("%v", api.stopped)
	}
}

func TestPoolGateAppliesToNewProxies(t *testing.T) {
	b := testBackend(t, &fakeAPI{})
	g := b.PullGate()
	g.Pause()
	if err := b.startProxy(context.Background(), testRunner, b.proxySock(testRunner)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Cleanup(context.Background(), testRunner) }()
	g.Resume() // must resume the proxy created while paused
	if b.paused != 0 || g.InFlight() != 0 {
		t.Fatal("gate state")
	}
}

func TestAdoptRestartsProxyOnSameSocket(t *testing.T) {
	api := &fakeAPI{inspectBinds: []string{"/data:/data:ro", "/usr/bin/rw:/usr/bin/rw:ro"}}
	b := testBackend(t, api)
	if err := os.MkdirAll(b.runnerDir(testRunner), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := b.Adopt(context.Background(), core.Handle{RunnerName: testRunner, BackendID: "cid"}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Cleanup(context.Background(), testRunner) }()
	if _, err := os.Stat(b.proxySock(testRunner)); err != nil {
		t.Fatalf("proxy socket: %v", err)
	}
	b.mu.Lock()
	p := b.proxies[testRunner]
	b.mu.Unlock()
	if p == nil || p.Socket() != b.proxySock(testRunner) {
		t.Fatal("proxy not registered on the runner's socket")
	}
	allowed := b.allowedFromBinds(testRunner, api.inspectBinds)
	has := func(x string) bool {
		for _, a := range allowed {
			if a == x {
				return true
			}
		}
		return false
	}
	if !has("/data") || !has(b.runnerDir(testRunner)) || has("/usr/bin/rw") {
		t.Fatalf("%v", allowed)
	}
	if err := b.Adopt(context.Background(), core.Handle{RunnerName: "../x"}); err == nil {
		t.Fatal("invalid name adopted")
	}
}

func TestEnsureImagePullsOncePerRefresh(t *testing.T) {
	api := &fakeAPI{missing: map[string]bool{"job:img": true}}
	b := testBackend(t, api)
	ctx := context.Background()
	if err := b.EnsureImage(ctx, "runner:img"); err != nil || len(api.pulls) != 0 {
		t.Fatalf("default image must not be pulled again: %v %v", err, api.pulls)
	}
	if err := b.EnsureImage(ctx, "job:img"); err != nil {
		t.Fatal(err)
	}
	api.missing = nil // now local
	if err := b.EnsureImage(ctx, "job:img"); err != nil || len(api.pulls) != 1 {
		t.Fatalf("second use within the refresh window must not pull: %v %v", err, api.pulls)
	}
}

func TestEnsureImageFailures(t *testing.T) {
	ctx := context.Background()
	api := &fakeAPI{pullErr: errors.New("registry down")}
	if err := testBackend(t, api).EnsureImage(ctx, "job:img"); err != nil {
		t.Fatalf("local copy must be used when the pull fails: %v", err)
	}
	api = &fakeAPI{pullErr: errors.New("registry down"), missing: map[string]bool{"job:img": true}}
	if err := testBackend(t, api).EnsureImage(ctx, "job:img"); err == nil {
		t.Fatal("missing image with a failed pull must error")
	}
}

func TestEnsureImageBuildsDeclaredImages(t *testing.T) {
	const tag = "runner-warden-build/desk:abc"
	api := &fakeAPI{missing: map[string]bool{tag: true}}
	b := testBackend(t, api)
	b.cfg.Builds = map[string]string{tag: "FROM base\nRUN apt-get install -y libgtk-3-dev\n"}
	if err := b.EnsureImage(context.Background(), tag); err != nil {
		t.Fatal(err)
	}
	if len(api.builds) != 1 || !strings.Contains(api.builds[0], "libgtk-3-dev") || len(api.pulls) != 0 {
		t.Fatalf("builds %v pulls %v", api.builds, api.pulls)
	}
	if err := b.EnsureImage(context.Background(), tag); err != nil || len(api.builds) != 1 {
		t.Fatal("a built tag is never rebuilt or pulled")
	}
	api.missing[tag], api.buildErr = true, "E: Unable to locate package"
	if err := b.EnsureImage(context.Background(), tag); err == nil {
		t.Fatal("build errors must surface")
	}
}
