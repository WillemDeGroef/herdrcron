package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// pausedUntilNov10 is the end of the timed pause in the fixtures: Tuesday
// 10 Nov 2026 08:00 UTC, five days after the test clock.
var pausedUntilNov10 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC).Format(time.RFC3339)

// editTask changes one task of the fixture's tasks.list answer.
func editTask(t *testing.T, f *fakeBackend, id string, edit func(*api.TaskSummary)) {
	t.Helper()
	f.mu.Lock()
	list := f.results[api.MethodTasksList].(api.TasksListResult)
	f.mu.Unlock()
	tasks := append([]api.TaskSummary(nil), list.Tasks...)
	for i := range tasks {
		if tasks[i].ID == id {
			edit(&tasks[i])
			f.set(api.MethodTasksList, api.TasksListResult{Tasks: tasks})
			return
		}
	}
	t.Fatalf("no task %s in the fixture", id)
}

// globalPause sets the fixture's status to a global pause: on every target,
// or on the named ones.
func globalPause(f *fakeBackend, targets ...string) {
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 3*24*3600 + 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: "reachable"},
			{Name: "vps", Reachability: "reachable"},
			{Name: "buildbox", Reachability: "unreachable"},
		},
		GlobalPause: api.GlobalPause{PausedAll: len(targets) == 0, PausedTargets: append([]string{}, targets...)},
	})
}

// TestMainScreenPausesGolden checks the NEXT column of paused tasks: the
// daemon still reports their next cron time, and the list must not show it.
func TestMainScreenPausesGolden(t *testing.T) {
	t.Run("task pauses", func(t *testing.T) {
		f := fixture() // nightly-e2e is paused until Nov 10
		editTask(t, f, "peppol-sh-deps", func(s *api.TaskSummary) {
			s.State, s.PausedUntil = api.StatePaused, api.PauseIndefinite
		})
		// A manual run shows as running over paused; the pause still holds.
		editTask(t, f, "factory-coordinator", func(s *api.TaskSummary) { s.PausedUntil = pausedUntilNov10 })
		tm := start(t, newTestModel(f, plain()))
		golden.RequireEqual(t, []byte(finalView(t, tm)))
	})
	t.Run("all targets paused", func(t *testing.T) {
		f := fixture()
		globalPause(f)
		for _, id := range []string{"peppol-sh-deps", "e2e-timeout"} {
			editTask(t, f, id, func(s *api.TaskSummary) { s.State = api.StatePaused })
		}
		tm := start(t, newTestModel(f, plain()))
		golden.RequireEqual(t, []byte(finalView(t, tm)))
	})
	t.Run("one target paused", func(t *testing.T) {
		f := fixture()
		globalPause(f, "vps")
		tm := start(t, newTestModel(f, plain()))
		golden.RequireEqual(t, []byte(finalView(t, tm)))
	})
}

// pausedDetail is coordinatorDetail with the task itself paused.
func pausedDetail(until string) api.TaskDetail {
	d := coordinatorDetail()
	d.Summary.State, d.Summary.PausedUntil, d.Task.PausedUntil = api.StatePaused, until, until
	d.Runs = d.Runs[1:] // no active run
	return d
}

func openDetail(t *testing.T, f *fakeBackend, d api.TaskDetail) *teatest.TestModel {
	t.Helper()
	f.set(api.MethodTasksGet, d)
	return openCoordinatorDetail(t, f)
}

// TestDetailPausesGolden checks the "Next fires" preview of the detail
// screen under each kind of pause.
func TestDetailPausesGolden(t *testing.T) {
	t.Run("paused indefinitely", func(t *testing.T) {
		tm := openDetail(t, fixture(), pausedDetail(api.PauseIndefinite))
		golden.RequireEqual(t, []byte(finalView(t, tm)))
	})
	t.Run("paused until a date", func(t *testing.T) {
		tm := openDetail(t, fixture(), pausedDetail(pausedUntilNov10))
		golden.RequireEqual(t, []byte(finalView(t, tm)))
	})
	t.Run("all targets paused", func(t *testing.T) {
		f := fixture()
		globalPause(f)
		tm := openDetail(t, f, coordinatorDetail()) // a run still going under the pause
		golden.RequireEqual(t, []byte(finalView(t, tm)))
	})
}

func TestDetailTargetPauseHidesFires(t *testing.T) {
	f := fixture()
	globalPause(f, "vps")
	view := finalView(t, openDetail(t, f, coordinatorDetail()))
	if !strings.Contains(view, "target vps is paused: no fires until resumed") {
		t.Errorf("detail does not say the target is paused:\n%s", view)
	}
	if strings.Contains(view, "Thu Nov 5 10:30") {
		t.Errorf("detail previews a fire under a global pause:\n%s", view)
	}
}

func TestDetailPauseOnAnotherTargetKeepsFires(t *testing.T) {
	f := fixture()
	globalPause(f, "local")
	view := finalView(t, openDetail(t, f, coordinatorDetail()))
	if !strings.Contains(view, "Thu Nov 5 10:30") || strings.Contains(view, "is paused") {
		t.Errorf("a pause on local changed the preview of a vps task:\n%s", view)
	}
}

// A fire at the very instant the pause ends runs: the daemon pauses only
// fire times before paused_until.
func TestDetailPausedUntilIncludesAFireAtThePauseEnd(t *testing.T) {
	view := finalView(t, openDetail(t, fixture(), pausedDetail(pausedUntilNov10)))
	i, j := strings.Index(view, "paused until Tue Nov 10 08:00 UTC, then"), strings.Index(view, "Tue Nov 10 08:00 UTC\n")
	if i < 0 || j < i {
		t.Errorf("no fire at the pause end after the pause line:\n%s", view)
	}
	if strings.Contains(view, "Tue Nov 10 07:30") {
		t.Errorf("detail previews a fire inside the pause:\n%s", view)
	}
}

// An expired pause the daemon has not cleared yet no longer holds fires.
func TestDetailExpiredPauseShowsFires(t *testing.T) {
	d := pausedDetail(ts(-time.Minute))
	d.Summary.State = api.StateIdle
	view := finalView(t, openDetail(t, fixture(), d))
	if !strings.Contains(view, "Thu Nov 5 10:30") || strings.Contains(view, "paused until Thu") {
		t.Errorf("an expired pause still holds the preview:\n%s", view)
	}
}

// Same moment, with the summary still saying paused: the time has passed,
// so the fires are shown. A paused task with no time is the other case
// (a global pause the status read has not shown) and lists none.
func TestDetailEndedPauseShowsFires(t *testing.T) {
	view := finalView(t, openDetail(t, fixture(), pausedDetail(ts(-time.Minute))))
	if !strings.Contains(view, "Thu Nov 5 10:30") || strings.Contains(view, "no fires until resumed") {
		t.Errorf("an ended pause still hides the fires:\n%s", view)
	}
}

func TestDetailPausedStateWithoutVisiblePauseHidesFires(t *testing.T) {
	d := coordinatorDetail()
	d.Summary.State = api.StatePaused
	view := finalView(t, openDetail(t, fixture(), d))
	if !strings.Contains(view, "paused: no fires until resumed") {
		t.Errorf("detail does not say the task is paused:\n%s", view)
	}
	if strings.Contains(view, "Thu Nov 5 10:30") {
		t.Errorf("detail previews a fire for a paused task with no end:\n%s", view)
	}
}

func TestFormSchedulePreviewFollowsPauses(t *testing.T) {
	open := func(t *testing.T, f *fakeBackend, until string) string {
		t.Helper()
		d := peppolDetail()
		d.Task.PausedUntil = until
		f.set(api.MethodTasksGet, d)
		tm := startForm(t, newTestModel(f, plain()))
		selectTask(t, tm, order, "peppol-sh-deps")
		tm.Send(key("e"))
		waitText(t, tm, "edit task peppol-sh-deps")
		tabs(tm, fSchedule-fName)
		waitText(t, tm, "› schedule")
		return finalView(t, tm)
	}
	t.Run("paused until a date", func(t *testing.T) {
		view := open(t, fixture(), pausedUntilNov10)
		if want := "paused until Tue Nov 10 08:00 UTC, then: Mon Nov 16 06:00 UTC"; !strings.Contains(view, want) {
			t.Errorf("preview lacks %q:\n%s", want, view)
		}
		if strings.Contains(view, "Mon Nov 9 06:00") {
			t.Errorf("preview shows a fire inside the pause:\n%s", view)
		}
	})
	t.Run("paused indefinitely", func(t *testing.T) {
		view := open(t, fixture(), api.PauseIndefinite)
		if !strings.Contains(view, "paused indefinitely: no fires until resumed") || strings.Contains(view, "Mon Nov 9 06:00") {
			t.Errorf("preview ignores the indefinite pause:\n%s", view)
		}
	})
	t.Run("all targets paused", func(t *testing.T) {
		f := fixture()
		globalPause(f)
		view := open(t, f, "")
		if !strings.Contains(view, "all targets are paused: no fires until resumed") || strings.Contains(view, "Mon Nov 9 06:00") {
			t.Errorf("preview ignores the global pause:\n%s", view)
		}
	})
}

// An @every interval with no daemon next-fire time counts from the end of
// the pause: the TUI does not know the phase.
func TestDetailPausedUntilCountsEveryFromThePauseEnd(t *testing.T) {
	d := pausedDetail(pausedUntilNov10)
	d.Summary.Schedule, d.Task.Schedule = "@every 2h", "@every 2h"
	view := finalView(t, openDetail(t, fixture(), d))
	if !strings.Contains(view, "then\n  Tue Nov 10 10:00 UTC\n  Tue Nov 10 12:00 UTC") {
		t.Errorf("@every does not count from the pause end:\n%s", view)
	}
}

// When the daemon has a next fire time, @every keeps that phase. A fire at
// the pause end runs; the preview does not start a new phase there.
func TestDetailEveryPauseKeepsTheDaemonPhase(t *testing.T) {
	d := pausedDetail(pausedUntilNov10)
	d.Summary.Schedule, d.Task.Schedule = "@every 2h", "@every 2h"
	// 10:00, 12:00, … lands on Tue 08:00, the pause end.
	d.Summary.NextFireAt = now.Format(time.RFC3339)
	view := finalView(t, openDetail(t, fixture(), d))
	if !strings.Contains(view, "then\n  Tue Nov 10 08:00 UTC\n  Tue Nov 10 10:00 UTC") {
		t.Errorf("@every did not keep the daemon phase:\n%s", view)
	}
	if strings.Contains(view, "Tue Nov 10 06:00") {
		t.Errorf("detail previews a fire inside the pause:\n%s", view)
	}
}

func TestEveryPhaseAfterSkipsThePause(t *testing.T) {
	until := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	h := hold{until: until}
	// 11:00, 13:00, … : the first fire the pause does not cover is 09:00.
	after, ok := everyPhaseAfter("@every 2h", now.Add(time.Hour).Format(time.RFC3339), h)
	if !ok {
		t.Fatal("everyPhaseAfter rejected a usable next fire")
	}
	fires, err := nextFires("@every 2h", "", "", time.UTC, after, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{until.Add(time.Hour), until.Add(3 * time.Hour)}
	if len(fires) != len(want) || !fires[0].Equal(want[0]) || !fires[1].Equal(want[1]) {
		t.Errorf("fires = %v, want %v", fires, want)
	}
}

func nextCell(t *testing.T, view, id string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, id) {
			return line
		}
	}
	t.Fatalf("no row for %s:\n%s", id, view)
	return ""
}

// An expired pause the daemon has not cleared yet shows the next fire.
func TestMainScreenExpiredPauseShowsNextFire(t *testing.T) {
	f := fixture()
	editTask(t, f, "e2e-timeout", func(s *api.TaskSummary) { s.PausedUntil = ts(-time.Minute) })
	row := nextCell(t, finalView(t, start(t, newTestModel(f, plain()))), "e2e-timeout")
	if !strings.HasSuffix(strings.TrimSpace(row), "Thu 10:20") {
		t.Errorf("row = %q, want the next fire Thu 10:20", row)
	}
}

// The summary can still say paused in the moment paused_until passes. The
// pause no longer holds, so NEXT is the next fire, not "paused".
func TestMainScreenEndedPauseShowsNextFire(t *testing.T) {
	f := fixture()
	editTask(t, f, "e2e-timeout", func(s *api.TaskSummary) {
		s.State, s.PausedUntil = api.StatePaused, ts(-time.Minute)
	})
	row := nextCell(t, finalView(t, start(t, newTestModel(f, plain()))), "e2e-timeout")
	if !strings.HasSuffix(strings.TrimSpace(row), "Thu 10:20") {
		t.Errorf("row = %q, want the next fire Thu 10:20", row)
	}
}

// A task paused with no time of its own (a global pause the status read
// has not shown yet) shows "paused", not a fire time.
func TestMainScreenPausedStateWithoutVisiblePauseShowsPaused(t *testing.T) {
	f := fixture()
	editTask(t, f, "e2e-timeout", func(s *api.TaskSummary) { s.State = api.StatePaused })
	row := nextCell(t, finalView(t, start(t, newTestModel(f, plain()))), "e2e-timeout")
	if !strings.HasSuffix(strings.TrimSpace(row), "40m ago  paused") {
		t.Errorf("row = %q, want NEXT paused", row)
	}
}
