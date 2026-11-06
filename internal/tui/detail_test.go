package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// startDetailSize is start with room for the detail screen.
func startDetailSize(t *testing.T, m Model) *teatest.TestModel {
	t.Helper()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 50))
	waitText(t, tm, "factory-coordinator")
	return tm
}

// detailFixture is fixture with a tasks.get answer for factory-coordinator.
func detailFixture() *fakeBackend {
	f := fixture()
	f.set(api.MethodTasksGet, coordinatorDetail())
	return f
}

func coordinatorDetail() api.TaskDetail {
	return api.TaskDetail{
		Summary: api.TaskSummary{ID: "factory-coordinator", Target: "vps", Schedule: "*/30 * * * *", State: api.StateRunning, Enabled: true},
		Task: api.TaskDef{
			ID: "factory-coordinator", Name: "Factory coordinator", Target: "vps", Cwd: "/srv/factory",
			Schedule: "*/30 * * * *", Timezone: "Europe/Brussels", Enabled: true,
			Agent:      api.AgentDef{Command: "claude", Args: []string{"--model", "opus"}},
			PromptFile: "prompts/factory-coordinator.md", Timeout: "45m",
			Overlap: "skip", Missed: "run_once", OnDone: "close", OnBlocked: "notify",
			Env: map[string]string{"GH_TOKEN": "s3cr3t-value", "FACTORY_MODE": "full"},
		},
		Version: "v1",
		Runs: []api.RunSummary{
			{ID: "r3", TaskID: "factory-coordinator", Status: "running", Trigger: "schedule", StartedAt: ts(-2 * time.Minute)},
			{ID: "r2x", TaskID: "factory-coordinator", Status: "failed", Trigger: "manual", StartedAt: ts(-time.Hour), EndedAt: ts(-time.Hour + 90*time.Second), ExitReason: "agent exited 1"},
			{ID: "r1x", TaskID: "factory-coordinator", Status: "done", Trigger: "schedule", StartedAt: ts(-2 * time.Hour), EndedAt: ts(-2*time.Hour + 12*time.Minute)},
		},
	}
}

func TestDetailShowsDefinition(t *testing.T) {
	f := detailFixture()
	tm := startDetailSize(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("enter"))
	got := waitCall(t, f, api.MethodTasksGet, 1)
	if want := (api.TaskIDParams{ID: "factory-coordinator"}); got[0] != want {
		t.Fatalf("tasks.get params = %+v, want %+v", got[0], want)
	}
	waitText(t, tm, "/srv/factory")
	view := finalView(t, tm)
	for _, s := range []string{
		"Factory coordinator", "vps", "*/30 * * * *", "Europe/Brussels", "claude --model opus",
		"prompts/factory-coordinator.md", "45m", "skip", "run_once", "close", "notify",
		"FACTORY_MODE", "GH_TOKEN", "running",
	} {
		if !strings.Contains(view, s) {
			t.Errorf("detail lacks %q:\n%s", s, view)
		}
	}
	if strings.Contains(view, "s3cr3t-value") {
		t.Errorf("detail shows an env value:\n%s", view)
	}
}

// openCoordinatorDetail starts the TUI and opens the detail of factory-coordinator.
func openCoordinatorDetail(t *testing.T, f *fakeBackend) *teatest.TestModel {
	t.Helper()
	tm := startDetailSize(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("enter"))
	waitText(t, tm, "/srv/factory")
	return tm
}

func TestDetailShowsNextFiveFires(t *testing.T) {
	tm := openCoordinatorDetail(t, detailFixture())
	view := finalView(t, tm)
	for _, s := range []string{"Thu Nov 5 10:30", "Thu Nov 5 11:00", "Thu Nov 5 12:30"} {
		if !strings.Contains(view, s) {
			t.Errorf("detail lacks fire %q:\n%s", s, view)
		}
	}
	if strings.Contains(view, "Thu Nov 5 13:00") {
		t.Errorf("detail shows more than 5 fires:\n%s", view)
	}
}

func TestDetailShowsRuns(t *testing.T) {
	tm := openCoordinatorDetail(t, detailFixture())
	view := finalView(t, tm)
	for _, line := range [][]string{
		{"running", "schedule", "Nov 5 09:58", "2m00s"},
		{"failed", "manual", "Nov 5 09:00", "1m30s", "agent exited 1"},
		{"done", "schedule", "Nov 5 08:00", "12m00s"},
	} {
		if !lineWith(view, line...) {
			t.Errorf("no run line with %q:\n%s", line, view)
		}
	}
}

// lineWith reports whether one line of view holds all of parts, in order.
func lineWith(view string, parts ...string) bool {
	for _, l := range strings.Split(view, "\n") {
		rest, ok := l, true
		for _, p := range parts {
			i := strings.Index(rest, p)
			if i < 0 {
				ok = false
				break
			}
			rest = rest[i+len(p):]
		}
		if ok {
			return true
		}
	}
	return false
}

func TestDetailOfInvalidTaskShowsError(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksGet, api.TaskDetail{
		Summary: api.TaskSummary{ID: "factory-coordinator", State: api.StateInvalid, Error: "schedule: bad cron"},
		Task:    api.TaskDef{ID: "factory-coordinator", Schedule: "nope", Cwd: "/srv/factory"},
	})
	tm := openCoordinatorDetail(t, f)
	view := finalView(t, tm)
	for _, s := range []string{"invalid", "schedule: bad cron"} {
		if !strings.Contains(view, s) {
			t.Errorf("detail lacks %q:\n%s", s, view)
		}
	}
}

func TestDetailEnterOnRunShowsItsOutput(t *testing.T) {
	f := detailFixture()
	f.set(api.MethodRunsOutput, api.RunsOutputResult{RunID: "r2x", Status: "failed", Output: "step one\nError: agent exited 1\n"})
	tm := openCoordinatorDetail(t, f)
	tm.Send(key("j"))
	tm.Send(key("enter"))
	got := waitCall(t, f, api.MethodRunsOutput, 1)
	if p := got[0].(api.RunsOutputParams); p.RunID != "r2x" {
		t.Fatalf("runs.output params = %+v, want run r2x", p)
	}
	waitText(t, tm, "Error: agent exited 1")
	tm.Send(key("esc"))
	waitText(t, tm, "Next fires")
	tm.Send(key("esc"))
	waitText(t, tm, "space on/off")
	finalView(t, tm)
}

func TestDetailCursorMovesUpAndDown(t *testing.T) {
	f := detailFixture()
	f.set(api.MethodRunsOutput, api.RunsOutputResult{RunID: "r1x", Status: "done", Output: "all good"})
	tm := openCoordinatorDetail(t, f)
	for _, k := range []string{"down", "down", "down", "up", "k", "j", "j"} {
		tm.Send(key(k))
	}
	tm.Send(key("enter"))
	got := waitCall(t, f, api.MethodRunsOutput, 1)
	if p := got[0].(api.RunsOutputParams); p.RunID != "r1x" {
		t.Fatalf("runs.output params = %+v, want run r1x (the last, clamped)", p)
	}
	finalView(t, tm)
}

func TestDetailQGoesBackToTheList(t *testing.T) {
	tm := openCoordinatorDetail(t, detailFixture())
	tm.Send(key("q"))
	waitText(t, tm, "space on/off")
	view := finalView(t, tm)
	if !strings.Contains(view, "peppolnews-ingest") {
		t.Errorf("q did not go back to the list:\n%s", view)
	}
}

func TestDetailRefreshesOnItsTasksEvents(t *testing.T) {
	f := detailFixture()
	tm := openCoordinatorDetail(t, f)
	waitCall(t, f, api.MethodTasksGet, 1)

	// Another task's events leave the detail alone.
	f.push(t, api.EventRunChanged, api.RunChangedEvent{Run: api.RunSummary{
		ID: "r6", TaskID: "e2e-timeout", Status: "done", StartedAt: ts(-40 * time.Minute), EndedAt: ts(0)}})
	f.push(t, api.EventTaskChanged, api.TaskChangedEvent{ID: "e2e-timeout", Task: &api.TaskSummary{ID: "e2e-timeout", State: api.StateIdle}})

	d := coordinatorDetail()
	d.Runs[0].Status, d.Runs[0].EndedAt, d.Runs[0].ExitReason = "done", ts(0), "agent finished"
	f.set(api.MethodTasksGet, d)
	f.push(t, api.EventRunChanged, api.RunChangedEvent{Run: d.Runs[0]})
	waitText(t, tm, "agent finished")
	if n := len(f.callsOf(api.MethodTasksGet)); n != 2 {
		t.Errorf("tasks.get called %d times, want 2 (once on open, once for its own run)", n)
	}

	d.Task.Cwd = "/srv/elsewhere"
	f.set(api.MethodTasksGet, d)
	f.push(t, api.EventTaskChanged, api.TaskChangedEvent{ID: "factory-coordinator", Task: &d.Summary})
	waitText(t, tm, "/srv/elsewhere")
	finalView(t, tm)
}

func TestDetailIgnoresEventsWhenClosed(t *testing.T) {
	f := detailFixture()
	tm := openCoordinatorDetail(t, f)
	tm.Send(key("esc"))
	waitText(t, tm, "space on/off")
	f.push(t, api.EventTaskChanged, api.TaskChangedEvent{ID: "factory-coordinator", Task: &api.TaskSummary{
		ID: "factory-coordinator", Target: "vps", Schedule: "0 9 * * *", State: api.StateIdle, Enabled: true}})
	waitText(t, tm, "09:00 daily")
	finalView(t, tm)
	if n := len(f.callsOf(api.MethodTasksGet)); n != 1 {
		t.Errorf("tasks.get called %d times after closing the detail, want 1", n)
	}
}

func TestDetailGolden(t *testing.T) {
	tm := openCoordinatorDetail(t, detailFixture())
	golden.RequireEqual(t, []byte(finalView(t, tm)))
}

func TestDetailScrollsOnASmallTerminal(t *testing.T) {
	f := detailFixture()
	f.set(api.MethodRunsOutput, api.RunsOutputResult{RunID: "r2x", Status: "failed", Output: "x"})
	tm := start(t, newTestModel(f, plain())) // 16 lines high
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("enter"))
	waitText(t, tm, "Factory coordinator") // the definition shows first
	tm.Send(key("j"))                      // selecting a run brings it into sight
	waitText(t, tm, "agent exited 1")
	view := finalView(t, tm)
	if n := strings.Count(view, "\n") + 1; n > 16 {
		t.Errorf("detail is %d lines, taller than the terminal:\n%s", n, view)
	}
	if !strings.Contains(view, "esc back") {
		t.Errorf("footer scrolled away:\n%s", view)
	}
}

func TestDetailPageKeysScroll(t *testing.T) {
	tm := start(t, newTestModel(detailFixture(), plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("enter"))
	waitText(t, tm, "Factory coordinator")
	tm.Send(tea.KeyMsg{Type: tea.KeyPgDown})
	tm.Send(tea.KeyMsg{Type: tea.KeyPgDown}) // clamped at the end
	waitText(t, tm, "Nov 5 08:00")
	view := finalView(t, tm)
	if strings.Contains(view, "Factory coordinator") {
		t.Errorf("pgdown did not scroll:\n%s", view)
	}
}

// TestDetailRunOutputClosesColours checks that a colour the pane left open
// does not bleed into the message line and footer.
func TestDetailRunOutputClosesColours(t *testing.T) {
	f := detailFixture()
	f.set(api.MethodRunsOutput, liveOutput("plain\n\x1b[31mred and never closed\n"))
	tm := openCoordinatorDetail(t, f)
	tm.Send(key("enter"))
	waitText(t, tm, "red and never closed")
	view := finalView(t, tm)
	_, after, _ := strings.Cut(view, "red and never closed")
	reset := strings.Index(after, "\x1b[0m")
	if foot := strings.Index(after, "esc back"); reset < 0 || reset > foot {
		t.Errorf("no colour reset between the output and the footer:\n%q", view)
	}
}
