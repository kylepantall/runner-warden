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
