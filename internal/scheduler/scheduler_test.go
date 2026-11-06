package scheduler_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/scheduler"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	clock  *clock.Fake
	store  *store.Store
	local  *target.Fake
	sched  *scheduler.Scheduler
	config config.Config
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
		local:  target.NewFake("local"),
		config: config.Config{Timezone: "Europe/Brussels", HistoryDays: 90},
	}
	e.sched = scheduler.New(scheduler.Options{
		Clock:   e.clock,
		Store:   st,
		Targets: map[string]target.Target{"local": e.local},
		Config:  e.config,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e
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
	if calls := e.local.Calls(); len(calls) != 1 || !slices.Equal(calls[0], []string{"claude", "--permission-mode", "acceptEdits"}) {
		t.Errorf("target calls = %q", calls)
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

func TestLateTickFiresOnceAndSkipsAhead(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	e.setTasks(valid(task("hourly", "@hourly")))
	e.clock.Set(time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC)) // daemon was asleep
	e.sched.Tick(e.ctx)
	if n := len(e.runs("hourly")); n != 1 {
		t.Errorf("%d runs, want 1", n)
	}
	want := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	if got, _ := e.sched.NextFire("hourly"); !got.Equal(want) {
		t.Errorf("NextFire = %v, want %v", got, want)
	}
}

func TestTargetErrorFailsTheRun(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	e.local.SetErr(errors.New("herdr: no server"))
	e.setTasks(valid(task("t", "@hourly")))
	e.runUntil(time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC))
	runs := e.runs("t")
	if len(runs) != 1 || runs[0].Status != store.StatusFailed || runs[0].ExitReason != "herdr: no server" {
		t.Fatalf("runs = %+v, want one failed run with the error as reason", runs)
	}
	if got := e.state("t").NextFireAt; !got.Equal(time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("next_fire_at = %v after a failed run", got)
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
	insert(store.NewRunID(now.AddDate(0, 0, -91)), now.AddDate(0, 0, -91), true)
	insert(store.NewRunID(now.AddDate(0, 0, -89)), now.AddDate(0, 0, -89), true)
	active := store.NewRunID(now.AddDate(0, 0, -95))
	insert(active, now.AddDate(0, 0, -95), false)

	e.sched.Tick(e.ctx)
	runs := e.runs("t")
	if len(runs) != 2 || runs[0].ID != active {
		t.Errorf("after prune: %+v, want the active and the 89-day-old run", runs)
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
