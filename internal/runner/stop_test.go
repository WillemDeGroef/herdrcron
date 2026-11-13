package runner_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// waitFor waits, in real time, until cond holds: the code under test runs
// in its own goroutine and blocks on the fake clock.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitClock waits until the code under test waits on the fake clock.
func (e *env) waitClock() {
	e.t.Helper()
	e.waitFor("a clock wait", func() bool { return e.clock.Waiters() > 0 })
}

// stopSteps names the stop calls in order: keys, waits are implied by the
// test driving the clock, then pane read and tab close.
func (e *env) stopSteps() []string {
	var out []string
	for _, c := range e.herdr.Calls() {
		if len(c) < 3 || c[0] != "herdr" {
			continue
		}
		switch cmd := c[1] + " " + c[2]; cmd {
		case "pane send-keys":
			k := "keys"
			for _, x := range c[4:] {
				k += " " + x
			}
			out = append(out, k)
		case "pane read", "tab close":
			out = append(out, cmd)
		}
	}
	return out
}

// driveStop moves the fake clock through a stop's two waits: 2 s between
// the Ctrl-C pairs, then 10 s before the tab closes.
func (e *env) driveStop() {
	e.t.Helper()
	e.waitClock()
	e.clock.Advance(2 * time.Second)
	e.waitClock()
	e.clock.Advance(10 * time.Second)
}

// waitEnded waits until the run's row has ended_at and returns it.
func (e *env) waitEnded(id string) store.Run {
	e.t.Helper()
	var r store.Run
	e.waitFor("run "+id+" to end", func() bool {
		r = e.get(id)
		return !r.EndedAt.IsZero()
	})
	return r
}

type stopResult struct {
	run store.Run
	err error
}

// stopAsync calls Stop in a goroutine; Stop blocks on the fake clock.
func (e *env) stopAsync(id string) <-chan stopResult {
	ch := make(chan stopResult, 1)
	go func() {
		r, err := e.runner.Stop(e.ctx, id)
		ch <- stopResult{r, err}
	}()
	return ch
}

func TestUserStopSendsKeysWaitsReadsAndClosesTheTab(t *testing.T) {
	e := newEnv(t)
	e.herdr.output = "last words\n"
	e.herdr.script(working)
	run := e.start(deps())
	e.poll(run.ID)

	res := e.stopAsync(run.ID)
	e.waitClock()
	if got, want := e.stopSteps(), []string{"keys esc", "keys ctrl+c ctrl+c"}; !slices.Equal(got, want) {
		t.Fatalf("before the 2 s wait: %q, want %q", got, want)
	}
	e.clock.Advance(2 * time.Second)
	e.waitClock()
	if got, want := e.stopSteps(), []string{"keys esc", "keys ctrl+c ctrl+c", "keys ctrl+c ctrl+c"}; !slices.Equal(got, want) {
		t.Fatalf("before the 10 s wait: %q, want %q", got, want)
	}
	e.clock.Advance(10 * time.Second)
	r := <-res
	if r.err != nil {
		t.Fatalf("Stop: %v", r.err)
	}
	want := []string{"keys esc", "keys ctrl+c ctrl+c", "keys ctrl+c ctrl+c", "pane read", "tab close"}
	if got := e.stopSteps(); !slices.Equal(got, want) {
		t.Errorf("stop calls = %q, want %q", got, want)
	}
	for _, c := range e.herdr.Calls() {
		if len(c) > 3 && c[1] == "pane" && c[2] == "send-keys" && c[3] != run.HerdrPane {
			t.Errorf("keys sent to pane %s, want %s", c[3], run.HerdrPane)
		}
	}
	got := e.get(run.ID)
	if got.Status != store.StatusStopped || got.ExitReason != "stopped by user" || got.EndedAt.IsZero() || got.OutputTail != "last words\n" {
		t.Errorf("stored run = %+v, want stopped by user with ended_at and the output tail", got)
	}
	if r.run.ID != run.ID || r.run.Status != store.StatusStopped || r.run.EndedAt.IsZero() {
		t.Errorf("Stop returned %+v, want the ended run", r.run)
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open after a stop")
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target")
	}
	if e.runner.Active(run.ID) {
		t.Errorf("stopped run is still tracked")
	}
}

func TestStopUnknownRunIsNotActive(t *testing.T) {
	e := newEnv(t)
	if _, err := e.runner.Stop(e.ctx, "nope"); !errors.Is(err, runner.ErrNotActive) {
		t.Errorf("Stop = %v, want ErrNotActive", err)
	}
}

// After Ctrl-C the agent exits to the shell, and closing the tab removes
// the pane: polls during the stop must not end the run as failed or lost.
func TestPollsDuringAStopLeaveTheRunToTheStop(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(deps())
	e.poll(run.ID)

	res := e.stopAsync(run.ID)
	e.waitClock()
	e.runner.PollTarget(e.ctx, "local")
	e.runner.PollTarget(e.ctx, "local")
	if got := e.get(run.ID); !got.EndedAt.IsZero() || got.Status != store.StatusRunning {
		t.Fatalf("after polls during the stop: %s, ended %v; want still running", got.Status, got.EndedAt)
	}
	if n := e.herdr.count("herdr", "pane", "process-info"); n != 0 {
		t.Errorf("%d process-info calls during a stop, want 0", n)
	}
	e.clock.Advance(2 * time.Second)
	e.waitClock()
	e.runner.PollTarget(e.ctx, "local")
	e.clock.Advance(10 * time.Second)
	if r := <-res; r.err != nil || r.run.Status != store.StatusStopped {
		t.Fatalf("Stop = %+v, %v; want stopped", r.run, r.err)
	}
	if got := e.get(run.ID); got.Status != store.StatusStopped {
		t.Errorf("status = %s, want stopped", got.Status)
	}
}

func TestTimeoutStopsTheRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	task := deps()
	task.Timeout.Duration = 2 * time.Hour
	run := e.start(task)
	e.poll(run.ID)

	e.clock.Advance(2*time.Hour - 30*time.Second)
	e.runner.PollTarget(e.ctx, "local")
	if e.clock.Waiters() != 0 || len(e.stopSteps()) != 0 {
		t.Fatalf("stop began before the timeout: %q", e.stopSteps())
	}
	e.clock.Advance(15 * time.Second)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusTimeout || got.ExitReason != "timeout after 2h" {
		t.Errorf("run = %s %q, want timeout %q", got.Status, got.ExitReason, "timeout after 2h")
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open after a timeout")
	}
}

// The timeout counts while the target does not answer.
func TestTimeoutFiresWhenThePaneListFails(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	task := deps()
	task.Timeout.Duration = 90 * time.Minute
	run := e.start(task)
	e.herdr.mu.Lock()
	e.herdr.serverDown = true
	e.herdr.mu.Unlock()
	e.clock.Advance(91 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusTimeout || got.ExitReason != "timeout after 1h30m" {
		t.Errorf("run = %s %q, want timeout %q", got.Status, got.ExitReason, "timeout after 1h30m")
	}
}

func TestOnBlockedStopStopsOnTheFirstBlockedSample(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked)
	task := deps()
	task.OnBlocked = "stop"
	run := e.start(task)
	e.poll(run.ID)
	if len(e.stopSteps()) != 0 {
		t.Fatalf("stop began while working")
	}
	e.poll(run.ID)
	e.driveStop()
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusStopped || got.ExitReason != "blocked (on_blocked: stop)" {
		t.Errorf("run = %s %q, want stopped %q", got.Status, got.ExitReason, "blocked (on_blocked: stop)")
	}
}

func TestOnBlockedNotifyKeepsTheRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked, blocked)
	run := e.start(deps()) // on_blocked: notify
	if got := e.statuses(run.ID, 3); !slices.Equal(got, []string{"running", "blocked", "blocked"}) {
		t.Fatalf("statuses = %v, want running, blocked, blocked", got)
	}
	if steps := e.stopSteps(); len(steps) != 0 || e.clock.Waiters() != 0 {
		t.Errorf("on_blocked: notify began a stop: %q", steps)
	}
}

func TestOnEndIsCalledOncePerEndedRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)       // done
	e.herdr.script(working, vanished)         // lost
	e.herdr.script(working, working, working) // stopped
	done, lost, stopped := e.start(deps()), e.start(deps()), e.start(deps())
	e.herdr.mu.Lock()
	e.herdr.failRun = true
	e.herdr.mu.Unlock()
	failed := e.start(deps())
	e.statuses(done.ID, 3)
	res := e.stopAsync(stopped.ID)
	e.driveStop()
	if r := <-res; r.err != nil {
		t.Fatal(r.err)
	}

	want := map[string]string{
		failed.ID: store.StatusFailed, lost.ID: store.StatusLost,
		done.ID: store.StatusDone, stopped.ID: store.StatusStopped,
	}
	got := e.endedRuns()
	if len(got) != len(want) {
		t.Fatalf("OnEnd called %d times, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		if want[r.ID] != r.Status || r.EndedAt.IsZero() {
			t.Errorf("OnEnd(%s) status %s ended %v, want %s with ended_at", r.ID, r.Status, r.EndedAt, want[r.ID])
		}
		delete(want, r.ID)
	}
}

func TestActiveRunsIncludesRunsBeingStopped(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working)
	e.herdr.script(working, working)
	a, b := e.start(deps()), e.start(deps())
	ids := func(runs []store.Run) []string {
		var out []string
		for _, r := range runs {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if got := ids(e.runner.ActiveRuns()); !slices.Equal(got, want) {
		t.Fatalf("ActiveRuns = %v, want %v", got, want)
	}
	res := e.stopAsync(a.ID)
	e.waitClock()
	if got := e.runner.ActiveRuns(); len(got) != 2 {
		t.Errorf("ActiveRuns during a stop = %d runs, want 2", len(got))
	}
	e.clock.Advance(2 * time.Second)
	e.waitClock()
	e.clock.Advance(10 * time.Second)
	<-res
	if got := e.runner.ActiveRuns(); len(got) != 1 || got[0].ID != b.ID || got[0].Status != store.StatusRunning {
		t.Errorf("ActiveRuns after the stop = %+v, want only %s", got, b.ID)
	}
}

func TestSecondStopWaitsForTheFirst(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(deps())
	first := e.stopAsync(run.ID)
	e.waitClock()
	second := e.stopAsync(run.ID)
	e.clock.Advance(2 * time.Second)
	e.waitClock()
	e.clock.Advance(10 * time.Second)
	for _, ch := range []<-chan stopResult{first, second} {
		if r := <-ch; r.err != nil || r.run.Status != store.StatusStopped {
			t.Errorf("Stop = %+v, %v; want the stopped run", r.run, r.err)
		}
	}
	if n := e.herdr.count("herdr", "pane", "send-keys"); n != 3 {
		t.Errorf("%d send-keys calls, want 3 (one stop)", n)
	}
	if n := len(e.endedRuns()); n != 1 {
		t.Errorf("OnEnd called %d times, want 1", n)
	}
}

// A daemon that shuts down mid-stop leaves the run open for Recover, and
// Run does not return before the stop has given up.
func TestShutdownDuringAStopLeavesTheRunOpen(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(deps())
	ctx, cancel := context.WithCancel(e.ctx)
	ran := make(chan struct{})
	go func() {
		e.runner.Run(ctx)
		close(ran)
	}()
	res := e.stopAsync(run.ID)
	e.waitFor("the first ctrl+c", func() bool { return e.herdr.count("herdr", "pane", "send-keys") == 2 })
	cancel()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if r := <-res; r.err == nil {
		t.Errorf("Stop = %+v, want an error for the interrupted stop", r.run)
	}
	if got := e.get(run.ID); !got.EndedAt.IsZero() || got.Status != store.StatusRunning {
		t.Errorf("run = %s ended %v, want still open", got.Status, got.EndedAt)
	}
	if n := e.herdr.count("herdr", "tab", "close"); n != 0 {
		t.Errorf("tab closed by an interrupted stop")
	}
	if _, err := e.runner.Stop(e.ctx, run.ID); err == nil {
		t.Errorf("Stop after Run returned succeeded")
	}
}

// A stop that lands after the herdr server restored the pane id must not
// send keys to the new shell or close its tab.
func TestUserStopOfAReplacedPaneEndsLost(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	run := e.start(deps())
	e.poll(run.ID)
	e.herdr.restartServer()

	got, err := e.runner.Stop(e.ctx, run.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got.Status != store.StatusLost || got.ExitReason != "pane replaced: herdr server restarted" || got.EndedAt.IsZero() {
		t.Errorf("Stop = %s %q ended %v, want lost", got.Status, got.ExitReason, got.EndedAt)
	}
	if steps := e.stopSteps(); len(steps) != 0 {
		t.Errorf("stop touched the restored pane: %q", steps)
	}
	if !e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("restored tab was closed")
	}
	notes := e.notifications()
	if len(notes) != 1 || notes[0].event != config.EventLost {
		t.Errorf("notifications = %+v, want one lost", notes)
	}
}

// A timeout decided before the pane list must not stop the shell a
// restarted server restored under the run's pane id.
func TestTimeoutOfAReplacedPaneEndsLost(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working)
	task := deps()
	task.Timeout.Duration = 2 * time.Hour
	run := e.start(task)
	e.poll(run.ID)
	e.herdr.restartServer()

	e.clock.Advance(2 * time.Hour)
	e.runner.PollTarget(e.ctx, "local")
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusLost || got.ExitReason != "pane replaced: herdr server restarted" {
		t.Errorf("run = %s %q, want lost %q", got.Status, got.ExitReason, "pane replaced: herdr server restarted")
	}
	if e.clock.Waiters() != 0 {
		t.Errorf("timeout waited to send keys")
	}
	if steps := e.stopSteps(); len(steps) != 0 {
		t.Errorf("stop touched the restored pane: %q", steps)
	}
	if !e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("restored tab was closed")
	}
	notes := e.notifications()
	if len(notes) != 1 || notes[0].event != config.EventLost {
		t.Errorf("notifications = %+v, want one lost", notes)
	}
}

// A run stored before herdr_terminal existed cannot be told from a
// restored pane, so a user stop still stops it.
func TestUserStopWithoutATerminalIDStillStops(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working)
	run := e.start(deps())
	e.poll(run.ID)
	old := e.get(run.ID)
	old.HerdrTerminal = ""
	if err := e.store.UpdateRun(e.ctx, old); err != nil {
		t.Fatal(err)
	}
	e.restart(deps())
	if !e.runner.Active(run.ID) {
		t.Fatal("recovered run is not tracked")
	}

	res := e.stopAsync(run.ID)
	e.driveStop()
	r := <-res
	if r.err != nil || r.run.Status != store.StatusStopped || r.run.ExitReason != "stopped by user" {
		t.Fatalf("Stop = %s %q, %v; want stopped by user", r.run.Status, r.run.ExitReason, r.err)
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
}
