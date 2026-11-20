package gh

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/kylepantall/runner-warden/internal/core"
)

type rec struct {
	mu  sync.Mutex
	ev  []string
	job map[string]core.Job
	st  []Stats
}

func newRec() *rec { return &rec{job: map[string]core.Job{}} }
func (r *rec) add(kind string, j core.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, kind)
	r.job[kind] = j
}
func (r *rec) Assigned(_ core.ScaleSetKey, j core.Job)   { r.add("assigned", j) }
func (r *rec) Reassigned(_ core.ScaleSetKey, j core.Job) { r.add("reassigned", j) }
func (r *rec) Started(_ core.ScaleSetKey, j core.Job)    { r.add("started", j) }
func (r *rec) Completed(_ core.ScaleSetKey, j core.Job)  { r.add("completed", j) }
func (r *rec) Stats(_ core.ScaleSetKey, s Stats) {
	r.mu.Lock()
	r.st = append(r.st, s)
	r.mu.Unlock()
}
func (r *rec) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}

func base(name string) scaleset.JobMessageBase {
	return scaleset.JobMessageBase{
		JobID: "guid", WorkflowRunID: 77, OwnerName: "acme", RepositoryName: "app",
		JobDisplayName: name, EventName: "push", RequestLabels: []string{"x-linux"},
		JobWorkflowRef: "acme/app/.github/workflows/ci.yml@refs/heads/main",
	}
}

func TestDispatchOrderAndConversion(t *testing.T) {
	r := newRec()
	l := newListener(orgKey, 1, r, nil, 4, "h", nil)
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:  3,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2, TotalRunningJobs: 1, TotalRegisteredRunners: 3, TotalBusyRunners: 1, TotalIdleRunners: 2},
		JobStartedMessages: []*scaleset.JobStarted{
			{RunnerID: 5, RunnerName: "run-1", JobMessageBase: base("a")},
		},
		JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base("b")}},
		JobCompletedMessages: []*scaleset.JobCompleted{
			{Result: "canceled", JobMessageBase: base("c")},
			{Result: "succeeded", RunnerName: "run-2", RunnerID: 6, JobMessageBase: base("d")},
			{Result: "canceled", RunnerName: "run-3", JobMessageBase: base("e")},
		},
	}
	if err := l.handle(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	want := []string{"reassigned", "assigned", "started", "completed", "completed"}
	got := r.kinds()
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v want %v", got, want)
		}
	}
	a := r.job["assigned"]
	if a.RunID != 77 || a.Owner != "acme" || a.Repo != "app" || a.Name != "b" || a.Event != "push" || a.ScaleSet != orgKey || a.JobID != "guid" {
		t.Fatalf("job %+v", a)
	}
	if s := r.job["started"]; s.RunnerName != "run-1" || s.RunnerID != 5 {
		t.Fatalf("started %+v", s)
	}
	if len(r.st) != 1 || r.st[0] != (Stats{Assigned: 2, Running: 1, Registered: 3, Busy: 1, Idle: 2}) {
		t.Fatalf("stats %+v", r.st)
	}
	// Duplicate delivery of the same message in a session is skipped.
	if err := l.handle(context.Background(), msg); err != nil || len(r.kinds()) != len(want) {
		t.Fatal("duplicate not skipped")
	}
	// Initial synthetic message is never treated as duplicate.
	init := &scaleset.RunnerScaleSetMessage{MessageID: -1, Statistics: &scaleset.RunnerScaleSetStatistic{}}
	_ = l.handle(context.Background(), init)
	_ = l.handle(context.Background(), init)
	if len(r.st) != 3 {
		t.Fatalf("initial stats not delivered, got %d", len(r.st))
	}
	if err := l.handle(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

type fakeSession struct {
	msgs     chan *scaleset.RunnerScaleSetMessage
	capSeen  chan int
	closed   atomicBool
	getError error
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set() { a.mu.Lock(); a.v = true; a.mu.Unlock() }

func (f *fakeSession) GetMessage(ctx context.Context, _ int, maxCap int) (*scaleset.RunnerScaleSetMessage, error) {
	select {
	case f.capSeen <- maxCap:
	default:
	}
	if f.getError != nil {
		return nil, f.getError
	}
	select {
	case m := <-f.msgs:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeSession) DeleteMessage(context.Context, int) error { return nil }
func (f *fakeSession) AcquireJobs(context.Context, []int64) ([]int64, error) {
	return nil, nil
}
func (f *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 4}}
}
func (f *fakeSession) Close(context.Context) error { f.closed.set(); return nil }

func TestRunReconnectsAndDeliversInitialStats(t *testing.T) {
	r := newRec()
	var mu sync.Mutex
	opens := 0
	sessions := []*fakeSession{}
	open := func(context.Context, string) (Session, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		switch opens {
		case 1:
			return nil, scaleset.ConflictError // another session active
		case 2:
			s := &fakeSession{msgs: make(chan *scaleset.RunnerScaleSetMessage, 1), capSeen: make(chan int, 8), getError: errors.New("poll failed")}
			sessions = append(sessions, s)
			return s, nil
		default:
			s := &fakeSession{msgs: make(chan *scaleset.RunnerScaleSetMessage, 1), capSeen: make(chan int, 8)}
			s.msgs <- &scaleset.RunnerScaleSetMessage{MessageID: 0, JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base("z")}}}
			sessions = append(sessions, s)
			return s, nil
		}
	}
	l := newListener(orgKey, 1, r, nil, 4, "h", open)
	l.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if k := r.kinds(); len(k) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no assigned event after reconnects")
		case <-time.After(5 * time.Millisecond):
		}
	}
	l.SetCapacity(9)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if opens < 3 {
		t.Fatalf("opens=%d", opens)
	}
	if len(r.st) < 2 {
		t.Fatalf("initial stats per session expected, got %d", len(r.st))
	}
	if r.st[0].Assigned != 4 {
		t.Fatalf("stats %+v", r.st[0])
	}
	if !sessions[len(sessions)-1].closed.v {
		t.Fatal("session not closed")
	}
}

func TestSetCapacityClampsAndSticks(t *testing.T) {
	l := newListener(orgKey, 1, newRec(), nil, 2, "h", nil)
	l.SetCapacity(-5)
	if l.capacity.Load() != 0 {
		t.Fatal("negative capacity not clamped")
	}
	l.SetCapacity(8)
	if l.capacity.Load() != 8 {
		t.Fatal("capacity not stored")
	}
}
