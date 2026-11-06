package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// syncExec runs the editor right away instead of suspending a terminal.
func syncExec(cmd *exec.Cmd, done func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg { return done(cmd.Run()) }
}

// editorModel is a test model whose editor runs script with the file as $1.
func editorModel(f *fakeBackend, script string) Model {
	m := newTestModel(f, plain())
	m.exec = syncExec
	m.editor = []string{"sh", "-c", script, "sh"}
	return m
}

func TestEditPromptSavesTheNewPrompt(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksGet, peppolDetail())
	f.set(api.MethodTasksUpdate, peppolDetail())
	log := filepath.Join(t.TempDir(), "seen")
	// Record what the editor got: the old prompt, the file name and mode.
	script := `cat "$1" > ` + log + `; echo "$1" >> ` + log + `; stat -c %a "$1" >> ` + log + `; printf 'new prompt\n' > "$1"`
	tm := start(t, editorModel(f, script))
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("E"))
	p := waitCall(t, f, api.MethodTasksUpdate, 1)[0].(api.TaskWriteParams)
	d := peppolDetail()
	if p.ID != d.Task.ID || p.Version != "v1" || p.Prompt != "new prompt\n" || p.Task.Name != d.Task.Name {
		t.Fatalf("update = %+v", p)
	}
	waitText(t, tm, "saved the prompt of peppol-sh-deps")
	finalView(t, tm)
	seen, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(seen), "\n"), "\n")
	if got := strings.Join(lines[:3], "\n") + "\n"; got != d.Prompt {
		t.Errorf("editor got %q, want %q", got, d.Prompt)
	}
	path := lines[3]
	if !strings.HasSuffix(path, ".md") || lines[4] != "600" {
		t.Errorf("temp file %s mode %s", path, lines[4])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp file %s left behind: %v", path, err)
	}
}

func TestEditPromptUnchanged(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksGet, peppolDetail())
	tm := start(t, editorModel(f, "true"))
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("E"))
	waitText(t, tm, "prompt unchanged")
	finalView(t, tm)
	if n := len(f.callsOf(api.MethodTasksUpdate)); n != 0 {
		t.Fatalf("%d updates", n)
	}
}

func TestEditPromptConflictAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *api.Error
		want string
	}{
		{"conflict", &api.Error{Code: api.CodeVersionMismatch, Message: "changed"},
			"prompt not saved: peppol-sh-deps changed on disk while you edited; press E to edit the current version"},
		{"validation", &api.Error{Code: api.CodeValidation, Message: "task is invalid",
			Data: map[string]any{"fields": map[string]any{"prompt": []any{"prompt is empty"}}}},
			"prompt not saved: prompt: prompt is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture()
			f.set(api.MethodTasksGet, peppolDetail())
			f.fail(api.MethodTasksUpdate, tc.err)
			tm := start(t, editorModel(f, `printf '' > "$1"`))
			selectTask(t, tm, order, "peppol-sh-deps")
			tm.Send(key("E"))
			waitText(t, tm, tc.want)
			finalView(t, tm)
		})
	}
}

func TestEditorFailureIsReported(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksGet, peppolDetail())
	tm := start(t, editorModel(f, "exit 3"))
	selectTask(t, tm, order, "peppol-sh-deps")
	tm.Send(key("E"))
	waitText(t, tm, "editor sh: exit status 3")
	finalView(t, tm)
	if n := len(f.callsOf(api.MethodTasksUpdate)); n != 0 {
		t.Fatalf("%d updates", n)
	}
}

func TestFormPromptOpensTheEditor(t *testing.T) {
	f := fixture()
	f.set(api.MethodTasksUpdate, peppolDetail())
	tm := openEdit(t, f, editorModel(f, `printf 'line one\nline two\n' > "$1"`))
	tabs(tm, fPrompt-fName)
	tm.Send(key("enter"))
	waitText(t, tm, `"line one" (2 lines)`)
	tm.Send(ctrl(tea.KeyCtrlS))
	p := waitCall(t, f, api.MethodTasksUpdate, 1)[0].(api.TaskWriteParams)
	if p.Prompt != "line one\nline two\n" {
		t.Fatalf("prompt = %q", p.Prompt)
	}
	finalView(t, tm)
}

func TestEditorArgv(t *testing.T) {
	m := Model{}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := m.editorArgv(); strings.Join(got, " ") != "vi" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("EDITOR", "nano -w")
	if got := m.editorArgv(); strings.Join(got, "|") != "nano|-w" {
		t.Errorf("$EDITOR = %q", got)
	}
	t.Setenv("VISUAL", "code --wait")
	if got := m.editorArgv(); strings.Join(got, "|") != "code|--wait" {
		t.Errorf("$VISUAL = %q", got)
	}
	t.Setenv("VISUAL", `"/opt/My Editor/bin/edit" --title 'a prompt'`)
	if got := m.editorArgv(); strings.Join(got, "|") != "/opt/My Editor/bin/edit|--title|a prompt" {
		t.Errorf("quoted $VISUAL = %q", got)
	}
	t.Setenv("VISUAL", `ed 'open`) // does not parse: split on spaces
	if got := m.editorArgv(); strings.Join(got, "|") != "ed|'open" {
		t.Errorf("unparsable $VISUAL = %q", got)
	}
	t.Setenv("VISUAL", "code --wait")
	m.editor = []string{"ed"}
	if got := m.editorArgv(); strings.Join(got, "|") != "ed" {
		t.Errorf("option = %q", got)
	}
}

// TestEditorTempFileIsMadeInACmd checks that the key only schedules the
// editor: the temp file is made by a Cmd (so a program that quits first
// leaves none) and is removed once the editor ends.
func TestEditorTempFileIsMadeInACmd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	m := editorModel(fixture(), `printf 'edited\n' > "$1"`)
	m, _ = m.openNewForm()
	m.form.focus = fPrompt
	m.form.focusField()
	entries := func() []os.DirEntry {
		t.Helper()
		e, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	m, cmd := m.update(key("enter"))
	if cmd == nil {
		t.Fatal("enter on the prompt gave no command")
	}
	if e := entries(); len(e) != 0 {
		t.Fatalf("the key made %d files before any Cmd ran", len(e))
	}
	msg := cmd()
	if e := entries(); len(e) != 1 {
		t.Fatalf("the Cmd made %d files, want the temp file", len(e))
	}
	m, cmd = m.update(msg)
	if cmd == nil {
		t.Fatal("no editor command")
	}
	m, _ = m.update(cmd())
	if m.form.prompt != "edited\n" {
		t.Errorf("prompt = %q", m.form.prompt)
	}
	if e := entries(); len(e) != 0 {
		t.Errorf("temp file left behind: %v", e)
	}
}

func TestEditorTempFileIsRemovedWhenTheEditorFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	m := editorModel(fixture(), "exit 3")
	m, _ = m.openNewForm()
	m.form.focus = fPrompt
	m.form.focusField()
	m, cmd := m.update(key("enter"))
	m, cmd = m.update(cmd())
	m, _ = m.update(cmd())
	if !strings.Contains(m.message, "exit status 3") {
		t.Errorf("message = %q", m.message)
	}
	if e, _ := os.ReadDir(dir); len(e) != 0 {
		t.Errorf("temp file left behind: %v", e)
	}
}
