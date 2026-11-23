package linuxhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
)

// fakeBackend records calls; it serves both as a host's agent backend and as
// the pool's local backend.
type fakeBackend struct {
	mu       sync.Mutex
	started  []core.RunnerSpec
	cleaned  []string
	list     []core.Handle
	prepared int
}

func (f *fakeBackend) Caps() core.Caps { return core.Caps{OS: core.Linux} }
func (f *fakeBackend) Prepare(context.Context) error {
	f.mu.Lock()
	f.prepared++
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) Start(_ context.Context, s core.RunnerSpec) (core.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, s)
	return core.Handle{RunnerName: s.Name, BackendID: "c-" + s.Name}, nil
}
func (f *fakeBackend) Stats(_ context.Context, h core.Handle) (core.Usage, error) {
	return core.Usage{Alive: h.BackendID == "c-r1", CPUSeconds: 7}, nil
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
	return append([]core.Handle(nil), f.list...), nil
}
func (f *fakeBackend) EnsureImage(context.Context, string) error { return errors.New("no such image") }
func (f *fakeBackend) Adopt(context.Context, core.Handle) error  { return nil }
func (f *fakeBackend) Persistent(context.Context) ([]core.PersistentContainer, error) {
	return nil, nil
}

// fakeSSH is a host reached over SSH: it runs agent requests against be.
type fakeSSH struct {
	mu        sync.Mutex
	be        *fakeBackend
	version   string // installed agent version, "" = not installed
	installed []byte
	down      bool
	reqs      []Request
}

func (s *fakeSSH) Command(ctx context.Context, cmd, stdin string) (string, string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return "", "", -1, errors.New("dial: no route to host")
	}
	switch {
	case strings.HasSuffix(cmd, " uname -m"):
		return map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH] + "\n", "", 0, nil
	case strings.Contains(cmd, " sh -c '"):
		s.installed = []byte(stdin)
		s.version = "v9"
		sum := sha256.Sum256([]byte(stdin))
		return hex.EncodeToString(sum[:]) + "  /usr/local/bin/runner-warden.new\n", "", 0, nil
	case strings.HasSuffix(cmd, "/usr/local/bin/runner-warden agent"):
		if s.version == "" {
			return "", "sh: 1: /usr/local/bin/runner-warden: not found", 127, nil
		}
		var req Request
		if err := json.Unmarshal([]byte(stdin), &req); err != nil {
			return "", err.Error(), 1, nil
		}
		s.reqs = append(s.reqs, req)
		b, _ := json.Marshal(Dispatch(ctx, s.be, req, s.version))
		return string(b) + "\n", "", 0, nil
	}
	return "", "unexpected command " + cmd, 1, nil
}

func newTestHost(t *testing.T, ssh *fakeSSH) *Host {
	t.Helper()
	self := filepath.Join(t.TempDir(), "rw")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	return NewHost(HostConfig{
		Name: "pc", Exec: "wsl -u root --", Binary: "/usr/local/bin/runner-warden", Cmd: ssh,
		Agent: AgentConfig{Instance: "main", HostID: "h1", Image: "img"}, Version: "v9", Self: self,
	})
}

func TestProbeInstallsAndPrepares(t *testing.T) {
	ssh := &fakeSSH{be: &fakeBackend{}}
	h := newTestHost(t, ssh)
	if err := h.Probe(context.Background()); err != nil || !h.Up() {
		t.Fatalf("probe: %v up=%v", err, h.Up())
	}
	if string(ssh.installed) != "binary" || ssh.be.prepared != 1 {
		t.Fatalf("installed %q prepared %d", ssh.installed, ssh.be.prepared)
	}
	// Already prepared and current: a probe only pings.
	if err := h.Probe(context.Background()); err != nil || ssh.be.prepared != 1 {
		t.Fatalf("re-probe: %v prepared %d", err, ssh.be.prepared)
	}
	ssh.version = "v8" // the host fell behind: reinstall
	if err := h.Probe(context.Background()); err != nil || ssh.version != "v9" {
		t.Fatalf("upgrade: %v %s", err, ssh.version)
	}
	ssh.down = true
	if err := h.Probe(context.Background()); err == nil || h.Up() {
		t.Fatal("unreachable host must be down")
	}
}

func TestHostStartSendsSpecOnStdin(t *testing.T) {
	ctx := context.Background()
	remote := &fakeBackend{}
	ssh := &fakeSSH{be: remote, version: "v9"}
	h := newTestHost(t, ssh)
	if _, err := h.Start(ctx, core.RunnerSpec{Name: "r1", JITConfig: "j"}); err == nil {
		t.Fatal("a host that never probed is down")
	}
	if err := h.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	hd, err := h.Start(ctx, core.RunnerSpec{Name: "r1", JITConfig: "j", Image: "job-img"})
	if err != nil || hd.BackendID != "c-r1" {
		t.Fatalf("%+v %v", hd, err)
	}
	if got := remote.started[0]; got.JITConfig != "j" || got.Image != "" {
		t.Fatalf("host got %+v (an image it cannot pull falls back to the default)", got)
	}
	if u, err := h.Stats(ctx, hd); err != nil || !u.Alive || u.CPUSeconds != 7 {
		t.Fatalf("stats %+v %v", u, err)
	}
	if err := h.Cleanup(ctx, "r1"); err != nil || len(remote.cleaned) != 1 {
		t.Fatalf("cleanup %v %v", err, remote.cleaned)
	}
}

func TestHostTrimCachesAsksTheAgent(t *testing.T) {
	ssh := &fakeSSH{be: &fakeBackend{}, version: "v9"}
	h := newTestHost(t, ssh)
	_, _, _ = h.TrimCaches(context.Background(), 123, []string{"o"})
	if len(ssh.reqs) != 1 || ssh.reqs[0].Op != OpCaches || ssh.reqs[0].CacheMax != 123 || ssh.reqs[0].CacheInUse[0] != "o" {
		t.Fatalf("%+v", ssh.reqs)
	}
}
