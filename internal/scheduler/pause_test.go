package scheduler_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func TestPauseExpiryIsReportedOnceAtPausedUntil(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	tk := task("t", "@daily")
	tk.PausedUntil = "2026-11-06T14:30:00Z"
	indef := task("indef", "@daily")
	indef.PausedUntil = "indefinite"
	e.setTasks(valid(tk, indef))

	// The scheduler wakes at paused_until, before the next fire time.
	if wake := e.sched.Tick(e.ctx); !wake.Equal(time.Date(2026, 11, 6, 14, 30, 0, 0, time.UTC)) {
		t.Errorf("wake = %v, want paused_until 14:30", wake)
	}
	e.runUntil(time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC))

	want := []expiry{{id: "t", at: time.Date(2026, 11, 6, 14, 30, 0, 0, time.UTC)}}
	if !slices.Equal(e.expired, want) {
		t.Errorf("expired = %v, want %v", e.expired, want)
	}
}

func TestPauseAlreadyExpiredAtStartIsReported(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	tk := task("t", "@daily")
	tk.PausedUntil = "2026-11-01T08:00:00+01:00"
	e.setTasks(valid(tk))
	e.sched.Tick(e.ctx)
	if len(e.expired) != 1 || e.expired[0].id != "t" {
		t.Errorf("expired = %v, want t", e.expired)
	}
}

func TestGlobalPauseSkipsFiresWithoutCatchUp(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	e.setTasks(valid(task("t", "@hourly")))
	e.sched.SetGlobalPause(true, nil)

	e.runUntil(time.Date(2026, 11, 6, 15, 30, 0, 0, time.UTC))
	if n := len(e.runs("t")); n != 0 {
		t.Fatalf("%d runs during global pause, want 0", n)
	}
	if got, _ := e.sched.NextFire("t"); !got.Equal(time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("NextFire during global pause = %v, want 16:00", got)
	}

	e.sched.SetGlobalPause(false, nil)
	e.runUntil(time.Date(2026, 11, 6, 16, 30, 0, 0, time.UTC))
	if got := scheduledAt(e.runs("t"), time.UTC); !slices.Equal(got, []string{"2026-11-06 16:00 UTC"}) {
		t.Errorf("fires after resume_all = %q, want only 16:00 (no catch-up)", got)
	}
}

func TestGlobalPauseOnOneTargetLeavesOtherTargets(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	vps := target.NewFake("vps")
	e.sched = e.newScheduler(map[string]target.Target{"local": e.local, "vps": vps})
	local := task("on-local", "@hourly")
	remote := task("on-vps", "@hourly")
	remote.Target = "vps"
	e.setTasks(valid(local, remote))
	e.sched.SetGlobalPause(false, []string{"vps"})

	e.runUntil(time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC))
	if n := len(e.runs("on-vps")); n != 0 {
		t.Errorf("on-vps: %d runs while vps is paused, want 0", n)
	}
	if n := len(e.runs("on-local")); n != 2 {
		t.Errorf("on-local: %d runs, want 2", n)
	}
}

func TestSetTaskRecomputesNextFireFromNow(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	off := task("t", "@hourly")
	off.Enabled = false
	e.setTasks(valid(off))

	e.clock.Set(time.Date(2026, 11, 6, 12, 20, 0, 0, time.UTC))
	on := off
	on.Enabled = true
	if err := e.sched.SetTask(e.ctx, config.TaskEntry{ID: "t", Task: on}); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	if got, ok := e.sched.NextFire("t"); !ok || !got.Equal(want) {
		t.Errorf("NextFire after enable = %v, %v; want %v", got, ok, want)
	}
	if got := e.state("t").NextFireAt; !got.Equal(want) {
		t.Errorf("task_state next_fire_at = %v, want %v", got, want)
	}

	if err := e.sched.SetTask(e.ctx, config.TaskEntry{ID: "t", Task: off}); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.sched.NextFire("t"); ok {
		t.Errorf("NextFire after disable = %v, want none", got)
	}
	if got := e.state("t").NextFireAt; !got.IsZero() {
		t.Errorf("task_state next_fire_at after disable = %v, want none", got)
	}
}

// A timed pause on a task that is off or invalid still has to be cleared
// when it passes, and the task must not fire while it waits.
func TestOffAndInvalidPauseExpiryIsReportedWithoutARun(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	off := task("off", "@hourly")
	off.Enabled = false
	off.PausedUntil = "2026-11-06T14:30:00Z"
	bad := task("bad", "@hourly")
	bad.PausedUntil = "2026-11-06T13:00:00Z"
	e.setTasks([]config.TaskEntry{
		{ID: off.ID, Task: off},
		{ID: bad.ID, Task: bad, Err: errors.New("unknown target")},
	})
	for _, id := range []string{"off", "bad"} {
		if _, ok := e.sched.NextFire(id); ok {
			t.Errorf("NextFire(%s) set, want none", id)
		}
	}

	e.runUntil(time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC))
	want := []expiry{
		{id: "bad", at: time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)},
		{id: "off", at: time.Date(2026, 11, 6, 14, 30, 0, 0, time.UTC)},
	}
	if !slices.Equal(e.expired, want) {
		t.Errorf("expired = %v, want %v", e.expired, want)
	}
	if n := len(e.runs("off")) + len(e.runs("bad")); n != 0 {
		t.Errorf("%d runs, want 0", n)
	}
}

func TestPauseClearFailureIsRetriedLater(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	e := newEnv(t, now)
	e.expireErr = errors.New("disk full")
	tk := task("t", "@daily")
	tk.PausedUntil = "2026-11-06T11:00:00Z"
	e.setTasks(valid(tk))

	wake := e.sched.Tick(e.ctx)
	if len(e.expired) != 1 {
		t.Fatalf("expired = %v, want one call", e.expired)
	}
	e.sched.Tick(e.ctx)
	if len(e.expired) != 1 {
		t.Fatalf("cleared again before the retry: %v", e.expired)
	}
	if !wake.After(now) {
		t.Fatalf("wake = %v, want a future retry so a failed clear does not spin", wake)
	}
	e.clock.Set(wake)
	e.sched.Tick(e.ctx)
	if len(e.expired) != 2 || !e.expired[1].at.Equal(wake) {
		t.Fatalf("expired = %v, want a retry at %v", e.expired, wake)
	}
}

// execHook runs fn at the start of Exec, then the embedded fake.
type execHook struct {
	*target.Fake
	fn func()
}

func (h *execHook) Exec(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	if h.fn != nil {
		h.fn()
	}
	return h.Fake.Exec(ctx, argv, stdin)
}

// Two tasks come due in one tick. The first Exec applies a pause or a
// disable; the fire still queued behind it must not start.
func TestQueuedFireSeesPauseThatLandsDuringTick(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(t *testing.T, e *env)
	}{
		{"global pause", func(_ *testing.T, e *env) { e.sched.SetGlobalPause(true, nil) }},
		{"disable", func(t *testing.T, e *env) {
			for _, id := range []string{"a", "b"} {
				tk := task(id, "@hourly")
				tk.Enabled = false
				if err := e.sched.SetTask(e.ctx, config.TaskEntry{ID: id, Task: tk}); err != nil {
					t.Errorf("disable %s: %v", id, err)
				}
			}
		}},
		{"task pause", func(t *testing.T, e *env) {
			for _, id := range []string{"a", "b"} {
				tk := task(id, "@hourly")
				tk.PausedUntil = "indefinite"
				if err := e.sched.SetTask(e.ctx, config.TaskEntry{ID: id, Task: tk}); err != nil {
					t.Errorf("pause %s: %v", id, err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
			var once sync.Once
			fake := target.NewFake("local")
			hook := &execHook{Fake: fake, fn: func() { once.Do(func() { tc.fn(t, e) }) }}
			e.local = fake
			e.sched = e.newScheduler(map[string]target.Target{"local": hook})
			e.setTasks(valid(task("a", "@hourly"), task("b", "@hourly")))
			e.clock.Set(time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC))
			e.sched.Tick(e.ctx)
			if n := len(e.runs("a")) + len(e.runs("b")); n != 1 {
				t.Fatalf("%d runs, want 1 (the fire already inside Exec; the queued one stays off)", n)
			}
		})
	}
}
