package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// targetChanges returns the reachability of every OnTargetChange call.
func (e *env) targetChanges() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, st := range e.targets {
		out = append(out, st.Reachability)
	}
	return out
}

func (e *env) setUnreachable(v bool) {
	e.herdr.mu.Lock()
	e.herdr.unreachable = v
	e.herdr.mu.Unlock()
}

func (e *env) targetState() runner.TargetState {
	e.t.Helper()
	st, ok := e.runner.TargetState("local")
	if !ok {
		e.t.Fatal("no state for target local")
	}
	return st
}

// The state machine of the spec's "target unreachable during a run": the
// target stops answering for three polls and comes back. The poll worker
// backs off 30 s, then 60 s, then 120 s; the run stays running and is
// tracked again when the target answers.
func TestTargetDropsForThreePollsAndReturns(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(deps())
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); e.runner.Run(ctx) }()
	defer func() { cancel(); <-done }()

	lists := func() int { return e.herdr.count("herdr", "pane", "list") }
	// tick advances the clock by d and waits for the poll worker to wait
	// again; it returns how many pane lists that made.
	tick := func(d time.Duration) int {
		t.Helper()
		before := lists()
		e.waitClock()
		e.clock.Advance(d)
		e.waitClock()
		return lists() - before
	}

	if n := tick(15 * time.Second); n != 1 {
		t.Fatalf("first poll made %d pane lists, want 1", n)
	}
	if st := e.targetState(); st.Reachability != runner.ReachReachable {
		t.Fatalf("target = %+v, want reachable", st)
	}

	e.setUnreachable(true)
	dropped := e.clock.Now().Add(15 * time.Second)
	waits := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	for i, backoff := range waits {
		if i == 0 {
			tick(15 * time.Second)
		} else {
			// Nothing is tried before the backoff is over.
			if n := tick(waits[i-1] - time.Second); n != 0 {
				t.Fatalf("poll %d: %d pane lists during the backoff, want 0", i+1, n)
			}
			if n := tick(time.Second); n != 1 {
				t.Fatalf("poll %d: %d pane lists when the backoff ended, want 1", i+1, n)
			}
		}
		st := e.targetState()
		if st.Reachability != runner.ReachUnreachable || !st.UnreachableSince.Equal(dropped) ||
			!st.NextRetryAt.Equal(e.clock.Now().Add(backoff)) || !strings.Contains(st.LastError, "unreachable") {
			t.Fatalf("after failed poll %d: target = %+v, want unreachable since %v, next retry in %v", i+1, st, dropped, backoff)
		}
		if got := e.get(run.ID); got.Status != store.StatusRunning || !got.EndedAt.IsZero() {
			t.Fatalf("after failed poll %d: run = %s ended %v, want still running", i+1, got.Status, got.EndedAt)
		}
	}

	e.setUnreachable(false)
	if n := tick(120 * time.Second); n != 1 {
		t.Fatalf("%d pane lists when the last backoff ended, want 1", n)
	}
	st := e.targetState()
	if st.Reachability != runner.ReachReachable || !st.NextRetryAt.IsZero() || st.LastError != "" || !st.LastSeenAt.Equal(e.clock.Now()) {
		t.Errorf("target after it answers = %+v, want reachable, seen now", st)
	}
	// Back to the poll interval.
	if n := tick(15 * time.Second); n != 1 {
		t.Errorf("%d pane lists one poll interval later, want 1", n)
	}
	if got := e.get(run.ID); got.Status != store.StatusRunning {
		t.Errorf("run after the target answers = %s, want running", got.Status)
	}
	want := []string{runner.ReachReachable, runner.ReachUnreachable, runner.ReachReachable}
	if got := e.targetChanges(); !slices.Equal(got, want) {
		t.Errorf("target changes = %v, want %v", got, want)
	}
}

func TestBackoffDoublesUpToFiveMinutes(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	e.start(deps())
	e.setUnreachable(true)
	var got []time.Duration
	for range 7 {
		e.runner.PollTarget(e.ctx, "local")
		st := e.targetState()
		got = append(got, st.NextRetryAt.Sub(e.clock.Now()))
		e.clock.Set(st.NextRetryAt)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if !slices.Equal(got, want) {
		t.Errorf("backoffs = %v, want %v", got, want)
	}
	if got := e.targetChanges(); !slices.Equal(got, []string{runner.ReachReachable, runner.ReachUnreachable}) {
		t.Errorf("target changes = %v, want one change to unreachable", got)
	}
}

// The spec's "target unreachable at fire time": the run is recorded as
// host_unreachable (not failed), and the target is marked unreachable.
func TestFireAtUnreachableTargetIsHostUnreachable(t *testing.T) {
	e := newEnv(t)
	e.setUnreachable(true)
	run := e.start(deps())
	if run.Status != store.StatusHostUnreachable || run.EndedAt.IsZero() || !strings.Contains(run.ExitReason, "unreachable") {
		t.Fatalf("run = %s ended %v %q, want host_unreachable with the reason", run.Status, run.EndedAt, run.ExitReason)
	}
	if got := e.get(run.ID); got.Status != store.StatusHostUnreachable {
		t.Errorf("stored run = %s, want host_unreachable", got.Status)
	}
	if got := e.targetChanges(); !slices.Equal(got, []string{runner.ReachUnreachable}) {
		t.Errorf("target changes = %v, want [unreachable]", got)
	}
	if ended := e.endedRuns(); len(ended) != 1 || ended[0].ID != run.ID {
		t.Errorf("OnEnd runs = %+v, want the host_unreachable run", ended)
	}
	if len(e.notifications()) != 0 {
		t.Errorf("notifications = %+v, want none for a run (the target notifies)", e.notifications())
	}

	// A second fire inside the backoff does not wait on the target.
	calls := len(e.herdr.Calls())
	e.clock.Advance(10 * time.Second)
	second := e.start(deps())
	if second.Status != store.StatusHostUnreachable {
		t.Errorf("second run = %s, want host_unreachable", second.Status)
	}
	if n := len(e.herdr.Calls()) - calls; n != 0 {
		t.Errorf("second fire made %d target calls, want 0", n)
	}
	// After the backoff the fire tries the target again.
	e.setUnreachable(false)
	e.clock.Advance(30 * time.Second)
	e.herdr.script(working)
	if third := e.start(deps()); third.Status != store.StatusRunning {
		t.Errorf("run after the target answers = %s %q, want running", third.Status, third.ExitReason)
	}
	if got := e.targetChanges(); !slices.Equal(got, []string{runner.ReachUnreachable, runner.ReachReachable}) {
		t.Errorf("target changes = %v, want [unreachable reachable]", got)
	}
}

// A target with no runs is probed so its reachability is known: at once
// while unknown, then every 5 minutes while it answers.
func TestIdleTargetIsProbed(t *testing.T) {
	e := newEnv(t)
	lists := func() int { return e.herdr.count("herdr", "pane", "list") }
	e.runner.PollTarget(e.ctx, "local")
	if lists() != 1 || e.targetState().Reachability != runner.ReachReachable {
		t.Fatalf("first idle poll: %d pane lists, target %+v; want 1, reachable", lists(), e.targetState())
	}
	e.clock.Advance(4 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	if lists() != 1 {
		t.Errorf("idle poll 4 min after a probe made a pane list")
	}
	e.clock.Advance(time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	if lists() != 2 {
		t.Errorf("idle poll 5 min after a probe made no pane list")
	}
	// A server that is not running still means the host answers.
	e.herdr.mu.Lock()
	e.herdr.serverDown = true
	e.herdr.mu.Unlock()
	e.clock.Advance(5 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	if st := e.targetState(); st.Reachability != runner.ReachReachable || !st.LastSeenAt.Equal(e.clock.Now()) {
		t.Errorf("target with herdr server down = %+v, want reachable, seen now", st)
	}
}
