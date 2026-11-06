package scheduler_test

import (
	"slices"
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
