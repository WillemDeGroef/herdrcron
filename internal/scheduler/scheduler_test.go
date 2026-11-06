package scheduler_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/scheduler"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// fakeRuns is a scheduler.Starter that stores each run as done, or returns
// err without storing anything, and records every call.
type fakeRuns struct {
	clock *clock.Fake
	store *store.Store

	mu    sync.Mutex
	calls []startCall
	err   error
}

type startCall struct {
	task        config.Task
	trigger     string
	scheduledAt time.Time
}

func (f *fakeRuns) Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error) {
	f.mu.Lock()
	f.calls = append(f.calls, startCall{task: t, trigger: trigger, scheduledAt: scheduledAt})
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return store.Run{}, err
	}
	now := f.clock.Now()
	run := store.Run{
		ID: store.NewRunID(now), TaskID: t.ID, Target: t.Target, Trigger: trigger,
		ScheduledAt: scheduledAt, StartedAt: now, EndedAt: now, Status: store.StatusDone,
	}
	return run, f.store.InsertRun(ctx, run)
}

func (f *fakeRuns) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeRuns) Calls() []startCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type env struct {
	t       *testing.T
	ctx     context.Context
	clock   *clock.Fake
	store   *store.Store
	starter *fakeRuns
	sched   *scheduler.Scheduler
	config  config.Config
}

func brussels(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func newEnv(t *testing.T, now time.Time) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "herdrcron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{
		t:      t,
		ctx:    context.Background(),
		clock:  clock.NewFake(now),
		store:  st,
		config: config.Config{Timezone: "Europe/Brussels", HistoryDays: 90},
	}
	e.starter = &fakeRuns{clock: e.clock, store: st}
	e.restart()
	return e
}

// restart replaces the scheduler with a new one on the same store, clock
// and starter, with e.config: a daemon restart.
func (e *env) restart() {
	e.sched = scheduler.New(scheduler.Options{
		Clock:  e.clock,
		Store:  e.store,
		Runs:   e.starter,
		Config: e.config,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func task(id, schedule string) config.Task {
	return config.Task{
		ID: id, Target: "local", Cwd: "/tmp", Schedule: schedule, Enabled: true,
		Agent:   config.Agent{Command: "claude", Args: []string{"--permission-mode", "acceptEdits"}},
		Timeout: config.Duration{Duration: time.Hour}, Overlap: "skip", Missed: "run_once",
	}
}

func valid(tasks ...config.Task) []config.TaskEntry {
	var out []config.TaskEntry
	for _, t := range tasks {
		out = append(out, config.TaskEntry{ID: t.ID, Task: t})
	}
	return out
}

func (e *env) setTasks(entries []config.TaskEntry) {
	e.t.Helper()
	if err := e.sched.SetTasks(e.ctx, entries); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state(id string) store.TaskState {
	e.t.Helper()
	st, _, err := e.store.GetTaskState(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) runs(id string) []store.Run {
	e.t.Helper()
	runs, err := e.store.ListRuns(e.ctx, id, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	slices.Reverse(runs) // oldest first
	return runs
}

// runUntil drives the scheduler with Tick, jumping the fake clock to each
// wake-up time, until the clock passes end.
func (e *env) runUntil(end time.Time) {
	e.t.Helper()
	for {
		wake := e.sched.Tick(e.ctx)
		if wake.IsZero() || wake.After(end) {
			e.clock.Set(end)
			e.sched.Tick(e.ctx)
			return
		}
		e.clock.Set(wake)
	}
}

func scheduledAt(runs []store.Run, loc *time.Location) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.ScheduledAt.In(loc).Format("2006-01-02 15:04 MST"))
	}
	return out
}

func TestEnabledTaskGetsNextFireAtInItsTimezone(t *testing.T) {
	loc := brussels(t)
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, loc)) // Friday
	weekly := task("weekly", "0 7 * * 1")
	ny := task("ny", "0 7 * * *")
	ny.Timezone = "America/New_York"
	e.setTasks(valid(weekly, ny))

	want := time.Date(2026, 11, 9, 7, 0, 0, 0, loc)
	if got, ok := e.sched.NextFire("weekly"); !ok || !got.Equal(want) {
		t.Errorf("NextFire(weekly) = %v, %v; want %v", got, ok, want)
	}
	if got := e.state("weekly").NextFireAt; !got.Equal(want) {
		t.Errorf("task_state next_fire_at = %v, want %v", got, want)
	}
	// It is 06:00 in New York; 07:00 EST is 12:00 UTC.
	wantNY := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if got, _ := e.sched.NextFire("ny"); !got.Equal(wantNY) {
		t.Errorf("NextFire(ny) = %v, want %v", got, wantNY)
	}
}

func TestTaskWithoutTimezoneUsesConfigTimezone(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))
	e.setTasks(valid(task("daily", "0 7 * * *")))
	want := time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC) // 07:00 CET
	if got, _ := e.sched.NextFire("daily"); !got.Equal(want) {
		t.Errorf("NextFire = %v, want %v", got, want)
	}
}

func TestOffAndInvalidTasksHaveNoNextFireAt(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	off := task("off", "@hourly")
	off.Enabled = false
	entries := append(valid(off), config.TaskEntry{ID: "broken", Err: errors.New("unknown target")})
	e.setTasks(entries)

	for _, id := range []string{"off", "broken"} {
		if got, ok := e.sched.NextFire(id); ok {
			t.Errorf("NextFire(%s) = %v, want none", id, got)
		}
		if got := e.state(id).NextFireAt; !got.IsZero() {
			t.Errorf("%s task_state next_fire_at = %v, want none", id, got)
		}
	}
	e.runUntil(e.clock.Now().Add(3 * time.Hour))
	if n := len(e.runs("")); n != 0 {
		t.Errorf("%d runs, want 0", n)
	}
}

func TestDisablingATaskClearsItsNextFireAt(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	tk := task("t", "@hourly")
	e.setTasks(valid(tk))
	if e.state("t").NextFireAt.IsZero() {
		t.Fatal("enabled task has no next_fire_at")
	}
	tk.Enabled = false
	e.setTasks(valid(tk))
	if got := e.state("t").NextFireAt; !got.IsZero() {
		t.Errorf("next_fire_at = %v after disable, want none", got)
	}
}

func TestFireRecordsRunAndRecomputesNextFireAt(t *testing.T) {
	loc := brussels(t)
	start := time.Date(2026, 11, 6, 12, 10, 0, 0, loc)
	e := newEnv(t, start)
	e.setTasks(valid(task("hourly", "@hourly")))

	wake := e.sched.Tick(e.ctx)
	fireAt := time.Date(2026, 11, 6, 13, 0, 0, 0, loc)
	if !wake.Equal(fireAt) {
		t.Fatalf("Tick wake = %v, want %v", wake, fireAt)
	}
	if n := len(e.runs("hourly")); n != 0 {
		t.Fatalf("%d runs before the fire time", n)
	}

	e.clock.Set(fireAt.Add(2 * time.Second)) // the timer is a little late
	wake = e.sched.Tick(e.ctx)

	runs := e.runs("hourly")
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	r := runs[0]
	if len(r.ID) != 26 {
		t.Errorf("run id %q is not a ULID", r.ID)
	}
	if r.Trigger != store.TriggerSchedule || r.Target != "local" || r.TaskID != "hourly" {
		t.Errorf("run = %+v", r)
	}
	if !r.ScheduledAt.Equal(fireAt) || !r.StartedAt.Equal(fireAt.Add(2*time.Second)) {
		t.Errorf("scheduled_at %v started_at %v", r.ScheduledAt, r.StartedAt)
	}
	if r.Status != store.StatusDone || r.EndedAt.IsZero() {
		t.Errorf("status %q ended_at %v, want done and ended", r.Status, r.EndedAt)
	}
	calls := e.starter.Calls()
	if len(calls) != 1 || calls[0].task.ID != "hourly" || calls[0].trigger != store.TriggerSchedule || !calls[0].scheduledAt.Equal(fireAt) {
		t.Errorf("Start calls = %+v, want one for hourly with trigger schedule at %v", calls, fireAt)
	}

	next := time.Date(2026, 11, 6, 14, 0, 0, 0, loc)
	if !wake.Equal(next) {
		t.Errorf("Tick wake after fire = %v, want %v", wake, next)
	}
	st := e.state("hourly")
	if st.LastRunID != r.ID || !st.LastFireAt.Equal(fireAt) || !st.NextFireAt.Equal(next) {
		t.Errorf("task_state = %+v, want last_run_id %s, last_fire_at %v, next_fire_at %v", st, r.ID, fireAt, next)
	}
}

func TestSlightlyLateTickStillFires(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	e.setTasks(valid(task("hourly", "@hourly")))
	e.clock.Set(time.Date(2026, 11, 6, 13, 0, 50, 0, time.UTC))
	e.sched.Tick(e.ctx)
	if n := len(e.runs("hourly")); n != 1 {
		t.Errorf("%d runs, want 1", n)
	}
}

// A fire time that passed long ago while the daemon ran (the host slept)
// is a missed fire: it does not start a scheduled run. With missed:
// run_once it starts one catch_up run for the latest missed fire.
func TestFireMissedWhileRunningIsCaughtUpOnce(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	e.setTasks(valid(task("hourly", "@hourly")))
	e.clock.Set(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC))
	e.sched.Tick(e.ctx)
	runs := e.runs("hourly")
	if len(runs) != 1 || runs[0].Trigger != store.TriggerCatchUp ||
		!runs[0].ScheduledAt.Equal(time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("runs = %+v, want one catch_up run scheduled at 17:00", runs)
	}
	want := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	if got, _ := e.sched.NextFire("hourly"); !got.Equal(want) {
		t.Errorf("NextFire = %v, want %v", got, want)
	}
	if got := e.state("hourly").NextFireAt; !got.Equal(want) {
		t.Errorf("task_state next_fire_at = %v, want %v", got, want)
	}
}

func TestReloadKeepsNextFireOfUnchangedTasks(t *testing.T) {
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	e := newEnv(t, start)
	every := task("every", "@every 2h")
	e.setTasks(valid(every))
	e.clock.Set(start.Add(90 * time.Minute))

	// Same schedule: the fire stays at 14:00, not now+2h.
	renamed := every
	renamed.Name = "renamed"
	e.setTasks(valid(renamed))
	if got, _ := e.sched.NextFire("every"); !got.Equal(start.Add(2 * time.Hour)) {
		t.Errorf("unchanged schedule: NextFire = %v, want 14:00", got)
	}
	// New schedule: recomputed from now.
	changed := every
	changed.Schedule = "@every 3h"
	e.setTasks(valid(changed))
	if got, _ := e.sched.NextFire("every"); !got.Equal(start.Add(90*time.Minute + 3*time.Hour)) {
		t.Errorf("changed schedule: NextFire = %v, want 16:30", got)
	}
	// Disable then enable: recomputed from now.
	changed.Enabled = false
	e.setTasks(valid(changed))
	e.clock.Set(start.Add(2 * time.Hour))
	changed.Enabled = true
	e.setTasks(valid(changed))
	if got, _ := e.sched.NextFire("every"); !got.Equal(start.Add(5 * time.Hour)) {
		t.Errorf("re-enabled: NextFire = %v, want 17:00", got)
	}
}

// A run that cannot be stored at all (Start returns an error) leaves no
// last_run_id, and the task still gets its next fire time.
func TestStartErrorStillMovesNextFire(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	e.starter.setErr(errors.New("database is locked"))
	e.setTasks(valid(task("t", "@hourly")))
	e.runUntil(time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC))
	if n := len(e.starter.Calls()); n != 1 {
		t.Fatalf("%d Start calls, want 1", n)
	}
	st := e.state("t")
	if st.LastRunID != "" {
		t.Errorf("last_run_id = %q, want none", st.LastRunID)
	}
	if !st.NextFireAt.Equal(time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("next_fire_at = %v after a failed start", st.NextFireAt)
	}
}

// The 13:00 slot is inside the pause. The tick runs at 13:00:50, after
// paused_until, and must still skip that slot. 14:00 is after the pause.
func TestPausedSlotStaysSkippedWhenTickLandsAfterPause(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	until := task("until", "@hourly")
	until.PausedUntil = "2026-11-06T13:00:30Z"
	bad := task("bad", "@hourly")
	bad.PausedUntil = "tomorrow"
	e.setTasks(valid(until, bad))

	e.clock.Set(time.Date(2026, 11, 6, 13, 0, 50, 0, time.UTC))
	e.sched.Tick(e.ctx)
	if n := len(e.runs("until")); n != 0 {
		t.Errorf("until: %d runs, want 0 (13:00 was inside the pause)", n)
	}
	if n := len(e.runs("bad")); n != 0 {
		t.Errorf("bad paused_until: %d runs, want 0", n)
	}

	e.clock.Set(time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC))
	e.sched.Tick(e.ctx)
	if got := scheduledAt(e.runs("until"), time.UTC); !slices.Equal(got, []string{"2026-11-06 14:00 UTC"}) {
		t.Errorf("until fires %q, want only 14:00", got)
	}
	if n := len(e.runs("bad")); n != 0 {
		t.Errorf("bad paused_until fired at 14:00: %d runs", n)
	}
}

func TestPausedTaskSkipsFireButKeepsSchedule(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	indef := task("indef", "@hourly")
	indef.PausedUntil = "indefinite"
	until := task("until", "@hourly")
	until.PausedUntil = "2026-11-06T14:30:00Z"
	e.setTasks(valid(indef, until))

	e.runUntil(time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC))
	if n := len(e.runs("indef")); n != 0 {
		t.Errorf("indefinite: %d runs, want 0", n)
	}
	if got := scheduledAt(e.runs("until"), time.UTC); !slices.Equal(got, []string{"2026-11-06 15:00 UTC", "2026-11-06 16:00 UTC"}) {
		t.Errorf("until 14:30: fires %q, want 15:00 and 16:00", got)
	}
	if got, _ := e.sched.NextFire("indef"); !got.Equal(time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("paused task NextFire = %v, want 17:00", got)
	}
}

func TestDSTSpringForwardBrussels(t *testing.T) {
	loc := brussels(t)
	e := newEnv(t, time.Date(2026, 3, 27, 12, 0, 0, 0, loc))
	e.setTasks(valid(task("nightly", "30 2 * * *"), task("hourly", "@hourly")))
	e.runUntil(time.Date(2026, 3, 30, 12, 0, 0, 0, loc))

	want := []string{"2026-03-28 02:30 CET", "2026-03-29 03:00 CEST", "2026-03-30 02:30 CEST"}
	if got := scheduledAt(e.runs("nightly"), loc); !slices.Equal(got, want) {
		t.Errorf("nightly fires %q, want %q", got, want)
	}
	// 2026-03-29 has 23 hours: 02:00 does not exist.
	var day []string
	for _, s := range scheduledAt(e.runs("hourly"), loc) {
		if s[:10] == "2026-03-29" {
			day = append(day, s[11:])
		}
	}
	if len(day) != 23 || day[2] != "03:00 CEST" {
		t.Errorf("hourly on 2026-03-29: %d fires %q, want 23 without 02:00", len(day), day)
	}
}

func TestDSTFallBackBrussels(t *testing.T) {
	loc := brussels(t)
	e := newEnv(t, time.Date(2026, 10, 23, 12, 0, 0, 0, loc))
	e.setTasks(valid(task("nightly", "30 2 * * *"), task("hourly", "@hourly")))
	e.runUntil(time.Date(2026, 10, 26, 12, 0, 0, 0, loc))

	want := []string{"2026-10-24 02:30 CEST", "2026-10-25 02:30 CEST", "2026-10-26 02:30 CET"}
	if got := scheduledAt(e.runs("nightly"), loc); !slices.Equal(got, want) {
		t.Errorf("nightly fires %q, want %q", got, want)
	}
	// 2026-10-25 has 25 hours: 02:00 happens in CEST and again in CET.
	var day []string
	for _, s := range scheduledAt(e.runs("hourly"), loc) {
		if s[:10] == "2026-10-25" {
			day = append(day, s[11:])
		}
	}
	if len(day) != 25 || day[2] != "02:00 CEST" || day[3] != "02:00 CET" {
		t.Errorf("hourly on 2026-10-25: %d fires %q, want 25 with 02:00 twice", len(day), day)
	}
}

func TestOldRunsArePruned(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	e.config.HistoryDays = 7
	e.sched = scheduler.New(scheduler.Options{Clock: e.clock, Store: e.store,
		Runs: e.starter, Config: e.config,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	insert := func(id string, started time.Time, ended bool) {
		r := store.Run{ID: id, TaskID: "t", Target: "local", Trigger: store.TriggerManual,
			StartedAt: started, Status: store.StatusRunning}
		if ended {
			r.EndedAt, r.Status = started.Add(time.Minute), store.StatusDone
		}
		if err := e.store.InsertRun(e.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	insert(store.NewRunID(now.AddDate(0, 0, -8)), now.AddDate(0, 0, -8), true)
	insert(store.NewRunID(now.AddDate(0, 0, -6)), now.AddDate(0, 0, -6), true)
	active := store.NewRunID(now.AddDate(0, 0, -30))
	insert(active, now.AddDate(0, 0, -30), false)

	e.sched.Tick(e.ctx)
	runs := e.runs("t")
	if len(runs) != 2 || runs[0].ID != active {
		t.Errorf("after prune: %+v, want the active and the 6-day-old run", runs)
	}
}

func TestRunLoopFiresOnTheFakeClock(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 59, 0, 0, time.UTC))
	e.setTasks(valid(task("hourly", "@hourly")))
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.sched.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	})

	waitFor(t, "scheduler waits on the clock", func() bool { return e.clock.Waiters() > 0 })
	e.clock.Advance(time.Minute)
	waitFor(t, "a run is recorded", func() bool { return len(e.runs("hourly")) == 1 })

	// Tasks set while the loop runs are scheduled too.
	e.setTasks(valid(task("hourly", "@hourly"), task("soon", "@every 1m")))
	waitFor(t, "scheduler waits again", func() bool { return e.clock.Waiters() > 0 })
	e.clock.Advance(time.Minute)
	waitFor(t, "the new task fires", func() bool { return len(e.runs("soon")) == 1 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The daemon is down from 12:10 to 17:30 across five hourly fires. On
// start it catches up once, for the latest missed fire.
func TestRestartCatchesUpMissedFiresOnce(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 10, 0, 0, time.UTC))
	e.setTasks(valid(task("hourly", "@hourly")))

	e.clock.Set(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC))
	e.restart()
	e.setTasks(valid(task("hourly", "@hourly")))
	wake := e.sched.Tick(e.ctx)

	latest := time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)
	calls := e.starter.Calls()
	if len(calls) != 1 || calls[0].trigger != store.TriggerCatchUp || !calls[0].scheduledAt.Equal(latest) {
		t.Fatalf("Start calls = %+v, want one catch_up at %v", calls, latest)
	}
	runs := e.runs("hourly")
	if len(runs) != 1 || runs[0].Trigger != store.TriggerCatchUp {
		t.Fatalf("runs = %+v, want one catch_up run", runs)
	}
	next := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	if !wake.Equal(next) {
		t.Errorf("Tick wake = %v, want %v", wake, next)
	}
	st := e.state("hourly")
	if st.LastRunID != runs[0].ID || !st.LastFireAt.Equal(latest) || !st.NextFireAt.Equal(next) {
		t.Errorf("task_state = %+v, want last_run_id %s, last_fire_at %v, next_fire_at %v", st, runs[0].ID, latest, next)
	}
	// Later ticks do not catch up again.
	e.sched.Tick(e.ctx)
	if n := len(e.starter.Calls()); n != 1 {
		t.Errorf("%d Start calls after a second tick, want 1", n)
	}
}

func TestMissedSkipStartsNothing(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 10, 0, 0, time.UTC))
	tk := task("hourly", "@hourly")
	tk.Missed = "skip"
	e.setTasks(valid(tk))

	e.clock.Set(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC))
	e.restart()
	e.setTasks(valid(tk))
	e.sched.Tick(e.ctx) // missed on restart
	e.clock.Set(time.Date(2026, 11, 6, 20, 30, 0, 0, time.UTC))
	e.sched.Tick(e.ctx) // missed while running
	if calls := e.starter.Calls(); len(calls) != 0 {
		t.Errorf("Start calls = %+v, want none", calls)
	}
	want := time.Date(2026, 11, 6, 21, 0, 0, 0, time.UTC)
	if got := e.state("hourly").NextFireAt; !got.Equal(want) {
		t.Errorf("next_fire_at = %v, want %v", got, want)
	}
}

// The daemon is down from Friday 6 Nov to Saturday 14 Nov 12:00. Only a
// missed fire in the last 24h (catch_up_window) is caught up: the daily
// 07:00 fire is, the weekly Friday 07:00 fire (29h old) is not.
func TestMissedFiresOlderThanCatchUpWindowAreIgnored(t *testing.T) {
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) // Friday
	e := newEnv(t, start)
	weekly := task("weekly", "0 7 * * 5") // Fridays 07:00
	weekly.Timezone = "UTC"
	daily := task("daily", "0 7 * * *")
	daily.Timezone = "UTC"
	e.setTasks(valid(weekly, daily))
	if got, _ := e.sched.NextFire("weekly"); !got.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly NextFire = %v", got)
	}

	e.clock.Set(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	e.restart()
	e.setTasks(valid(weekly, daily))
	e.sched.Tick(e.ctx)

	if n := len(e.runs("weekly")); n != 0 {
		t.Errorf("weekly: %d runs, want 0 (missed fire older than 24h)", n)
	}
	runs := e.runs("daily")
	if len(runs) != 1 || runs[0].Trigger != store.TriggerCatchUp ||
		!runs[0].ScheduledAt.Equal(time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("daily runs = %+v, want one catch_up at Saturday 07:00", runs)
	}
	if got, _ := e.sched.NextFire("weekly"); !got.Equal(time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("weekly NextFire = %v, want next Friday 07:00", got)
	}
}

func TestCustomCatchUpWindow(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 10, 0, 0, time.UTC))
	daily := task("daily", "0 7 * * *")
	daily.Timezone = "UTC"
	e.setTasks(valid(daily))

	// Down until Saturday 12:00: the 07:00 fire is 5h old.
	e.clock.Set(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	e.config.CatchUpWindow = config.Duration{Duration: 4 * time.Hour}
	e.restart()
	e.setTasks(valid(daily))
	e.sched.Tick(e.ctx)
	if n := len(e.runs("daily")); n != 0 {
		t.Errorf("window 4h: %d runs, want 0", n)
	}

	// Same outage with a 6h window: caught up.
	e2 := newEnv(t, time.Date(2026, 11, 6, 12, 10, 0, 0, time.UTC))
	e2.setTasks(valid(daily))
	e2.clock.Set(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	e2.config.CatchUpWindow = config.Duration{Duration: 6 * time.Hour}
	e2.restart()
	e2.setTasks(valid(daily))
	e2.sched.Tick(e2.ctx)
	if runs := e2.runs("daily"); len(runs) != 1 || runs[0].Trigger != store.TriggerCatchUp {
		t.Errorf("window 6h: runs = %+v, want one catch_up", runs)
	}
}

// The latest missed fire (17:00) is inside the pause; no catch-up.
func TestPausedMissedFireIsNotCaughtUp(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 10, 0, 0, time.UTC))
	tk := task("hourly", "@hourly")
	e.setTasks(valid(tk))

	e.clock.Set(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC))
	tk.PausedUntil = "2026-11-06T17:15:00Z"
	e.restart()
	e.setTasks(valid(tk))
	e.sched.Tick(e.ctx)
	if calls := e.starter.Calls(); len(calls) != 0 {
		t.Errorf("Start calls = %+v, want none", calls)
	}
	if got, _ := e.sched.NextFire("hourly"); !got.Equal(time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("NextFire = %v, want 18:00", got)
	}
}

func TestRestartWithoutMissedFireKeepsStoredNext(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	every := task("every", "@every 2h")
	e.setTasks(valid(every))
	stored := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)

	e.clock.Set(time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC))
	e.restart()
	e.setTasks(valid(every))
	wake := e.sched.Tick(e.ctx)
	if calls := e.starter.Calls(); len(calls) != 0 {
		t.Errorf("Start calls = %+v, want none", calls)
	}
	if !wake.Equal(stored) {
		t.Errorf("Tick wake = %v, want stored %v (not now+2h)", wake, stored)
	}
	if got := e.state("every").NextFireAt; !got.Equal(stored) {
		t.Errorf("next_fire_at = %v, want %v", got, stored)
	}
}

// A Starter that returns a zero run (overlap skipped or queued it) gets
// nothing recorded for the catch-up.
func TestCatchUpNotStartedRecordsNothing(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 10, 0, 0, time.UTC))
	e.setTasks(valid(task("hourly", "@hourly")))
	e.clock.Set(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC))
	skip := &skipRuns{}
	e.sched = scheduler.New(scheduler.Options{Clock: e.clock, Store: e.store, Runs: skip,
		Config: e.config, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e.setTasks(valid(task("hourly", "@hourly")))
	e.sched.Tick(e.ctx)
	if skip.n != 1 {
		t.Fatalf("%d Start calls, want 1", skip.n)
	}
	st := e.state("hourly")
	if st.LastRunID != "" || !st.LastFireAt.IsZero() {
		t.Errorf("task_state = %+v, want no last run", st)
	}
	if !st.NextFireAt.Equal(time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("next_fire_at = %v, want 18:00", st.NextFireAt)
	}
}

// skipRuns is a Starter that never starts a run, like a dispatcher whose
// overlap policy skipped the fire.
type skipRuns struct{ n int }

func (s *skipRuns) Start(context.Context, config.Task, string, time.Time) (store.Run, error) {
	s.n++
	return store.Run{}, nil
}
