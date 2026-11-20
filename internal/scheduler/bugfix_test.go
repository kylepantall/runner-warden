package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/core"
)

func (h *harness) stopCount(name string) int {
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	n := 0
	for _, s := range h.run.stops {
		if s == name {
			n++
		}
	}
	return n
}

func (h *harness) runnerOf(id string) string {
	for _, l := range h.s.Snapshot().Lanes {
		for _, j := range l.Jobs {
			if j.JobID == id {
				return j.Runner
			}
		}
	}
	return ""
}

// ---- 1: jobs without an id ----

func TestRestoredJobsWithoutIDDoNotCollide(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	j1 := core.Job{Owner: "a", Repo: "r", Name: "one"}
	j2 := core.Job{Owner: "a", Repo: "r", Name: "two"}
	h.s.Restore([]RestoredRunner{
		{Name: "old-1", Lane: l, Created: h.clk.Now(), Job: &j1, Started: true},
		{Name: "old-2", Lane: lane("a", 2), Created: h.clk.Now(), Job: &j2, Started: true},
	})
	st := h.s.Snapshot()
	n := 0
	for _, ls := range st.Lanes {
		n += len(ls.Jobs)
	}
	if n != 2 {
		t.Fatalf("both restored jobs must be tracked, got %d", n)
	}
	free := h.free()
	h.done(l, core.Job{}, "old-1", "succeeded") // completion without a job id
	if h.free() != free+gib {
		t.Fatalf("only the first runner's memory is released: free=%d", h.free())
	}
	if h.s.Snapshot().Runners != 1 {
		t.Fatal("second runner must stay")
	}
	h.tick(4 * time.Minute)
	if h.stopCount("old-1") != 1 || h.stopCount("old-2") != 0 {
		t.Fatalf("stops=%v", h.run.stops)
	}
}

func TestRestoredJobAdoptsRealIDFromStartedAndCompleted(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("a", 1)
	h.s.Restore([]RestoredRunner{{Name: "old-1", Lane: l, Created: h.clk.Now(), Job: &core.Job{Owner: "a", Repo: "r", Name: "x"}, Started: true}})
	free := h.free()
	h.done(l, core.Job{JobID: "real-1", Owner: "a", Repo: "r", Name: "x"}, "old-1", "succeeded")
	if h.free() != free+gib || h.s.Snapshot().Runners != 0 {
		t.Fatal("unknown id must match the job by runner name")
	}
}

// ---- 2 / 7: runner death always reaps, never cancels the run ----

func TestRunnerDeathReaps(t *testing.T) {
	t.Run("mid-job after grace, once", func(t *testing.T) {
		h := newHarness(t, nil)
		l := lane("a", 1)
		j := h.assign(l, h.job("a", "x"))
		rn := h.begin(l, j)
		h.s.RunnerDied(rn, false)
		h.wait()
		if h.stopCount(rn) != 0 {
			t.Fatal("grace first")
		}
		h.s.RunnerDied(rn, false)
		h.wait()
		h.tick(4 * time.Minute)
		h.done(l, j, rn, "canceled")
		h.tick(4 * time.Minute)
		if h.stopCount(rn) != 1 {
			t.Fatalf("stops=%v", h.run.stops)
		}
	})
	t.Run("spare runner at once", func(t *testing.T) {
		h := newHarness(t, nil)
		h.s.Restore([]RestoredRunner{{Name: "idle", Lane: lane("a", 1), Created: h.clk.Now()}})
		h.s.RunnerDied("idle", false)
		h.wait()
		if h.stopCount("idle") != 1 || h.free() != 10*gib {
			t.Fatalf("stops=%v", h.run.stops)
		}
	})
	t.Run("job done at once", func(t *testing.T) {
		h := newHarness(t, nil)
		l := lane("a", 1)
		j := h.assign(l, h.job("a", "x"))
		rn := h.begin(l, j)
		h.s.HookCompleted(HookRequest{Runner: rn})
		h.s.RunnerDied(rn, false)
		h.wait()
		h.tick(4 * time.Minute)
		if h.stopCount(rn) != 1 {
			t.Fatalf("stops=%v", h.run.stops)
		}
	})
	t.Run("exit after Completed does not wait for grace", func(t *testing.T) {
		h := newHarness(t, nil)
		l := lane("a", 1)
		j := h.assign(l, h.job("a", "x"))
		rn := h.begin(l, j)
		h.done(l, j, rn, "succeeded")
		h.s.RunnerDied(rn, false)
		h.wait()
		if h.stopCount(rn) != 1 {
			t.Fatalf("stops=%v", h.run.stops)
		}
		h.tick(4 * time.Minute)
		if h.stopCount(rn) != 1 {
			t.Fatal("stopped twice")
		}
	})
	t.Run("died before taking its job", func(t *testing.T) {
		h := newHarness(t, nil)
		j := h.assign(lane("a", 1), h.job("a", "x"))
		rn := h.runnerOf(j.JobID)
		h.s.RunnerDied(rn, false)
		h.wait()
		if h.stopCount(rn) != 1 || h.state(j.JobID) != StateHeld {
			t.Fatalf("stops=%v state=%s", h.run.stops, h.state(j.JobID))
		}
	})
}

// ---- 5: job started while the daemon was down ----

func TestHookBindsJobForRunnerRestoredWithoutOne(t *testing.T) {
	h := newHarness(t, costRules(heavy("build", 4*gib)))
	l := lane("a", 1)
	h.s.Restore([]RestoredRunner{{Name: "old-1", Lane: l, Created: h.clk.Now()}})
	h.stats(l, 1, 1) // GitHub: that job is running
	resp, err := h.s.Hook(context.Background(), HookRequest{Runner: "old-1", RunID: 77, Repository: "a/r", JobKey: "build", Workflow: "CI"})
	if err != nil || !resp.Allow || resp.Reason != ReasonGranted {
		t.Fatalf("%+v %v", resp, err)
	}
	if h.free() != 6*gib {
		t.Fatalf("cost from rules, free=%d", h.free())
	}
	var found bool
	for _, ls := range h.s.Snapshot().Lanes {
		for _, j := range ls.Jobs {
			found = found || (j.Runner == "old-1" && j.State == StateRunning && j.RunID == 77)
		}
	}
	if !found {
		t.Fatal("job must be bound and running")
	}
	h.tick(11 * time.Minute) // NeverJob must not recycle it, no phantom for it
	if h.stopCount("old-1") != 0 || h.phantoms() != 0 || h.nStarts() != 0 {
		t.Fatalf("stops=%v phantoms=%d starts=%d", h.run.stops, h.phantoms(), h.nStarts())
	}
	// the real Started event adopts the id
	h.s.Started(l.ScaleSetKey, l.Lane, core.Job{JobID: "real", RunID: 77, Owner: "a", Repo: "r", Name: "build", RunnerName: "old-1"})
	h.wait()
	h.done(l, core.Job{JobID: "real", RunID: 77, Owner: "a", Repo: "r", Name: "build"}, "old-1", "succeeded")
	if h.free() != 10*gib {
		t.Fatalf("free=%d", h.free())
	}
}

func TestHookCompletedBindsJobForRunnerWithoutOne(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RerunOnLoss = true })
	h.s.Restore([]RestoredRunner{{Name: "old-1", Lane: lane("a", 1), Created: h.clk.Now()}})
	if !h.s.HookCompleted(HookRequest{Runner: "old-1", RunID: 5, Repository: "a/r", JobKey: "x"}) {
		t.Fatal("known runner")
	}
	h.s.RunnerDied("old-1", false)
	h.wait()
	if h.stopCount("old-1") != 1 {
		t.Fatal("normal exit reaps at once")
	}
}

// ---- 8: Windows VM memory ----

func winLane(n int) LaneKey {
	return LaneKey{core.ScaleSetKey{Target: core.Target{Owner: "a"}, OS: core.Windows}, n}
}

func TestWindowsGrantReservesVMBeforeStart(t *testing.T) {
	up := false
	vms := &fakeVMs{m: map[string]int64{}}
	h := newHarness(t, func(o *Options) {
		o.VMs = vms
		o.RunnerOverhead = gib / 2
		o.VMCost = func(LaneKey) (int64, bool) { return 8 * gib, up }
	})
	j := h.assign(winLane(1), h.job("a", "w"))
	g, ok := h.startsFor(j.JobID)
	if !ok {
		t.Fatal("must start: 8.5 GiB fits")
	}
	if g.MemBytes != gib/2 {
		t.Fatalf("job cost is overhead only, got %d", g.MemBytes)
	}
	want := 10*gib - gib/2 - 8*gib
	if h.free() != want {
		t.Fatalf("VM memory not reserved before start: free=%d", h.free())
	}
	j2 := h.assign(winLane(2), h.job("a", "w2"))
	if _, ok := h.startsFor(j2.JobID); ok {
		t.Fatal("second VM does not fit")
	}
	// the VM boots: the pending reservation converts, nothing is counted twice
	vms.mu.Lock()
	vms.m["win-1"] = 8 * gib
	vms.mu.Unlock()
	h.tick(time.Second)
	if h.free() != want {
		t.Fatalf("double counted after VM up: free=%d want=%d", h.free(), want)
	}
	up = true // a waiting job now finds the VM up: overhead only
	h.tick(time.Second)
	if _, ok := h.startsFor(j2.JobID); !ok {
		t.Fatal("VM already up: only overhead needed")
	}
	if h.free() != want-gib/2 {
		t.Fatalf("free=%d", h.free())
	}
}

func TestWindowsPendingVMReleasedWhenStartFails(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.VMCost = func(LaneKey) (int64, bool) { return 8 * gib, false } })
	h.run.startErr = context.Canceled
	j := h.assign(winLane(1), h.job("a", "w"))
	if h.free() != 10*gib || h.state(j.JobID) != StateHeld {
		t.Fatalf("free=%d state=%s", h.free(), h.state(j.JobID))
	}
}

// ---- 10: churn to another lane while the runner starts ----

func TestChurnToOtherLaneDetachesRunnerAndStartsOneThere(t *testing.T) {
	h := newHarness(t, nil)
	a, b := lane("a", 1), lane("a", 2)
	j := h.assign(a, h.job("a", "x"))
	first := h.runnerOf(j.JobID)
	h.tick(5 * time.Second)
	h.done(a, j, "", "canceled")
	h.assign(b, j) // GitHub reassigned it to lane 2
	if h.nStarts() != 2 {
		t.Fatalf("lane 2 must start its own runner, starts=%d", h.nStarts())
	}
	h.run.mu.Lock()
	last := h.run.lanes[len(h.run.lanes)-1]
	h.run.mu.Unlock()
	if last != b {
		t.Fatalf("second runner in lane %d", last.Lane)
	}
	if now := h.runnerOf(j.JobID); now == first || now == "" {
		t.Fatalf("job still points at lane 1's runner %q", now)
	}
	if h.free() != 9*gib { // only the new runner holds the job's memory; the old one is a spare
		t.Fatalf("free=%d", h.free())
	}
	if h.stopCount(first) != 0 {
		t.Fatal("spare is kept")
	}
}

func TestReassignToOtherLaneWithoutRunnerMovesQueue(t *testing.T) {
	h := newHarness(t, costRules(heavy("big", 20*gib)))
	a, b := lane("a", 1), lane("a", 2)
	j := h.assign(a, h.job("a", "big"))
	h.assign(b, j)
	var inA, inB int
	for _, ls := range h.s.Snapshot().Lanes {
		for _, x := range ls.Jobs {
			if x.JobID == j.JobID && ls.Lane == a {
				inA++
			}
			if x.JobID == j.JobID && ls.Lane == b {
				inB++
			}
		}
	}
	if inA != 0 || inB != 1 {
		t.Fatalf("inA=%d inB=%d", inA, inB)
	}
}

// ---- 15: pinned cores are fixed at StartRunner ----

func pinOpts(o *Options) {
	o.Cores = []admission.Core{{ID: 0, Threads: []int{0, 4}}, {ID: 1, Threads: []int{1, 5}}, {ID: 2, Threads: []int{2, 6}}, {ID: 3, Threads: []int{3, 7}}}
	o.ReservedCPUs = []int{0, 4}
	o.Costs = []admission.Rule{{Match: "*/*/*/pin", Mem: gib, PinnedCores: 2}}
}

func (h *harness) pinned() int { return h.s.Snapshot().Ledger.PinnedCores }

func TestRunnerNeverGainsPinnedCores(t *testing.T) {
	h := newHarness(t, pinOpts)
	l := lane("a", 1)
	light := h.assign(l, h.job("a", "light"))
	rn := h.begin(l, light) // started unpinned
	pin := h.job("a", "pin")
	pin.RunnerName = rn
	h.s.Started(l.ScaleSetKey, l.Lane, pin) // the runner takes a pinned job
	h.wait()
	if h.pinned() != 0 {
		t.Fatalf("ledger pinned %d cores the container never had", h.pinned())
	}
}

func TestRunnerKeepsItsCoresUntilRelease(t *testing.T) {
	h := newHarness(t, pinOpts)
	l := lane("a", 1)
	pin := h.assign(l, h.job("a", "pin"))
	rn := h.begin(l, pin)
	if h.pinned() != 2 {
		t.Fatalf("pinned=%d", h.pinned())
	}
	light := h.job("a", "light")
	light.RunnerName = rn
	h.s.Started(l.ScaleSetKey, l.Lane, light) // takes an unpinned job instead
	h.wait()
	if h.pinned() != 2 {
		t.Fatalf("the cpuset does not shrink: pinned=%d", h.pinned())
	}
	h.done(l, light, rn, "succeeded")
	if h.pinned() != 0 {
		t.Fatalf("release frees what was assigned: pinned=%d", h.pinned())
	}
}

func TestParkedRunnerGetsItsCoresAtStart(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		pinOpts(o)
		o.Budget = 3 * gib
		o.Costs = append(o.Costs, heavy("fill", 3*gib))
		o.Lanes = []LaneKey{lane("a", 1), lane("a", 2)}
	})
	f := h.assign(lane("a", 1), h.job("a", "fill"))
	h.begin(lane("a", 1), f)
	p := h.assign(lane("a", 2), h.job("a", "pin"))
	h.assign(lane("a", 1), h.job("a", "pin"))
	h.tick(3 * time.Minute) // all lanes held: one parked promotion
	var parked *Grant
	h.run.mu.Lock()
	for i := range h.run.starts {
		if h.run.starts[i].Parked {
			parked = &h.run.starts[i]
		}
	}
	h.run.mu.Unlock()
	if parked == nil {
		t.Fatal("expected a parked promotion")
	}
	if parked.CPUSet == "" {
		t.Fatal("a parked pinned job's runner is started with its cores")
	}
	_ = p
}

// A runner GitHub reports busy, whose job we never saw, must not be recycled
// as a never-job runner, and its Completed (matched by runner name) frees it.
func TestRestoreBusyRunnerWithoutJob(t *testing.T) {
	h := newHarness(t, nil)
	l := lane("o", 1)
	h.s.Restore([]RestoredRunner{{Name: "rn-x", Lane: l, Started: true}})
	h.wait()
	h.tick(h.s.o.NeverJob + time.Minute)
	h.run.mu.Lock()
	stops := len(h.run.stops)
	h.run.mu.Unlock()
	if stops != 0 {
		t.Fatalf("busy restored runner stopped %d times", stops)
	}
	h.done(l, core.Job{JobID: "real-1", Owner: "o", Repo: "r", Name: "x"}, "rn-x", "succeeded")
	h.tick(h.s.o.ReapGrace + time.Second)
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	if len(h.run.stops) != 1 {
		t.Fatalf("runner not reaped after completion: stops=%d", len(h.run.stops))
	}
}
