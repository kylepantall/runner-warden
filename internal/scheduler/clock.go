package scheduler

import "time"

// Clock abstracts time so tests are deterministic.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker is the subset of time.Ticker the scheduler needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// RealClock uses the wall clock.
type RealClock struct{}

// Now returns the wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// NewTicker returns a ticker on the real clock.
func (RealClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTicker struct{ *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }
