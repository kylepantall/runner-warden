package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/core"
)

const gib = int64(1 << 30)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *fakeClock) NewTicker(time.Duration) Ticker { return &fakeTicker{ch: make(chan time.Time)} }

type fakeTicker struct{ ch chan time.Time }

func (t *fakeTicker) C() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()               {}

type fakeRunners struct {
	mu        sync.Mutex
	starts    []Grant
	lanes     []LaneKey
	stops     []string
	cancels   []int64
	reruns    []string
	startErr  error
	rerunErrs int
	calls     int // every StartRunner call, failed ones included
}

func (f *fakeRunners) StartRunner(_ context.Context, l LaneKey, g Grant) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.startErr != nil {
		return "", f.startErr
	}
	f.starts = append(f.starts, g)
	f.lanes = append(f.lanes, l)
	return "rn-" + g.Key, nil
}
func (f *fakeRunners) StopRunner(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, n)
	return nil
}
func (f *fakeRunners) ForceCancelRun(_ context.Context, _, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	return nil
}
func (f *fakeRunners) RerunJob(_ context.Context, _, _ string, id int64, job string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rerunErrs > 0 {
		f.rerunErrs--
		return fmt.Errorf("transient")
	}
	f.reruns = append(f.reruns, fmt.Sprintf("%d/%s", id, job))
	return nil
}

type fakeCap struct {
	mu sync.Mutex
	m  map[LaneKey]int
}

func (c *fakeCap) SetCapacity(l LaneKey, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[l] = n
}
func (c *fakeCap) get(l LaneKey) int { c.mu.Lock(); defer c.mu.Unlock(); return c.m[l] }

type fakeVMs struct {
	mu sync.Mutex
	m  map[string]int64
}

func (v *fakeVMs) VMsUp() map[string]int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]int64{}
	for k, x := range v.m {
		out[k] = x
	}
	return out
}

type harness struct {
	t   *testing.T
	s   *Scheduler
	clk *fakeClock
	run *fakeRunners
	cap *fakeCap
	n   int
}

func tgt(name string) core.ScaleSetKey {
	return core.ScaleSetKey{Target: core.Target{Owner: name}, OS: core.Linux}
}

func lane(name string, n int) LaneKey { return LaneKey{tgt(name), n} }

func newHarness(t *testing.T, mut func(*Options)) *harness {
	h := &harness{t: t, clk: &fakeClock{t: time.Unix(1_000_000, 0)}, run: &fakeRunners{}, cap: &fakeCap{m: map[LaneKey]int{}}}
	o := Options{
		Clock: h.clk, Runners: h.run, Capacity: h.cap,
		Budget: 10 * gib, DefaultJobMem: 1 * gib, LightThreshold: 1 * gib,
		Lanes: []LaneKey{lane("a", 1), lane("a", 2), lane("a", 3), lane("a", 4)},
	}
	if mut != nil {
		mut(&o)
	}
	h.s = New(o)
	return h
}

func (h *harness) wait() { h.s.Wait() }

func (h *harness) job(owner, name string) core.Job {
	h.n++
	return core.Job{JobID: fmt.Sprintf("j%d", h.n), RunID: int64(100 + h.n), Owner: owner, Repo: "r", Name: name,
		WorkflowRef: owner + "/r/.github/workflows/ci.yml@refs/heads/main"}
}

func (h *harness) assign(l LaneKey, j core.Job) core.Job {
	h.s.Assigned(l.ScaleSetKey, l.Lane, j)
	h.wait()
	return j
}

func (h *harness) startsFor(id string) (Grant, bool) {
	h.run.mu.Lock()
	defer h.run.mu.Unlock()
	for _, g := range h.run.starts {
		if g.Job.JobID == id {
			return g, true
		}
	}
	return Grant{}, false
}

func (h *harness) nStarts() int { h.run.mu.Lock(); defer h.run.mu.Unlock(); return len(h.run.starts) }

// begin fires Started for a job whose runner we launched.
func (h *harness) begin(l LaneKey, j core.Job) string {
	g, ok := h.startsFor(j.JobID)
	if !ok {
		h.t.Fatalf("no runner started for %s", j.JobID)
	}
	j.RunnerName = "rn-" + g.Key
	h.s.Started(l.ScaleSetKey, l.Lane, j)
	h.wait()
	return j.RunnerName
}

func (h *harness) done(l LaneKey, j core.Job, runner, result string) {
	j.RunnerName, j.Result = runner, result
	h.s.Completed(l.ScaleSetKey, l.Lane, j)
	h.wait()
}

func (h *harness) tick(d time.Duration) {
	h.clk.Advance(d)
	h.s.Tick()
	h.wait()
}

func (h *harness) free() int64 { return h.s.Snapshot().Ledger.Free }

func (h *harness) state(id string) JobState {
	for _, l := range h.s.Snapshot().Lanes {
		for _, j := range l.Jobs {
			if j.JobID == id {
				return j.State
			}
		}
	}
	return ""
}

var _ = admission.Core{}
