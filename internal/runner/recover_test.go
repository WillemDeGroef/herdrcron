package runner_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// insertRun stores a run as a previous daemon left it.
func (e *env) insertRun(run store.Run) {
	e.t.Helper()
	if err := e.store.InsertRun(e.ctx, run); err != nil {
		e.t.Fatal(err)
	}
}

// logLines returns the JSON log records with message msg.
func (e *env) logLines(msg string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(e.log.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			e.t.Fatalf("log line: %v\n%s", err, line)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// restart replaces the runner with a new one on the same store and fake
// herdr, as a daemon restart does, and recovers the open runs.
func (e *env) restart(tasks ...config.Task) {
	e.t.Helper()
	e.runner = e.newRunner()
	if err := e.runner.Recover(e.ctx, tasks); err != nil {
		e.t.Fatalf("Recover: %v", err)
	}
}

func (e *env) runCount() int {
	e.t.Helper()
	runs, err := e.store.ListRuns(e.ctx, "", 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(runs)
}

func TestRecoverContinuesTrackingARunningRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, working, idle, idle)
	run := e.start(deps())
	e.poll(run.ID)

	e.restart(deps())
	if !e.runner.Active(run.ID) {
		t.Fatalf("recovered run is not tracked")
	}
	if got := e.get(run.ID).Status; got != store.StatusRunning {
		t.Errorf("status after recovery = %s, want running", got)
	}
	if got := e.statuses(run.ID, 3); !slices.Equal(got, []string{"running", "running", "done"}) {
		t.Errorf("statuses = %v, want running, running, done", got)
	}
	if n := e.runCount(); n != 1 {
		t.Errorf("%d run rows, want 1", n)
	}
	if n := len(e.endedRuns()); n != 1 {
		t.Errorf("OnEnd called %d times, want 1", n)
	}
}

func TestRecoverMarksARunWhosePaneIsGoneLost(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, vanished)
	run := e.start(deps())
	e.poll(run.ID)

	e.restart(deps())
	got := e.get(run.ID)
	if got.Status != store.StatusLost || got.EndedAt.IsZero() {
		t.Errorf("run = %s ended %v, want lost with ended_at", got.Status, got.EndedAt)
	}
	if e.runner.Active(run.ID) {
		t.Errorf("lost run is still tracked")
	}
}

func TestRecoverKeepsARunOpenWhileTheTargetDoesNotAnswer(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, idle, idle)
	run := e.start(deps())
	e.herdr.mu.Lock()
	e.herdr.unreachable = true
	e.herdr.mu.Unlock()

	e.restart(deps())
	if got := e.get(run.ID); got.Status != store.StatusRunning || !got.EndedAt.IsZero() {
		t.Fatalf("run = %s ended %v, want still running", got.Status, got.EndedAt)
	}
	if !e.runner.Active(run.ID) {
		t.Fatalf("run not tracked while the target does not answer")
	}
	e.herdr.mu.Lock()
	e.herdr.unreachable = false
	e.herdr.mu.Unlock()
	if got := e.statuses(run.ID, 4); !slices.Equal(got, []string{"running", "running", "running", "done"}) {
		t.Errorf("statuses = %v, want running, running, running, done", got)
	}
}

func TestRecoverFailsARunWithoutPane(t *testing.T) {
	e := newEnv(t)
	run := store.Run{
		ID: store.NewRunID(e.clock.Now()), TaskID: "deps", Target: "local", Trigger: store.TriggerManual,
		StartedAt: e.clock.Now(), Status: store.StatusStarting,
	}
	if err := e.store.InsertRun(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	e.restart(deps())
	got := e.get(run.ID)
	if got.Status != store.StatusFailed || got.ExitReason != "daemon stopped during launch" || got.EndedAt.IsZero() {
		t.Errorf("run = %+v, want failed %q", got, "daemon stopped during launch")
	}
	if n := len(e.endedRuns()); n != 1 {
		t.Errorf("OnEnd called %d times, want 1", n)
	}
}

func TestRecoverMarksARunOnAnUnconfiguredTargetLost(t *testing.T) {
	e := newEnv(t)
	run := store.Run{
		ID: store.NewRunID(e.clock.Now()), TaskID: "deps", Target: "gone", Trigger: store.TriggerManual,
		StartedAt: e.clock.Now(), Status: store.StatusRunning, HerdrPane: "w1:p1", HerdrTab: "w1:t1",
	}
	if err := e.store.InsertRun(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	e.restart() // the task is gone too
	got := e.get(run.ID)
	if got.Status != store.StatusLost || got.ExitReason != "target gone not configured" || got.EndedAt.IsZero() {
		t.Errorf("run = %+v, want lost %q", got, "target gone not configured")
	}
}

// A run stored as starting never had its agent launched, even when its tab
// and pane exist: the daemon stopped between opening the tab and storing
// the running status. Tracking it would end it done at the shell prompt.
func TestRecoverFailsAStartingRunWithAPane(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(shell)
	run := e.start(deps())
	run.Status = store.StatusStarting
	if err := e.store.UpdateRun(e.ctx, run); err != nil {
		t.Fatal(err)
	}

	e.restart(deps())
	got := e.get(run.ID)
	if got.Status != store.StatusFailed || got.ExitReason != "daemon stopped during launch" || got.EndedAt.IsZero() {
		t.Errorf("run = %s %q ended %v, want failed %q", got.Status, got.ExitReason, got.EndedAt, "daemon stopped during launch")
	}
	if e.runner.Active(run.ID) {
		t.Errorf("starting run is tracked")
	}
	if e.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab of a run that never launched left open")
	}
}

// A run left blocked (the daemon stopped mid-stop) is stopped again after
// recovery when its task says on_blocked: stop, though its status does
// not change.
func TestRecoverStopsARunLeftBlockedWithOnBlockedStop(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked)
	task := deps()
	task.OnBlocked = "stop"
	run := e.start(deps()) // notify: stays blocked
	if got := e.statuses(run.ID, 2); !slices.Equal(got, []string{"running", "blocked"}) {
		t.Fatalf("statuses = %v, want running, blocked", got)
	}

	e.restart(task)
	e.runner.PollTarget(e.ctx, "local")
	e.driveStop()
	got := e.waitEnded(run.ID)
	if got.Status != store.StatusStopped || got.ExitReason != "blocked (on_blocked: stop)" {
		t.Errorf("run = %s %q, want stopped %q", got.Status, got.ExitReason, "blocked (on_blocked: stop)")
	}
	if n := e.herdr.count("herdr", "pane", "send-keys"); n != 3 {
		t.Errorf("%d send-keys calls, want 3 (one stop)", n)
	}
}

// A herdr server that is not running has lost every pane (the host
// rebooted, say): the runs it held are lost, not kept open.
func TestPollEndsRunsLostWhenTheHerdrServerIsNotRunning(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	e.herdr.script(working)
	a, b := e.start(deps()), e.start(deps())
	e.poll(a.ID)
	e.herdr.mu.Lock()
	e.herdr.serverDown = true
	e.herdr.mu.Unlock()

	e.poll(a.ID)
	for _, id := range []string{a.ID, b.ID} {
		got := e.get(id)
		if got.Status != store.StatusLost || got.ExitReason != "herdr server not running" || got.EndedAt.IsZero() {
			t.Errorf("run %s = %s %q ended %v, want lost %q", id, got.Status, got.ExitReason, got.EndedAt, "herdr server not running")
		}
		if e.runner.Active(id) {
			t.Errorf("lost run %s is still tracked", id)
		}
	}
	if n := len(e.endedRuns()); n != 2 {
		t.Errorf("OnEnd called %d times, want 2", n)
	}
}

// A failed store write for one recovered run is not logged again for the
// runs after it.
func TestRecoverLogsAStoreErrorOnlyForItsRun(t *testing.T) {
	e := newEnv(t)
	bad := store.Run{
		ID: store.NewRunID(e.clock.Now()), TaskID: "deps", Target: "gone", Trigger: store.TriggerManual,
		StartedAt: e.clock.Now(), Status: store.StatusRunning, HerdrPane: "w1:p1", HerdrTab: "w1:t1",
	}
	e.insertRun(bad)
	e.clock.Advance(time.Minute)
	e.herdr.script(working)
	good := e.start(deps())

	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_bad BEFORE UPDATE ON runs WHEN OLD.id = '` + bad.ID +
		`' BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`); err != nil {
		t.Fatal(err)
	}

	e.restart(deps())
	if !e.runner.Active(good.ID) {
		t.Fatalf("good run not tracked")
	}
	lines := e.logLines("cannot store run end")
	if len(lines) != 1 || lines[0]["run_id"] != bad.ID {
		t.Errorf("cannot-store lines = %v, want one for %s", lines, bad.ID)
	}
}

// herdr restores its session when the server starts again after a crash:
// the run's pane id exists again, but it is a new shell without the agent.
func TestPollEndsARunLostWhenARestartedHerdrServerRestoredItsPane(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, working)
	run := e.start(deps())
	if got := e.poll(run.ID); got != store.StatusRunning {
		t.Fatalf("status = %s, want running", got)
	}
	e.herdr.restartServer()

	e.poll(run.ID)
	got := e.get(run.ID)
	if got.Status != store.StatusLost || got.ExitReason != "pane replaced: herdr server restarted" || got.EndedAt.IsZero() {
		t.Errorf("run = %s %q ended %v, want lost %q", got.Status, got.ExitReason, got.EndedAt, "pane replaced: herdr server restarted")
	}
	if e.runner.Active(run.ID) {
		t.Errorf("lost run is still tracked")
	}
	notes := e.notifications()
	if len(notes) != 1 || notes[0].event != config.EventLost {
		t.Errorf("notifications = %+v, want one lost", notes)
	}
	if n := e.herdr.count("herdr", "pane", "read"); n != 0 {
		t.Errorf("%d pane reads of a pane that is not the run's", n)
	}
}

// The daemon was down while the herdr server restarted: recovery must not
// adopt the restored pane either.
func TestRecoverMarksARunLostWhenARestartedHerdrServerRestoredItsPane(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working)
	run := e.start(deps())
	e.poll(run.ID)
	e.herdr.restartServer()

	e.restart(deps())
	got := e.get(run.ID)
	if got.Status != store.StatusLost || got.ExitReason != "pane replaced: herdr server restarted" || got.EndedAt.IsZero() {
		t.Errorf("run = %s %q ended %v, want lost %q", got.Status, got.ExitReason, got.EndedAt, "pane replaced: herdr server restarted")
	}
}

// A run stored before runs.herdr_terminal existed has no terminal id to
// compare: it is tracked by its pane id, as before.
func TestRecoverTracksARunStoredWithoutATerminalID(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, working)
	run := e.start(deps())
	e.poll(run.ID)
	old := e.get(run.ID)
	old.HerdrTerminal = ""
	if err := e.store.UpdateRun(e.ctx, old); err != nil {
		t.Fatal(err)
	}

	e.restart(deps())
	if got := e.poll(run.ID); got != store.StatusRunning || !e.runner.Active(run.ID) {
		t.Errorf("status = %s, want running and tracked", got)
	}
}
