package clock_test

import (
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func fired(ch <-chan time.Time) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestFakeAfterFiresWhenTimeIsAdvancedPastIt(t *testing.T) {
	c := clock.NewFake(t0)
	ch := c.After(time.Minute)
	if c.Waiters() != 1 {
		t.Fatalf("Waiters = %d, want 1", c.Waiters())
	}
	c.Advance(59 * time.Second)
	if fired(ch) {
		t.Fatal("fired before its time")
	}
	c.Advance(time.Second)
	if !fired(ch) {
		t.Fatal("did not fire at its time")
	}
	if got := c.Now(); !got.Equal(t0.Add(time.Minute)) {
		t.Errorf("Now = %v", got)
	}
	if c.Waiters() != 0 {
		t.Errorf("Waiters = %d after firing, want 0", c.Waiters())
	}
}

func TestFakeAfterNonPositiveFiresAtOnce(t *testing.T) {
	c := clock.NewFake(t0)
	if !fired(c.After(0)) {
		t.Fatal("After(0) did not fire at once")
	}
}

func TestFakeSet(t *testing.T) {
	c := clock.NewFake(t0)
	ch := c.After(time.Hour)
	c.Set(t0.Add(2 * time.Hour))
	if !fired(ch) {
		t.Fatal("Set past the deadline did not fire")
	}
}

func TestRealNow(t *testing.T) {
	if d := time.Since(clock.Real().Now()); d < 0 || d > time.Second {
		t.Errorf("Real().Now() off by %v", d)
	}
}
