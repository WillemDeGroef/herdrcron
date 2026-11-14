package runner_test

import (
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// startLostCreate starts a scheduled run whose create command ("tab
// create" or "workspace create") herdr runs while the reply is lost: the
// tab exists, the run has no herdr ids and is stored as host_unreachable,
// and the target stays unreachable.
func (e *env) startLostCreate(cmd string) store.Run {
	e.t.Helper()
	before := len(e.herdr.openTabs())
	e.herdr.mu.Lock()
	e.herdr.loseCreateReply = cmd
	e.herdr.mu.Unlock()
	run, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
	if err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	if run.Status != store.StatusHostUnreachable || run.HerdrTab != "" {
		e.t.Fatalf("run with a lost %s reply = %+v, want host_unreachable without a tab id", cmd, run)
	}
	if got := len(e.herdr.openTabs()); got != before+1 {
		e.t.Fatalf("open tabs after the lost %s reply = %d, want %d: the tab was created", cmd, got, before+1)
	}
	return run
}

func (e *env) lostCreates() []store.LostCreate {
	e.t.Helper()
	recs, err := e.store.ListLostCreates(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return recs
}

// Issue #57: herdr created the tab and the reply to `tab create` was lost.
// When the target answers again the empty hc- tab is closed. The tab of
// another run of the task, which carries the same label, and a tab the user
// opened meanwhile stay open.
func TestTabOfALostTabCreateReplyIsClosedWhenTheTargetAnswers(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	first := e.start(deps())
	lost := e.startLostCreate("tab create")
	if got := e.lostCreates(); len(got) != 1 || got[0].RunID != lost.ID {
		t.Fatalf("stored lost creates = %+v, want one for run %s", got, lost.ID)
	}
	users := e.herdr.addTab(first.HerdrWorkspace, "notes")

	// Still unreachable: the tab is not looked for yet.
	lists := e.herdr.count("herdr", "tab", "list")
	e.runner.PollTarget(e.ctx, "local")
	if got := e.herdr.count("herdr", "tab", "list"); got != lists {
		t.Errorf("%d tab lists while the target is unreachable, want none", got-lists)
	}

	e.reconnect()
	e.runner.PollTarget(e.ctx, "local")
	if got, want := e.herdr.openTabs(), []string{first.HerdrTab, users}; !slices.Equal(got, want) {
		t.Errorf("open tabs after the target answered = %q, want %q: only the orphan is closed", got, want)
	}
	if got := e.get(first.ID); got.Status != store.StatusRunning {
		t.Errorf("the other run = %+v, want it still running", got)
	}
	if got := e.lostCreates(); len(got) != 0 {
		t.Errorf("stored lost creates after the close = %+v, want none", got)
	}

	// Settled once: later polls do not look again.
	lists = e.herdr.count("herdr", "tab", "list")
	closes := e.herdr.count("herdr", "tab", "close")
	e.poll(first.ID)
	if l, c := e.herdr.count("herdr", "tab", "list"), e.herdr.count("herdr", "tab", "close"); l != lists || c != closes {
		t.Errorf("a later poll made %d tab lists and %d tab closes, want none", l-lists, c-closes)
	}
}

// The next launch on the target closes the orphan before it opens its own
// tab, so the orphan is never mistaken for a tab of a later run.
func TestNextLaunchClosesTheOrphanTabFirst(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	first := e.start(deps())
	e.startLostCreate("tab create")

	e.reconnect()
	e.herdr.script(working)
	next := e.start(deps())
	if next.Status != store.StatusRunning {
		t.Fatalf("run after the target answered = %+v, want running", next)
	}
	if got, want := e.herdr.openTabs(), []string{first.HerdrTab, next.HerdrTab}; !slices.Equal(got, want) {
		t.Errorf("open tabs = %q, want %q: the orphan closed, both runs' tabs open", got, want)
	}
}

// A create that never reached herdr left no tab: nothing is closed, also
// not a tab that was already there with the same label.
func TestTabCreateThatNeverArrivedClosesNothing(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	first := e.start(deps())
	e.herdr.mu.Lock()
	e.herdr.unreachableFor = "tab create"
	e.herdr.mu.Unlock()
	run, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
	if err != nil || run.Status != store.StatusHostUnreachable {
		t.Fatalf("Start = %+v, %v, want host_unreachable", run, err)
	}
	e.herdr.mu.Lock()
	e.herdr.unreachableFor = ""
	e.herdr.mu.Unlock()
	users := e.herdr.addTab(first.HerdrWorkspace, "notes")

	e.reconnect()
	e.runner.PollTarget(e.ctx, "local")
	if n := e.herdr.count("herdr", "tab", "close"); n != 0 {
		t.Errorf("%d tab closes, want none", n)
	}
	if got, want := e.herdr.openTabs(), []string{first.HerdrTab, users}; !slices.Equal(got, want) {
		t.Errorf("open tabs = %q, want %q", got, want)
	}
	if got := e.lostCreates(); len(got) != 0 {
		t.Errorf("stored lost creates = %+v, want none once the target answered", got)
	}
}

// Two new tabs with the label: herdrcron cannot tell which one it created,
// so it closes neither.
func TestAmbiguousOrphanTabIsLeftOpen(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	first := e.start(deps())
	e.startLostCreate("tab create")
	e.herdr.addTab(first.HerdrWorkspace, "hc-deps-0800")

	e.reconnect()
	e.runner.PollTarget(e.ctx, "local")
	if n := e.herdr.count("herdr", "tab", "close"); n != 0 {
		t.Errorf("%d tab closes, want none", n)
	}
	if got := e.lostCreates(); len(got) != 0 {
		t.Errorf("stored lost creates = %+v, want none: the question is settled", got)
	}
}

// The lost create is in the store: a daemon restarted during the outage
// still closes the orphan.
func TestOrphanTabIsClosedAfterADaemonRestart(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)
	first := e.start(deps())
	e.statuses(first.ID, 3) // done; keep_tab leaves its tab open
	e.startLostCreate("tab create")

	e.runner = e.newRunner()
	e.reconnect()
	if err := e.runner.Recover(e.ctx, []config.Task{deps()}); err != nil {
		t.Fatal(err)
	}
	if got, want := e.herdr.openTabs(), []string{first.HerdrTab}; !slices.Equal(got, want) {
		t.Errorf("open tabs after the restart = %q, want %q", got, want)
	}
	if got := e.lostCreates(); len(got) != 0 {
		t.Errorf("stored lost creates = %+v, want none", got)
	}
}

// The reply to `workspace create` was lost: the new hc- workspace has the
// one tab herdr made with it, and that tab is closed.
func TestTabOfALostWorkspaceCreateReplyIsClosed(t *testing.T) {
	e := newEnv(t)
	e.startLostCreate("workspace create")

	e.reconnect()
	e.runner.PollTarget(e.ctx, "local")
	if got := e.herdr.openTabs(); len(got) != 0 {
		t.Errorf("open tabs = %q, want none", got)
	}
	if got := e.lostCreates(); len(got) != 0 {
		t.Errorf("stored lost creates = %+v, want none", got)
	}
}

// A tab somebody added to the new workspace means its tabs are no longer
// only herdrcron's: the workspace is left alone.
func TestLostWorkspaceWithASecondTabIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.startLostCreate("workspace create")
	e.herdr.addTab("w1", "notes")

	e.reconnect()
	e.runner.PollTarget(e.ctx, "local")
	if n := e.herdr.count("herdr", "tab", "close"); n != 0 {
		t.Errorf("%d tab closes, want none", n)
	}
}

// A target that is replaced is another machine: the lost create is dropped
// and no tab is closed there.
func TestLostCreateIsDroppedWhenItsTargetIsReplaced(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)
	first := e.start(deps())
	e.statuses(first.ID, 3)
	e.startLostCreate("tab create")
	tabs := e.herdr.openTabs()

	e.reconnect()
	other := e.herdr // the same tabs, as if the new host had them too
	deferred := e.runner.Reconfigure(envConfig(map[string]config.Target{"local": {Host: "box"}}),
		func(string, config.Target) target.Target { return other })
	if len(deferred) != 0 {
		t.Fatalf("deferred = %q, want none", deferred)
	}
	e.runner.PollTarget(e.ctx, "local")
	if got := e.herdr.openTabs(); !slices.Equal(got, tabs) {
		t.Errorf("open tabs = %q, want %q: nothing closed on the new target", got, tabs)
	}
	if got := e.lostCreates(); len(got) != 0 {
		t.Errorf("stored lost creates = %+v, want none", got)
	}
}

// While the herdr server is down its tabs cannot be listed, and it restores
// them when it starts: the lost create is kept until the server answers.
func TestLostCreateIsKeptWhileTheHerdrServerIsDown(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)
	first := e.start(deps())
	e.statuses(first.ID, 3)
	e.startLostCreate("tab create")

	e.reconnect()
	setDown := func(v bool) {
		e.herdr.mu.Lock()
		e.herdr.serverDown = v
		e.herdr.mu.Unlock()
	}
	setDown(true)
	e.runner.PollTarget(e.ctx, "local")
	e.runner.PollTarget(e.ctx, "local")
	if got := e.lostCreates(); len(got) != 1 {
		t.Fatalf("stored lost creates with the server down = %+v, want the one kept", got)
	}

	setDown(false)
	e.clock.Advance(5 * time.Minute)
	e.runner.PollTarget(e.ctx, "local")
	e.runner.PollTarget(e.ctx, "local")
	if got, want := e.herdr.openTabs(), []string{first.HerdrTab}; !slices.Equal(got, want) {
		t.Errorf("open tabs once the server is back = %q, want %q", got, want)
	}
}
