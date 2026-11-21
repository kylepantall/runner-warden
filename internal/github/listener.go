package gh

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/kylepantall/runner-warden/internal/core"
)

// Stats is the scale-set service's view of a scale set.
type Stats struct {
	Assigned   int // jobs assigned (running + assigned-unstarted)
	Running    int
	Registered int // registered runners
	Busy       int
	Idle       int
}

// Events receives scale-set events. Calls come from a single goroutine, in the
// order documented on Listener. Redelivery after errors or reconnects means
// every method must be idempotent.
type Events interface {
	Assigned(key core.ScaleSetKey, j core.Job)
	// Reassigned: GitHub cancelled an assigned job that had no runner
	// (result canceled, empty runner name). The job is assigned again every
	// ~5 minutes; it is not a completion.
	Reassigned(key core.ScaleSetKey, j core.Job)
	Started(key core.ScaleSetKey, j core.Job)
	Completed(key core.ScaleSetKey, j core.Job)
	Stats(key core.ScaleSetKey, s Stats)
}

// ResultCanceled is the result GitHub reports for a reassigned job.
const ResultCanceled = "canceled"

// sessionOpener opens a message session.
type sessionOpener func(ctx context.Context, owner string) (Session, error)

// Listener runs the scale-set long poll for one scale set.
//
// Order of event delivery within one message (deterministic):
//  1. Reassigned (canceled with no runner), in message order
//  2. Assigned
//  3. Started
//  4. Completed (after Started: a short job's Started and Completed can share a message)
//  5. Stats (initial session statistics arrive as a synthetic first message)
//
// JobAvailable messages are ignored (the service never sends them for scale
// sets; jobs arrive as JobAssigned).
type Listener struct {
	key    core.ScaleSetKey
	events Events
	log    *slog.Logger
	open   sessionOpener
	owner  string
	ssID   int

	// Backoff bounds; zero values use defaults.
	MinBackoff, MaxBackoff time.Duration
	// ConflictBackoff is the minimum wait after a session conflict.
	ConflictBackoff time.Duration
	// sleep is replaced in tests.
	sleep func(ctx context.Context, d time.Duration) error

	capacity atomic.Int64
	mu       sync.Mutex
	cur      *listener.Listener
	lastID   int // last message id handled in this session
}

// NewListener builds a Listener for key. Ensure must have run for key.
func (s *ScaleSets) NewListener(key core.ScaleSetKey, events Events, log *slog.Logger, capacity int) (*Listener, error) {
	id, err := s.mustID(key)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	l := newListener(key, id, events, log, capacity, host, func(ctx context.Context, owner string) (Session, error) {
		return s.OpenSession(ctx, key, owner)
	})
	return l, nil
}

func newListener(key core.ScaleSetKey, ssID int, events Events, log *slog.Logger, capacity int, owner string, open sessionOpener) *Listener {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	l := &Listener{key: key, ssID: ssID, events: events, log: log, open: open, owner: owner, lastID: listener.InitialMessageID}
	l.capacity.Store(int64(capacity))
	return l
}

// SetCapacity sets maxCapacity sent to the service on each poll (effective on
// the next poll, up to ~50s later). Safe for concurrent use.
func (l *Listener) SetCapacity(n int) {
	if n < 0 {
		n = 0
	}
	l.capacity.Store(int64(n))
	l.mu.Lock()
	if l.cur != nil {
		l.cur.SetMaxRunners(n)
	}
	l.mu.Unlock()
}

func (l *Listener) backoffBounds() (lo, hi time.Duration) {
	lo, hi = l.MinBackoff, l.MaxBackoff
	if lo <= 0 {
		lo = time.Second
	}
	if hi <= 0 {
		hi = time.Minute
	}
	return lo, hi
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run polls until ctx is done, reconnecting with jittered exponential backoff
// after any error. A session conflict (another active session for the scale
// set) is logged and retried, never fatal. It returns ctx.Err() on shutdown.
func (l *Listener) Run(ctx context.Context) error {
	lo, hi := l.backoffBounds()
	wait := lo
	sleep := l.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for ctx.Err() == nil {
		started := time.Now()
		err := l.runSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A session that lived a while was healthy: restart the backoff.
		if time.Since(started) > 2*hi {
			wait = lo
		}
		d := wait
		if errors.Is(err, scaleset.ConflictError) {
			l.log.Warn("scale set session conflict, another listener is active", "owner", l.key.Target.String(), "os", l.key.OS, "retryIn", wait.String())
			d = max(d, l.ConflictBackoff)
		} else {
			l.log.Warn("scale set listener error, reconnecting", "owner", l.key.Target.String(), "os", l.key.OS, "error", err.Error(), "retryIn", wait.String())
		}
		if serr := sleep(ctx, jitter(d)); serr != nil {
			return serr
		}
		wait = min(wait*2, hi)
	}
	return ctx.Err()
}

func jitter(d time.Duration) time.Duration {
	return d - time.Duration(rand.Int64N(int64(d)/4+1)) //nolint:gosec // retry jitter, not security-sensitive
}

func (l *Listener) runSession(ctx context.Context) error {
	sess, err := l.open(ctx, l.owner)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close(context.WithoutCancel(ctx)) }()

	inner, err := listener.New(sess, listener.Config{
		ScaleSetID: l.ssID,
		MaxRunners: int(l.capacity.Load()),
		Logger:     l.log,
	})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.cur = inner
	l.lastID = listener.InitialMessageID
	l.mu.Unlock()
	// Pick up a SetCapacity that raced with creation.
	inner.SetMaxRunners(int(l.capacity.Load()))
	defer func() {
		l.mu.Lock()
		l.cur = nil
		l.mu.Unlock()
	}()
	return inner.Run(ctx, scalerFunc(l.handle))
}

type scalerFunc func(ctx context.Context, m *scaleset.RunnerScaleSetMessage) error

func (f scalerFunc) Scale(ctx context.Context, m *scaleset.RunnerScaleSetMessage) error {
	return f(ctx, m)
}

func (l *Listener) handle(_ context.Context, m *scaleset.RunnerScaleSetMessage) error {
	if m == nil {
		return nil
	}
	// The same message can be redelivered when its ack failed; skip it within a
	// session. Across sessions redelivery is possible: Events is idempotent.
	if m.MessageID >= 0 && m.MessageID == l.lastID {
		l.log.Debug("duplicate message skipped", "messageId", m.MessageID)
		return nil
	}
	l.dispatch(m)
	l.lastID = m.MessageID
	return nil
}

// dispatch delivers events in lifecycle order. A short job's Started and
// Completed can share one message, so Started must come before Completed;
// a reassignment (canceled, no runner) precedes the Assigned that follows it.
func (l *Listener) dispatch(m *scaleset.RunnerScaleSetMessage) {
	var completed []core.Job
	for _, j := range m.JobCompletedMessages {
		job := l.job(j.JobMessageBase)
		job.RunnerName, job.RunnerID, job.Result = j.RunnerName, j.RunnerID, j.Result
		if j.Result == ResultCanceled && j.RunnerName == "" {
			l.events.Reassigned(l.key, job)
			continue
		}
		completed = append(completed, job)
	}
	for _, j := range m.JobAssignedMessages {
		l.events.Assigned(l.key, l.job(j.JobMessageBase))
	}
	for _, j := range m.JobStartedMessages {
		job := l.job(j.JobMessageBase)
		job.RunnerName, job.RunnerID = j.RunnerName, j.RunnerID
		l.events.Started(l.key, job)
	}
	for _, job := range completed {
		l.events.Completed(l.key, job)
	}
	if len(m.JobAvailableMessages) > 0 {
		l.log.Debug("job available messages ignored", "count", len(m.JobAvailableMessages))
	}
	if s := m.Statistics; s != nil {
		l.events.Stats(l.key, Stats{
			Assigned:   s.TotalAssignedJobs,
			Running:    s.TotalRunningJobs,
			Registered: s.TotalRegisteredRunners,
			Busy:       s.TotalBusyRunners,
			Idle:       s.TotalIdleRunners,
		})
	}
}

func (l *Listener) job(b scaleset.JobMessageBase) core.Job {
	return core.Job{
		JobID:       b.JobID,
		RunID:       b.WorkflowRunID,
		Owner:       b.OwnerName,
		Repo:        b.RepositoryName,
		WorkflowRef: b.JobWorkflowRef,
		Name:        b.JobDisplayName,
		Event:       b.EventName,
		Labels:      append([]string(nil), b.RequestLabels...),
		QueueTime:   b.QueueTime,
		AssignedAt:  b.ScaleSetAssignTime,
		ScaleSet:    l.key,
	}
}
