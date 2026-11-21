package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct{ calls [][]string }

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil
}

func TestEnsureSliceIdempotent(t *testing.T) {
	root := t.TempDir()
	rec := &recorder{}
	h := Host{Root: root, Run: rec.run}
	const name = "runner-warden-main.slice"

	if err := h.EnsureSlice(name, 22<<30, 0); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "etc/systemd/system", name)
	b, err := os.ReadFile(file) //nolint:gosec // test path under a temp root
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"MemoryAccounting=yes", "MemoryMax=23622320128", "MemorySwapMax=0"} {
		if !strings.Contains(s, want) {
			t.Errorf("unit missing %s", want)
		}
	}
	if strings.Contains(s, "MemoryHigh") {
		t.Error("MemoryHigh must never be written")
	}
	if len(rec.calls) != 2 || rec.calls[0][1] != "daemon-reload" || rec.calls[1][1] != "start" {
		t.Fatalf("calls %v", rec.calls)
	}

	st1, _ := os.Stat(file)
	rec.calls = nil
	if err := h.EnsureSlice(name, 22<<30, 0); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(file)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("unchanged unit rewritten")
	}
	if len(rec.calls) != 1 || rec.calls[0][1] != "start" {
		t.Fatalf("unchanged: calls %v", rec.calls)
	}

	rec.calls = nil
	if err := h.EnsureSlice(name, 10<<30, 0); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("changed: calls %v", rec.calls)
	}
}

func TestEnsureSliceRejectsBadName(t *testing.T) {
	h := Host{Root: t.TempDir(), Run: (&recorder{}).run}
	for _, n := range []string{"x", "../a.slice", "a b.slice"} {
		if h.EnsureSlice(n, 1, 0) == nil {
			t.Errorf("accepted %q", n)
		}
	}
}

func TestSliceUnitOmissions(t *testing.T) {
	s := SliceUnit(0, -1)
	if strings.Contains(s, "MemoryMax") || strings.Contains(s, "MemorySwapMax") {
		t.Error("expected omitted limits")
	}
}

func TestDockerDefaultCgroupParent(t *testing.T) {
	root := t.TempDir()
	h := Host{Root: root}
	if v, err := h.DockerDefaultCgroupParent(); err != nil || v != "" {
		t.Fatalf("missing file: %q %v", v, err)
	}
	dir := filepath.Join(root, "etc/docker")
	_ = os.MkdirAll(dir, 0o750)
	_ = os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(`{"cgroup-parent":"x.slice","log-driver":"json-file"}`), 0o600)
	if v, err := h.DockerDefaultCgroupParent(); err != nil || v != "x.slice" {
		t.Fatalf("got %q %v", v, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(`{bad`), 0o600)
	if _, err := h.DockerDefaultCgroupParent(); err == nil {
		t.Fatal("expected parse error")
	}
}

// Under the service's ProtectSystem=full the unit can't be rewritten: a changed
// budget is applied at runtime instead of failing the daemon.
func TestEnsureSliceReadOnlyAppliesRuntime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	rec := &recorder{}
	h := Host{Root: root, Run: rec.run}
	const name = "runner-warden-main.slice"
	if err := h.EnsureSlice(name, 6<<30, 0); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "etc/systemd/system")
	if err := os.Chmod(dir, 0o555); err != nil { //nolint:gosec // test makes the unit dir read-only on purpose
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // restore the temp dir for cleanup
	rec.calls = nil
	if err := h.EnsureSlice(name, 14<<30, 0); err != nil {
		t.Fatalf("read-only change: %v", err)
	}
	last := rec.calls[len(rec.calls)-1]
	if len(last) < 4 || last[1] != "set-property" || last[2] != "--runtime" || last[3] != name || last[4] != "MemoryMax=15032385536" {
		t.Fatalf("calls %v", rec.calls)
	}
}
