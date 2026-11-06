package scheduler_test

import (
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

func onVPS(t config.Task) config.Task {
	t.Target = "vps"
	return t
}

// The vps target is down for the 12:00, 13:00 and 14:00 fires: each is a
// host_unreachable run. When it answers at 14:30, missed: run_once starts
// one catch_up run, for the latest of them.
func TestHostUnreachableFiresAreCaughtUpOnceWhenTheTargetAnswers(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
	e.setTasks(valid(onVPS(task("hourly", "@hourly"))))
	e.starter.setUnreachable(true)
	e.runUntil(time.Date(2026, 11, 6, 14, 30, 0, 0, time.UTC))
	if n := len(e.runs("hourly")); n != 3 {
		t.Fatalf("%d runs while vps is down, want 3 host_unreachable", n)
	}

	e.starter.setUnreachable(false)
	e.sched.CatchUpTarget(e.ctx, "vps")
	calls := e.starter.Calls()[3:]
	latest := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	if len(calls) != 1 || calls[0].trigger != store.TriggerCatchUp || !calls[0].scheduledAt.Equal(latest) {
		t.Fatalf("Start calls after vps answers = %+v, want one catch_up for %v", calls, latest)
	}
	if st := e.state("hourly"); st.LastRunID != e.runs("hourly")[3].ID {
		t.Errorf("task_state last_run_id = %s, want the catch_up run", st.LastRunID)
	}

	// The target answering again later catches nothing up twice.
	e.sched.CatchUpTarget(e.ctx, "vps")
	if n := len(e.starter.Calls()); n != 4 {
		t.Errorf("%d Start calls after a second CatchUpTarget, want 4", n)
	}
}

func TestCatchUpTargetFollowsTheMissedPolicyAndWindow(t *testing.T) {
	skip := onVPS(task("skip", "@hourly"))
	skip.Missed = "skip"
	local := task("local", "@hourly")
	old := onVPS(task("old", "@hourly"))
	manual := onVPS(task("manual", "0 0 1 1 *"))
	for name, tc := range map[string]struct {
		task config.Task
		// at is when the target answers again.
		at time.Time
	}{
		"missed skip":                {skip, time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC)},
		"other target":               {local, time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC)},
		"older than catch_up_window": {old, time.Date(2026, 11, 7, 12, 30, 0, 0, time.UTC)},
		"a manual run is not a fire": {manual, time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC)},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
			e.setTasks(valid(tc.task))
			e.starter.setUnreachable(true)
			if tc.task.ID == "manual" {
				e.clock.Set(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
				if _, err := e.starter.Start(e.ctx, tc.task, store.TriggerManual, time.Time{}); err != nil {
					t.Fatal(err)
				}
			} else {
				e.runUntil(time.Date(2026, 11, 6, 12, 1, 0, 0, time.UTC))
			}
			if runs := e.runs(tc.task.ID); len(runs) != 1 || runs[0].Status != store.StatusHostUnreachable {
				t.Fatalf("runs = %+v, want one host_unreachable", runs)
			}
			before := len(e.starter.Calls())
			e.starter.setUnreachable(false)
			e.clock.Set(tc.at)
			e.sched.CatchUpTarget(e.ctx, "vps")
			if calls := e.starter.Calls()[before:]; len(calls) != 0 {
				t.Errorf("Start calls = %+v, want none", calls)
			}
		})
	}
}

// A paused task's missed fire is not caught up, as with a restart.
func TestCatchUpTargetSkipsAPausedTask(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
	tk := onVPS(task("hourly", "@hourly"))
	e.setTasks(valid(tk))
	e.starter.setUnreachable(true)
	e.runUntil(time.Date(2026, 11, 6, 12, 1, 0, 0, time.UTC))

	e.starter.setUnreachable(false)
	tk.PausedUntil = "indefinite"
	e.setTasks(valid(tk))
	e.clock.Set(time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC))
	e.sched.CatchUpTarget(e.ctx, "vps")
	if n := len(e.starter.Calls()); n != 1 {
		t.Errorf("%d Start calls, want only the host_unreachable fire", n)
	}
}
