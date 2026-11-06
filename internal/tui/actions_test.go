package tui

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// waitCall waits until f has received n calls of method and returns them.
func waitCall(t *testing.T, f *fakeBackend, method string, n int) []any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := f.callsOf(method)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d %s calls, got %d", n, method, len(got))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// selectTask moves the cursor down to id (the list starts at the top).
func selectTask(t *testing.T, tm *teatest.TestModel, order []string, id string) {
	t.Helper()
	for _, o := range order {
		if o == id {
			return
		}
		tm.Send(key("down"))
	}
	t.Fatalf("%s not in %v", id, order)
}

var order = []string{"release-notes", "factory-coordinator", "e2e-timeout", "nightly-e2e", "peppol-sh-deps", "peppolnews-ingest"}

func TestSpaceTogglesEnabled(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksSetEnabled, api.TaskSummary{ID: "peppolnews-ingest", Target: "vps", State: api.StateIdle, Enabled: true})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "peppolnews-ingest")
	tm.Send(key("space"))
	got := waitCall(t, f, api.MethodTasksSetEnabled, 1)
	if want := (api.SetEnabledParams{ID: "peppolnews-ingest", Enabled: true}); got[0] != want {
		t.Fatalf("set_enabled params = %+v, want %+v", got[0], want)
	}
	waitText(t, tm, "enabled peppolnews-ingest")

	tm.Send(key("up")) // peppol-sh-deps, enabled
	tm.Send(key("space"))
	got = waitCall(t, f, api.MethodTasksSetEnabled, 2)
	if want := (api.SetEnabledParams{ID: "peppol-sh-deps", Enabled: false}); got[1] != want {
		t.Fatalf("set_enabled params = %+v, want %+v", got[1], want)
	}
	finalView(t, tm)
}

func TestPauseMenu(t *testing.T) {
	tomorrow8 := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC).Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		keys  []string
		until string
	}{
		{"one hour", []string{"p", "1"}, ts(time.Hour)},
		{"tomorrow 08:00", []string{"p", "2"}, tomorrow8},
		{"date", []string{"p", "3", "2", "0", "2", "6", "-", "1", "1", "-", "1", "0", " ", "0", "9", ":", "3", "0", "enter"},
			time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC).Format(time.RFC3339)},
		{"indefinitely", []string{"p", "4"}, api.PauseIndefinite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			f.set(api.MethodTasksPause, api.TaskSummary{ID: "e2e-timeout", State: api.StatePaused, PausedUntil: tc.until})
			tm := start(t, newTestModel(f, plain()))
			selectTask(t, tm, order, "e2e-timeout")
			for _, k := range tc.keys {
				tm.Send(key(k))
			}
			got := waitCall(t, f, api.MethodTasksPause, 1)
			if want := (api.PauseParams{ID: "e2e-timeout", Until: tc.until}); got[0] != want {
				t.Fatalf("pause params = %+v, want %+v", got[0], want)
			}
			finalView(t, tm)
		})
	}
}

func TestPauseMenuShowsChoices(t *testing.T) {
	tm := start(t, newTestModel(fixture(), plain()))
	tm.Send(key("p"))
	waitText(t, tm, "indefinitely")
	view := finalView(t, tm)
	for _, s := range []string{"1 h", "tomorrow 08:00", "a date", "indefinitely"} {
		if !strings.Contains(view, s) {
			t.Errorf("pause menu lacks %q:\n%s", s, view)
		}
	}
}

func TestPauseDateInThePastIsRefused(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	for _, k := range []string{"p", "3", "2", "0", "2", "0", "-", "0", "1", "-", "0", "1", "enter"} {
		tm.Send(key(k))
	}
	waitText(t, tm, "in the past")
	finalView(t, tm)
	if got := f.callsOf(api.MethodTasksPause); len(got) != 0 {
		t.Fatalf("pause called with %+v", got)
	}
}

func TestPOnPausedTaskResumes(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksResume, api.TaskSummary{ID: "nightly-e2e", State: api.StateIdle, Enabled: true})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "nightly-e2e")
	tm.Send(key("p"))
	got := waitCall(t, f, api.MethodTasksResume, 1)
	if want := (api.TaskIDParams{ID: "nightly-e2e"}); got[0] != want {
		t.Fatalf("resume params = %+v, want %+v", got[0], want)
	}
	finalView(t, tm)
}

func TestPauseAll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice string
		method string
		target string
	}{
		{"selected target", "1", api.MethodPauseAll, "buildbox"},
		{"all targets", "2", api.MethodPauseAll, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			f.set(tc.method, api.GlobalPause{PausedAll: true})
			tm := start(t, newTestModel(f, plain()))
			tm.Send(key("P"))
			tm.Send(key(tc.choice))
			got := waitCall(t, f, tc.method, 1)
			if want := (api.TargetParams{Target: tc.target}); got[0] != want {
				t.Fatalf("%s params = %+v, want %+v", tc.method, got[0], want)
			}
			// The state is read again: the banner comes from status.
			waitCall(t, f, api.MethodStatus, 2)
			finalView(t, tm)
		})
	}
}

func TestPauseAllOffersResumeAndShowsBanner(t *testing.T) {
	f := fixture()
	f.set(api.MethodStatus, api.StatusResult{GlobalPause: api.GlobalPause{PausedAll: true, PausedTargets: []string{"buildbox"}}})
	f.set(api.MethodResumeAll, api.GlobalPause{})
	tm := start(t, newTestModel(f, plain()))
	tm.Send(key("P"))
	waitText(t, tm, "resume all targets")
	tm.Send(key("1"))
	got := waitCall(t, f, api.MethodResumeAll, 1)
	if want := (api.TargetParams{Target: "buildbox"}); got[0] != want {
		t.Fatalf("resume_all params = %+v, want %+v", got[0], want)
	}
	if view := finalView(t, tm); !strings.Contains(view, "ALL TARGETS PAUSED") {
		t.Errorf("no global pause banner:\n%s", view)
	}
}

func TestRunNow(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksRunNow, api.RunNowResult{RunSummary: api.RunSummary{ID: "r9", TaskID: "e2e-timeout", Status: "running"}})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "e2e-timeout")
	tm.Send(key("r"))
	got := waitCall(t, f, api.MethodTasksRunNow, 1)
	if want := (api.RunNowParams{ID: "e2e-timeout"}); got[0] != want {
		t.Fatalf("run_now params = %+v, want %+v", got[0], want)
	}
	waitText(t, tm, "started run r9")
	finalView(t, tm)
}

func TestRunNowAsksBeforeParallel(t *testing.T) {
	f := fixture()
	f.fail(api.MethodTasksRunNow, &api.Error{Code: api.CodeRunActive, Message: "task has an active run"})
	f.set(api.MethodTasksRunNow, api.RunNowResult{RunSummary: api.RunSummary{ID: "r9", TaskID: "factory-coordinator", Status: "running"}})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("r"))
	waitText(t, tm, "in parallel?")
	tm.Send(key("y"))
	got := waitCall(t, f, api.MethodTasksRunNow, 2)
	if want := (api.RunNowParams{ID: "factory-coordinator", ForceParallel: true}); got[1] != want {
		t.Fatalf("second run_now params = %+v, want %+v", got[1], want)
	}
	finalView(t, tm)
}

func TestStopAsksFirst(t *testing.T) {
	f := fixture()
	f.set(api.MethodRunsStop, api.RunSummary{ID: "r3", Status: "stopped"})
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("s"))
	waitText(t, tm, "Stop run r3 of factory-coordinator? y/n")
	tm.Send(key("n"))
	waitText(t, tm, "cancelled")
	if got := f.callsOf(api.MethodRunsStop); len(got) != 0 {
		t.Fatalf("stop called after n: %+v", got)
	}
	tm.Send(key("s"))
	tm.Send(key("y"))
	got := waitCall(t, f, api.MethodRunsStop, 1)
	if want := (api.RunsStopParams{RunID: "r3"}); got[0] != want {
		t.Fatalf("stop params = %+v, want %+v", got[0], want)
	}
	finalView(t, tm)
}

func TestStopWithoutActiveRun(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "e2e-timeout")
	tm.Send(key("s"))
	waitText(t, tm, "no active run")
	finalView(t, tm)
}

// TestQuitLeavesDaemonAlone checks q quits without changing anything:
// only reads reach the daemon.
func TestQuitLeavesDaemonAlone(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	tm.Send(key("q"))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		switch c.Method {
		case api.MethodStatus, api.MethodTasksList, api.MethodRunsList:
		default:
			t.Errorf("quitting called %s", c.Method)
		}
	}
}

func TestHelp(t *testing.T) {
	tm := start(t, newTestModel(fixture(), plain()))
	tm.Send(key("?"))
	waitText(t, tm, "any key closes")
	tm.Send(key("x"))
	view := finalView(t, tm)
	if !strings.Contains(view, "factory-coordinator") {
		t.Fatalf("help did not close:\n%s", view)
	}
}

func TestFilter(t *testing.T) {
	tm := start(t, newTestModel(fixture(), plain()))
	for _, k := range []string{"/", "p", "e", "p", "p", "o", "l", "enter"} {
		tm.Send(key(k))
	}
	waitText(t, tm, "filter: peppol")
	view := finalView(t, tm)
	if strings.Contains(view, "factory-coordinator") || !strings.Contains(view, "peppol-sh-deps") || !strings.Contains(view, "peppolnews-ingest") {
		t.Fatalf("filter peppol shows:\n%s", view)
	}
}

func TestGroupByTarget(t *testing.T) {
	tm := start(t, newTestModel(fixture(), plain()))
	tm.Send(key("g"))
	waitText(t, tm, "▾ buildbox")
	view := finalView(t, tm)
	assertOrder(t, view, []string{"▾ buildbox", "release-notes", "▾ local", "e2e-timeout", "peppol-sh-deps",
		"▾ vps", "factory-coordinator", "nightly-e2e", "peppolnews-ingest"})
}

func TestColours(t *testing.T) {
	tm := start(t, newTestModel(fixture(), coloured()))
	tm.Send(key("down")) // the cursor (reverse video) off the blocked row
	view := finalView(t, tm)
	const (
		warning  = "33"
		critical = "31"
		accent   = "36"
		dim      = "2"
	)
	lines := map[string]string{}
	for _, l := range strings.Split(view, "\n") {
		for _, id := range order {
			if strings.Contains(l, id) {
				lines[id] = l
			}
		}
	}
	for id, code := range map[string]string{
		"release-notes":     warning,
		"e2e-timeout":       critical, // last run timed out
		"nightly-e2e":       dim,
		"peppolnews-ingest": dim,
	} {
		if !hasSGR(lines[id], code) {
			t.Errorf("row %s lacks %q: %q", id, code, lines[id])
		}
	}
	if !hasSGR(lines["nightly-e2e"], critical) {
		t.Errorf("failed last run of nightly-e2e is not critical: %q", lines["nightly-e2e"])
	}
	if l := lines["factory-coordinator"]; !hasSGR(l, accent) {
		t.Errorf("running row lacks the accent: %q", l)
	}
	if l := lines["peppol-sh-deps"]; hasSGR(l, warning) || hasSGR(l, critical) || hasSGR(l, dim) {
		t.Errorf("idle row is coloured: %q", l)
	}
}

// hasSGR reports whether line sets the SGR attribute code (alone or
// combined with others, as in "\x1b[2;31m").
func hasSGR(line, code string) bool {
	return regexp.MustCompile(`\x1b\[(\d+;)*` + code + `(;\d+)*m`).MatchString(line)
}

func TestEventsUpdateTheList(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	f.push(t, api.EventTaskChanged, api.TaskChangedEvent{ID: "peppol-sh-deps", Task: &api.TaskSummary{
		ID: "peppol-sh-deps", Target: "local", Schedule: "30 6 * * 2", State: api.StateIdle, Enabled: true}})
	waitText(t, tm, "Tue 06:30")
	f.push(t, api.EventTaskChanged, api.TaskChangedEvent{ID: "brand-new", Task: &api.TaskSummary{
		ID: "brand-new", Target: "local", Schedule: "@daily", State: api.StateIdle, Enabled: true}})
	waitText(t, tm, "brand-new")
	f.push(t, api.EventTaskChanged, api.TaskChangedEvent{ID: "peppolnews-ingest", Deleted: true})
	f.push(t, api.EventRunChanged, api.RunChangedEvent{Run: api.RunSummary{
		ID: "r3", TaskID: "factory-coordinator", Status: "done", StartedAt: ts(-2 * time.Minute), EndedAt: ts(-time.Minute)}})
	f.push(t, api.EventTargetChanged, api.TargetChangedEvent{Target: api.TargetStatus{Name: "buildbox", Reachability: "reachable"}})
	waitText(t, tm, "buildbox ●")
	view := finalView(t, tm)
	if strings.Contains(view, "peppolnews-ingest") {
		t.Errorf("deleted task still shown:\n%s", view)
	}
	if !strings.Contains(view, "✓ 1m ago") {
		t.Errorf("ended run not shown as last run:\n%s", view)
	}
}

func TestEndedRunIsNoLongerStoppable(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	f.push(t, api.EventRunChanged, api.RunChangedEvent{Run: api.RunSummary{
		ID: "r5", TaskID: "release-notes", Status: "stopped", StartedAt: ts(-time.Hour), EndedAt: ts(0)}})
	waitText(t, tm, "✗ just now")
	tm.Send(key("s"))
	waitText(t, tm, "no active run")
	finalView(t, tm)
}

func TestReconnectsAfterLostStream(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	f.mu.Lock()
	close(f.events)
	f.events = make(chan api.Notification, 16)
	f.mu.Unlock()
	waitText(t, tm, "reconnecting")
	// It subscribes again and re-reads the state.
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		subs := f.subs
		f.mu.Unlock()
		if subs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never subscribed again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitCall(t, f, api.MethodTasksList, 2)
	view := finalView(t, tm)
	if strings.Contains(view, "reconnecting") {
		t.Errorf("banner stays after reconnect:\n%s", view)
	}
	if !reflect.DeepEqual(len(f.callsOf(api.MethodRunsList)), 2) {
		t.Errorf("runs not re-read")
	}
}

func TestLongListScrollsToTheCursor(t *testing.T) {
	f := fixture()
	var tasks []api.TaskSummary
	for i := range 40 {
		tasks = append(tasks, api.TaskSummary{ID: fmt.Sprintf("task-%02d", i), Target: "local", Schedule: "@hourly",
			State: api.StateIdle, Enabled: true, NextFireAt: ts(time.Duration(i+1) * time.Minute)})
	}
	f.set(api.MethodTasksList, api.TasksListResult{Tasks: tasks})
	tm := teatest.NewTestModel(t, newTestModel(f, plain()), teatest.WithInitialTermSize(100, 12))
	waitText(t, tm, "task-00")
	tm.Send(key("G"))
	waitText(t, tm, "task-39")
	view := finalView(t, tm)
	if strings.Contains(view, "task-00") {
		t.Errorf("first task still shown after G:\n%s", view)
	}
	if n := strings.Count(view, "\n") + 1; n != 12 {
		t.Errorf("view has %d lines, want the terminal height 12:\n%s", n, view)
	}
	if !strings.Contains(view, "q quit") {
		t.Errorf("footer scrolled away:\n%s", view)
	}
}

func TestPOnGloballyPausedTaskPointsToP(t *testing.T) {
	f := fixture()
	tasks := f.results[api.MethodTasksList].(api.TasksListResult)
	tasks.Tasks[0].State = api.StatePaused // peppol-sh-deps, paused only by a global pause
	f.set(api.MethodTasksList, tasks)
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("p"))
	waitText(t, tm, "P resumes it")
	finalView(t, tm)
	if got := f.callsOf(api.MethodTasksResume); len(got) != 0 {
		t.Fatalf("resume called: %+v", got)
	}
}

func TestScheduleText(t *testing.T) {
	for in, want := range map[string]string{
		"0 7 * * 1": "Mon 07:00", "30 2 * * *": "02:30 daily", "0 9 * * 0": "Sun 09:00", "0 9 * * 7": "Sun 09:00",
		"*/30 * * * *": "*/30 * * * *", "@every 2h": "@every 2h", "0 7 1 * *": "0 7 1 * *", "0 7 * * 1-5": "0 7 * * 1-5",
	} {
		if got := scheduleText(in); got != want {
			t.Errorf("scheduleText(%q) = %q, want %q", in, got, want)
		}
	}
}
