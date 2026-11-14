package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/dispatch"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// dropNow is the time the dispatcher's clock gives in these tests.
var dropNow = t0.Add(30 * time.Minute)

// dropHarness is a harness on st with a fixed clock that collects the rows
// given to OnDropped.
type dropHarness struct {
	harness
	mu      sync.Mutex
	dropped []store.Run
}

func newDropHarness(t *testing.T, cfg config.Config, st *store.Store, recovered ...store.Run) *dropHarness {
	t.Helper()
	h := &dropHarness{}
	logs := &bytes.Buffer{}
	runs := newFakeRunner()
	d := dispatch.New(dispatch.Options{
		Runs: runs, Store: st, Config: cfg,
		Log: slog.New(slog.NewTextHandler(&syncWriter{w: logs}, nil)),
		Now: func() time.Time { return dropNow },
		OnDropped: func(r store.Run) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.dropped = append(h.dropped, r)
		},
	})
	runs.d = d
	for _, r := range recovered {
		runs.recover(r.ID, r.TaskID, r.Target)
	}
	h.harness = harness{d: d, runs: runs, logs: logs}
	return h
}

func (h *dropHarness) onDropped() []store.Run {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]store.Run(nil), h.dropped...)
}

// history returns the task's rows in table runs, newest first.
func history(t *testing.T, st *store.Store, taskID string) []store.Run {
	t.Helper()
	runs, err := st.ListRuns(context.Background(), taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func storedFires(t *testing.T, st *store.Store) []store.QueuedFire {
	t.Helper()
	fires, err := st.ListQueuedFires(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return fires
}

// assertDropped checks that the task's history is exactly one dropped row
// for the given fire, and returns it.
func assertDropped(t *testing.T, st *store.Store, taskID, target, trigger string, scheduledAt time.Time, reason string) store.Run {
	t.Helper()
	rows := history(t, st, taskID)
	if len(rows) != 1 {
		t.Fatalf("history of %s = %+v, want one dropped row", taskID, rows)
	}
	r := rows[0]
	if r.Status != store.StatusDropped || r.TaskID != taskID || r.Target != target || r.Trigger != trigger ||
		!r.ScheduledAt.Equal(scheduledAt) {
		t.Fatalf("history row of %s = %+v, want a dropped %s fire on %s planned at %v", taskID, r, trigger, target, scheduledAt)
	}
	if !r.StartedAt.Equal(dropNow) || !r.EndedAt.Equal(dropNow) {
		t.Fatalf("history row of %s starts %v and ends %v, want both at the drop (%v)", taskID, r.StartedAt, r.EndedAt, dropNow)
	}
	if !strings.Contains(r.ExitReason, reason) {
		t.Fatalf("history row of %s has reason %q, want it to contain %q", taskID, r.ExitReason, reason)
	}
	if r.HerdrTab != "" || r.HerdrPane != "" {
		t.Fatalf("history row of %s has herdr ids: %+v", taskID, r)
	}
	return r
}

// The fires DropTargets discards, on the max_parallel queue and as a
// pending overlap fire, show up in history with the reason.
func TestDropTargetsRecordsTheDroppedFiresInHistory(t *testing.T) {
	st := openStore(t)
	cfg := config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 1}, "local": {MaxParallel: 4}}}
	h := newDropHarness(t, cfg, st)
	submit(t, h.harness, task("a", "vps", "parallel"), store.TriggerSchedule, t0, false)
	if !submit(t, h.harness, task("b", "vps", "parallel"), store.TriggerSchedule, t0.Add(time.Minute), false).Queued {
		t.Fatal("want b queued behind a")
	}
	submit(t, h.harness, task("c", "local", "queue"), store.TriggerSchedule, t0, false)
	if !submit(t, h.harness, task("c", "local", "queue"), store.TriggerManual, time.Time{}, false).Queued {
		t.Fatal("want the second fire of c pending")
	}

	h.d.DropTargets(context.Background(), []string{"vps", "local"})

	b := assertDropped(t, st, "b", "vps", store.TriggerSchedule, t0.Add(time.Minute), "target vps removed from config")
	c := assertDropped(t, st, "c", "local", store.TriggerManual, time.Time{}, "target local removed from config")
	if fires := storedFires(t, st); len(fires) != 0 {
		t.Fatalf("stored fires = %+v, want none", fires)
	}
	got := h.onDropped()
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != c.ID {
		t.Fatalf("OnDropped got %+v, want the rows of b and c", got)
	}
}

// A fire that was counted for launch just before reload removed its target
// is dropped inside the launch. It gets its row too.
func TestFireDroppedAtLaunchBecauseItsTargetIsGoneIsRecorded(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	h.runs.duringSnapshot = func() { h.d.DropTargets(context.Background(), []string{"local"}) }
	out := submit(t, h.harness, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	if out.Queued || out.Skipped || out.Run.Status != store.StatusDropped || out.Run.ID == "" {
		t.Fatalf("Submit = %+v, want the dropped history row", out)
	}
	if n := len(h.runs.startCalls()); n != 0 {
		t.Fatalf("Start called %d times, want 0", n)
	}
	assertDropped(t, st, "a", "local", store.TriggerSchedule, t0, "target local removed from config")
}

// A task deleted while its fire waits: the daemon refuses the start and no
// run exists. The fire left the queue, so history must say what happened.
func TestQueuedFireWhoseStartIsRefusedIsRecorded(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	first := submit(t, h.harness, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	if !submit(t, h.harness, task("b", "local", "parallel"), store.TriggerSchedule, t0.Add(time.Minute), false).Queued {
		t.Fatal("want b queued behind a")
	}
	h.runs.refuse = map[string]error{"b": errors.New("task b is gone")}
	h.runs.end(t, first.Run.ID)

	assertDropped(t, st, "b", "local", store.TriggerSchedule, t0.Add(time.Minute), "task b is gone")
	if fires := storedFires(t, st); len(fires) != 0 {
		t.Fatalf("stored fires = %+v, want none", fires)
	}
}

// Start can also decline a fire without an error (the daemon does for a
// catch-up whose target was replaced). A queued fire it declines is gone
// from the queue all the same, so it is recorded.
func TestQueuedFireThatStartDeclinesWithoutErrorIsRecorded(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	first := submit(t, h.harness, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	submit(t, h.harness, task("b", "local", "parallel"), store.TriggerCatchUp, t0, false)
	h.runs.decline = map[string]bool{"b": true}
	h.runs.end(t, first.Run.ID)

	assertDropped(t, st, "b", "local", store.TriggerCatchUp, t0, "not started")
	if got := h.onDropped(); len(got) != 1 {
		t.Fatalf("OnDropped got %+v, want the one row of b", got)
	}
}

// A fire that starts leaves no dropped row behind.
func TestQueuedFireThatStartsIsNotRecordedAsDropped(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	first := submit(t, h.harness, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	submit(t, h.harness, task("b", "local", "parallel"), store.TriggerSchedule, t0, false)
	h.runs.end(t, first.Run.ID)
	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times, want 2", n)
	}
	if rows := history(t, st, "b"); len(rows) != 0 {
		t.Fatalf("history of b = %+v, want nothing written by the dispatcher", rows)
	}
	if got := h.onDropped(); len(got) != 0 {
		t.Fatalf("OnDropped got %+v, want nothing", got)
	}
}

// The three ways Restore drops a stored fire each leave a history row.
func TestRestoreRecordsTheFiresItDrops(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	old := t0.Add(-3 * time.Hour)
	for _, f := range []store.QueuedFire{
		{TaskID: "gone", Target: "vps", Trigger: store.TriggerSchedule, ScheduledAt: t0},
		{TaskID: "late", Target: "local", Trigger: store.TriggerCatchUp, ScheduledAt: old},
		{TaskID: "twice", Target: "local", Trigger: store.TriggerSchedule, ScheduledAt: t0, Pending: true},
		{TaskID: "twice", Target: "local", Trigger: store.TriggerManual, Pending: true},
	} {
		if _, err := st.AddQueuedFire(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	cfg := oneSlot()
	cfg.CatchUpWindow = config.Duration{Duration: time.Hour}
	// twice still has its run, so its first pending fire keeps waiting.
	h := newDropHarness(t, cfg, st, store.Run{ID: "old", TaskID: "twice", Target: "local"})
	if err := h.d.Restore(ctx, []config.Task{task("late", "local", "parallel"), task("twice", "local", "queue")}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	assertDropped(t, st, "gone", "vps", store.TriggerSchedule, t0, "task removed or invalid")
	assertDropped(t, st, "late", "local", store.TriggerCatchUp, old, "older than catch_up_window (1h0m0s)")
	assertDropped(t, st, "twice", "local", store.TriggerManual, time.Time{}, "already has a pending fire")
	fires := storedFires(t, st)
	if len(fires) != 1 || fires[0].TaskID != "twice" || fires[0].Trigger != store.TriggerSchedule {
		t.Fatalf("stored fires = %+v, want only the first pending fire of twice", fires)
	}
	if got := h.onDropped(); len(got) != 3 {
		t.Fatalf("OnDropped got %d rows, want 3: %+v", len(got), got)
	}
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// A fire that starts to wait is stored even when the caller's context is
// already cancelled: the fire waits in memory, so a restart must find it.
func TestFireQueuedUnderACancelledContextIsStillStored(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	submit(t, h.harness, task("a", "local", "queue"), store.TriggerSchedule, t0, false)
	out, err := h.d.Submit(cancelled(), task("b", "local", "parallel"), store.TriggerManual, time.Time{}, false)
	if err != nil || !out.Queued {
		t.Fatalf("Submit b = %+v, %v; want Queued", out, err)
	}
	out, err = h.d.Submit(cancelled(), task("a", "local", "queue"), store.TriggerSchedule, t0.Add(time.Hour), false)
	if err != nil || !out.Queued {
		t.Fatalf("second Submit a = %+v, %v; want Queued", out, err)
	}
	fires := storedFires(t, st)
	if len(fires) != 2 || fires[0].TaskID != "b" || fires[0].Pending || fires[1].TaskID != "a" || !fires[1].Pending {
		t.Fatalf("stored fires = %+v, want b in the queue and a pending", fires)
	}
}

// A drop is written even when the caller's context is cancelled: a row left
// in queued_fires would bring the dropped fire back after a restart.
func TestDropUnderACancelledContextIsStillWritten(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	submit(t, h.harness, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	submit(t, h.harness, task("b", "local", "parallel"), store.TriggerSchedule, t0, false)

	h.d.DropTargets(cancelled(), []string{"local"})

	assertDropped(t, st, "b", "local", store.TriggerSchedule, t0, "target local removed from config")
	if fires := storedFires(t, st); len(fires) != 0 {
		t.Fatalf("stored fires = %+v, want none", fires)
	}
}

// Releasing a pending fire follows the caller's context. Cancelled, the
// write does not happen; the fire keeps its pending row and is not lost: it
// still starts from memory, and a restart would release the row again.
func TestReleaseUnderACancelledContextSkipsTheWriteAndKeepsTheFire(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	a, b := task("a", "local", "queue"), task("b", "local", "parallel")
	first := submit(t, h.harness, a, store.TriggerSchedule, t0, false)
	submit(t, h.harness, b, store.TriggerSchedule, t0, false)                // waits for the slot
	submit(t, h.harness, a, store.TriggerSchedule, t0.Add(time.Hour), false) // pending behind a's run
	before := storedFires(t, st)

	// a's run ends; the runner's context is already cancelled.
	h.d.RunEnded(cancelled(), h.runs.detach(t, first.Run.ID))

	starts := h.runs.startCalls()
	if len(starts) != 2 || starts[1].TaskID != "b" {
		t.Fatalf("starts = %+v, want b started in the freed slot", starts)
	}
	fires := storedFires(t, st)
	if len(fires) != 1 || fires[0].ID != before[1].ID || !fires[0].Pending {
		t.Fatalf("stored fires = %+v, want a's fire unchanged (%+v): the release was cancelled", fires, before[1])
	}
	// In memory the fire did move to the queue: it starts when b ends.
	h.runs.end(t, starts[1].ID)
	starts = h.runs.startCalls()
	if len(starts) != 3 || starts[2].TaskID != "a" || !starts[2].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want a's released fire started last", starts)
	}
	if fires := storedFires(t, st); len(fires) != 0 {
		t.Fatalf("stored fires = %+v, want none once the fire started", fires)
	}
}

// After Stop the dispatcher starts no waiting fire. The fire stays in the
// store, and the next daemon restores and starts it.
func TestStopKeepsWaitingFiresInTheStoreForTheNextDaemon(t *testing.T) {
	st := openStore(t)
	a, b := task("a", "local", "parallel"), task("b", "local", "parallel")
	h := newDropHarness(t, oneSlot(), st)
	first := submit(t, h.harness, a, store.TriggerSchedule, t0, false)
	submit(t, h.harness, b, store.TriggerSchedule, t0.Add(time.Minute), false)

	h.d.Stop()
	h.runs.end(t, first.Run.ID)
	h.d.Resume(context.Background())
	h.d.Drain(context.Background())

	if n := len(h.runs.startCalls()); n != 1 {
		t.Fatalf("Start called %d times after Stop, want only the first run", n)
	}
	fires := storedFires(t, st)
	if len(fires) != 1 || fires[0].TaskID != "b" {
		t.Fatalf("stored fires = %+v, want b kept", fires)
	}
	if rows := history(t, st, "b"); len(rows) != 0 {
		t.Fatalf("history of b = %+v, want no dropped row: the fire is kept", rows)
	}

	next := newDropHarness(t, oneSlot(), st)
	restore(t, next.harness, a, b)
	starts := next.runs.startCalls()
	if len(starts) != 1 || starts[0].TaskID != "b" || !starts[0].ScheduledAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("next daemon started %+v, want b's kept fire", starts)
	}
}

// The runner records a dropped row itself when the target is already gone
// by the time a queued fire starts. That fire did not run, so task_state
// stays unchanged and OnDropped is not called a second time.
func TestQueuedFireDroppedByTheRunnerIsNotRecordedAsFired(t *testing.T) {
	st := openStore(t)
	h := newDropHarness(t, oneSlot(), st)
	first := submit(t, h.harness, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	if !submit(t, h.harness, task("b", "local", "parallel"), store.TriggerSchedule, t0.Add(time.Minute), false).Queued {
		t.Fatal("want b queued behind a")
	}
	h.runs.dropLaunch = true
	h.runs.end(t, first.Run.ID)
	if _, ok, err := st.GetTaskState(context.Background(), "b"); err != nil || ok {
		t.Fatalf("task state of b: ok=%v err=%v, want none (a dropped fire did not run)", ok, err)
	}
	if got := h.onDropped(); len(got) != 0 {
		t.Fatalf("OnDropped = %+v, want none: the runner stored the row", got)
	}
}
