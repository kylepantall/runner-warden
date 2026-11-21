package pulllock

import (
	"context"
	"sync"
	"time"
)

// Gate adapts a Lock to the Pause/Resume/InFlight shape of imagegc.PullGate.
// Pause keeps new pulls out; InFlight reports 1 until the pulls already
// running have finished, then 0 (and stays 0 while paused, since nothing can
// start). Resume lets pulls go again. Calls nest.
type Gate struct {
	l *Lock

	mu     sync.Mutex
	pauses int
	gate   *File // exclusive on <path>.gate while paused
	main   *File // exclusive on <path> once drained
}

// NewGate returns a Gate over l.
func NewGate(l *Lock) *Gate { return &Gate{l: l} }

// gateWait bounds how long Pause waits for pulls that are mid-acquire; those
// hold the gate file for an instant only.
const gateWait = 5 * time.Second

// Pause stops new pulls.
func (g *Gate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pauses++
	if g.pauses == 1 {
		ctx, cancel := context.WithTimeout(context.Background(), gateWait)
		defer cancel()
		// On failure InFlight keeps retrying and reports busy meanwhile.
		g.gate, _ = acquire(ctx, g.l.gatePath(), true)
	}
}

// Resume undoes one Pause.
func (g *Gate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pauses == 0 {
		return
	}
	g.pauses--
	if g.pauses == 0 {
		g.main.Close()
		g.gate.Close()
		g.main, g.gate = nil, nil
	}
}

// InFlight is 0 once paused and no pull holds the shared lock, else 1. An
// unusable lock file also reads as busy, so GC skips rather than racing pulls.
func (g *Gate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pauses == 0 {
		// Not paused: report a held shared lock without taking anything.
		f, err := try(g.l.path, true)
		if err != nil || f == nil {
			return 1
		}
		f.Close()
		return 0
	}
	if g.gate == nil {
		f, err := try(g.l.gatePath(), true)
		if err != nil || f == nil {
			return 1
		}
		g.gate = f
	}
	if g.main == nil {
		f, err := try(g.l.path, true)
		if err != nil || f == nil {
			return 1
		}
		g.main = f
	}
	return 0
}
