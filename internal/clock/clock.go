// Package clock provides a time abstraction so business logic can be tested
// deterministically. Production code uses [Real]; tests use [Fake].
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time. All decay-and-reinforce logic accepts a
// Clock so tests can fast-forward without sleeping.
type Clock interface {
	Now() time.Time
}

// Real returns the wall-clock time.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

// Fake is a controllable Clock for tests.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake creates a Fake initialized to the given time.
func NewFake(t time.Time) *Fake { return &Fake{t: t} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Set replaces the fake's current time.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t
}

// Advance moves the fake's clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
