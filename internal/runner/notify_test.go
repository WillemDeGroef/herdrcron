package runner_test

import (
	"slices"
	"testing"
	"time"
)

func (e *env) events() []string {
	var out []string
	for _, n := range e.notifications() {
		out = append(out, n.event)
	}
	return out
}

func TestBlockedNotifiesOncePerChangeToBlocked(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked, blocked, blocked, working, blocked, blocked)
	run := e.start(deps())
	e.statuses(run.ID, 4)
	if got := e.events(); !slices.Equal(got, []string{"blocked"}) {
		t.Fatalf("after one change to blocked and two more blocked polls: events = %q, want one blocked", got)
	}
	e.statuses(run.ID, 3)
	if got := e.events(); !slices.Equal(got, []string{"blocked", "blocked"}) {
		t.Fatalf("after a second change to blocked: events = %q, want two blocked", got)
	}
	want := "herdrcron: deps blocked (target local, run " + run.ID + ")"
	if got := e.notifications()[0].message; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestRecoveredBlockedRunDoesNotNotifyAgain(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked, blocked)
	run := e.start(deps())
	e.statuses(run.ID, 2)
	e.restart(deps())
	e.poll(run.ID)
	if got := e.events(); !slices.Equal(got, []string{"blocked"}) {
		t.Errorf("events = %q, want the one blocked from before the restart", got)
	}
}

func TestOnBlockedStopNotifiesBlockedButNotTheStop(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked)
	task := deps()
	task.OnBlocked = "stop"
	run := e.start(task)
	e.statuses(run.ID, 2)
	e.driveStop()
	e.waitEnded(run.ID)
	if got := e.events(); !slices.Equal(got, []string{"blocked"}) {
		t.Errorf("events = %q, want only blocked", got)
	}
}

func TestEndsThatNotify(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, vanished)         // lost
	e.herdr.script(working, idle, idle)       // done: no notification
	e.herdr.script(working, working, working) // stopped: no notification
	lost, done, stopped := e.start(deps()), e.start(deps()), e.start(deps())
	e.statuses(lost.ID, 3)
	res := e.stopAsync(stopped.ID)
	e.driveStop()
	<-res
	for _, id := range []string{lost.ID, done.ID, stopped.ID} {
		e.waitEnded(id)
	}
	want := []notification{{"lost", "herdrcron: deps lost: pane gone (target local, run " + lost.ID + ")"}}
	if got := e.notifications(); !slices.Equal(got, want) {
		t.Errorf("notifications = %q, want %q", got, want)
	}
}

func TestFailedRunNotifies(t *testing.T) {
	e := newEnv(t)
	e.herdr.preflight = "agent command claude not found on PATH"
	run := e.start(deps())
	got := e.notifications()
	if len(got) != 1 || got[0].event != "failed" {
		t.Fatalf("notifications = %q, want one failed", got)
	}
	r := e.get(run.ID)
	if want := "herdrcron: deps failed: " + r.ExitReason + " (target local, run " + run.ID + ")"; got[0].message != want {
		t.Errorf("message = %q, want %q", got[0].message, want)
	}
}

func TestTimeoutNotifies(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	task := deps()
	task.Timeout.Duration = time.Hour
	run := e.start(task)
	e.clock.Advance(time.Hour)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	e.waitEnded(run.ID)
	if got := e.events(); !slices.Equal(got, []string{"timeout"}) {
		t.Errorf("events = %q, want timeout", got)
	}
}
