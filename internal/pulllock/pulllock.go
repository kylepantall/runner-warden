// Package pulllock is the cross-process gate between image pulls (held shared
// by every runner's Docker proxy, which lives in its own process) and image GC
// (held exclusively around a prune pass).
//
// Two flock(2) files implement a writer-preferring lock so a steady stream of
// overlapping pulls cannot starve GC: <path>.gate is held shared, briefly, by a
// pull while it takes <path>; GC holds <path>.gate exclusively for the whole
// pass, which keeps new pulls out, and then takes <path> exclusively, which
// waits for the pulls already running.
package pulllock

import (
	"context"
	"os"
	"time"
)

// pollEvery is how often a blocked acquire retries (flock is polled with
// LOCK_NB so a wait can honour a context).
const pollEvery = 50 * time.Millisecond

// Lock is the pair of lock files next to path.
type Lock struct{ path string }

// New returns a Lock rooted at path (the file is created on first use).
func New(path string) *Lock { return &Lock{path: path} }

// Path is the main lock file.
func (l *Lock) Path() string { return l.path }

func (l *Lock) gatePath() string { return l.path + ".gate" }

// File is a held lock; Close releases it.
type File struct{ f *os.File }

// Close releases the lock. Safe on nil and repeatedly.
func (f *File) Close() {
	if f == nil || f.f == nil {
		return
	}
	_ = f.f.Close() // closing the descriptor drops the flock
	f.f = nil
}

func open(path string) (*os.File, error) {
	return os.OpenFile(path, //nolint:gosec // fixed lock file under the state dir
		os.O_RDWR|os.O_CREATE, 0o600)
}

// acquire polls for a flock of the given kind until ctx is done.
func acquire(ctx context.Context, path string, exclusive bool) (*File, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		ok, err := tryFlock(f, exclusive)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if ok {
			return &File{f}, nil
		}
		t.Reset(pollEvery)
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// try is a single non-blocking attempt; (nil, nil) means it is held elsewhere.
func try(path string, exclusive bool) (*File, error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	ok, err := tryFlock(f, exclusive)
	if err != nil || !ok {
		_ = f.Close()
		return nil, err
	}
	return &File{f}, nil
}

// RLock takes the shared side, held for the duration of a pull. It waits while
// GC holds or is waiting for the exclusive side.
func (l *Lock) RLock(ctx context.Context) (*File, error) {
	gate, err := acquire(ctx, l.gatePath(), false)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	return acquire(ctx, l.path, false)
}

// Lock takes the exclusive side: it blocks new pulls, then waits for running
// ones, up to ctx. Close the result to let pulls proceed.
func (l *Lock) Lock(ctx context.Context) (*Exclusive, error) {
	gate, err := acquire(ctx, l.gatePath(), true)
	if err != nil {
		return nil, err
	}
	main, err := acquire(ctx, l.path, true)
	if err != nil {
		gate.Close()
		return nil, err
	}
	return &Exclusive{gate: gate, main: main}, nil
}

// Exclusive is the held exclusive side.
type Exclusive struct{ gate, main *File }

// Close releases both files.
func (e *Exclusive) Close() {
	if e == nil {
		return
	}
	e.main.Close()
	e.gate.Close()
}
