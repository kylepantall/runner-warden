package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	mu        sync.Mutex
	created   []*container.Config
	hostCfgs  []*container.HostConfig
	names     []string
	started   []string
	removed   []string
	stopped   []string
	nets      []network.Summary
	vols      []volume.Volume
	netRemove []string
	volRemove []string
	conts     []container.Summary // returned by every ContainerList
	running   bool
	lastList  []container.ListOptions
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
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: f.running}}}, nil
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
func (f *fakeAPI) ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeAPI) ImageInspect(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error) {
	return image.InspectResponse{}, nil
}

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
	b, err := newBackend(Config{
		Instance: "main", HostID: "h1", StateDir: shortDir(t),
		Image: "runner:img", DockerSocket: filepath.Join(shortDir(t), "docker.sock"),
		Slice: "runner-warden-main.slice", RunnerUID: 1001, TmpfsSize: 1 << 30, OOMScoreAdj: 500,
		HookBinary: "/usr/bin/rw", HookDir: "/run/rw/hook",
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
		Name: "my-ci-h1-abcd", JITConfig: "JIT", CPUSet: "2-3",
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
		"ACTIONS_RUNNER_INPUT_JITCONFIG":    "JIT",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED":   filepath.Join(dir, hookDir, "job-started.sh"),
		"ACTIONS_RUNNER_HOOK_JOB_COMPLETED": filepath.Join(dir, hookDir, "job-completed.sh"),
		"RUNNER_WARDEN_SOCKET":              "/run/rw/hook/hook.sock",
		"RUNNER_WARDEN_RUNNER":              spec.Name,
		"FOO":                               "bar",
		"GOMODCACHE":                        "/go/pkg",
	}
	for k, v := range checks {
		if env[k] != v {
			t.Errorf("env %s=%q want %q", k, env[k], v)
		}
	}
	binds := map[string]bool{}
	for _, s := range hc.Binds {
		binds[s] = true
	}
	for _, want := range []string{
		sock + ":/var/run/docker.sock", dir + ":" + dir,
		"/usr/bin/rw:/usr/bin/rw:ro", "/run/rw/hook:/run/rw/hook",
		"/data:/data:ro", "/cache/go:/go/pkg",
	} {
		if !binds[want] {
			t.Errorf("missing bind %s in %v", want, hc.Binds)
		}
	}
	if hc.CgroupParent != "runner-warden-main.slice" || hc.OomScoreAdj != 500 || hc.CpusetCpus != "2-3" {
		t.Errorf("hostconfig %+v", hc.Resources)
	}
	if hc.Memory != 0 || hc.MemorySwap != 0 {
		t.Error("runner must not get a memory limit")
	}
	if len(hc.GroupAdd) != 1 || hc.GroupAdd[0] != "998" || cfg.User != "1001" {
		t.Errorf("group/user %v %s", hc.GroupAdd, cfg.User)
	}
	if len(hc.Devices) != 1 || hc.Devices[0].PathOnHost != "/dev/kvm" {
		t.Errorf("devices %v", hc.Devices)
	}
	if _, ok := hc.Tmpfs["/tmp"]; !ok {
		t.Error("no /tmp tmpfs")
	}
	if cfg.WorkingDir != dir || cfg.Entrypoint[len(cfg.Entrypoint)-1] != dir {
		t.Error("runner dir must be the same path in the container")
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
	spec := core.RunnerSpec{Name: "my-ci-h1-0001", JITConfig: "J"}
	h, err := b.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.BackendID != "cid"+spec.Name || len(api.started) != 1 || api.names[0] != spec.Name {
		t.Fatalf("handle %+v started %v", h, api.started)
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
	if err := b.Cleanup(context.Background(), "never-started"); err != nil {
		t.Fatal(err)
	}
}

func TestListFiltersByLabel(t *testing.T) {
	api := &fakeAPI{conts: []container.Summary{{ID: "x", Created: 100, Labels: map[string]string{naming.MetaRunner: "r1"}}}}
	b := testBackend(t, api)
	hs, err := b.List(context.Background())
	if err != nil || len(hs) != 1 || hs[0].RunnerName != "r1" || hs[0].BackendID != "x" {
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
	if err := b.startProxy("r1", b.proxySock("r1")); err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup(context.Background(), "r1")
	g.Resume() // must resume the proxy created while paused
	if b.paused != 0 || g.InFlight() != 0 {
		t.Fatal("gate state")
	}
}
