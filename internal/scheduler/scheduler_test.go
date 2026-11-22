package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/core"
)

func costRules(r ...admission.Rule) func(*Options) {
	return func(o *Options) { o.Costs = r }
}

func heavy(name string, mem int64) admission.Rule {
	return admission.Rule{Match: "*/*/*/" + name, Mem: mem}
}

func TestCapacityIsStartedPlusOne(t *testing.T) {
	h := newHarness(t, nil)
	l1 := lane("a", 1)
	if h.cap.get(l1) != 1 {
		t.Fatalf("idle cap=%d", h.cap.get(l1))
	}
	j := h.assign(l1, h.job("a", "x"))
	if h.cap.get(l1) != 1 { // assigned-unstarted is the +1
		t.Fatalf("assigned cap=%d", h.cap.get(l1))
	}
	r := h.begin(l1, j)
	if h.cap.get(l1) != 2 {
		t.Fatalf("started cap=%d", h.cap.get(l1))
	}
	h.s.Drain(true)
	if h.cap.get(l1) != 1 || h.cap.get(lane("a", 2)) != 0 {
		t.Fatalf("drain caps=%d,%d", h.cap.get(l1), h.cap.get(lane("a", 2)))
	}
	h.done(l1, j, r, "succeeded")
	if h.cap.get(l1) != 0 {
		t.Fatalf("drained cap=%d", h.cap.get(l1))
	}
	h.s.Drain(false)
	if h.cap.get(l1) != 1 {
		t.Fatalf("undrained cap=%d", h.cap.get(l1))
	}
}

func TestDrainStartsNothingNew(t *testing.T) {
	h := newHarness(t, nil)
	h.s.Drain(true)
	j := h.assign(lane("a", 1), h.job("a", "x"))
	if h.nStarts() != 0 || h.state(j.JobID) != StateHeld {
		t.Fatal("drain must keep jobs held")
	}
	h.s.Drain(false)
	h.wait()
	if h.nStarts() != 1 {
		t.Fatal("undrain should start it")
	}
}

func TestHeavyHeldWhileLightsFlow(t *testing.T) {
	h := newHarness(t, costRules(heavy("big*", 6*gib), heavy("huge", 9*gib)))
	r1 := h.assign(lane("a", 1), h.job("a", "big1"))
	h.begin(lane("a", 1), r1)
	hv := h.assign(lane("a", 2), h.job("a", "huge")) // needs 9, only 4 free
	if h.state(hv.JobID) != StateHeld {
		t.Fatal("heavy head should be held")
	}
	for i := 0; i < 3; i++ {
		l := h.assign(lane("a", 3), h.job("a", "light"))
		if _, ok := h.startsFor(l.JobID); !ok {
			t.Fatalf("light %d not started", i)
		}
		h.begin(lane("a", 3), l)
	}
	if _, ok := h.startsFor(hv.JobID); ok {
		t.Fatal("heavy must still wait")
	}
}

func TestAntiStarvationBlocksHeavierOvertakers(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Costs = []admission.Rule{heavy("old", 9*gib), heavy("mid", 3*gib), heavy("light", 1*gib)}
	})
	r := h.assign(lane("a", 1), h.job("a", "mid"))
	h.begin(lane("a", 1), r)                       // 3 of 10 used
	o := h.assign(lane("a", 2), h.job("a", "old")) // needs 9, held
	// before reserve_after a mid job may overtake
	m1 := h.assign(lane("a", 3), h.job("a", "mid"))
	if _, ok := h.startsFor(m1.JobID); !ok {
		t.Fatal("mid should overtake before reserve_after")
	}
	h.tick(6 * time.Minute)
	m2 := h.assign(lane("a", 4), h.job("a", "mid"))
	if _, ok := h.startsFor(m2.JobID); ok {
		t.Fatal("mid must not overtake after reserve_after")
	}
	l := h.assign(lane("a", 4), h.job("a", "light")) // second job on lane 4 is queued behind m2
	_ = l
	if _, ok := h.startsFor(o.JobID); ok {
		t.Fatal("old cannot fit yet")
	}
}

func TestLightOvertakesAfterReserveAfter(t *testing.T) {
	h := newHarness(t, costRules(heavy("old", 9*gib), heavy("mid", 3*gib)))
	r := h.assign(lane("a", 1), h.job("a", "mid"))
	h.begin(lane("a", 1), r)
	h.assign(lane("a", 2), h.job("a", "old"))
	h.tick(6 * time.Minute)
	l := h.assign(lane("a", 3), h.job("a", "light"))
	if _, ok := h.startsFor(l.JobID); !ok {
		t.Fatal("light jobs may always overtake")
	}
}

func TestOldHeavyAdmittedWhenMemoryFrees(t *testing.T) {
	h := newHarness(t, costRules(heavy("old", 9*gib), heavy("mid", 5*gib)))
	r := h.assign(lane("a", 1), h.job("a", "mid"))
	rn := h.begin(lane("a", 1), r)
	o := h.assign(lane("a", 2), h.job("a", "old"))
	h.tick(6 * time.Minute)
	m := h.assign(lane("a", 3), h.job("a", "mid"))
	if _, ok := h.startsFor(m.JobID); ok {
		t.Fatal("blocked mid")
	}
	h.done(lane("a", 1), r, rn, "succeeded")
	if _, ok := h.startsFor(o.JobID); !ok {
		t.Fatal("old heavy must go first after release")
	}
	if _, ok := h.startsFor(m.JobID); ok {
		t.Fatal("mid still has to wait")
	}
}

func twoLaneOpts(o *Options) {
	o.Lanes = []LaneKey{lane("a", 1), lane("a", 2)}
	o.MaxParked = 4
}

func TestAllLanesHeldPromotesParkedAndHookUnblocks(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		twoLaneOpts(o)
		o.Costs = []admission.Rule{heavy("fill", 9*gib), heavy("big", 8*gib)}
	})
	f := h.assign(lane("a", 1), h.job("a", "fill"))
	fr := h.begin(lane("a", 1), f)
	b1 := h.assign(lane("a", 1), h.job("a", "big"))
	b2 := h.assign(lane("a", 2), h.job("a", "big"))
	h.tick(time.Minute)
	if h.nStarts() != 1 {
		t.Fatal("nothing promoted before hol_grace")
	}
	h.tick(90 * time.Second)
	if h.nStarts() != 2 {
		t.Fatalf("expected one promotion, starts=%d", h.nStarts())
	}
	var pj, other core.Job
	if g, ok := h.startsFor(b1.JobID); ok && g.Parked {
		pj, other = b1, b2
	} else if g, ok := h.startsFor(b2.JobID); ok && g.Parked {
		pj, other = b2, b1
	} else {
		t.Fatal("promoted runner must be parked")
	}
	if _, ok := h.startsFor(other.JobID); ok {
		t.Fatal("only one promotion")
	}
	pl := lane("a", 1)
	if pj.JobID == b2.JobID {
		pl = lane("a", 2)
	}
	rn := h.begin(pl, pj)
	if h.state(pj.JobID) != StateParked {
		t.Fatalf("state=%s", h.state(pj.JobID))
	}
	type res struct {
		r   HookResponse
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := h.s.Hook(context.Background(), HookRequest{Runner: rn})
		ch <- res{r, err}
	}()
	select {
	case <-ch:
		t.Fatal("hook should block while parked")
	case <-time.After(50 * time.Millisecond):
	}
	h.done(lane("a", 1), f, fr, "succeeded")
	r := <-ch
	if r.err != nil || !r.r.Allow || r.r.Reason != ReasonGranted {
		t.Fatalf("%+v", r)
	}
	if h.state(pj.JobID) != StateRunning {
		t.Fatalf("state=%s", h.state(pj.JobID))
	}
}

func TestHoldMaxPromotesSingleHeld(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Costs = []admission.Rule{heavy("fill", 9*gib), heavy("big", 8*gib)}
	})
	f := h.assign(lane("a", 1), h.job("a", "fill"))
	h.begin(lane("a", 1), f)
	b := h.assign(lane("a", 2), h.job("a", "big")) // other lanes empty: not all held
	h.tick(10 * time.Minute)
	if _, ok := h.startsFor(b.JobID); ok {
		t.Fatal("no promotion before hold_max")
	}
	h.tick(25 * time.Minute)
	g, ok := h.startsFor(b.JobID)
	if !ok || !g.Parked {
		t.Fatal("hold_max should promote")
	}
}

func TestMaxParkedRespected(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		twoLaneOpts(o)
		o.MaxParked = 1
		o.Budget = 4 * gib
		o.RunnerOverhead = 100 << 20
		o.Costs = []admission.Rule{heavy("big", 8*gib)}
	})
	h.assign(lane("a", 1), h.job("a", "big"))
	h.assign(lane("a", 2), h.job("a", "big"))
	h.tick(3 * time.Minute)
	h.tick(10 * time.Second)
	if h.nStarts() != 1 {
		t.Fatalf("starts=%d", h.nStarts())
	}
}

func TestAdmissionTimeout(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		twoLaneOpts(o)
		o.Costs = []admission.Rule{heavy("fill", 9*gib), heavy("big", 8*gib)}
	})
	f := h.assign(lane("a", 1), h.job("a", "fill"))
	h.begin(lane("a", 1), f)
	b := h.assign(lane("a", 1), h.job("a", "big"))
	h.assign(lane("a", 2), h.job("a", "big"))
	h.tick(3 * time.Minute)
	rn := h.begin(lane("a", 1), b)
	ch := make(chan HookResponse, 1)
	go func() { r, _ := h.s.Hook(context.Background(), HookRequest{Runner: rn}); ch <- r }()
	time.Sleep(20 * time.Millisecond)
	h.tick(41 * time.Minute)
	r := <-ch
	if r.Allow || r.Reason != ReasonAdmissionTimeout {
		t.Fatalf("%+v", r)
	}
}

func TestHookContextCancel(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := h.s.Hook(ctx, HookRequest{Runner: "ghost"}); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestHookKnownRunnerImmediateAndUnknownFailsOpen(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	rn := h.begin(l, j)
	r, err := h.s.Hook(context.Background(), HookRequest{Runner: rn})
	if err != nil || !r.Allow {
		t.Fatalf("%+v %v", r, err)
	}
	ch := make(chan HookResponse, 1)
	go func() { r, _ := h.s.Hook(context.Background(), HookRequest{Runner: "stranger"}); ch <- r }()
	time.Sleep(20 * time.Millisecond)
	h.tick(time.Minute)
	if r := <-ch; !r.Allow || r.Reason != ReasonUnknownRunner {
		t.Fatalf("%+v", r)
	}
}

func TestChurnKeepsAgeAndDoesNotRestart(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 9*gib), heavy("fill", 5*gib)))
	f := h.assign(lane("a", 1), h.job("a", "fill"))
	h.begin(lane("a", 1), f)
	b := h.assign(lane("a", 2), h.job("a", "big")) // held
	h.tick(5 * time.Minute)
	h.done(lane("a", 2), b, "", "canceled")
	if h.state(b.JobID) != StateUnbound {
		t.Fatalf("state=%s", h.state(b.JobID))
	}
	h.tick(10 * time.Second)
	h.assign(lane("a", 2), b)
	if h.state(b.JobID) != StateHeld {
		t.Fatal("reassigned job is held again")
	}
	for _, l := range h.s.Snapshot().Lanes {
		for _, j := range l.Jobs {
			if j.JobID == b.JobID && j.Age < 5*time.Minute {
				t.Fatalf("age lost: %v", j.Age)
			}
		}
	}
	if h.nStarts() != 1 || len(h.run.stops) != 0 {
		t.Fatal("churn must not start or stop runners")
	}
}

func TestChurnWhileStartingKeepsSingleRunner(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	h.tick(5 * time.Minute)
	h.done(l, j, "", "canceled")
	free := h.free()
	h.assign(l, j)
	if h.nStarts() != 1 || len(h.run.stops) != 0 || h.free() != free {
		t.Fatal("runner must be reused across churn")
	}
	if h.state(j.JobID) != StateStarting {
		t.Fatalf("state=%s", h.state(j.JobID))
	}
	h.begin(l, j)
	if h.state(j.JobID) == StateHeld {
		t.Fatal("started")
	}
}

func TestChurnReissuedJobIDKeepsAge(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 20*gib)))
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "big"))
	h.tick(4 * time.Minute)
	h.done(l, j, "", "canceled")
	j2 := j
	j2.JobID = "new-id"
	h.assign(l, j2)
	snap := h.s.Snapshot()
	var age time.Duration
	var count int
	for _, ln := range snap.Lanes {
		for _, js := range ln.Jobs {
			count++
			age = js.Age
		}
	}
	if count != 1 || age < 4*time.Minute {
		t.Fatalf("count=%d age=%v", count, age)
	}
}

func TestUnboundDroppedAfterGrace(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 20*gib)))
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "big"))
	h.done(l, j, "", "canceled")
	h.tick(2 * time.Minute)
	if h.state(j.JobID) != "" {
		t.Fatal("unbound job should be dropped")
	}
}

func TestNeverJobRecyclesRunner(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	free := h.free()
	h.tick(11 * time.Minute)
	if len(h.run.stops) != 1 {
		t.Fatalf("stops=%v", h.run.stops)
	}
	if h.free() != free+gib {
		t.Fatal("reservation should be released")
	}
	h.tick(11 * time.Second)
	if h.nStarts() != 2 { // job still unstarted: a fresh runner is planned
		t.Fatalf("starts=%d", h.nStarts())
	}
	_ = j
}

func TestRunnerDeathRerunsOnlyTheLostJobOnce(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RerunOnLoss = true })
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	sib := h.assign(lane("a", 2), h.job("a", "sibling"))
	h.begin(lane("a", 2), sib)
	rn := h.begin(l, j)
	h.s.RunnerDied(rn, false)
	h.wait()
	h.s.RunnerDied(rn, false)
	h.wait()
	if len(h.run.cancels) != 0 {
		t.Fatal("a lost runner must not cancel the run (matrix siblings)")
	}
	h.tick(time.Minute)
	if len(h.run.reruns) != 0 {
		t.Fatal("rerun waits for GitHub to fail the lost job")
	}
	h.done(l, j, rn, "canceled")
	h.tick(time.Minute)
	if len(h.run.reruns) != 1 || len(h.run.cancels) != 0 {
		t.Fatalf("reruns=%v cancels=%v", h.run.reruns, h.run.cancels)
	}
	// same run+job lost again: never rerun twice
	j2 := j
	j2.JobID = "again"
	h.assign(l, j2)
	rn2 := h.begin(l, j2)
	h.s.RunnerDied(rn2, false)
	h.done(l, j2, rn2, "canceled")
	h.tick(time.Minute)
	if len(h.run.reruns) != 1 {
		t.Fatalf("reruns=%v", h.run.reruns)
	}
}

func TestOOMDeathDoesNotRerun(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RerunOnLoss = true })
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	rn := h.begin(l, j)
	h.s.RunnerDied(rn, true)
	h.wait()
	h.done(l, j, rn, "canceled")
	h.tick(time.Minute)
	if len(h.run.cancels) != 0 || len(h.run.reruns) != 0 {
		t.Fatalf("cancels=%v reruns=%v", h.run.cancels, h.run.reruns)
	}
}

func TestNormalExitAfterJobCompletedHookIsNotALoss(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RerunOnLoss = true })
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	rn := h.begin(l, j)
	if !h.s.HookCompleted(HookRequest{Runner: rn}) {
		t.Fatal("known runner")
	}
	h.s.RunnerDied(rn, false)
	h.wait()
	if len(h.run.cancels) != 0 {
		t.Fatal("no cancel after normal exit")
	}
}

func TestRerunRetriesOnError(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RerunOnLoss = true })
	h.run.rerunErrs = 1
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	rn := h.begin(l, j)
	h.s.RunnerDied(rn, false)
	h.done(l, j, rn, "canceled")
	h.tick(time.Minute)
	h.tick(time.Minute)
	if len(h.run.reruns) != 1 {
		t.Fatalf("reruns=%v", h.run.reruns)
	}
}

func TestCompletedReleasesAndReaps(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	rn := h.begin(l, j)
	if h.free() != 10*gib-gib {
		t.Fatalf("free=%d", h.free())
	}
	h.done(l, j, rn, "succeeded")
	if h.free() != 10*gib {
		t.Fatal("memory not released")
	}
	if len(h.run.stops) != 0 {
		t.Fatal("reap waits for grace")
	}
	h.tick(4 * time.Minute)
	if len(h.run.stops) != 1 || h.run.stops[0] != rn {
		t.Fatalf("stops=%v", h.run.stops)
	}
}

func TestStartFailureRetriesLater(t *testing.T) {
	h := newHarness(t, nil)
	h.run.startErr = errors.New("docker down")
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	if h.state(j.JobID) != StateHeld || h.free() != 10*gib {
		t.Fatalf("state=%s", h.state(j.JobID))
	}
	h.run.mu.Lock()
	h.run.startErr = nil
	h.run.mu.Unlock()
	h.tick(11 * time.Second)
	if h.nStarts() != 1 {
		t.Fatal("should retry after backoff")
	}
}

func TestLaneFIFOOnlyHeadGetsRunner(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	a := h.assign(l, h.job("a", "x"))
	h.clk.Advance(time.Second)
	b := h.assign(l, h.job("a", "y")) // second unstarted job in the same lane
	if _, ok := h.startsFor(b.JobID); ok {
		t.Fatal("only the lane head may be started")
	}
	h.begin(l, a)
	if _, ok := h.startsFor(b.JobID); !ok {
		t.Fatal("next job starts after head started")
	}
}

func TestPinnedCoresNeverShared(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Cores = []admission.Core{{ID: 0, Threads: []int{0, 4}}, {ID: 1, Threads: []int{1, 5}}, {ID: 2, Threads: []int{2, 6}}, {ID: 3, Threads: []int{3, 7}}}
		o.ReservedCPUs = []int{0, 4}
		o.Costs = []admission.Rule{{Match: "*/*/*/pin", Mem: gib, PinnedCores: 2}}
	})
	j1 := h.assign(lane("a", 1), h.job("a", "pin"))
	j2 := h.assign(lane("a", 2), h.job("a", "pin"))
	g1, ok1 := h.startsFor(j1.JobID)
	if !ok1 || g1.CPUSet == "" {
		t.Fatal("first pinned job should start with a cpuset")
	}
	if _, ok := h.startsFor(j2.JobID); ok {
		t.Fatal("only 3 pinnable cores: second 2-core job must wait")
	}
	other := h.assign(lane("a", 3), h.job("a", "light")) // unpinned jobs are unaffected
	if _, ok := h.startsFor(other.JobID); !ok {
		t.Fatal("unpinned job should start")
	}
	rn := h.begin(lane("a", 1), j1)
	h.done(lane("a", 1), j1, rn, "succeeded")
	g2, ok := h.startsFor(j2.JobID)
	if !ok || g2.CPUSet != g1.CPUSet {
		t.Fatalf("second job takes the freed cores: %+v", g2)
	}
}

func TestWeightedFairnessWhenContended(t *testing.T) {
	ka, kb := tgt("a"), tgt("b")
	h := newHarness(t, func(o *Options) {
		o.Budget = 6 * gib
		o.ExternalReserved = 2 * gib
		o.Lanes = []LaneKey{{ka, 1}, {ka, 2}, {kb, 1}, {kb, 2}}
		o.Targets = map[core.Target]TargetOpts{ka.Target: {Weight: 1}, kb.Target: {Weight: 2}}
		o.Costs = []admission.Rule{heavy("w", 2*gib)}
	})
	a1 := h.assign(LaneKey{ka, 1}, h.job("a", "w"))
	b1 := h.assign(LaneKey{kb, 1}, h.job("b", "w"))
	h.begin(LaneKey{ka, 1}, a1)
	h.begin(LaneKey{kb, 1}, b1)
	a2 := h.assign(LaneKey{ka, 2}, h.job("a", "w")) // older
	h.clk.Advance(time.Second)
	b2 := h.assign(LaneKey{kb, 2}, h.job("b", "w"))
	if h.nStarts() != 2 {
		t.Fatal("budget full")
	}
	h.s.SetExternalReserved(0) // frees exactly one slot
	h.wait()
	if _, ok := h.startsFor(b2.JobID); !ok {
		t.Fatal("weight-2 target should win despite a2 being older")
	}
	if _, ok := h.startsFor(a2.JobID); ok {
		t.Fatal("a2 must wait")
	}
}

func TestWorkConservingSingleTargetTakesWholeBudget(t *testing.T) {
	ls := []LaneKey{lane("a", 1), lane("a", 2), lane("a", 3), lane("a", 4), lane("a", 5)}
	h := newHarness(t, func(o *Options) {
		o.Lanes = ls
		o.Budget = 10 * gib
		o.Costs = []admission.Rule{heavy("w", 2*gib)}
		o.Targets = map[core.Target]TargetOpts{{Owner: "a"}: {Weight: 1}, {Owner: "b"}: {Weight: 5}}
	})
	for _, l := range ls {
		h.assign(l, h.job("a", "w"))
	}
	if h.nStarts() != 5 || h.free() != 0 {
		t.Fatalf("starts=%d free=%d", h.nStarts(), h.free())
	}
}

func TestTargetAndGlobalMax(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Targets = map[core.Target]TargetOpts{{Owner: "a"}: {Weight: 1, Max: 2}}
	})
	for i := 1; i <= 4; i++ {
		h.assign(lane("a", i), h.job("a", "x"))
	}
	if h.nStarts() != 2 {
		t.Fatalf("starts=%d", h.nStarts())
	}
	h2 := newHarness(t, func(o *Options) { o.MaxRunners = 1 })
	h2.assign(lane("a", 1), h2.job("a", "x"))
	h2.assign(lane("a", 2), h2.job("a", "x"))
	if h2.nStarts() != 1 {
		t.Fatal("max_runners")
	}
}

func TestVMReservationBlocksUntilVMDown(t *testing.T) {
	vms := &fakeVMs{m: map[string]int64{"win": 8 * gib}}
	h := newHarness(t, func(o *Options) { o.VMs = vms; o.Costs = []admission.Rule{heavy("big", 5*gib)} })
	h.tick(time.Second)
	j := h.assign(lane("a", 1), h.job("a", "big"))
	if _, ok := h.startsFor(j.JobID); ok {
		t.Fatal("VM holds the memory")
	}
	vms.mu.Lock()
	delete(vms.m, "win")
	vms.mu.Unlock()
	h.tick(time.Second)
	if _, ok := h.startsFor(j.JobID); !ok {
		t.Fatal("VM down frees memory")
	}
}

func TestRestoreCountsRunningRunners(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	job := h.job("a", "x")
	h.s.Restore([]RestoredRunner{
		{Name: "old-1", Lane: l, Created: h.clk.Now(), Job: &job, Started: true},
		{Name: "idle-1", Lane: lane("a", 2), Created: h.clk.Now()},
	})
	if h.free() >= 10*gib {
		t.Fatal("restored reservations must count")
	}
	if h.cap.get(l) != 2 {
		t.Fatalf("cap=%d", h.cap.get(l))
	}
	if h.state(job.JobID) != StateRunning {
		t.Fatalf("state=%s", h.state(job.JobID))
	}
	h.tick(11 * time.Minute) // idle restored runner recycles
	if len(h.run.stops) != 1 || h.run.stops[0] != "idle-1" {
		t.Fatalf("stops=%v", h.run.stops)
	}
}

func TestStartedForForeignRunnerReservesMemory(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j := h.job("a", "x")
	j.RunnerName = "adopted"
	h.s.Started(l.ScaleSetKey, l.Lane, j)
	if h.free() != 10*gib-gib {
		t.Fatalf("free=%d", h.free())
	}
	h.done(l, j, "adopted", "succeeded")
	if h.free() != 10*gib {
		t.Fatal("released")
	}
}

type recObserver struct{ got []JobResult }

func (r *recObserver) JobFinished(j JobResult) { r.got = append(r.got, j) }

func TestObserverSeesFinishedJob(t *testing.T) {
	obs := &recObserver{}
	h := newHarness(t, func(o *Options) { o.Observer = obs })
	l := lane("a", 1)
	j := h.assign(l, h.job("a", "x"))
	rn := h.begin(l, j)
	h.clk.Advance(time.Minute)
	h.done(l, j, rn, "succeeded")
	if len(obs.got) != 1 || obs.got[0].Finished.Sub(obs.got[0].Started) != time.Minute || obs.got[0].Cost != gib {
		t.Fatalf("%+v", obs.got)
	}
}

func TestSnapshotShape(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 20*gib)))
	l := lane("a", 1)
	h.assign(l, h.job("a", "big"))
	st := h.s.Snapshot()
	if st.Held != 1 || len(st.Lanes) != 4 || st.Lanes[0].Jobs[0].Workflow != "ci.yml" {
		t.Fatalf("%+v", st)
	}
}

func TestRunStopsOnContext(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.s.Run(ctx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOptionsDefaults(t *testing.T) {
	var o Options
	o.defaults()
	if o.NeverJob != 10*time.Minute || o.MaxParked != 4 || o.UnboundDrop != 90*time.Second {
		t.Fatalf("%+v", o)
	}
}

func TestCPUBudgetHoldsJobsThatDoNotFit(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.CPUBudget, o.DefaultJobCPU = 4000, 2000
		o.Costs = []admission.Rule{{Match: "a/r/ci.yml/heavy", Mem: gib, CPU: 3000}}
	})
	l1, l2 := lane("a", 1), lane("a", 2)
	heavy := h.assign(l1, h.job("a", "heavy"))
	if _, ok := h.startsFor(heavy.JobID); !ok {
		t.Fatal("heavy job not started on an idle host")
	}
	light := h.assign(l2, h.job("a", "light"))
	if _, ok := h.startsFor(light.JobID); ok || h.state(light.JobID) != StateHeld {
		t.Fatalf("2 cpus started with 1 free: state=%s", h.state(light.JobID))
	}
	if got := h.s.Snapshot().Ledger.CPUReserved; got != 3000 {
		t.Fatalf("reserved=%d", got)
	}
	r := h.begin(l1, heavy)
	h.done(l1, heavy, r, "succeeded")
	if _, ok := h.startsFor(light.JobID); !ok {
		t.Fatal("held job not started once CPU freed")
	}
}

func TestRemoteWindowsJobTakesNoCPU(t *testing.T) {
	win := LaneKey{core.ScaleSetKey{Target: core.Target{Owner: "a"}, OS: core.Windows}, 1}
	win2 := LaneKey{win.ScaleSetKey, 2}
	remote := true
	h := newHarness(t, func(o *Options) {
		o.CPUBudget, o.DefaultJobCPU = 4000, 2000
		o.Lanes = append(o.Lanes, win, win2)
		o.VMCost = func(LaneKey) (int64, bool) { return 0, true }
		o.RemoteVM = func(LaneKey) bool { return remote }
	})
	j := h.assign(win, h.job("a", "win"))
	if _, ok := h.startsFor(j.JobID); !ok {
		t.Fatal("job not started")
	}
	if got := h.s.Snapshot().Ledger.CPUReserved; got != 0 {
		t.Fatalf("remote job reserved %d millicpus here", got)
	}
	remote = false
	if j2 := h.assign(win2, h.job("a", "win2")); func() bool { _, ok := h.startsFor(j2.JobID); return !ok }() {
		t.Fatal("second job not started")
	}
	if got := h.s.Snapshot().Ledger.CPUReserved; got != 2000 {
		t.Fatalf("local VM job should reserve its CPU, got %d", got)
	}
	// The remote job starting later must not be charged here because the
	// lane's next job would now go to a local VM.
	h.begin(win, j)
	if got := h.s.Snapshot().Ledger.CPUReserved; got != 2000 {
		t.Fatalf("remote job charged after it started: %d", got)
	}
}

func TestLinuxJobsOverflowToHost(t *testing.T) {
	up := true
	h := newHarness(t, func(o *Options) {
		o.DefaultJobMem = 6 * gib
		o.LinuxHosts = []RemoteHost{{Name: "pc", Slots: 1, Serves: func(t core.Target) bool { return t.Owner == "a" }}}
		o.HostUp = func(string) bool { return up }
	})
	j1 := h.assign(lane("a", 1), h.job("a", "one"))
	if g, ok := h.startsFor(j1.JobID); !ok || g.Host != "" {
		t.Fatalf("first job runs here: %+v %v", g, ok)
	}
	free := h.free()
	j2 := h.assign(lane("a", 2), h.job("a", "two"))
	g2, ok := h.startsFor(j2.JobID)
	if !ok || g2.Host != "pc" || g2.MemBytes != 0 {
		t.Fatalf("overflow goes to the host: %+v %v", g2, ok)
	}
	r2 := h.begin(lane("a", 2), j2)
	if h.free() != free {
		t.Fatalf("host job reserved memory here: %d -> %d", free, h.free())
	}
	j3 := h.assign(lane("a", 3), h.job("a", "three"))
	if _, ok := h.startsFor(j3.JobID); ok {
		t.Fatal("host slot is full; the job must wait")
	}
	up = false
	h.done(lane("a", 2), j2, r2, "succeeded")
	h.tick(time.Second)
	if _, ok := h.startsFor(j3.JobID); ok {
		t.Fatal("an unreachable host takes no job")
	}
	up = true
	h.tick(time.Second)
	if g, ok := h.startsFor(j3.JobID); !ok || g.Host != "pc" {
		t.Fatalf("freed slot takes the next job: %+v %v", g, ok)
	}
}

func TestLocalJobsNeverGoToHost(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.DefaultJobMem = 6 * gib
		o.Costs = []admission.Rule{{Match: "a/r/ci.yml/deploy", Local: true}, {Match: "a/r/ci.yml/emu", Devices: []string{"/dev/kvm"}}}
		o.LinuxHosts = []RemoteHost{{Name: "pc", Slots: 4}}
	})
	h.assign(lane("a", 1), h.job("a", "fill"))
	for i, name := range []string{"deploy", "emu"} {
		j := h.assign(lane("a", 2+i), h.job("a", name))
		if g, ok := h.startsFor(j.JobID); ok {
			t.Fatalf("%s went to %q", name, g.Host)
		}
	}
}

func TestRestoredHostRunnerReservesNothing(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.LinuxHosts = []RemoteHost{{Name: "pc", Slots: 1}}
	})
	free := h.free()
	j := h.job("a", "x")
	j.RunnerName = "rn-pc"
	h.s.Restore([]RestoredRunner{{Name: "rn-pc", Lane: lane("a", 1), Job: &j, Started: true, Host: "pc"}})
	h.wait()
	if h.free() != free {
		t.Fatalf("restored host runner reserved memory: %d -> %d", free, h.free())
	}
	// Its slot is taken: an overflow job waits for it.
	h.s.locked(func() {
		if got := h.s.freeHost(lane("a", 2)); got != "" {
			t.Errorf("slot should be used, got %q", got)
		}
	})
}

func TestHeldOldJobDoesNotBlockOverflow(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.DefaultJobMem = 4 * gib
		o.Costs = []admission.Rule{{Match: "a/r/ci.yml/big", Mem: 9 * gib, Local: true}}
		o.ReserveAfter = time.Minute
		o.LinuxHosts = []RemoteHost{{Name: "pc", Slots: 2}}
	})
	h.assign(lane("a", 1), h.job("a", "small")) // 4GiB here, 6GiB free
	big := h.assign(lane("a", 2), h.job("a", "big"))
	h.tick(2 * time.Minute) // big is now past ReserveAfter
	j := h.assign(lane("a", 3), h.job("a", "next"))
	if g, ok := h.startsFor(j.JobID); !ok || g.Host != "pc" {
		t.Fatalf("overflow must not wait behind a held local job: %+v %v", g, ok)
	}
	if _, ok := h.startsFor(big.JobID); ok {
		t.Fatal("big job cannot fit yet")
	}
}

func TestMacJobsRunOnlyOnMacHosts(t *testing.T) {
	mac := LaneKey{core.ScaleSetKey{Target: core.Target{Owner: "a"}, OS: core.MacOS}, 1}
	mac2 := LaneKey{mac.ScaleSetKey, 2}
	h := newHarness(t, func(o *Options) {
		o.Lanes = append(o.Lanes, mac, mac2)
		o.DefaultJobMem = 2 * gib // above light_threshold: blockable
		o.ReserveAfter = time.Minute
		o.HostsOnly = map[core.OS]bool{core.MacOS: true}
		o.LinuxHosts = []RemoteHost{{Name: "pc", Slots: 1}, {Name: "mac", OS: core.MacOS, Slots: 1}}
	})
	free := h.free()
	j := h.assign(mac, h.job("a", "ios"))
	if g, ok := h.startsFor(j.JobID); !ok || g.Host != "mac" {
		t.Fatalf("mac job goes to the mac: %+v %v", g, ok)
	}
	if h.free() != free {
		t.Fatal("mac job reserved memory here")
	}
	j2 := h.assign(mac2, h.job("a", "ios2"))
	h.tick(2 * time.Minute) // j2 waits for the busy mac past ReserveAfter
	if _, ok := h.startsFor(j2.JobID); ok {
		t.Fatal("one job per mac")
	}
	l := h.assign(lane("a", 1), h.job("a", "lin"))
	if g, ok := h.startsFor(l.JobID); !ok || g.Host != "" {
		t.Fatalf("a waiting mac job must not hold back linux jobs here: %+v %v", g, ok)
	}
}
