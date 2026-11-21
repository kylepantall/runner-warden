package pulllock

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newLock(t *testing.T) *Lock { return New(filepath.Join(t.TempDir(), ".pull.lock")) }

func short(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestSharedPullsDoNotBlockEachOther(t *testing.T) {
	l := newLock(t)
	a, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatalf("second shared lock must not wait: %v", err)
	}
	b.Close()
}

func TestExclusiveWaitsForPull(t *testing.T) {
	l := newLock(t)
	pull, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Lock(short(t, 200*time.Millisecond)); err == nil {
		t.Fatal("exclusive must wait while a pull holds the shared lock")
	}
	pull.Close()
	ex, err := l.Lock(short(t, time.Second))
	if err != nil {
		t.Fatalf("exclusive after pull finished: %v", err)
	}
	ex.Close()
}

func TestPullWaitsForExclusive(t *testing.T) {
	l := newLock(t)
	ex, err := l.Lock(short(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.RLock(short(t, 200*time.Millisecond)); err == nil {
		t.Fatal("pull must wait while GC holds the lock")
	}
	ex.Close()
	p, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
}

// A waiting exclusive keeps new pulls out, so overlapping pulls cannot starve it.
func TestPendingExclusiveBlocksNewPulls(t *testing.T) {
	l := newLock(t)
	pull, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer pull.Close()
	got := make(chan error, 1)
	go func() {
		ex, err := l.Lock(short(t, 2*time.Second))
		if err == nil {
			ex.Close()
		}
		got <- err
	}()
	time.Sleep(150 * time.Millisecond) // let it take the gate
	if _, err := l.RLock(short(t, 200*time.Millisecond)); err == nil {
		t.Fatal("new pull must queue behind a pending exclusive")
	}
	pull.Close()
	if err := <-got; err != nil {
		t.Fatalf("exclusive should win once the pull ends: %v", err)
	}
}

func TestGateDrainsAndResumes(t *testing.T) {
	l := newLock(t)
	g := NewGate(l)
	pull, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	g.Pause()
	if g.InFlight() == 0 {
		t.Fatal("a running pull must read as in flight")
	}
	pull.Close()
	if g.InFlight() != 0 {
		t.Fatal("drained gate must read 0")
	}
	if _, err := l.RLock(short(t, 200*time.Millisecond)); err == nil {
		t.Fatal("paused gate must hold new pulls")
	}
	g.Resume()
	p, err := l.RLock(short(t, time.Second))
	if err != nil {
		t.Fatalf("pulls proceed after resume: %v", err)
	}
	p.Close()
}

func TestGateNests(t *testing.T) {
	g := NewGate(newLock(t))
	g.Pause()
	g.Pause()
	g.Resume()
	if g.InFlight() != 0 {
		t.Fatal("still paused after one Resume")
	}
	if _, err := g.l.RLock(short(t, 150*time.Millisecond)); err == nil {
		t.Fatal("nested pause must still hold pulls")
	}
	g.Resume()
	g.Resume() // extra is harmless
}
