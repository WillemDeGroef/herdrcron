package tui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// startForm starts m like start, then makes the terminal tall enough for the form.
func startForm(t *testing.T, m Model) *teatest.TestModel {
	t.Helper()
	tm := start(t, m)
	tm.Send(tea.WindowSizeMsg{Width: 110, Height: 32})
	return tm
}

// typeText types s into the focused field.
func typeText(tm *teatest.TestModel, s string) {
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func ctrl(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// tabs moves the form focus n fields down.
func tabs(tm *teatest.TestModel, n int) {
	for range n {
		tm.Send(ctrl(tea.KeyTab))
	}
}

// Form field order, for tabbing.
const (
	fID = iota
	fName
	fTarget
	fCwd
	fSchedule
	fTimezone
	fEnabled
	fPausedUntil
	fCommand
	fArgs
	fPromptFile
	fTimeout
	fOverlap
	fMissed
	fOnDone
	fOnBlocked
	fEnv
	fPrompt
)

// peppolDetail is the tasks.get answer for peppol-sh-deps.
func peppolDetail() api.TaskDetail {
	return api.TaskDetail{
		Summary: api.TaskSummary{ID: "peppol-sh-deps", Target: "local", Schedule: "0 7 * * 1", State: api.StateIdle, Enabled: true},
		Task: api.TaskDef{
			ID: "peppol-sh-deps", Name: "peppol.sh dependency update", Target: "local", Cwd: "~/src/peppol-sh",
			Schedule: "0 7 * * 1", Timezone: "Europe/Brussels", Enabled: true,
			Agent:      api.AgentDef{Command: "claude", Args: []string{"--permission-mode", "acceptEdits", "--append-system-prompt", "be brief"}},
			PromptFile: "prompts/peppol-sh-deps.md", Timeout: "2h",
			Overlap: "skip", Missed: "run_once", OnDone: "close_tab", OnBlocked: "notify",
			Env: map[string]string{"FOO": "bar"},
		},
		Prompt:  "Update the dependencies.\nRun the tests.\nOpen a PR.\n",
		Version: "v1",
	}
}

func TestNewFormCreatesWithDefaults(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksCreate, api.TaskDetail{Task: api.TaskDef{ID: "my-task"}})
	tm := startForm(t, newTestModel(f, plain()))
	tm.Send(key("n"))
	waitText(t, tm, "new task")
	typeText(tm, "my-task")
	tabs(tm, fSchedule)
	typeText(tm, "@daily")
	tabs(tm, fCommand-fSchedule)
	typeText(tm, "claude")
	tabs(tm, fTimeout-fCommand)
	typeText(tm, "30m")
	tm.Send(ctrl(tea.KeyCtrlS))
	got := waitCall(t, f, api.MethodTasksCreate, 1)
	want := api.TaskWriteParams{Task: api.TaskDef{
		ID: "my-task", Target: "local", Schedule: "@daily", Enabled: true,
		Agent: api.AgentDef{Command: "claude"}, Timeout: "30m",
		Overlap: "skip", Missed: "run_once", OnDone: "close_tab", OnBlocked: "notify",
	}}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("create params = %+v, want %+v", got[0], want)
	}
	waitText(t, tm, "saved my-task")
	finalView(t, tm)
}

// openEdit starts the TUI on f and opens the edit form of peppol-sh-deps.
func openEdit(t *testing.T, f *fakeBackend, m Model) *teatest.TestModel {
	t.Helper()
	f.set(api.MethodTasksGet, peppolDetail())
	tm := startForm(t, m)
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("e"))
	waitText(t, tm, "edit task peppol-sh-deps")
	return tm
}

func TestEditFormUpdatesKeepingVersionAndPrompt(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksUpdate, peppolDetail())
	tm := openEdit(t, f, newTestModel(f, plain()))
	if got := waitCall(t, f, api.MethodTasksGet, 1)[0]; got != (api.TaskIDParams{ID: "peppol-sh-deps"}) {
		t.Fatalf("tasks.get params = %+v", got)
	}
	// The focus starts on name (the id is fixed).
	typeText(tm, " (weekly)")
	tm.Send(ctrl(tea.KeyCtrlS))
	got := waitCall(t, f, api.MethodTasksUpdate, 1)
	d := peppolDetail()
	d.Task.Name += " (weekly)"
	want := api.TaskWriteParams{ID: "peppol-sh-deps", Version: "v1", Task: d.Task, Prompt: d.Prompt}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("update params =\n%+v\nwant\n%+v", got[0], want)
	}
	waitText(t, tm, "saved peppol-sh-deps")
	if n := len(f.callsOf(api.MethodTasksList)); n < 2 {
		waitCall(t, f, api.MethodTasksList, 2) // reloaded after the save
	}
	finalView(t, tm)
}

func TestFormShowsValidationErrorsByField(t *testing.T) {
	for name, data := range map[string]any{
		"typed": api.ValidationErrorData{Fields: map[string][]string{
			"schedule": {"schedule \"bad\": nope"}, "agent.command": {"agent.command is missing"}, "task": {"something odd"}}},
		// As it arrives over the socket.
		"decoded": map[string]any{"fields": map[string]any{
			"schedule": []any{"schedule \"bad\": nope"}, "agent.command": []any{"agent.command is missing"}, "task": []any{"something odd"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixture()
			f.fail(api.MethodTasksUpdate, &api.Error{Code: api.CodeValidation, Message: "task is invalid", Data: data})
			tm := openEdit(t, f, newTestModel(f, plain()))
			typeText(tm, "x")
			tm.Send(ctrl(tea.KeyCtrlS))
			waitText(t, tm, "something odd")
			view := finalView(t, tm)
			for _, want := range []string{
				`schedule      0 7 * * 1  ✗ schedule "bad": nope`,
				"agent command claude  ✗ agent.command is missing",
				"✗ task: something odd",
				"peppol.sh dependency updatex", // the input is kept
			} {
				if !strings.Contains(view, want) {
					t.Errorf("view lacks %q:\n%s", want, view)
				}
			}
		})
	}
}

func TestFormVersionMismatchExplainsAndReloads(t *testing.T) {
	f := fixture()
	f.fail(api.MethodTasksUpdate, &api.Error{Code: api.CodeVersionMismatch, Message: "task peppol-sh-deps changed since it was read; read it again"})
	tm := openEdit(t, f, newTestModel(f, plain()))
	typeText(tm, "x")
	tm.Send(ctrl(tea.KeyCtrlS))
	waitText(t, tm, "changed on disk since this form was opened")

	// The file on disk now has another name and version.
	d := peppolDetail()
	d.Task.Name, d.Version = "renamed by hand", "v2"
	f.set(api.MethodTasksGet, d)
	// ctrl+r would lose the edits: it asks first.
	tm.Send(ctrl(tea.KeyCtrlR))
	waitText(t, tm, "Reload peppol-sh-deps from disk and lose your edits? y/n")
	tm.Send(key("n"))
	waitText(t, tm, "cancelled")
	if n := len(f.callsOf(api.MethodTasksGet)); n != 1 {
		t.Fatalf("tasks.get called %d times after declining the reload, want 1", n)
	}
	tm.Send(ctrl(tea.KeyCtrlR))
	waitText(t, tm, "lose your edits? y/n")
	tm.Send(key("y"))
	waitText(t, tm, "renamed by hand")
	f.set(api.MethodTasksUpdate, d)
	tm.Send(ctrl(tea.KeyCtrlS))
	got := waitCall(t, f, api.MethodTasksUpdate, 2)
	if p := got[1].(api.TaskWriteParams); p.Version != "v2" || p.Task.Name != "renamed by hand" {
		t.Fatalf("second update = %+v, want version v2 and the reloaded name", p)
	}
	waitText(t, tm, "saved peppol-sh-deps")
	finalView(t, tm)
}

func TestFormEscAsksBeforeDiscarding(t *testing.T) {
	f := fixture()
	tm := openEdit(t, f, newTestModel(f, plain()))
	typeText(tm, "x")
	tm.Send(key("esc"))
	waitText(t, tm, "Discard the changes to peppol-sh-deps? y/n")
	tm.Send(key("n"))
	waitText(t, tm, "cancelled")
	tm.Send(key("esc"))
	waitText(t, tm, "Discard the changes")
	tm.Send(key("y"))
	waitText(t, tm, "discarded the changes")
	view := finalView(t, tm)
	if !strings.Contains(view, "TARGET") {
		t.Fatalf("not back on the main list:\n%s", view)
	}
	if n := len(f.callsOf(api.MethodTasksUpdate)); n != 0 {
		t.Fatalf("%d updates", n)
	}
}

func TestFormEscWithoutChangesCloses(t *testing.T) {
	f := fixture()
	tm := openEdit(t, f, newTestModel(f, plain()))
	tm.Send(key("esc"))
	waitText(t, tm, "TARGET")
	finalView(t, tm)
}

func TestFormSchedulePreview(t *testing.T) {
	for _, tc := range []struct {
		name, defaultTZ, tz, want string
	}{
		{"the TUI's zone", "", "", "next: Fri Nov 6 09:00 UTC · Sat Nov 7 09:00 UTC · Sun Nov 8 09:00 UTC"},
		{"config timezone", "Europe/Brussels", "", "next: Fri Nov 6 08:00 UTC · Sat Nov 7 08:00 UTC · Sun Nov 8 08:00 UTC"},
		{"task timezone", "Europe/Brussels", "America/New_York", "next: Thu Nov 5 14:00 UTC · Fri Nov 6 14:00 UTC · Sat Nov 7 14:00 UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			st := api.StatusResult{Targets: []api.TargetStatus{{Name: "local"}}, Timezone: tc.defaultTZ}
			f.set(api.MethodStatus, st)
			tm := startForm(t, newTestModel(f, plain()))
			tm.Send(key("n"))
			tabs(tm, fTimezone)
			typeText(tm, tc.tz)
			tm.Send(ctrl(tea.KeyShiftTab))
			typeText(tm, "0 9 * * *")
			waitText(t, tm, tc.want)
			finalView(t, tm)
		})
	}
}

func TestFormScheduleErrorInline(t *testing.T) {
	tm := startForm(t, newTestModel(fixture(), plain()))
	tm.Send(key("n"))
	tabs(tm, fSchedule)
	typeText(tm, "0 9 * *")
	waitText(t, tm, "✗ expected exactly 5 fields")
	finalView(t, tm)
}

func TestFormPickersToggleArgsAndEnv(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksCreate, api.TaskDetail{})
	tm := startForm(t, newTestModel(f, plain()))
	tm.Send(key("n"))
	typeText(tm, "t1")
	tabs(tm, fTarget)
	tm.Send(ctrl(tea.KeyRight)) // local → vps
	tabs(tm, fEnabled-fTarget)
	tm.Send(key("space")) // off
	tabs(tm, fArgs-fEnabled)
	typeText(tm, `--a 'b c' "d\"e" f\ g`)
	tabs(tm, fOverlap-fArgs)
	tm.Send(ctrl(tea.KeyLeft)) // skip → parallel
	tabs(tm, fOnDone-fOverlap)
	tm.Send(key("space")) // close_tab → keep_tab
	tabs(tm, fEnv-fOnDone)
	typeText(tm, `FOO=1 BAR='x y'`)
	tm.Send(ctrl(tea.KeyCtrlS))
	p := waitCall(t, f, api.MethodTasksCreate, 1)[0].(api.TaskWriteParams)
	finalView(t, tm)
	if p.Task.Target != "vps" || p.Task.Enabled || p.Task.Overlap != "parallel" || p.Task.OnDone != "keep_tab" {
		t.Errorf("task = %+v", p.Task)
	}
	if want := []string{"--a", "b c", `d"e`, "f g"}; !reflect.DeepEqual(p.Task.Agent.Args, want) {
		t.Errorf("args = %q, want %q", p.Task.Agent.Args, want)
	}
	if want := map[string]string{"FOO": "1", "BAR": "x y"}; !reflect.DeepEqual(p.Task.Env, want) {
		t.Errorf("env = %v, want %v", p.Task.Env, want)
	}
}

func TestFormRefusesBadArgsAndEnvLocally(t *testing.T) {
	f := fixture()
	tm := startForm(t, newTestModel(f, plain()))
	tm.Send(key("n"))
	tabs(tm, fArgs)
	typeText(tm, `'open`)
	tabs(tm, fEnv-fArgs)
	typeText(tm, `NOEQUALS`)
	tm.Send(ctrl(tea.KeyCtrlS))
	waitText(t, tm, `✗ "NOEQUALS" is not KEY=VALUE`)
	view := finalView(t, tm)
	if !strings.Contains(view, "✗ unterminated ' quote") {
		t.Errorf("no args error:\n%s", view)
	}
	if n := len(f.callsOf(api.MethodTasksCreate)); n != 0 {
		t.Fatalf("%d creates", n)
	}
}

func TestFormGolden(t *testing.T) {
	f := fixture()
	tm := openEdit(t, f, newTestModel(f, plain()))
	tabs(tm, fSchedule-fName)
	waitText(t, tm, "› schedule")
	golden.RequireEqual(t, []byte(finalView(t, tm)))
}

func TestEditFormReadErrorStaysOnTheList(t *testing.T) {
	f := fixture()
	tm := startForm(t, newTestModel(f, plain()))
	f.fail(api.MethodTasksGet, &api.Error{Code: api.CodeInvalidParams, Message: "unknown task"})
	tm.Send(key("e"))
	waitText(t, tm, "read release-notes: unknown task")
	if v := finalView(t, tm); !strings.Contains(v, "TARGET") {
		t.Fatalf("not on the list:\n%s", v)
	}
}

// TestFormEditClearsThatFieldsDaemonErrors checks that editing a field
// drops the daemon's errors for it (the schedule preview shows again),
// while the other fields keep theirs until the next save.
func TestFormEditClearsThatFieldsDaemonErrors(t *testing.T) {
	f := fixture()
	f.fail(api.MethodTasksUpdate, &api.Error{Code: api.CodeValidation, Message: "task is invalid", Data: api.ValidationErrorData{
		Fields: map[string][]string{"schedule": {"schedule is odd"}, "agent.command": {"agent.command is missing"}}}})
	tm := openEdit(t, f, newTestModel(f, plain()))
	typeText(tm, "x")
	tm.Send(ctrl(tea.KeyCtrlS))
	waitText(t, tm, "schedule is odd")
	tabs(tm, fSchedule-fName)
	tm.Send(ctrl(tea.KeyBackspace))
	typeText(tm, "2")
	waitText(t, tm, "next: Tue Nov 10 06:00 UTC")
	view := finalView(t, tm)
	if strings.Contains(view, "schedule is odd") {
		t.Errorf("the schedule's error stayed after editing it:\n%s", view)
	}
	if !strings.Contains(view, "agent.command is missing") {
		t.Errorf("another field's error went away:\n%s", view)
	}
}

func TestFormPickerEditClearsItsDaemonErrors(t *testing.T) {
	f := fixture()
	f.fail(api.MethodTasksUpdate, &api.Error{Code: api.CodeValidation, Message: "task is invalid", Data: api.ValidationErrorData{
		Fields: map[string][]string{"overlap": {"overlap is odd"}}}})
	tm := openEdit(t, f, newTestModel(f, plain()))
	typeText(tm, "x")
	tm.Send(ctrl(tea.KeyCtrlS))
	waitText(t, tm, "overlap is odd")
	tabs(tm, fOverlap-fName)
	tm.Send(ctrl(tea.KeyRight))
	waitText(t, tm, "‹ queue ›")
	if view := finalView(t, tm); strings.Contains(view, "overlap is odd") {
		t.Errorf("the overlap error stayed after changing it:\n%s", view)
	}
}

func TestOpenEditFormCoversTheListWhileLoading(t *testing.T) {
	m := newTestModel(fixture(), plain())
	m, _ = m.openEditForm("peppol-sh-deps")
	if m.screen != screenForm {
		t.Fatalf("screen = %v, want the form while the task loads", m.screen)
	}
	if !strings.Contains(m.View(), "loading the task") {
		t.Fatalf("view =\n%s", m.View())
	}
	// enter is the list's "open detail". On the loading form it must not leave.
	m, _ = m.key(key("enter"))
	if m.screen != screenForm {
		t.Fatalf("enter left the loading form for screen %v", m.screen)
	}
	// A load for this open is applied. One from a form the user already
	// closed is not, and it does not pull them back.
	m, _, handled := m.formMsg(formLoadedMsg{seq: m.form.seq, det: peppolDetail()})
	if !handled || m.screen != screenForm || !strings.Contains(m.View(), "edit task peppol-sh-deps") {
		t.Fatalf("the load did not open the edit form:\n%s", m.View())
	}
	seq := m.form.seq
	m = m.closeForm()
	m, _, _ = m.formMsg(formLoadedMsg{seq: seq, det: peppolDetail()})
	if m.screen != screenMain {
		t.Fatalf("a load for a closed form reopened it")
	}
}

func TestOpenNewFormResetsTheErrorFlag(t *testing.T) {
	m := newTestModel(fixture(), plain())
	m.setMessage("", errors.New("old failure"))
	m, _ = m.openNewForm()
	if m.message != "" || m.msgErr {
		t.Errorf("message %q, msgErr %v after opening the form", m.message, m.msgErr)
	}
}
