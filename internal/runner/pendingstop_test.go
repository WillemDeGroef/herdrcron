package runner_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

const (
	reasonTabMayBeOpen = "stopped by user; target unreachable, tab may still be open"
	reasonTabClosed    = "stopped by user; tab closed after the target came back"
)

// stopUnreachable starts a run, makes the target unreachable and stops the
// run: the stop returns without waiting, with the run ended.
func (e *env) stopUnreachable() store.Run {
	e.t.Helper()
	e.herdr.output = "last words\n"
	e.herdr.script(working)
	run := e.start(deps())
	e.poll(run.ID)
	e.setUnreachable(true)
	got, err := e.runner.Stop(e.ctx, run.ID)
	if err != nil {
		e.t.Fatalf("Stop: %v", err)
	}
	return got
}

// waitReason waits until the run's row has the exit reason.
func (e *env) waitReason(id, reason string) store.Run {
	e.t.Helper()
	var r store.Run
	e.waitFor("run "+id+" to have exit reason "+reason, func() bool {
		r = e.get(id)
		return r.ExitReason == reason
	})
	return r
}

// Issue #35: a stop that cannot reach the target ends the run and says the
// tab may still be open; when the target answers again the stop sequence
// runs, the tab is closed and the row says so.
func TestStopOnUnreachableTargetClosesTheTabWhenItReturns(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()

	if run.Status != store.StatusStopped || run.ExitReason != reasonTabMayBeOpen || run.EndedAt.IsZero() {
		t.Fatalf("Stop returned %s %q ended %v, want stopped %q", run.Status, run.ExitReason, run.EndedAt, reasonTabMayBeOpen)
	}
	if got := e.get(run.ID); got.Status != store.StatusStopped || got.ExitReason != reasonTabMayBeOpen || got.EndedAt.IsZero() {
		t.Fatalf("stored run = %s %q ended %v, want stopped %q", got.Status, got.ExitReason, got.EndedAt, reasonTabMayBeOpen)
	}
	if e.runner.Active(run.ID) {
		t.Errorf("stopped run is still tracked")
	}
	if !e.herdr.tabOpen(run.HerdrTab) {
		t.Fatalf("tab closed on an unreachable target")
	}
	if st := e.targetState(); st.Reachability != runner.ReachUnreachable {
		t.Errorf("target = %+v, want unreachable after the failed stop", st)
	}
	// The stop gives up at the first call that does not get through.
	if got, want := e.stopSteps(), []string{"keys esc"}; !slices.Equal(got, want) {
		t.Errorf("stop calls on the unreachable target = %q, want %q", got, want)
	}

	// Still unreachable: nothing more than the probe is tried.
	e.clock.Advance(30 * time.Second)
	e.runner.PollTarget(e.ctx, "local")
	if got := e.stopSteps(); len(got) != 1 {
		t.Errorf("stop calls while still unreachable = %q, want none added", got)
	}

	e.setUnreachable(false)
	e.clock.Advance(time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	got := e.waitReason(run.ID, reasonTabClosed)
	if got.Status != store.StatusStopped || !got.EndedAt.Equal(run.EndedAt) || got.OutputTail != "last words\n" {
		t.Errorf("run after the target came back = %+v, want stopped, the same ended_at, the output tail", got)
	}
	want := []string{"keys esc", "keys esc", "keys ctrl+c ctrl+c", "keys ctrl+c ctrl+c", "pane read", "tab close"}
	if steps := e.stopSteps(); !slices.Equal(steps, want) {
		t.Errorf("stop calls = %q, want %q", steps, want)
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open after the target came back")
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target")
	}

	// Done once: later polls do not stop it again.
	e.runner.PollTarget(e.ctx, "local")
	e.runner.PollTarget(e.ctx, "local")
	if steps := e.stopSteps(); !slices.Equal(steps, want) {
		t.Errorf("stop calls after later polls = %q, want %q", steps, want)
	}
}

// A daemon restart does not forget the stop: the new runner finishes it.
func TestPendingStopIsFinishedAfterARestart(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	e.restart(deps())
	if got := e.get(run.ID); got.ExitReason != reasonTabMayBeOpen {
		t.Fatalf("exit reason while still unreachable = %q, want %q", got.ExitReason, reasonTabMayBeOpen)
	}

	e.setUnreachable(false)
	e.restart(deps())
	e.driveStop()
	e.waitReason(run.ID, reasonTabClosed)
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open after the restart")
	}
	if e.runner.Active(run.ID) {
		t.Errorf("ended run is tracked again after the restart")
	}

	// Nothing is left to do for the next daemon.
	e.restart(deps())
	e.runner.PollTarget(e.ctx, "local")
	if got := e.stopSteps(); len(got) != 6 {
		t.Errorf("stop calls after the stop was finished = %q, want none added", got)
	}
}

// The target drops again while the stop is being finished: it stays
// pending and is tried again from the start.
func TestPendingStopSurvivesTheTargetDroppingAgain(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.waitClock()
	e.setUnreachable(true)
	e.clock.Advance(2 * time.Second)
	e.waitFor("the target to be unreachable again", func() bool {
		return e.targetState().Reachability == runner.ReachUnreachable
	})
	if got := e.get(run.ID); got.ExitReason != reasonTabMayBeOpen {
		t.Fatalf("exit reason = %q, want %q", got.ExitReason, reasonTabMayBeOpen)
	}

	e.setUnreachable(false)
	e.waitFor("the stop to be finished", func() bool {
		if e.get(run.ID).ExitReason == reasonTabClosed {
			return true
		}
		e.runner.PollTarget(e.ctx, "local")
		e.clock.Advance(10 * time.Second)
		return false
	})
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
}

// A timeout on an unreachable target ends the run the same way, and the
// finished stop is an amendment, not a second end: one timeout
// notification.
func TestTimeoutOnUnreachableTargetClosesTheTabWhenItReturns(t *testing.T) {
	e := newEnv(t)
	var amended []store.Run
	e.onAmend = func(r store.Run) {
		e.mu.Lock()
		defer e.mu.Unlock()
		amended = append(amended, r)
	}
	e.runner = e.newRunner()
	e.herdr.script(working)
	task := deps()
	task.Timeout.Duration = 90 * time.Minute
	run := e.start(task)
	e.poll(run.ID)
	e.setUnreachable(true)
	e.clock.Advance(91 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	got := e.waitEnded(run.ID)
	const open = "timeout after 1h30m; target unreachable, tab may still be open"
	if got.Status != store.StatusTimeout || got.ExitReason != open {
		t.Fatalf("run = %s %q, want timeout %q", got.Status, got.ExitReason, open)
	}

	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	const closed = "timeout after 1h30m; tab closed after the target came back"
	got = e.waitReason(run.ID, closed)
	if got.Status != store.StatusTimeout || e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("run = %s, tab open %v; want timeout with the tab closed", got.Status, e.herdr.tabOpen(run.HerdrTab))
	}
	if n := len(e.notifications()); n != 1 {
		t.Errorf("%d notifications %v, want 1 for the timeout", n, e.notifications())
	}
	if n := len(e.endedRuns()); n != 1 {
		t.Errorf("OnEnd called %d times, want 1", n)
	}
	e.waitFor("OnAmend", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(amended) == 1 && amended[0].ID == run.ID && amended[0].ExitReason == closed
	})
}

// The tab is already gone when the target comes back (the host rebooted,
// say): the row says so.
func TestPendingStopOfATabThatIsGone(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	e.herdr.mu.Lock()
	delete(e.herdr.tabs, run.HerdrTab)
	e.herdr.mu.Unlock()
	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	e.waitReason(run.ID, "stopped by user; tab was gone when the target came back")
}

// A daemon that died between recording the cleanup and ending the run left
// a pending cleanup for an open run: the run is tracked again, and its tab
// is not closed behind its back.
func TestRecoverDropsThePendingCleanupOfAnOpenRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, working)
	run := e.start(deps())
	e.poll(run.ID)
	if err := e.store.SetPendingCleanup(e.ctx, run.ID, true); err != nil {
		t.Fatal(err)
	}
	e.restart(deps())
	e.runner.PollTarget(e.ctx, "local")
	if !e.runner.Active(run.ID) {
		t.Errorf("open run is not tracked after the restart")
	}
	if pending, err := e.store.ListPendingCleanups(e.ctx); err != nil || len(pending) != 0 {
		t.Errorf("pending cleanups = %v, %v; want none", pending, err)
	}
	if got := e.stopSteps(); len(got) != 0 {
		t.Errorf("stop calls for a run nobody stopped: %q", got)
	}
}

// The poll worker itself retries: with no active run left on the target,
// its probe after the backoff finds the target and the stop is finished.
func TestPollWorkerFinishesAPendingStop(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); e.runner.Run(ctx) }()
	defer func() { cancel(); <-done }()

	e.setUnreachable(false)
	e.waitFor("the worker to finish the stop", func() bool {
		if e.get(run.ID).ExitReason == reasonTabClosed {
			return true
		}
		e.waitClock()
		e.clock.Advance(2 * time.Second)
		return false
	})
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
}

// The target drops between the output read and the tab close of a stop:
// the tail that was read is kept, and the tab is closed later.
func TestStopKeepsTheTailWhenTheTargetDropsBeforeTheTabClose(t *testing.T) {
	e := newEnv(t)
	e.herdr.output = "last words\n"
	e.herdr.script(working)
	run := e.start(deps())
	e.poll(run.ID)
	e.herdr.mu.Lock()
	e.herdr.unreachableFor = "tab close"
	e.herdr.mu.Unlock()
	res := e.stopAsync(run.ID)
	e.driveStop()
	r := <-res
	if r.err != nil || r.run.ExitReason != reasonTabMayBeOpen || r.run.OutputTail != "last words\n" {
		t.Fatalf("Stop = %q tail %q, %v; want %q with the output tail", r.run.ExitReason, r.run.OutputTail, r.err, reasonTabMayBeOpen)
	}
	e.herdr.mu.Lock()
	e.herdr.unreachableFor = ""
	e.herdr.mu.Unlock()
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	e.waitReason(run.ID, reasonTabClosed)
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
}
