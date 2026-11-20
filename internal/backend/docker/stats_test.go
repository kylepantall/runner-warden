package docker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/kylepantall/runner-warden/internal/core"
)

func writeCg(t *testing.T, dir string, cpuUsec, cur, peak, anon, oom int, io string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"cpu.stat":       "usage_usec " + itoa(cpuUsec) + "\nuser_usec 1\n",
		"memory.current": itoa(cur) + "\n",
		"memory.peak":    itoa(peak) + "\n",
		"memory.stat":    "anon " + itoa(anon) + "\nfile 5\n",
		"memory.events":  "low 0\noom_kill " + itoa(oom) + "\n",
		"io.stat":        io,
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestExpandSlice(t *testing.T) {
	if got := expandSlice("runner-warden-main.slice"); got != "runner.slice/runner-warden.slice/runner-warden-main.slice" {
		t.Fatalf("%s", got)
	}
	if got := expandSlice("job.slice"); got != "job.slice" {
		t.Fatalf("%s", got)
	}
}

func TestReadIOBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "io.stat")
	_ = os.WriteFile(p, []byte("8:0 rbytes=100 wbytes=50 rios=3 wios=1\n259:0 rbytes=1 wbytes=2 dbytes=999\n"), 0o644)
	if got := readIOBytes(p); got != 153 {
		t.Fatalf("%d", got)
	}
}

func TestStatsSumsContainersAndStaysMonotonic(t *testing.T) {
	api := &fakeAPI{running: true}
	b := testBackend(t, api)
	slice := filepath.Join(b.cgroupRoot, expandSlice(b.cfg.Slice))
	writeCg(t, filepath.Join(slice, "docker-run1.scope"), 2_000_000, 100, 150, 80, 0, "8:0 rbytes=10 wbytes=10\n")
	writeCg(t, filepath.Join(slice, "docker-job1.scope"), 3_000_000, 400, 500, 300, 1, "8:0 rbytes=5 wbytes=5\n")
	api.conts = []container.Summary{{ID: "job1"}}

	diag := filepath.Join(b.runnerDir("r"), "_diag")
	_ = os.MkdirAll(diag, 0o755)
	_ = os.WriteFile(filepath.Join(diag, "Runner_1.log"), make([]byte, 1234), 0o644)

	h := core.Handle{RunnerName: "r", BackendID: "run1"}
	u, err := b.Stats(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Alive || u.CPUSeconds != 5 || u.MemCurrent != 500 || u.MemPeak != 650 || u.OOMKills != 1 || u.IOBytes != 30 || u.LogBytes != 1234 {
		t.Fatalf("%+v", u)
	}

	// The job container is removed: its cgroup disappears. Counters must not drop.
	if err := os.RemoveAll(filepath.Join(slice, "docker-job1.scope")); err != nil {
		t.Fatal(err)
	}
	api.conts = nil
	u, err = b.Stats(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if u.CPUSeconds != 5 || u.OOMKills != 1 || u.IOBytes != 30 || u.MemCurrent != 100 || u.MemPeak != 650 {
		t.Fatalf("after job exit %+v", u)
	}
}

func TestStatsDeadRunner(t *testing.T) {
	b := testBackend(t, &fakeAPI{running: false})
	u, err := b.Stats(context.Background(), core.Handle{RunnerName: "r", BackendID: "gone"})
	if err != nil || u.Alive {
		t.Fatalf("%+v %v", u, err)
	}
}
