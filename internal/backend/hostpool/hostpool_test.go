package hostpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/autocache"
	"github.com/kylepantall/runner-warden/internal/core"
)

type fakeBackend struct {
	mu      sync.Mutex
	started []core.RunnerSpec
	cleaned []string
	list    []core.Handle
	down    bool
	up      bool
	name    string
}

func (f *fakeBackend) Caps() core.Caps               { return core.Caps{OS: core.Linux} }
func (f *fakeBackend) Prepare(context.Context) error { return nil }
func (f *fakeBackend) Start(_ context.Context, s core.RunnerSpec) (core.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, s)
	return core.Handle{RunnerName: s.Name, BackendID: "c-" + s.Name}, nil
}
func (f *fakeBackend) Stats(_ context.Context, h core.Handle) (core.Usage, error) {
	return core.Usage{Alive: h.BackendID == "c-r1"}, nil
}
func (f *fakeBackend) Stop(context.Context, core.Handle, time.Duration) error { return nil }
func (f *fakeBackend) Cleanup(_ context.Context, n string) error {
	f.mu.Lock()
	f.cleaned = append(f.cleaned, n)
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) List(context.Context) ([]core.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("no route to host")
	}
	return append([]core.Handle(nil), f.list...), nil
}
func (f *fakeBackend) EnsureImage(context.Context, string) error { return nil }
func (f *fakeBackend) Adopt(context.Context, core.Handle) error  { return nil }
func (f *fakeBackend) Persistent(context.Context) ([]core.PersistentContainer, error) {
	return nil, nil
}
func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Up() bool     { f.mu.Lock(); defer f.mu.Unlock(); return f.up }
func (f *fakeBackend) Probe(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = !f.down
	if f.down {
		return errors.New("down")
	}
	return nil
}

func TestPoolRoutesByHost(t *testing.T) {
	ctx := context.Background()
	local, remote := &fakeBackend{}, &fakeBackend{name: "pc"}
	p := NewPool(local, core.Caps{}, []Member{remote}, nil)
	if err := p.Prepare(ctx); err != nil || !remote.Up() {
		t.Fatalf("restore follows Prepare and must see the host: %v", err)
	}
	env := map[string]string{HostEnv: "pc", "A": "b"}
	hd, err := p.Start(ctx, core.RunnerSpec{Name: "r1", JITConfig: "j", Env: env})
	if err != nil || hd.BackendID != "pc/c-r1" {
		t.Fatalf("%+v %v", hd, err)
	}
	if env[HostEnv] != "pc" {
		t.Fatal("caller's env map changed")
	}
	if got := remote.started[0]; got.Env[HostEnv] != "" || got.Env["A"] != "b" {
		t.Fatalf("host got %+v", got)
	}
	if u, err := p.Stats(ctx, hd); err != nil || !u.Alive {
		t.Fatalf("stats must reach the host with its own id: %+v %v", u, err)
	}
	if _, err := p.Start(ctx, core.RunnerSpec{Name: "r2", JITConfig: "j"}); err != nil || len(local.started) != 1 {
		t.Fatal("no host: start here")
	}
	if err := p.Cleanup(ctx, "r1"); err != nil || len(remote.cleaned) != 1 || len(local.cleaned) != 0 {
		t.Fatalf("cleanup r1: local %v remote %v", local.cleaned, remote.cleaned)
	}
	_ = p.Cleanup(ctx, "r9") // unknown: everywhere
	if len(local.cleaned) != 1 || len(remote.cleaned) != 2 {
		t.Fatalf("cleanup unknown: local %v remote %v", local.cleaned, remote.cleaned)
	}
}

func TestPoolWithoutLocal(t *testing.T) {
	ctx := context.Background()
	remote := &fakeBackend{name: "mac", list: []core.Handle{{RunnerName: "r", BackendID: "r"}}}
	p := NewPool(nil, core.Caps{OS: core.MacOS}, []Member{remote}, nil)
	if err := p.Prepare(ctx); err != nil || p.Caps().OS != core.MacOS {
		t.Fatalf("%v %+v", err, p.Caps())
	}
	if _, err := p.Start(ctx, core.RunnerSpec{Name: "x"}); err == nil {
		t.Fatal("no local backend: a start without a host must fail")
	}
	if hs, err := p.List(ctx); err != nil || len(hs) != 1 || hs[0].BackendID != "mac/r" {
		t.Fatalf("%+v %v", hs, err)
	}
	if err := p.Cleanup(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestPoolListKeepsLastHostListBriefly(t *testing.T) {
	ctx := context.Background()
	local := &fakeBackend{list: []core.Handle{{RunnerName: "here", BackendID: "c1"}}}
	remote := &fakeBackend{name: "pc", list: []core.Handle{{RunnerName: "there", BackendID: "c2"}}}
	p := NewPool(local, core.Caps{}, []Member{remote}, nil)
	hs, err := p.List(ctx)
	if err != nil || len(hs) != 2 || hs[1].BackendID != "pc/c2" {
		t.Fatalf("%+v %v", hs, err)
	}
	remote.down = true
	if hs, _ := p.List(ctx); len(hs) != 2 {
		t.Fatalf("a short outage keeps the host's runners: %+v", hs)
	}
	p.mu.Lock()
	p.last["pc"] = listed{at: time.Now().Add(-listGrace - time.Second), hs: p.last["pc"].hs}
	p.mu.Unlock()
	if hs, _ := p.List(ctx); len(hs) != 1 {
		t.Fatalf("a long outage drops them: %+v", hs)
	}
}

func TestHostOf(t *testing.T) {
	if h, id := HostOf("pc/abc"); h != "pc" || id != "abc" {
		t.Fatal(h, id)
	}
	if h, id := HostOf("abc"); h != "" || id != "abc" {
		t.Fatal(h, id)
	}
}

type trimmer struct {
	*fakeBackend
	limit int64
	inUse []string
}

func (t *trimmer) TrimCaches(_ context.Context, limit int64, inUse []string) ([]autocache.Usage, []autocache.Usage, error) {
	t.limit, t.inUse = limit, inUse
	return []autocache.Usage{{Scope: "o"}}, []autocache.Usage{{Scope: "old"}}, nil
}

func TestTrimCachesOnHost(t *testing.T) {
	tr := &trimmer{fakeBackend: &fakeBackend{name: "pc"}}
	plain := &fakeBackend{name: "other"}
	p := NewPool(&fakeBackend{}, core.Caps{}, []Member{tr, plain}, nil)
	if _, _, ok, _ := p.TrimCaches(context.Background(), "pc", 10, nil); ok {
		t.Fatal("a down host is skipped")
	}
	_ = p.Prepare(context.Background())
	kept, gone, ok, err := p.TrimCaches(context.Background(), "pc", 10, []string{"o"})
	if !ok || err != nil || len(kept) != 1 || len(gone) != 1 || tr.limit != 10 || tr.inUse[0] != "o" {
		t.Fatalf("%v %v %v %v", kept, gone, ok, err)
	}
	if _, _, ok, _ := p.TrimCaches(context.Background(), "other", 10, nil); ok {
		t.Fatal("a member without caches is skipped")
	}
}
