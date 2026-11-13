package dispatch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/dispatch"
	"github.com/WillemDeGroef/herdrcron/internal/scheduler"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

var _ scheduler.Starter = (*dispatch.Dispatcher)(nil)

// fakeRunner records Start calls and keeps an in-memory set of active runs.
// It calls back into the dispatcher like runner.Runner does.
type fakeRunner struct {
	mu     sync.Mutex
	d      *dispatch.Dispatcher
	n      int
	starts []store.Run
	// startErrs holds ctx.Err() of each Start call, in order.
	startErrs []error
	active    map[string]store.Run
	// failLaunch makes Start return an ended, failed run, after calling
	// RunEnded from inside Start.
	failLaunch bool
	// duringStart, when set, runs inside the next Start before it returns
	// (and before a failed launch calls RunEnded).
	duringStart func()
	// duringSnapshot, when set, runs inside the next ActiveRuns after the
	// list is taken, so the dispatcher gets a list that is already stale.
	duringSnapshot func()
}

func newFakeRunner() *fakeRunner { return &fakeRunner{active: map[string]store.Run{}} }

func (f *fakeRunner) Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error) {
	f.mu.Lock()
	f.n++
	r := store.Run{
		ID: fmt.Sprintf("r%d", f.n), TaskID: t.ID, Target: t.Target, Trigger: trigger,
		ScheduledAt: scheduledAt, StartedAt: time.Now(), Status: store.StatusRunning,
	}
	f.starts = append(f.starts, r)
	f.startErrs = append(f.startErrs, ctx.Err())
	fail, hook := f.failLaunch, f.duringStart
	f.duringStart = nil
	if hook != nil {
		f.mu.Unlock()
		hook()
		f.mu.Lock()
	}
	if fail {
		r.Status, r.EndedAt = store.StatusFailed, time.Now()
		f.mu.Unlock()
		f.d.RunEnded(ctx, r)
		return r, nil
	}
	f.active[r.ID] = r
	f.mu.Unlock()
	return r, nil
}

func (f *fakeRunner) ActiveRuns() []store.Run {
	f.mu.Lock()
	out := make([]store.Run, 0, len(f.active))
	for _, r := range f.active {
		out = append(out, r)
	}
	hook := f.duringSnapshot
	f.duringSnapshot = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out
}

// recover adds a run the dispatcher never started, as after a daemon restart.
func (f *fakeRunner) recover(id, taskID, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[id] = store.Run{ID: id, TaskID: taskID, Target: target, Status: store.StatusRunning}
}

// end ends an active run and tells the dispatcher, like the runner does.
func (f *fakeRunner) end(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	r, ok := f.active[id]
	delete(f.active, id)
	f.mu.Unlock()
	if !ok {
		t.Fatalf("end %s: not active", id)
	}
	r.Status, r.EndedAt = store.StatusDone, time.Now()
	f.d.RunEnded(context.Background(), r)
}

// detach drops an active run from ActiveRuns without telling the dispatcher,
// like the runner does while it cleans up before calling RunEnded.
func (f *fakeRunner) detach(t *testing.T, id string) store.Run {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.active[id]
	if !ok {
		t.Fatalf("detach %s: not active", id)
	}
	delete(f.active, id)
	r.Status, r.EndedAt = store.StatusDone, time.Now()
	return r
}

func (f *fakeRunner) startCtxErrs() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.startErrs...)
}

func (f *fakeRunner) startCalls() []store.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Run(nil), f.starts...)
}

type harness struct {
	d    *dispatch.Dispatcher
	runs *fakeRunner
	logs *bytes.Buffer
}

func newHarness(t *testing.T, cfg config.Config, st *store.Store) harness {
	t.Helper()
	logs := &bytes.Buffer{}
	runs := newFakeRunner()
	d := dispatch.New(dispatch.Options{
		Runs: runs, Store: st, Config: cfg,
		Log: slog.New(slog.NewTextHandler(&syncWriter{w: logs}, nil)),
	})
	runs.d = d
	return harness{d: d, runs: runs, logs: logs}
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func task(id, target, overlap string) config.Task {
	return config.Task{ID: id, Target: target, Overlap: overlap, Enabled: true}
}

var t0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func submit(t *testing.T, h harness, tk config.Task, trigger string, at time.Time, force bool) dispatch.Outcome {
	t.Helper()
	out, err := h.d.Submit(context.Background(), tk, trigger, at, force)
	if err != nil {
		t.Fatalf("Submit %s %s: %v", tk.ID, trigger, err)
	}
	return out
}

func TestSkipScheduledFireWhileActiveIsSkippedAndLoggedOnce(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "skip")
	if out := submit(t, h, tk, store.TriggerSchedule, t0, false); out.Run.ID == "" {
		t.Fatalf("first fire did not start a run: %+v", out)
	}
	h.logs.Reset()

	out := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)

	if !out.Skipped || out.Queued || out.Run.ID != "" {
		t.Fatalf("outcome = %+v, want Skipped only", out)
	}
	if n := len(h.runs.startCalls()); n != 1 {
		t.Fatalf("Start called %d times, want 1", n)
	}
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one log line, got %q", h.logs.String())
	}
	for _, want := range []string{"task_id=nightly", "target=local", "scheduled_at=2026-03-01T10:00:00"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %q lacks %q", lines[0], want)
		}
	}
}

func TestSkipManualFireWhileActiveReturnsErrActive(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "skip")
	submit(t, h, tk, store.TriggerSchedule, t0, false)

	out, err := h.d.Submit(context.Background(), tk, store.TriggerManual, time.Time{}, false)

	if !errors.Is(err, dispatch.ErrActive) {
		t.Fatalf("err = %v, want ErrActive", err)
	}
	if out.Run.ID != "" || out.Queued {
		t.Fatalf("outcome = %+v, want nothing started or queued", out)
	}
	if n := len(h.runs.startCalls()); n != 1 {
		t.Fatalf("Start called %d times, want 1", n)
	}
}

func TestSkipManualFireForcedStartsAParallelRun(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "skip")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)

	out := submit(t, h, tk, store.TriggerManual, time.Time{}, true)

	if out.Run.ID == "" || out.Run.ID == first.Run.ID || out.Run.Trigger != store.TriggerManual {
		t.Fatalf("outcome = %+v, want a second, manual run", out)
	}
	if n := len(h.runs.ActiveRuns()); n != 2 {
		t.Fatalf("%d active runs, want 2", n)
	}
}

func TestQueueKeepsOnePendingFireAndStartsItWhenTheRunEnds(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)

	second := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)
	third := submit(t, h, tk, store.TriggerCatchUp, t0.Add(2*time.Hour), false)

	if !second.Queued || second.Run.ID != "" {
		t.Fatalf("second fire = %+v, want Queued", second)
	}
	if !third.Skipped || third.Queued || third.Run.ID != "" {
		t.Fatalf("third fire = %+v, want dropped (a fire is already pending)", third)
	}
	if n := len(h.runs.startCalls()); n != 1 {
		t.Fatalf("Start called %d times before the run ended, want 1", n)
	}

	h.runs.end(t, first.Run.ID)

	starts := h.runs.startCalls()
	if len(starts) != 2 {
		t.Fatalf("Start called %d times, want 2", len(starts))
	}
	if got := starts[1]; got.Trigger != store.TriggerSchedule || !got.ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("pending fire started as %s at %s, want schedule at %s", got.Trigger, got.ScheduledAt, t0.Add(time.Hour))
	}

	h.runs.end(t, starts[1].ID)
	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times after the pending run ended, want 2 (the dropped fire stays dropped)", n)
	}
}

func TestParallelStartsASecondRun(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "parallel")
	submit(t, h, tk, store.TriggerSchedule, t0, false)

	out := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)

	if out.Run.ID == "" {
		t.Fatalf("outcome = %+v, want a started run", out)
	}
	if n := len(h.runs.ActiveRuns()); n != 2 {
		t.Fatalf("%d active runs, want 2", n)
	}
}

func twoTargets() config.Config {
	return config.Config{Targets: map[string]config.Target{
		"vps": {MaxParallel: 2}, "local": {MaxParallel: 4},
	}}
}

func TestMaxParallelQueuesFiresFIFOAcrossTasksOfATarget(t *testing.T) {
	h := newHarness(t, twoTargets(), nil)
	a := submit(t, h, task("a", "vps", "parallel"), store.TriggerSchedule, t0, false)
	submit(t, h, task("b", "vps", "parallel"), store.TriggerSchedule, t0, false)

	c := submit(t, h, task("c", "vps", "parallel"), store.TriggerManual, time.Time{}, false)
	dd := submit(t, h, task("d", "vps", "skip"), store.TriggerSchedule, t0.Add(time.Minute), false)
	other := submit(t, h, task("e", "local", "parallel"), store.TriggerSchedule, t0, false)

	if !c.Queued || !dd.Queued {
		t.Fatalf("fires above max_parallel: c=%+v d=%+v, want both Queued", c, dd)
	}
	if other.Run.ID == "" {
		t.Fatalf("fire on another target = %+v, want started", other)
	}
	if n := len(h.runs.startCalls()); n != 3 {
		t.Fatalf("Start called %d times, want 3", n)
	}

	h.runs.end(t, a.Run.ID)

	starts := h.runs.startCalls()
	if len(starts) != 4 || starts[3].TaskID != "c" || starts[3].Trigger != store.TriggerManual {
		t.Fatalf("after one run ended, starts = %+v, want c (first in queue) started", starts)
	}

	h.runs.end(t, other.Run.ID) // another target: frees nothing on vps
	if n := len(h.runs.startCalls()); n != 4 {
		t.Fatalf("Start called %d times after a local run ended, want 4", n)
	}

	h.runs.end(t, starts[3].ID)
	starts = h.runs.startCalls()
	if len(starts) != 5 || starts[4].TaskID != "d" || !starts[4].ScheduledAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("starts = %+v, want d started next with its scheduled_at", starts)
	}
}

func TestQueuedTaskIsBusyForOverlap(t *testing.T) {
	h := newHarness(t, config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 1}}}, nil)
	submit(t, h, task("a", "vps", "skip"), store.TriggerSchedule, t0, false)
	tb := task("b", "vps", "skip")
	if out := submit(t, h, tb, store.TriggerSchedule, t0, false); !out.Queued {
		t.Fatalf("b first fire = %+v, want Queued", out)
	}

	out := submit(t, h, tb, store.TriggerSchedule, t0.Add(time.Hour), false)

	if !out.Skipped {
		t.Fatalf("b second fire = %+v, want Skipped (b waits in the queue)", out)
	}
}

func TestRecoveredRunsCountForOverlapAndCapacity(t *testing.T) {
	h := newHarness(t, twoTargets(), nil)
	h.runs.recover("old1", "a", "vps")
	h.runs.recover("old2", "x", "vps")

	skipped := submit(t, h, task("a", "vps", "skip"), store.TriggerSchedule, t0, false)
	queued := submit(t, h, task("b", "vps", "parallel"), store.TriggerSchedule, t0, false)

	if !skipped.Skipped {
		t.Fatalf("a = %+v, want Skipped: its recovered run is active", skipped)
	}
	if !queued.Queued {
		t.Fatalf("b = %+v, want Queued: two recovered runs fill vps", queued)
	}

	h.runs.end(t, "old2")

	if starts := h.runs.startCalls(); len(starts) != 1 || starts[0].TaskID != "b" {
		t.Fatalf("starts = %+v, want b started when a recovered run ended", starts)
	}
}

func TestFailedLaunchDoesNotDeadlockAndFreesItsSlot(t *testing.T) {
	h := newHarness(t, config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 1}}}, nil)
	h.runs.failLaunch = true

	done := make(chan dispatch.Outcome)
	go func() {
		out, _ := h.d.Submit(context.Background(), task("a", "vps", "queue"), store.TriggerSchedule, t0, false)
		done <- out
	}()
	var out dispatch.Outcome
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Submit deadlocked on a failed launch")
	}
	if out.Run.Status != store.StatusFailed {
		t.Fatalf("outcome = %+v, want the failed run", out)
	}

	h.runs.failLaunch = false
	next := submit(t, h, task("b", "vps", "skip"), store.TriggerSchedule, t0, false)

	if next.Run.ID == "" {
		t.Fatalf("next fire = %+v, want started: the failed launch freed its slot", next)
	}
}

func TestFireQueuedDuringAFailedLaunchStartsAfterIt(t *testing.T) {
	h := newHarness(t, config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 1}}}, nil)
	h.runs.failLaunch = true
	var queued dispatch.Outcome
	h.runs.duringStart = func() {
		h.runs.mu.Lock()
		h.runs.failLaunch = false
		h.runs.mu.Unlock()
		queued = submit(t, h, task("b", "vps", "skip"), store.TriggerSchedule, t0, false)
	}

	submit(t, h, task("a", "vps", "skip"), store.TriggerSchedule, t0, false)

	if !queued.Queued {
		t.Fatalf("b fired during a's launch = %+v, want Queued", queued)
	}
	if starts := h.runs.startCalls(); len(starts) != 2 || starts[1].TaskID != "b" {
		t.Fatalf("starts = %+v, want b started once a's failed launch freed the slot", starts)
	}
}

func TestRunEndingWhileActiveRunsIsReadDoesNotStrandFires(t *testing.T) {
	h := newHarness(t, config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 1}}}, nil)
	tk := task("a", "vps", "queue")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)
	h.runs.duringSnapshot = func() { h.runs.end(t, first.Run.ID) }

	submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)

	if starts := h.runs.startCalls(); len(starts) != 2 {
		t.Fatalf("Start called %d times, want 2: the fire must not wait for a run that already ended", len(starts))
	}
}

func TestDeferredScheduledStartRecordsTheFire(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := newHarness(t, config.Config{}, st)
	tk := task("nightly", "local", "queue")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)
	submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)

	h.runs.end(t, first.Run.ID)

	starts := h.runs.startCalls()
	if len(starts) != 2 {
		t.Fatalf("Start called %d times, want 2", len(starts))
	}
	ts, ok, err := st.GetTaskState(context.Background(), "nightly")
	if err != nil || !ok {
		t.Fatalf("task state: ok=%v err=%v", ok, err)
	}
	if ts.LastRunID != starts[1].ID || !ts.LastFireAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("task state = %+v, want last run %s fired at %s", ts, starts[1].ID, t0.Add(time.Hour))
	}
}

func TestDeferredManualStartRecordsNothing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := newHarness(t, config.Config{}, st)
	tk := task("nightly", "local", "queue")
	first := submit(t, h, tk, store.TriggerManual, time.Time{}, false)
	submit(t, h, tk, store.TriggerManual, time.Time{}, false)

	h.runs.end(t, first.Run.ID)

	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times, want 2", n)
	}
	if _, ok, err := st.GetTaskState(context.Background(), "nightly"); err != nil || ok {
		t.Fatalf("task state stored for manual runs: ok=%v err=%v", ok, err)
	}
}

func TestConcurrentFiresNeverExceedMaxParallel(t *testing.T) {
	h := newHarness(t, config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 3}}}, nil)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.d.Submit(context.Background(), task(fmt.Sprintf("t%d", i), "vps", "parallel"), store.TriggerSchedule, t0, false)
		}()
	}
	wg.Wait()
	if n := len(h.runs.ActiveRuns()); n != 3 {
		t.Fatalf("%d active runs, want 3", n)
	}
	for len(h.runs.ActiveRuns()) > 0 {
		h.runs.end(t, h.runs.ActiveRuns()[0].ID)
		if n := len(h.runs.ActiveRuns()); n > 3 {
			t.Fatalf("%d active runs, want at most 3", n)
		}
	}
	if n := len(h.runs.startCalls()); n != 20 {
		t.Fatalf("Start called %d times, want 20", n)
	}
}

func TestQueueFireWhileEndingRunIsCleanedUpDoesNotStartASecondRun(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	a := submit(t, h, tk, store.TriggerSchedule, t0, false)
	if out := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false); !out.Queued {
		t.Fatalf("second fire = %+v, want Queued", out)
	}
	ended := h.runs.detach(t, a.Run.ID) // gone from ActiveRuns, RunEnded not yet called

	out := submit(t, h, tk, store.TriggerSchedule, t0.Add(2*time.Hour), false)

	if !out.Skipped || out.Run.ID != "" {
		t.Fatalf("third fire = %+v, want dropped: a fire is already pending", out)
	}
	if n := len(h.runs.startCalls()); n != 1 {
		t.Fatalf("Start called %d times before RunEnded, want 1", n)
	}

	h.d.RunEnded(context.Background(), ended)

	starts := h.runs.startCalls()
	if len(starts) != 2 || !starts[1].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the pending fire started once", starts)
	}
	if n := len(h.runs.ActiveRuns()); n != 1 {
		t.Fatalf("%d active runs, want 1", n)
	}
}

func TestQueuedFiresDoNotInheritTheSubmittersCancellation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := newHarness(t, config.Config{Targets: map[string]config.Target{"vps": {MaxParallel: 1}}}, st)
	h.runs.failLaunch = true
	h.runs.duringStart = func() {
		h.runs.mu.Lock()
		h.runs.failLaunch = false
		h.runs.mu.Unlock()
		// Fired by the scheduler while the manual run launches: it waits.
		if out := submit(t, h, task("b", "vps", "skip"), store.TriggerSchedule, t0, false); !out.Queued {
			t.Errorf("b = %+v, want Queued", out)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run_now client has gone away

	_, _ = h.d.Submit(ctx, task("a", "vps", "skip"), store.TriggerManual, time.Time{}, false)

	starts, errs := h.runs.startCalls(), h.runs.startCtxErrs()
	if len(starts) != 2 || starts[1].TaskID != "b" {
		t.Fatalf("starts = %+v, want b started after a's failed launch", starts)
	}
	if errs[1] != nil {
		t.Fatalf("b started with a cancelled context: %v", errs[1])
	}
	ts, ok, err := st.GetTaskState(context.Background(), "b")
	if err != nil || !ok || ts.LastRunID != starts[1].ID {
		t.Fatalf("task state for b = %+v ok=%v err=%v, want its fire recorded", ts, ok, err)
	}
}

// A forced parallel run is overlap parallel on top of a queue task. The
// one pending fire waits until neither that run nor the original is active.
func TestForceParallelEndDoesNotStartTheQueuedFire(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)
	if out := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false); !out.Queued {
		t.Fatalf("second fire = %+v, want Queued", out)
	}
	forced := submit(t, h, tk, store.TriggerManual, time.Time{}, true)

	h.runs.end(t, forced.Run.ID)

	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times, want 2: ending the forced run must leave the queued fire waiting", n)
	}
	third := submit(t, h, tk, store.TriggerSchedule, t0.Add(2*time.Hour), false)
	if !third.Skipped || third.Run.ID != "" {
		t.Fatalf("third fire = %+v, want dropped: the one pending slot is still held", third)
	}

	h.runs.end(t, first.Run.ID)

	starts := h.runs.startCalls()
	if len(starts) != 3 || !starts[2].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the original pending fire started once the task is idle", starts)
	}
}

func TestOriginalEndWhileForceParallelIsActiveDoesNotStartTheQueuedFire(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)
	if out := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false); !out.Queued {
		t.Fatalf("second fire = %+v, want Queued", out)
	}
	forced := submit(t, h, tk, store.TriggerManual, time.Time{}, true)

	h.runs.end(t, first.Run.ID)

	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times, want 2: the queued fire waits for the forced run too", n)
	}
	third := submit(t, h, tk, store.TriggerSchedule, t0.Add(2*time.Hour), false)
	if !third.Skipped {
		t.Fatalf("third fire = %+v, want dropped while the forced run is active", third)
	}

	h.runs.end(t, forced.Run.ID)

	starts := h.runs.startCalls()
	if len(starts) != 3 || !starts[2].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the original pending fire once the forced run ends", starts)
	}
}

func TestFailedForceParallelDoesNotStartTheQueuedFire(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)
	if out := submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false); !out.Queued {
		t.Fatalf("second fire = %+v, want Queued", out)
	}
	h.runs.failLaunch = true

	out := submit(t, h, tk, store.TriggerManual, time.Time{}, true)

	if out.Run.Status != store.StatusFailed {
		t.Fatalf("forced run = %+v, want failed", out.Run)
	}
	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times, want 2: the failed forced run must not start the queued fire", n)
	}

	h.runs.failLaunch = false
	h.runs.end(t, first.Run.ID)
	starts := h.runs.startCalls()
	if len(starts) != 3 || !starts[2].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the pending fire once the original run ends", starts)
	}
}

func TestQueueFireDuringLaunchStaysPendingUntilThatRunEnds(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	var queued dispatch.Outcome
	h.runs.duringStart = func() {
		queued = submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)
	}

	submit(t, h, tk, store.TriggerSchedule, t0, false)

	if !queued.Queued || queued.Run.ID != "" {
		t.Fatalf("fire during launch = %+v, want Queued", queued)
	}
	if n := len(h.runs.startCalls()); n != 1 {
		t.Fatalf("Start called %d times, want 1: the fire during launch stays pending", n)
	}

	h.runs.end(t, h.runs.startCalls()[0].ID)
	starts := h.runs.startCalls()
	if len(starts) != 2 || !starts[1].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the pending fire after the launching run ends", starts)
	}
}

func TestQueueFireDuringAFailedLaunchStartsOnceTheTaskIsIdle(t *testing.T) {
	h := newHarness(t, config.Config{}, nil)
	tk := task("nightly", "local", "queue")
	h.runs.failLaunch = true
	var queued dispatch.Outcome
	h.runs.duringStart = func() {
		h.runs.mu.Lock()
		h.runs.failLaunch = false
		h.runs.mu.Unlock()
		queued = submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)
	}

	out := submit(t, h, tk, store.TriggerSchedule, t0, false)

	if out.Run.Status != store.StatusFailed {
		t.Fatalf("first run = %+v, want failed", out.Run)
	}
	if !queued.Queued {
		t.Fatalf("fire during launch = %+v, want Queued", queued)
	}
	starts := h.runs.startCalls()
	if len(starts) != 2 || !starts[1].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the pending fire started once the failed launch left the task idle", starts)
	}
}

// Fires waiting on a target reload removed are discarded. Ending the runs
// that were already active must not start them, on the max_parallel queue
// or as a pending overlap fire.
func TestDropTargetsDiscardsWaitingFires(t *testing.T) {
	cfg := config.Config{Targets: map[string]config.Target{
		"vps":   {MaxParallel: 1},
		"local": {MaxParallel: 4},
	}}
	h := newHarness(t, cfg, nil)
	first := submit(t, h, task("a", "vps", "skip"), store.TriggerSchedule, t0, false)
	if first.Run.ID == "" {
		t.Fatal("first vps fire did not start")
	}
	if out := submit(t, h, task("b", "vps", "parallel"), store.TriggerSchedule, t0, false); !out.Queued {
		t.Fatalf("vps fire above max_parallel = %+v, want Queued", out)
	}
	overlap := submit(t, h, task("c", "local", "queue"), store.TriggerSchedule, t0, false)
	if overlap.Run.ID == "" {
		t.Fatal("local fire did not start")
	}
	if out := submit(t, h, task("c", "local", "queue"), store.TriggerSchedule, t0.Add(time.Hour), false); !out.Queued {
		t.Fatalf("overlap fire = %+v, want Queued", out)
	}

	h.d.DropTargets([]string{"vps", "local"})
	h.runs.end(t, first.Run.ID)
	h.runs.end(t, overlap.Run.ID)

	if n := len(h.runs.startCalls()); n != 2 {
		t.Fatalf("Start called %d times, want 2: the waiting fires were dropped", n)
	}
}

// Queued fires dropped by DropTargets are removed from queued_fires too.
func TestDropTargetsDeletesStoredQueuedFires(t *testing.T) {
	st := openStore(t)
	h := newHarness(t, oneSlot(), st)
	first := submit(t, h, task("a", "local", "parallel"), store.TriggerSchedule, t0, false)
	if first.Run.ID == "" {
		t.Fatal("first fire did not start")
	}
	if !submit(t, h, task("b", "local", "parallel"), store.TriggerSchedule, t0, false).Queued {
		t.Fatal("want b queued")
	}
	h.d.DropTargets([]string{"local"})
	fires, err := st.ListQueuedFires(context.Background())
	if err != nil || len(fires) != 0 {
		t.Fatalf("stored fires = %+v err=%v, want none after DropTargets", fires, err)
	}
}

// restart returns a new dispatcher and runner on the same store, as a
// daemon restart gives. recovered are the runs the new runner still tracks.
func restart(t *testing.T, cfg config.Config, st *store.Store, recovered ...store.Run) harness {
	t.Helper()
	h := newHarness(t, cfg, st)
	for _, r := range recovered {
		h.runs.recover(r.ID, r.TaskID, r.Target)
	}
	return h
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// restore restores and resumes one minute after t0.
func restore(t *testing.T, h harness, tasks ...config.Task) {
	t.Helper()
	if err := h.d.Restore(context.Background(), tasks, t0.Add(time.Minute)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.d.Resume(context.Background())
}

func oneSlot() config.Config {
	return config.Config{Targets: map[string]config.Target{"local": {MaxParallel: 1}}}
}

func TestRestoreStartsMaxParallelFiresInTheirQueueOrder(t *testing.T) {
	st := openStore(t)
	a, b, c := task("a", "local", "parallel"), task("b", "local", "parallel"), task("c", "local", "parallel")
	h := newHarness(t, oneSlot(), st)
	first := submit(t, h, a, store.TriggerSchedule, t0, false)
	if !submit(t, h, b, store.TriggerSchedule, t0.Add(time.Minute), false).Queued ||
		!submit(t, h, c, store.TriggerManual, time.Time{}, false).Queued {
		t.Fatal("want b and c queued behind a")
	}

	// The daemon restarts; a's run ended meanwhile, so the slot is free.
	h = restart(t, oneSlot(), st)
	restore(t, h, a, b, c)
	starts := h.runs.startCalls()
	if len(starts) != 1 || starts[0].TaskID != "b" || starts[0].Trigger != store.TriggerSchedule ||
		!starts[0].ScheduledAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after restore: starts = %+v, want only b's scheduled fire (first run was %s)", starts, first.Run.ID)
	}
	ts, ok, err := st.GetTaskState(context.Background(), "b")
	if err != nil || !ok || ts.LastRunID != starts[0].ID {
		t.Errorf("task state of b = %+v ok=%v err=%v, want the restored fire recorded", ts, ok, err)
	}
	h.runs.end(t, starts[0].ID)
	starts = h.runs.startCalls()
	if len(starts) != 2 || starts[1].TaskID != "c" || starts[1].Trigger != store.TriggerManual {
		t.Fatalf("after b ended: starts = %+v, want c's manual fire second", starts)
	}

	// Both fires started: a further restart has nothing to restore.
	h = restart(t, oneSlot(), st)
	restore(t, h, a, b, c)
	if starts := h.runs.startCalls(); len(starts) != 0 {
		t.Errorf("second restore started %+v, want nothing", starts)
	}
}

func TestRestoreKeepsAPendingFireBehindItsRecoveredRun(t *testing.T) {
	st := openStore(t)
	tk := task("nightly", "local", "queue")
	h := newHarness(t, config.Config{}, st)
	first := submit(t, h, tk, store.TriggerSchedule, t0, false)
	submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)

	h = restart(t, config.Config{}, st, first.Run)
	restore(t, h, tk)
	if starts := h.runs.startCalls(); len(starts) != 0 {
		t.Fatalf("restore started %+v while the task's run is active", starts)
	}
	// The restored fire is the task's one pending fire: a further one is dropped.
	if out := submit(t, h, tk, store.TriggerSchedule, t0.Add(2*time.Hour), false); !out.Skipped {
		t.Errorf("fire after restore = %+v, want skipped", out)
	}
	h.runs.end(t, first.Run.ID)
	starts := h.runs.startCalls()
	if len(starts) != 1 || !starts[0].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after the run ended: starts = %+v, want the restored fire", starts)
	}
}

func TestRestoreStartsAPendingFireWhoseRunEndedWithTheOldDaemon(t *testing.T) {
	st := openStore(t)
	tk := task("nightly", "local", "queue")
	h := newHarness(t, config.Config{}, st)
	submit(t, h, tk, store.TriggerSchedule, t0, false)
	submit(t, h, tk, store.TriggerSchedule, t0.Add(time.Hour), false)

	h = restart(t, config.Config{}, st) // the first run did not survive
	restore(t, h, tk)
	starts := h.runs.startCalls()
	if len(starts) != 1 || !starts[0].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("starts = %+v, want the restored fire", starts)
	}
	h = restart(t, config.Config{}, st, starts[0])
	restore(t, h, tk)
	if starts := h.runs.startCalls(); len(starts) != 0 {
		t.Errorf("second restore started %+v, want nothing", starts)
	}
}

func TestRestoreDropsTheFireOfATaskThatIsGone(t *testing.T) {
	st := openStore(t)
	a, b := task("a", "local", "parallel"), task("b", "local", "parallel")
	h := newHarness(t, oneSlot(), st)
	submit(t, h, a, store.TriggerSchedule, t0, false)
	submit(t, h, b, store.TriggerSchedule, t0, false)

	h = restart(t, oneSlot(), st)
	restore(t, h, a) // b was deleted while the daemon was down
	if starts := h.runs.startCalls(); len(starts) != 0 {
		t.Fatalf("starts = %+v, want nothing", starts)
	}
	if !strings.Contains(h.logs.String(), "queued fire dropped") {
		t.Errorf("log does not tell the fire was dropped:\n%s", h.logs.String())
	}
	fires, err := st.ListQueuedFires(context.Background())
	if err != nil || len(fires) != 0 {
		t.Errorf("stored fires = %+v err=%v, want none", fires, err)
	}
}

func TestRestoreDropsAScheduledFireOlderThanCatchUpWindow(t *testing.T) {
	st := openStore(t)
	cfg := oneSlot()
	cfg.CatchUpWindow = config.Duration{Duration: time.Hour}
	a, b, c := task("a", "local", "parallel"), task("b", "local", "parallel"), task("c", "local", "parallel")
	h := newHarness(t, cfg, st)
	submit(t, h, a, store.TriggerSchedule, t0, false)
	submit(t, h, b, store.TriggerSchedule, t0.Add(-2*time.Hour), false) // too old at restore
	submit(t, h, c, store.TriggerManual, time.Time{}, false)            // manual fires do not age

	h = restart(t, cfg, st)
	restore(t, h, a, b, c)
	starts := h.runs.startCalls()
	if len(starts) != 1 || starts[0].TaskID != "c" {
		t.Fatalf("starts = %+v, want only c's manual fire", starts)
	}
	if !strings.Contains(h.logs.String(), "older than catch_up_window") {
		t.Errorf("log does not tell the old fire was dropped:\n%s", h.logs.String())
	}
	if fires, err := st.ListQueuedFires(context.Background()); err != nil || len(fires) != 0 {
		t.Errorf("stored fires = %+v err=%v, want none", fires, err)
	}
}

// A's pending fire was stored before B joined the capacity queue. A's run
// ended while the daemon was down, so the pending fire is released behind
// B, as it would be when a live run ends.
func TestRestoreReleasesAnIdlePendingFireBehindFiresAlreadyQueued(t *testing.T) {
	st := openStore(t)
	a, b := task("a", "local", "queue"), task("b", "local", "parallel")
	h := newHarness(t, oneSlot(), st)
	submit(t, h, a, store.TriggerSchedule, t0, false)
	if !submit(t, h, a, store.TriggerSchedule, t0.Add(time.Hour), false).Queued {
		t.Fatal("want a's second fire pending")
	}
	if !submit(t, h, b, store.TriggerSchedule, t0.Add(time.Minute), false).Queued {
		t.Fatal("want b queued for a free slot")
	}

	h = restart(t, oneSlot(), st) // a's run did not survive
	restore(t, h, a, b)
	starts := h.runs.startCalls()
	if len(starts) != 1 || starts[0].TaskID != "b" {
		t.Fatalf("starts = %+v, want b, which was already waiting for the slot", starts)
	}
	h.runs.end(t, starts[0].ID)
	starts = h.runs.startCalls()
	if len(starts) != 2 || starts[1].TaskID != "a" || !starts[1].ScheduledAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after b ended: starts = %+v, want a's pending fire", starts)
	}
}

// A failed delete leaves the row. Starting the fire anyway would start it
// a second time after a restart, so the drain leaves it queued.
func TestDrainDoesNotStartAFireWhoseRowCannotBeDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := task("a", "local", "parallel"), task("b", "local", "parallel")
	h := newHarness(t, oneSlot(), st)
	first := submit(t, h, a, store.TriggerSchedule, t0, false)
	if !submit(t, h, b, store.TriggerSchedule, t0.Add(time.Minute), false).Queued {
		t.Fatal("want b queued")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	h.runs.end(t, first.Run.ID)
	if starts := h.runs.startCalls(); len(starts) != 1 {
		t.Fatalf("starts = %+v, want only a: b stays queued when its row cannot be deleted", starts)
	}
	if !strings.Contains(h.logs.String(), "cannot remove queued fire") {
		t.Errorf("log does not tell the delete failed:\n%s", h.logs.String())
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	fires, err := reopened.ListQueuedFires(context.Background())
	if err != nil || len(fires) != 1 || fires[0].TaskID != "b" {
		t.Fatalf("stored fires = %+v err=%v, want b still queued", fires, err)
	}
}
