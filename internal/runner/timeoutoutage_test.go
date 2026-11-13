package runner_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

const (
	timeoutTabMayBeOpen      = "timeout after 10m; target unreachable, tab may still be open"
	timeoutTabClosed         = "timeout after 10m; tab closed after the target came back"
	timeoutTabClosedReplaced = "timeout after 10m; pane replaced: herdr server restarted; tab closed after the target came back"
)

// stopSequence is step 10 on the target, as stopSteps names it.
var stopSequence = []string{"keys esc", "keys ctrl+c ctrl+c", "keys ctrl+c ctrl+c", "pane read", "tab close"}

// interrupted reports whether Ctrl-C reached the agent in the first pane.
// The poll worker writes the flag from another goroutine, so the read
// holds the fake's lock; order[0] is the run's pane when the test opened one tab.
func (e *env) interrupted() bool {
	e.herdr.mu.Lock()
	defer e.herdr.mu.Unlock()
	return e.herdr.panes[e.herdr.order[0]].interrupted
}

// timeoutTask is deps with a 10 minute timeout.
func timeoutTask() config.Task {
	task := deps()
	task.Timeout.Duration = 10 * time.Minute
	return task
}

// Issue #36: the timeout of a run passes while its target is in an outage.
// The run ends as timeout at once, with nothing delivered to the pane, and
// the full stop sequence is sent to the pane when the target answers again.
func TestTimeoutDuringAnOutageSendsTheStopSequenceWhenTheTargetReturns(t *testing.T) {
	e := newEnv(t)
	e.herdr.output = "last words\n"
	e.herdr.script(working)
	run := e.start(timeoutTask())
	e.poll(run.ID)

	// The outage starts well before the timeout: the target is known to be
	// unreachable, and backing off, when the timeout passes.
	e.setUnreachable(true)
	if got := e.poll(run.ID); got != store.StatusRunning {
		t.Fatalf("status at the start of the outage = %s, want running", got)
	}
	if st := e.targetState(); st.Reachability != runner.ReachUnreachable {
		t.Fatalf("target = %+v, want unreachable", st)
	}
	e.clock.Advance(10 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusTimeout || got.ExitReason != timeoutTabMayBeOpen {
		t.Fatalf("run = %s %q, want timeout %q", got.Status, got.ExitReason, timeoutTabMayBeOpen)
	}
	if e.runner.Active(run.ID) {
		t.Errorf("timed-out run is still tracked")
	}
	if e.interrupted() || !e.herdr.tabOpen(run.HerdrTab) {
		t.Fatalf("interrupted %v, tab open %v during the outage; want the agent untouched", e.interrupted(), e.herdr.tabOpen(run.HerdrTab))
	}
	// The backoff has expired, so the stop tries esc once. The target is
	// still down, so that call does not get through, and nothing else is sent.
	tried := e.stopSteps()
	if len(tried) > 1 || (len(tried) == 1 && tried[0] != "keys esc") {
		t.Fatalf("stop calls while the target was down = %q, want at most the esc that discovered the outage", tried)
	}

	// Still down at the next retries: the stop stays pending.
	for range 3 {
		e.clock.Advance(5 * time.Minute)
		e.runner.PollTarget(e.ctx, "local")
	}
	if steps := e.stopSteps(); !slices.Equal(steps, tried) {
		t.Fatalf("stop calls while still unreachable = %q, want %q", steps, tried)
	}

	e.setUnreachable(false)
	e.clock.Advance(5 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	after := e.waitReason(run.ID, timeoutTabClosed)
	if steps := e.stopSteps()[len(tried):]; !slices.Equal(steps, stopSequence) {
		t.Errorf("stop calls once the target answers = %q, want %q", steps, stopSequence)
	}
	if !e.interrupted() {
		t.Errorf("Ctrl-C did not reach the agent")
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open after the target came back")
	}
	if after.Status != store.StatusTimeout || !after.EndedAt.Equal(got.EndedAt) || after.OutputTail != "last words\n" {
		t.Errorf("run after the target came back = %+v, want timeout, the same ended_at, the output tail", after)
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target")
	}
	if n := len(e.notifications()); n != 1 {
		t.Errorf("%d notifications %v, want 1 for the timeout", n, e.notifications())
	}

	// Done once: a later poll does not stop it again.
	sent := e.stopSteps()
	e.runner.PollTarget(e.ctx, "local")
	if steps := e.stopSteps(); !slices.Equal(steps, sent) {
		t.Errorf("stop calls after a later poll = %q, want %q", steps, sent)
	}
}

// The timeout passes while the daemon is down and the target unreachable:
// the new daemon ends the run as timeout, and a later one, started when the
// target answers again, sends the stop sequence.
func TestTimeoutDuringAnOutageAcrossRestarts(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	task := timeoutTask()
	run := e.start(task)
	e.poll(run.ID)
	e.setUnreachable(true)
	e.clock.Advance(11 * time.Minute)

	e.restart(task)
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusTimeout || got.ExitReason != timeoutTabMayBeOpen {
		t.Fatalf("run = %s %q, want timeout %q", got.Status, got.ExitReason, timeoutTabMayBeOpen)
	}
	if e.interrupted() || !e.herdr.tabOpen(run.HerdrTab) {
		t.Fatalf("interrupted %v, tab open %v during the outage; want the agent untouched", e.interrupted(), e.herdr.tabOpen(run.HerdrTab))
	}
	// The new daemon does not yet know the target is down, so the stop
	// tries esc once. That call does not get through, and nothing else is sent.
	tried := e.stopSteps()
	if len(tried) > 1 || (len(tried) == 1 && tried[0] != "keys esc") {
		t.Fatalf("stop calls while the target was down = %q, want at most the esc that discovered the outage", tried)
	}

	e.setUnreachable(false)
	e.restart(task)
	e.driveStop()
	e.waitReason(run.ID, timeoutTabClosed)
	if steps := e.stopSteps()[len(tried):]; !slices.Equal(steps, stopSequence) {
		t.Errorf("stop calls after the restart = %q, want %q", steps, stopSequence)
	}
	if !e.interrupted() || e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("interrupted %v, tab open %v; want the agent stopped and the tab closed", e.interrupted(), e.herdr.tabOpen(run.HerdrTab))
	}
	if e.runner.Active(run.ID) {
		t.Errorf("ended run is tracked again after the restart")
	}
	if n := len(e.notifications()); n != 1 {
		t.Errorf("%d notifications %v, want 1 for the timeout", n, e.notifications())
	}
	if n := len(e.endedRuns()); n != 1 {
		t.Errorf("OnEnd called %d times, want 1", n)
	}
}

// The poll worker does it all without a caller: it ends the run when the
// timeout passes during the outage, keeps retrying the target, and sends the
// stop sequence when it answers.
func TestPollWorkerStopsATimedOutRunWhenTheTargetReturns(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(timeoutTask())
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); e.runner.Run(ctx) }()
	defer func() { cancel(); <-done }()

	e.setUnreachable(true)
	e.waitFor("the run to time out", func() bool {
		if !e.get(run.ID).EndedAt.IsZero() {
			return true
		}
		e.waitClock()
		e.clock.Advance(15 * time.Second)
		return false
	})
	if got := e.get(run.ID); got.Status != store.StatusTimeout || got.ExitReason != timeoutTabMayBeOpen {
		t.Fatalf("run = %s %q, want timeout %q", got.Status, got.ExitReason, timeoutTabMayBeOpen)
	}
	if e.interrupted() {
		t.Fatalf("Ctrl-C reached the agent on an unreachable target")
	}

	e.setUnreachable(false)
	e.waitFor("the worker to finish the stop", func() bool {
		if e.get(run.ID).ExitReason == timeoutTabClosed {
			return true
		}
		e.waitClock()
		e.clock.Advance(2 * time.Second)
		return false
	})
	if !e.interrupted() || e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("interrupted %v, tab open %v; want the agent stopped and the tab closed", e.interrupted(), e.herdr.tabOpen(run.HerdrTab))
	}
	if steps := e.stopSteps(); !slices.Equal(steps, stopSequence) {
		t.Errorf("stop calls = %q, want %q", steps, stopSequence)
	}
	if n := len(e.notifications()); n != 1 {
		t.Errorf("%d notifications %v, want 1 for the timeout", n, e.notifications())
	}
}

// A herdr server that restarted during the outage restored the pane as a
// new shell. cleanup finishes a timeout and a user stop the same way: it
// must not send keys to that shell. It still closes the tab and records
// the replacement. The terminal id is the one stored on the run, including
// after a daemon restart.
func TestTimeoutDuringAnOutageClosesARestoredPaneWithoutSendingKeys(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(timeoutTask())
	e.poll(run.ID)
	terminal := e.get(run.ID).HerdrTerminal
	if terminal == "" {
		t.Fatal("run stored no terminal id")
	}

	e.setUnreachable(true)
	e.clock.Advance(10 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusTimeout || got.ExitReason != timeoutTabMayBeOpen {
		t.Fatalf("run = %s %q, want timeout %q", got.Status, got.ExitReason, timeoutTabMayBeOpen)
	}
	// The runner learns the outage from the first key, which does not get
	// through. Ctrl-C must not have been sent.
	if e.interrupted() {
		t.Fatal("Ctrl-C reached the agent on an unreachable target")
	}
	tried := e.stopSteps()

	// The server came back with the session restored: same pane id, new shell.
	e.herdr.restartServer()
	e.herdr.output = "restored shell\n"
	e.setUnreachable(false)
	e.restart(timeoutTask())
	after := e.waitReason(run.ID, timeoutTabClosedReplaced)
	if steps := e.stopSteps()[len(tried):]; !slices.Equal(steps, []string{"tab close"}) {
		t.Errorf("stop calls once the target answers = %q, want only the tab close", steps)
	}
	if e.interrupted() {
		t.Errorf("Ctrl-C reached the restored shell")
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
	if after.Status != store.StatusTimeout || !after.EndedAt.Equal(got.EndedAt) || after.OutputTail != "" || after.HerdrTerminal != terminal {
		t.Errorf("run = %+v, want timeout, the same ended_at, no output from the new shell, terminal %q", after, terminal)
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target")
	}
	if n := len(e.notifications()); n != 1 {
		t.Errorf("%d notifications %v, want 1 for the timeout", n, e.notifications())
	}

	// Recorded, so a later poll does not send keys either.
	sent := e.stopSteps()
	e.runner.PollTarget(e.ctx, "local")
	if steps := e.stopSteps(); !slices.Equal(steps, sent) {
		t.Errorf("stop calls after a later poll = %q, want %q", steps, sent)
	}
}

// A run stored before herdr_terminal existed cannot be told from a restored
// pane, so the pending stop still sends the keys.
func TestTimeoutDuringAnOutageStillStopsWhenNoTerminalIDWasStored(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(timeoutTask())
	e.poll(run.ID)
	e.setUnreachable(true)
	e.clock.Advance(10 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	if got := e.waitEnded(run.ID); got.ExitReason != timeoutTabMayBeOpen {
		t.Fatalf("exit reason = %q, want %q", got.ExitReason, timeoutTabMayBeOpen)
	}
	tried := e.stopSteps()
	row := e.get(run.ID)
	row.HerdrTerminal = ""
	if err := e.store.UpdateRun(e.ctx, row); err != nil {
		t.Fatal(err)
	}

	e.herdr.restartServer()
	e.setUnreachable(false)
	e.restart(timeoutTask())
	e.driveStop()
	e.waitReason(run.ID, timeoutTabClosed)
	if steps := e.stopSteps()[len(tried):]; !slices.Equal(steps, stopSequence) {
		t.Errorf("stop calls once the target answers = %q, want %q", steps, stopSequence)
	}
	if !e.interrupted() || e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("interrupted %v, tab open %v; want the agent stopped and the tab closed", e.interrupted(), e.herdr.tabOpen(run.HerdrTab))
	}
}
