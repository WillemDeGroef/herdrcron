package scheduler_test

import (
	"fmt"
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

// A newer manual run after host_unreachable must not hide the missed scheduled
// fire: catch-up uses the latest non-manual run.
func TestCatchUpTargetIgnoresManualRunsThatWouldHideHostUnreachable(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
	e.setTasks(valid(onVPS(task("hourly", "@hourly"))))
	e.starter.setUnreachable(true)
	e.runUntil(time.Date(2026, 11, 6, 12, 1, 0, 0, time.UTC))
	runs := e.runs("hourly")
	if len(runs) != 1 || runs[0].Status != store.StatusHostUnreachable {
		t.Fatalf("runs = %+v, want one host_unreachable", runs)
	}
	hu := runs[0]
	e.starter.setUnreachable(false)
	manual := store.Run{
		ID: store.NewRunID(hu.StartedAt.Add(time.Second)), TaskID: "hourly", Target: "vps",
		Trigger: store.TriggerManual, ScheduledAt: time.Time{}, StartedAt: hu.StartedAt.Add(time.Minute),
		EndedAt: hu.StartedAt.Add(2 * time.Minute), Status: store.StatusDone,
	}
	if err := e.store.InsertRun(e.ctx, manual); err != nil {
		t.Fatal(err)
	}
	e.clock.Set(time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC))
	e.sched.CatchUpTarget(e.ctx, "vps")
	calls := e.starter.Calls()[1:]
	if len(calls) != 1 || calls[0].trigger != store.TriggerCatchUp || !calls[0].scheduledAt.Equal(hu.ScheduledAt) {
		t.Fatalf("Start calls after catch-up = %+v, want one catch_up for scheduled_at %v", calls, hu.ScheduledAt)
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

// adoptOldest makes the fake runner adopt the task's oldest run, as the
// runner does for a launch whose reply was lost: the row is running again.
func (e *env) adoptOldest(id string) store.Run {
	e.t.Helper()
	runs := e.runs(id)
	lost := runs[0]
	lost.Status, lost.EndedAt = store.StatusRunning, time.Time{}
	e.starter.setAdopt(func(config.Task) (store.Run, bool, error) {
		if err := e.store.UpdateRun(e.ctx, lost); err != nil {
			return store.Run{}, false, err
		}
		e.starter.setAdopt(nil)
		return lost, true, nil
	})
	return lost
}

// Issue #37: the 12:00 launch reached the target and its reply was lost,
// then the 13:00 and 14:00 fires found the target down. When it answers,
// the runner adopts the 12:00 run, so no catch_up run starts: not now, not
// when the target answers again, and not after a daemon restart.
func TestCatchUpTargetStartsNothingWhenTheLostRunIsAdopted(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
	tasks := valid(onVPS(task("hourly", "@hourly")))
	e.setTasks(tasks)
	e.starter.setUnreachable(true)
	e.runUntil(time.Date(2026, 11, 6, 14, 30, 0, 0, time.UTC))
	lost := e.adoptOldest("hourly")

	e.starter.setUnreachable(false)
	e.sched.CatchUpTarget(e.ctx, "vps")
	if calls := e.starter.Calls()[3:]; len(calls) != 0 {
		t.Fatalf("Start calls after the lost run was adopted = %+v, want none", calls)
	}
	if got := e.runs("hourly")[0]; got.Status != store.StatusRunning {
		t.Fatalf("lost run = %s, want adopted as running", got.Status)
	}
	latest := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	if st := e.state("hourly"); st.LastRunID != lost.ID || !st.LastFireAt.Equal(latest) {
		t.Errorf("task_state = run %s fire %v, want the adopted run %s for the %v fire", st.LastRunID, st.LastFireAt, lost.ID, latest)
	}

	e.sched.CatchUpTarget(e.ctx, "vps")
	e.restart()
	e.setTasks(tasks)
	e.sched.CatchUpTarget(e.ctx, "vps")
	if n := len(e.starter.Calls()); n != 3 {
		t.Errorf("%d Start calls after the target answered again and a restart, want 3", n)
	}
}

// A lost run is adopted whatever the missed policy says: it is running.
func TestCatchUpTargetAdoptsForATaskWithMissedSkip(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
	skip := onVPS(task("skip", "@hourly"))
	skip.Missed = "skip"
	e.setTasks(valid(skip))
	e.starter.setUnreachable(true)
	e.runUntil(time.Date(2026, 11, 6, 12, 1, 0, 0, time.UTC))
	e.adoptOldest("skip")

	e.starter.setUnreachable(false)
	e.sched.CatchUpTarget(e.ctx, "vps")
	if got := e.runs("skip")[0]; got.Status != store.StatusRunning {
		t.Errorf("lost run = %s, want adopted as running", got.Status)
	}
	if n := len(e.starter.Calls()); n != 1 {
		t.Errorf("%d Start calls, want only the host_unreachable fire", n)
	}
}

// While the runner cannot tell whether the lost run exists (the target
// dropped again), no catch_up run starts; the next time the target answers
// and nothing is adopted, the fire is caught up.
func TestCatchUpTargetWaitsWhenAdoptionCannotBeChecked(t *testing.T) {
	e := newEnv(t, time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC))
	e.setTasks(valid(onVPS(task("hourly", "@hourly"))))
	e.starter.setUnreachable(true)
	e.runUntil(time.Date(2026, 11, 6, 12, 1, 0, 0, time.UTC))
	e.starter.setUnreachable(false)
	e.starter.setAdopt(func(config.Task) (store.Run, bool, error) {
		return store.Run{}, false, fmt.Errorf("pane list: target vps unreachable")
	})

	e.clock.Set(time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC))
	e.sched.CatchUpTarget(e.ctx, "vps")
	if n := len(e.starter.Calls()); n != 1 {
		t.Fatalf("%d Start calls while adoption cannot be checked, want 1", n)
	}

	e.starter.setAdopt(nil)
	e.sched.CatchUpTarget(e.ctx, "vps")
	if calls := e.starter.Calls()[1:]; len(calls) != 1 || calls[0].trigger != store.TriggerCatchUp {
		t.Errorf("Start calls once the target answers = %+v, want one catch_up", calls)
	}
}
