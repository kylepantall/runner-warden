package scheduler

import (
	"testing"
	"time"
)

func (h *harness) stats(l LaneKey, assigned, running int) {
	h.s.Stats(l.ScaleSetKey, l.Lane, LaneStats{Assigned: assigned, Running: running})
	h.wait()
}

func (h *harness) phantoms() int {
	n := 0
	for _, l := range h.s.Snapshot().Lanes {
		for _, j := range l.Jobs {
			if j.Phantom {
				n++
			}
		}
	}
	return n
}

func TestPhantomStartsRunnerAfterGraceAndBindsRealJob(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 4*gib)))
	l := lane("a", 1)
	h.stats(l, 1, 0) // restart: GitHub shows an assigned job we never heard of
	h.tick(30 * time.Second)
	if h.nStarts() != 0 || h.phantoms() != 0 {
		t.Fatal("nothing before the grace period")
	}
	h.tick(20 * time.Second)
	if h.nStarts() != 1 || h.phantoms() != 1 {
		t.Fatalf("starts=%d phantoms=%d", h.nStarts(), h.phantoms())
	}
	if h.free() != 9*gib { // default job mem 1 GiB
		t.Fatalf("free=%d", h.free())
	}
	g := h.run.starts[0]
	real := h.job("a", "big")
	real.RunnerName = "rn-" + g.Key
	h.s.Started(l.ScaleSetKey, l.Lane, real)
	h.wait()
	if h.phantoms() != 0 || h.state(real.JobID) == "" {
		t.Fatalf("phantom must give way to the real job, phantoms=%d", h.phantoms())
	}
	if h.free() != 6*gib { // resized to the real cost
		t.Fatalf("free=%d", h.free())
	}
	if h.nStarts() != 1 {
		t.Fatal("no second runner")
	}
}

func TestRealAssignedBeforeGraceCreatesNoPhantom(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	h.stats(l, 1, 0)
	h.tick(20 * time.Second)
	h.assign(l, h.job("a", "x"))
	h.tick(60 * time.Second)
	if h.phantoms() != 0 || h.nStarts() != 1 {
		t.Fatalf("phantoms=%d starts=%d", h.phantoms(), h.nStarts())
	}
}

func TestRealAssignedAfterPhantomMergesWithoutSecondRunner(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 4*gib)))
	l := lane("a", 1)
	h.stats(l, 1, 0)
	h.tick(50 * time.Second)
	if h.nStarts() != 1 {
		t.Fatal("phantom runner expected")
	}
	g := h.run.starts[0]
	j := h.assign(l, h.job("a", "big"))
	if h.phantoms() != 0 || h.nStarts() != 1 || h.state(j.JobID) != StateStarting {
		t.Fatalf("phantoms=%d starts=%d state=%s", h.phantoms(), h.nStarts(), h.state(j.JobID))
	}
	if h.free() != 6*gib {
		t.Fatalf("free=%d", h.free())
	}
	j.RunnerName = "rn-" + g.Key
	h.s.Started(l.ScaleSetKey, l.Lane, j)
	h.wait()
	if h.state(j.JobID) == StateStarting || h.nStarts() != 1 {
		t.Fatal("bound to the phantom's runner")
	}
}

func TestPhantomDroppedWhenDeficitDisappears(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 20*gib)))
	l := lane("a", 1)
	h.stats(l, 1, 0)
	h.tick(50 * time.Second)
	if h.phantoms() != 1 {
		t.Fatal("phantom expected (held: nothing fits)")
	}
	h.stats(l, 0, 0)
	if h.phantoms() != 0 {
		t.Fatal("deficit gone: placeholder must go")
	}
}

func TestPhantomHeldPhantomIsTrimmedNotDoubled(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.DefaultJobMem = 20 * gib })
	l := lane("a", 1)
	h.stats(l, 2, 1) // one unstarted job unknown
	h.tick(50 * time.Second)
	h.tick(10 * time.Second)
	if h.phantoms() != 1 {
		t.Fatalf("phantoms=%d", h.phantoms())
	}
}

func TestNoPhantomWhileDraining(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	h.s.Drain(true)
	h.stats(l, 1, 0)
	h.tick(2 * time.Minute)
	if h.phantoms() != 0 || h.nStarts() != 0 {
		t.Fatal("drain must not create placeholders")
	}
}

func TestRunningJobsDoNotCountAsDeficit(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	h.begin(l, j)
	h.stats(l, 1, 1) // one running, none waiting
	h.tick(2 * time.Minute)
	if h.phantoms() != 0 || h.nStarts() != 1 {
		t.Fatal("no deficit")
	}
}

func TestStaleStatsIgnoredAndStartedInvalidatesThem(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	h.stats(l, 0, 0) // initial
	j := h.assign(l, h.job("a", "x"))
	h.stats(l, 1, 0) // job assigned, known
	h.clk.Advance(5 * time.Second)
	rn := h.begin(l, j)
	// stats still say "1 assigned, 0 running" but the job started since
	h.tick(2 * time.Minute)
	if h.phantoms() != 0 {
		t.Fatal("stats predating Started must not count")
	}
	h.done(l, j, rn, "succeeded")
	h.stats(l, 1, 0)
	h.tick(10 * time.Second)
	h.tick(3 * time.Minute) // stats now older than StatsMaxAge (and not initial)
	if h.phantoms() != 0 {
		t.Fatal("old non-initial stats must be ignored")
	}
}

func TestInitialStatsTrustedPastMaxAge(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	h.stats(l, 1, 0)
	h.tick(5 * time.Minute) // no further stats arrive
	if h.phantoms() != 1 || h.nStarts() != 1 {
		t.Fatalf("phantoms=%d starts=%d", h.phantoms(), h.nStarts())
	}
}
