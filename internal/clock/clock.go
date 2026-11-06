// Package clock abstracts the wall clock so the scheduler can be tested
// with a fake one.
package clock

import (
	"sync"
	"time"
)

// Clock tells the time and waits.
type Clock interface {
	Now() time.Time
	// After sends the time on the returned channel once d has passed.
	After(d time.Duration) <-chan time.Time
}

type system struct{}

func (system) Now() time.Time                         { return time.Now() }
func (system) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Real is the system clock.
func Real() Clock { return system{} }

// Fake is a Clock that only moves when told to. It is safe for concurrent
// use.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewFake returns a fake clock set to now.
func NewFake(now time.Time) *Fake { return &Fake{now: now} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that receives the fake time once the clock has
// been moved d past the current time. A d <= 0 fires at once.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, waiter{at: f.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock forward by d and fires the waiters that are due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(f.now.Add(d))
}

// Set moves the clock to t and fires the waiters that are due.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(t)
}

func (f *Fake) set(t time.Time) {
	f.now = t
	kept := f.waiters[:0]
	for _, w := range f.waiters {
		if w.at.After(t) {
			kept = append(kept, w)
			continue
		}
		w.ch <- t
	}
	f.waiters = kept
}

// Waiters returns the number of pending After calls, including ones whose
// caller stopped listening. Tests use it to wait until the code under test
// is blocked on the clock.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}
