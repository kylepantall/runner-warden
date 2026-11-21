package docker

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/dockerproxy"
)

// fakeLauncher serves each "unit" in-process, the way the systemd unit would
// serve it in its own process: from the spec file, until stopped.
type fakeLauncher struct {
	delay time.Duration // before the socket appears
	never bool          // never serve

	mu       sync.Mutex
	launches []string
	stops    []string
	units    map[string]context.CancelFunc
	done     map[string]chan struct{}
}

func (f *fakeLauncher) Launch(_ context.Context, unit, specPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, unit)
	if f.never {
		return nil
	}
	spec, err := dockerproxy.ReadSpec(specPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	if f.units == nil {
		f.units, f.done = map[string]context.CancelFunc{}, map[string]chan struct{}{}
	}
	f.units[unit], f.done[unit] = cancel, done
	go func() {
		defer close(done)
		time.Sleep(f.delay)
		_ = dockerproxy.Serve(ctx, spec, dockerproxy.ServeOptions{PollEvery: time.Hour})
	}()
	return nil
}

func (f *fakeLauncher) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	f.stops = append(f.stops, unit)
	cancel, done := f.units[unit], f.done[unit]
	delete(f.units, unit)
	f.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}

// kill drops the proxy without going through Stop (a crash, or a reboot).
func (f *fakeLauncher) kill(unit string) {
	f.mu.Lock()
	cancel, done := f.units[unit], f.done[unit]
	delete(f.units, unit)
	f.mu.Unlock()
	cancel()
	<-done
}

func (f *fakeLauncher) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launches), len(f.stops)
}

// sockAtCreate records whether the proxy socket was already accepting
// connections when the runner container was created.
type sockAtCreate struct {
	*fakeAPI
	sock  string
	alive bool
}

func (s *sockAtCreate) ContainerCreate(ctx context.Context, c *container.Config, hc *container.HostConfig, nc *network.NetworkingConfig, p *ocispec.Platform, name string) (container.CreateResponse, error) {
	s.alive = proxyAlive(s.sock)
	return s.fakeAPI.ContainerCreate(ctx, c, hc, nc, p, name)
}

func launcherBackend(t *testing.T, api dockerAPI, l *fakeLauncher) *Backend {
	t.Helper()
	b := testBackend(t, api)
	b.launcher = l
	return b
}

func TestSpecRoundTrip(t *testing.T) {
	b := testBackend(t, &fakeAPI{})
	b.cfg.PersistentNames = []string{"cache-*"}
	cfg := b.proxyConfig(testRunner, b.proxySock(testRunner), []string{"/data", "/x"})
	in := dockerproxy.Spec{Config: cfg, RunnerContainer: testRunner, LogLevel: "debug"}
	path := b.proxySpecPath(testRunner)
	if err := dockerproxy.WriteSpec(path, in); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("spec must be 0600: %v %v", fi, err)
	}
	out, err := dockerproxy.ReadSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	in.Config.Log = nil
	if !specEqual(in, out) {
		t.Fatalf("round trip\n in %+v\nout %+v", in, out)
	}
}

func specEqual(a, b dockerproxy.Spec) bool {
	x, y := a.Config, b.Config
	if a.RunnerContainer != b.RunnerContainer || a.LogLevel != b.LogLevel {
		return false
	}
	if x.Socket != y.Socket || x.Upstream != y.Upstream || x.Runner != y.Runner || x.Instance != y.Instance ||
		x.HostID != y.HostID || x.Slice != y.Slice || x.OOMScoreAdj != y.OOMScoreAdj || x.PullLock != y.PullLock || x.MaxBody != y.MaxBody {
		return false
	}
	return strsEq(x.CacheBinds, y.CacheBinds) && strsEq(x.AllowedBinds, y.AllowedBinds) &&
		strsEq(x.PersistentNames, y.PersistentNames) && len(x.CacheEnv) == len(y.CacheEnv) && x.CacheEnv["GOMODCACHE"] == y.CacheEnv["GOMODCACHE"]
}

func strsEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStartLaunchesUnitAndWaitsForSocket(t *testing.T) {
	l := &fakeLauncher{delay: 300 * time.Millisecond}
	api := &sockAtCreate{fakeAPI: &fakeAPI{}}
	b := launcherBackend(t, api, l)
	api.sock = b.proxySock(testRunner)
	spec := core.RunnerSpec{Name: testRunner, JITConfig: "J"}
	if err := os.MkdirAll(b.hookDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Cleanup(context.Background(), testRunner) }()
	if n, _ := l.counts(); n != 1 || l.launches[0] != b.proxyUnit(testRunner) {
		t.Fatalf("launches %v", l.launches)
	}
	if !api.alive {
		t.Fatal("runner container created before the proxy socket was up")
	}
	b.mu.Lock()
	inproc := len(b.proxies)
	b.mu.Unlock()
	if inproc != 0 {
		t.Fatal("proxy must not run inside the daemon")
	}
	if _, err := os.Stat(b.proxySpecPath(testRunner)); err != nil {
		t.Fatalf("spec file: %v", err)
	}
}

func TestStartFailsWhenProxyNeverComesUp(t *testing.T) {
	l := &fakeLauncher{never: true}
	api := &fakeAPI{}
	b := launcherBackend(t, api, l)
	// Don't wait the full production limit: the ctx bounds the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	spec := core.RunnerSpec{Name: testRunner, JITConfig: "J"}
	if err := os.MkdirAll(b.hookDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, spec); err == nil {
		t.Fatal("start must fail without a proxy")
	}
	if len(api.created) != 0 {
		t.Fatal("runner container must not be created without its proxy")
	}
	if _, stops := l.counts(); stops == 0 {
		t.Fatal("the failed unit must be stopped")
	}
}

func TestAdoptLeavesLiveProxyAlone(t *testing.T) {
	l := &fakeLauncher{}
	b := launcherBackend(t, &fakeAPI{}, l)
	if err := os.MkdirAll(b.runnerDir(testRunner), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := b.startProxy(context.Background(), testRunner, b.proxySock(testRunner)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Cleanup(context.Background(), testRunner) }()
	launches, stops := l.counts()

	// "Daemon restart": a fresh backend over the same state.
	b2 := launcherBackend(t, &fakeAPI{}, l)
	b2.cfg = b.cfg
	if err := b2.Adopt(context.Background(), core.Handle{RunnerName: testRunner, BackendID: "cid"}); err != nil {
		t.Fatal(err)
	}
	if n, s := l.counts(); n != launches || s != stops {
		t.Fatalf("a live proxy must not be relaunched (launches %d->%d, stops %d->%d)", launches, n, stops, s)
	}
}

func TestAdoptRelaunchesDeadProxy(t *testing.T) {
	l := &fakeLauncher{}
	b := launcherBackend(t, &fakeAPI{}, l)
	if err := os.MkdirAll(b.runnerDir(testRunner), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := b.startProxy(context.Background(), testRunner, b.proxySock(testRunner)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Cleanup(context.Background(), testRunner) }()
	l.kill(b.proxyUnit(testRunner))
	if proxyAlive(b.proxySock(testRunner)) {
		t.Fatal("test setup: proxy should be dead")
	}
	before, _ := l.counts()
	if err := b.Adopt(context.Background(), core.Handle{RunnerName: testRunner, BackendID: "cid"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := l.counts(); n != before+1 {
		t.Fatalf("dead proxy must be relaunched once, launches %d->%d", before, n)
	}
	if !proxyAlive(b.proxySock(testRunner)) {
		t.Fatal("relaunched proxy not serving")
	}
}

func TestCleanupStopsUnit(t *testing.T) {
	l := &fakeLauncher{}
	b := launcherBackend(t, &fakeAPI{}, l)
	if err := os.MkdirAll(b.runnerDir(testRunner), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := b.startProxy(context.Background(), testRunner, b.proxySock(testRunner)); err != nil {
		t.Fatal(err)
	}
	_, stopsBefore := l.counts()
	if err := b.Cleanup(context.Background(), testRunner); err != nil {
		t.Fatal(err)
	}
	if _, s := l.counts(); s <= stopsBefore || l.stops[len(l.stops)-1] != b.proxyUnit(testRunner) {
		t.Fatalf("cleanup must stop the proxy unit, stops %v", l.stops)
	}
	if proxyAlive(b.proxySock(testRunner)) {
		t.Fatal("proxy still serving after cleanup")
	}
	if _, err := os.Stat(b.proxySpecPath(testRunner)); !os.IsNotExist(err) {
		t.Fatalf("spec file must be removed: %v", err)
	}
}
