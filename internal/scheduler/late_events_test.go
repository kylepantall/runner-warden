package scheduler

import "testing"

// A short job's Completed can be processed before its Started (seen live when
// both arrived in one message). The late Started must not resurrect the job,
// the runner's exit must not force-cancel the run, and memory must be free.
func TestLateStartedAfterCompletedIsIgnored(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("o", 1)
	free0 := h.free()
	j := h.assign(l, h.job("o", "short"))
	g, ok := h.startsFor(j.JobID)
	if !ok {
		t.Fatal("runner not started")
	}
	runner := "rn-" + g.Key
	h.done(l, j, runner, "succeeded")

	j.RunnerName = runner
	h.s.Started(l.ScaleSetKey, l.Lane, j)
	h.wait()
	h.s.RunnerDied(runner, false)
	h.wait()

	h.run.mu.Lock()
	cancels := len(h.run.cancels)
	h.run.mu.Unlock()
	if cancels != 0 {
		t.Fatalf("run force-cancelled %d times after a normal finish", cancels)
	}
	if h.state(j.JobID) != "" {
		t.Fatalf("finished job resurrected as %q", h.state(j.JobID))
	}
	if h.free() != free0 {
		t.Fatalf("memory not released: free %d want %d", h.free(), free0)
	}
}

// Normal order with both events in one batch.
func TestStartedThenCompletedThenExit(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("o", 1)
	j := h.assign(l, h.job("o", "short"))
	runner := h.begin(l, j)
	h.done(l, j, runner, "succeeded")
	h.s.RunnerDied(runner, false)
	h.wait()
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	if len(h.run.cancels) != 0 {
		t.Fatal("normal exit force-cancelled the run")
	}
}
