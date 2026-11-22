package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/daemon"
	"github.com/kylepantall/runner-warden/internal/scheduler"
)

func TestRenderStatusFoldsIdleLanes(t *testing.T) {
	rep := daemon.StatusReport{Scheduler: scheduler.Status{Lanes: []scheduler.LaneStatus{
		{Name: "acme/linux/l1", Jobs: []scheduler.JobStatus{{Repo: "app", Workflow: "ci.yml", Name: "build", State: "running", Runner: "r-1"}}},
		{Name: "acme/linux/l2"},
		{Name: "acme/windows/l1"},
	}}}
	var b bytes.Buffer
	renderStatus(&b, rep)
	out := b.String()
	if !strings.Contains(out, "acme/linux/l1") || !strings.Contains(out, "r-1") {
		t.Fatal("busy lane and its runner must be listed")
	}
	if strings.Contains(out, "acme/linux/l2") || strings.Contains(out, "acme/windows/l1") {
		t.Fatal("idle lanes must be summarised, not listed one by one")
	}
}

func TestWatchStatusRetriesAndStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	fetch := func(context.Context) (daemon.StatusReport, error) {
		if calls.Add(1) >= 3 {
			cancel()
		}
		return daemon.StatusReport{}, errors.New("down")
	}
	done := make(chan error, 1)
	go func() { done <- watchStatus(ctx, &bytes.Buffer{}, time.Millisecond, fetch) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel must end the watch cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
	if calls.Load() < 3 {
		t.Fatalf("a failed fetch must not end the watch (calls=%d)", calls.Load())
	}
}

func TestFrameRedrawsInPlace(t *testing.T) {
	f := frame("a\nbbbbbb\nc\nd\ne\n", 4, 3)
	if !strings.HasPrefix(f, "\x1b[H") || !strings.HasSuffix(f, "\x1b[J") || strings.Contains(f, "\x1b[2J") {
		t.Fatalf("frame must home and clear below, never clear the whole screen: %q", f)
	}
	if strings.Contains(f, "bbbbb") {
		t.Fatal("lines must be cut to the width, or they wrap and scroll")
	}
	if strings.Count(f, "\r\n") != 2 || strings.Contains(f, "\nd") {
		t.Fatalf("frame must fit the height: %q", f)
	}
}

func TestRenderStatusShowsMachines(t *testing.T) {
	rep := daemon.StatusReport{
		Hostname:       "box",
		VMsUp:          map[string]int64{"build": 8 << 30},
		WindowsVMs:     []string{"build", "spare"},
		WindowsHosts:   map[string]bool{"pc": true},
		LinuxHosts:     map[string]bool{"pc-wsl": true},
		LinuxHostNames: []string{"pc-wsl"},
		Runners: []daemon.RunnerStatus{
			{Name: "r-lin", OS: "linux", RunID: 1, Alive: true},
			{Name: "r-win", OS: "windows", VM: "pc", RunID: 2, Alive: true},
			{Name: "r-wsl", OS: "linux", VM: "pc-wsl", RunID: 3, Alive: true},
		},
		Scheduler: scheduler.Status{Lanes: []scheduler.LaneStatus{
			{Name: "o/windows/l1", Jobs: []scheduler.JobStatus{{Repo: "app", Name: "w", Runner: "r-win"}}},
			{Name: "o/linux/l2", Jobs: []scheduler.JobStatus{{Repo: "app", Name: "l", Runner: "r-wsl"}}},
		}},
	}
	var b bytes.Buffer
	renderStatus(&b, rep)
	out := b.String()
	for _, want := range []string{"MACHINE", "box", "build", "spare", "pc", "linux host"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	var jobLine, wslLine string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "o/windows/l1") {
			jobLine = l
		}
		if strings.HasPrefix(l, "o/linux/l2") {
			wslLine = l
		}
	}
	if !strings.Contains(jobLine, " pc ") || !strings.Contains(wslLine, " pc-wsl ") {
		t.Fatalf("job rows must name the machine their runner is on: %q %q", jobLine, wslLine)
	}
}
