package runner_test

import (
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

const (
	reasonTargetRemoved = "stopped by user; target removed from config, tab not closed"
	reasonTargetExpired = "stopped by user; target did not come back within history_days, tab not closed"
	reasonReplacedTab   = "stopped by user; pane replaced: herdr server restarted; tab closed after the target came back"
)

// noPendingCleanups waits until the store has no pending cleanup left.
func (e *env) noPendingCleanups() {
	e.t.Helper()
	e.waitFor("the pending cleanups to clear", func() bool {
		pending, err := e.store.ListPendingCleanups(e.ctx)
		return err == nil && len(pending) == 0
	})
}

// Issue #58: a reload that removes the target drops its pending stops. The
// row says the tab was not closed, and the amendment is reported.
func TestPendingStopIsDroppedWhenItsTargetIsRemoved(t *testing.T) {
	e := newEnv(t)
	var amended []store.Run
	e.recordAmends(&amended)
	run := e.stopUnreachable()
	steps := e.stopSteps()

	e.runner.Reconfigure(envConfig(map[string]config.Target{}), func(string, config.Target) target.Target { return nil })
	got := e.waitReason(run.ID, reasonTargetRemoved)
	if got.Status != store.StatusStopped || !got.EndedAt.Equal(run.EndedAt) {
		t.Errorf("run after the drop = %s ended %v, want stopped and the same ended_at", got.Status, got.EndedAt)
	}
	e.noPendingCleanups()
	if got := e.amendedRuns(&amended); len(got) != 1 || got[0].ExitReason != reasonTargetRemoved {
		t.Errorf("OnAmend = %+v, want one call with %q", got, reasonTargetRemoved)
	}
	if got := e.stopSteps(); !slices.Equal(got, steps) {
		t.Errorf("stop calls after the drop = %q, want %q", got, steps)
	}
	if n := len(e.endedRuns()); n != 1 {
		t.Errorf("OnEnd called %d times, want 1", n)
	}

	// Gone for the next daemon too.
	e.restart(deps())
	if got := e.get(run.ID); got.ExitReason != reasonTargetRemoved {
		t.Errorf("exit reason after a restart = %q, want %q", got.ExitReason, reasonTargetRemoved)
	}
}

// The target was removed while the daemon was down: Recover drops the
// pending stop and the row says so.
func TestRecoverDropsThePendingStopOfARemovedTarget(t *testing.T) {
	e := newEnv(t)
	started := e.clock.Now().Add(-time.Hour)
	run := store.Run{
		ID: store.NewRunID(started), TaskID: "deps", Target: "box", Trigger: store.TriggerManual,
		StartedAt: started, EndedAt: started.Add(time.Minute), Status: store.StatusStopped,
		HerdrTab: "w1:t9", HerdrPane: "w1:p9", ExitReason: reasonTabMayBeOpen,
	}
	e.insertRun(run)
	if err := e.store.SetPendingCleanup(e.ctx, run.ID, true); err != nil {
		t.Fatal(err)
	}
	var amended []store.Run
	e.onAmend = func(r store.Run) {
		e.mu.Lock()
		defer e.mu.Unlock()
		amended = append(amended, r)
	}

	e.restart(deps())
	if got := e.get(run.ID); got.ExitReason != reasonTargetRemoved {
		t.Fatalf("exit reason = %q, want %q", got.ExitReason, reasonTargetRemoved)
	}
	if pending, err := e.store.ListPendingCleanups(e.ctx); err != nil || len(pending) != 0 {
		t.Errorf("pending cleanups = %v, %v; want none", pending, err)
	}
	if got := e.amendedRuns(&amended); len(got) != 1 || got[0].ID != run.ID {
		t.Errorf("OnAmend = %+v, want one call for the run", got)
	}
	if got := e.stopSteps(); len(got) != 0 {
		t.Errorf("stop calls = %q, want none: local is not the run's target", got)
	}
}

// A target that stays unreachable for history_days after the run ended
// will not get its stop finished: the pending stop is dropped, so retention
// can remove the run.
func TestPendingStopIsDroppedAfterHistoryDays(t *testing.T) {
	e := newEnv(t)
	var amended []store.Run
	e.recordAmends(&amended)
	run := e.stopUnreachable()
	steps := e.stopSteps()

	e.clock.Advance(89 * 24 * time.Hour)
	e.runner.PollTarget(e.ctx, "local")
	if got := e.get(run.ID); got.ExitReason != reasonTabMayBeOpen {
		t.Fatalf("exit reason inside history_days = %q, want %q", got.ExitReason, reasonTabMayBeOpen)
	}
	if pending, err := e.store.ListPendingCleanups(e.ctx); err != nil || len(pending) != 1 {
		t.Fatalf("pending cleanups inside history_days = %v, %v; want the run kept", pending, err)
	}

	// The next daemon finds it too old.
	e.clock.Advance(2 * 24 * time.Hour)
	e.restart(deps())
	e.waitReason(run.ID, reasonTargetExpired)
	e.noPendingCleanups()
	if got := e.amendedRuns(&amended); len(got) != 1 || got[0].ExitReason != reasonTargetExpired {
		t.Errorf("OnAmend = %+v, want one call with %q", got, reasonTargetExpired)
	}
	if got := e.stopSteps(); !slices.Equal(got, steps) {
		t.Errorf("stop calls after the drop = %q, want %q", got, steps)
	}
	// Retention no longer keeps the run.
	n, err := e.store.DeleteRunsBefore(e.ctx, e.clock.Now().AddDate(0, 0, -config.DefaultHistoryDays))
	if err != nil || n != 1 {
		t.Errorf("DeleteRunsBefore = %d, %v; want the run deleted", n, err)
	}
}

// history_days from the config is the limit, and a poll of the running
// daemon applies it.
func TestPendingStopIsDroppedAfterTheConfiguredHistoryDays(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	cfg := envConfig(map[string]config.Target{"local": {}})
	cfg.HistoryDays = 7
	e.runner.Reconfigure(cfg, func(string, config.Target) target.Target { return nil })

	e.clock.Advance(8 * 24 * time.Hour)
	e.runner.PollTarget(e.ctx, "local")
	e.waitReason(run.ID, reasonTargetExpired)
	e.noPendingCleanups()
}

// An old pending stop whose target answers is finished, not dropped.
func TestOldPendingStopIsFinishedWhenItsTargetAnswers(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	e.clock.Advance(100 * 24 * time.Hour)
	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	e.waitReason(run.ID, reasonTabClosed)
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
}

// failAmend makes every update of the run's row fail until the returned
// function is called.
func (e *env) failAmend(id string) (restore func()) {
	e.t.Helper()
	db := e.openDB()
	if _, err := db.Exec(`CREATE TRIGGER fail_amend BEFORE UPDATE ON runs WHEN OLD.id = '` + id +
		`' BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`); err != nil {
		e.t.Fatal(err)
	}
	return func() {
		e.t.Helper()
		if _, err := db.Exec(`DROP TRIGGER fail_amend`); err != nil {
			e.t.Fatal(err)
		}
	}
}

// Issue #58: the daemon dies after closing the tab and before amending the
// row. The next daemon finds the tab gone, but knows the close was under
// way: it records the close and the output tail that was read, and does
// not send the stop keys again.
func TestPendingStopInterruptedAfterTheTabCloseRecordsTheClose(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	restore := e.failAmend(run.ID)
	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	e.waitFor("the amend to fail", func() bool {
		return len(e.logLines("cannot store the finished stop")) >= 1
	})
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Fatal("tab left open")
	}
	steps := e.stopSteps()
	restore()

	// The old runner is not used again: the daemon died here.
	var amended []store.Run
	e.onAmend = func(r store.Run) {
		e.mu.Lock()
		defer e.mu.Unlock()
		amended = append(amended, r)
	}
	e.restart(deps())
	got := e.waitReason(run.ID, reasonTabClosed)
	if got.OutputTail != "last words\n" {
		t.Errorf("output tail = %q, want the tail read before the tab was closed", got.OutputTail)
	}
	e.noPendingCleanups()
	if want := append(steps, "tab close"); !slices.Equal(e.stopSteps(), want) {
		t.Errorf("stop calls = %q, want %q: only the tab close is tried again", e.stopSteps(), want)
	}
	if got := e.amendedRuns(&amended); len(got) != 1 || got[0].ExitReason != reasonTabClosed {
		t.Errorf("OnAmend = %+v, want one call with %q", got, reasonTabClosed)
	}
}

// The same crash when the pane was replaced by a new shell: the next
// daemon still records the replacement.
func TestPendingStopOfAReplacedPaneInterruptedAfterTheTabClose(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	e.herdr.restartServer()
	restore := e.failAmend(run.ID)
	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.waitFor("the amend to fail", func() bool {
		return len(e.logLines("cannot store the finished stop")) >= 1
	})
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Fatal("tab left open")
	}
	restore()

	e.restart(deps())
	got := e.waitReason(run.ID, reasonReplacedTab)
	if got.OutputTail != "" {
		t.Errorf("output tail = %q, want none: the new shell's text is not the run's", got.OutputTail)
	}
	e.noPendingCleanups()
	if got, want := e.stopSteps(), []string{"keys esc", "tab close", "tab close"}; !slices.Equal(got, want) {
		t.Errorf("stop calls = %q, want %q: no keys for the new shell", got, want)
	}
}

// The daemon dies between storing that it is about to close the tab and
// the close: the next daemon closes the tab and keeps the stored tail.
func TestPendingStopInterruptedBeforeTheTabCloseClosesIt(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	if err := e.store.MarkCleanupClosing(e.ctx, run.ID, false, "saved tail\n"); err != nil {
		t.Fatal(err)
	}
	e.setUnreachable(false)
	e.restart(deps())
	got := e.waitReason(run.ID, reasonTabClosed)
	if got.OutputTail != "saved tail\n" {
		t.Errorf("output tail = %q, want the stored one", got.OutputTail)
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab left open")
	}
	if got, want := e.stopSteps(), []string{"keys esc", "tab close"}; !slices.Equal(got, want) {
		t.Errorf("stop calls = %q, want %q", got, want)
	}
}

// A tab that was already gone before the stop got to it is still recorded
// as gone after a crash: nothing was about to close it.
func TestPendingStopOfATabThatIsGoneStaysGoneAfterACrash(t *testing.T) {
	e := newEnv(t)
	run := e.stopUnreachable()
	e.herdr.mu.Lock()
	delete(e.herdr.tabs, run.HerdrTab)
	for _, p := range e.herdr.panes {
		p.closed = p.closed || p.tab == run.HerdrTab
	}
	e.herdr.mu.Unlock()
	restore := e.failAmend(run.ID)
	e.setUnreachable(false)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	e.waitFor("the amend to fail", func() bool {
		return len(e.logLines("cannot store the finished stop")) >= 1
	})
	restore()

	e.restart(deps())
	e.driveStop()
	e.waitReason(run.ID, "stopped by user; tab was gone when the target came back")
	e.noPendingCleanups()
}
