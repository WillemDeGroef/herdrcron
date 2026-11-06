package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// Editor messages.
type (
	// promptGotMsg is the task read for E.
	promptGotMsg struct {
		det api.TaskDetail
		err error
	}
	// promptEditedMsg is E's prompt back from the editor.
	promptEditedMsg struct {
		det  api.TaskDetail
		text string
		err  error
	}
	// formPromptMsg is the form's prompt back from the editor.
	formPromptMsg struct {
		seq  int
		text string
		err  error
	}
)

// editorArgv is the editor command: Options.Editor, else $VISUAL, else
// $EDITOR, else vi. The variables are split into words as a shell would
// (splitWords), or on spaces when they do not parse.
func (m Model) editorArgv() []string {
	if len(m.editor) > 0 {
		return m.editor
	}
	for _, env := range []string{"VISUAL", "EDITOR"} {
		v := os.Getenv(env)
		words, err := splitWords(v)
		if err != nil {
			words = strings.Fields(v)
		}
		if len(words) > 0 {
			return words
		}
	}
	return []string{"vi"}
}

// editorReadyMsg is a temp file written for the editor, to run it on.
type editorReadyMsg struct {
	path string
	done func(text string, err error) tea.Msg
}

// openEditor writes text to a private temp file, runs the editor on it in
// the foreground and hands the file's new text to done. The file is made
// by the returned Cmd, so none is made if the program quits first, and is
// removed once the editor ends.
func (m Model) openEditor(text string, done func(text string, err error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		f, err := os.CreateTemp("", "herdrcron-prompt-*.md") // mode 0600
		if err != nil {
			return done("", fmt.Errorf("editor: %w", err))
		}
		path := f.Name()
		_, werr := f.WriteString(text)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(path)
			return done("", fmt.Errorf("editor: %w", werr))
		}
		return editorReadyMsg{path: path, done: done}
	}
}

// runEditor runs the editor on the temp file of msg and removes the file
// when the editor ends.
func (m Model) runEditor(msg editorReadyMsg) tea.Cmd {
	argv, path, done := m.editorArgv(), msg.path, msg.done
	cmd := exec.Command(argv[0], append(argv[1:len(argv):len(argv)], path)...)
	return m.exec(cmd, func(err error) tea.Msg {
		defer func() { _ = os.Remove(path) }()
		if err != nil {
			return done("", fmt.Errorf("editor %s: %w", argv[0], err))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return done("", fmt.Errorf("editor: %w", err))
		}
		return done(string(data), nil)
	})
}

// editPrompt opens task id's prompt in the editor and saves it.
func (m Model) editPrompt(id string) (Model, tea.Cmd) {
	ctx, b := m.ctx, m.backend
	return m, func() tea.Msg {
		var det api.TaskDetail
		if err := b.Call(ctx, api.MethodTasksGet, api.TaskIDParams{ID: id}, &det); err != nil {
			return promptGotMsg{err: fmt.Errorf("read %s: %w", id, err)}
		}
		if det.Task.ID == "" {
			det.Task.ID = id
		}
		return promptGotMsg{det: det}
	}
}

// editFormPrompt opens the form's prompt in the editor.
func (m Model) editFormPrompt() tea.Cmd {
	seq := m.form.seq
	return m.openEditor(m.form.prompt, func(text string, err error) tea.Msg {
		return formPromptMsg{seq: seq, text: text, err: err}
	})
}

// savePrompt writes det's task back with a new prompt.
func (m Model) savePrompt(det api.TaskDetail, prompt string) tea.Cmd {
	ctx, b, id := m.ctx, m.backend, det.Task.ID
	p := api.TaskWriteParams{ID: id, Version: det.Version, Task: det.Task, Prompt: prompt}
	return func() tea.Msg {
		err := b.Call(ctx, api.MethodTasksUpdate, p, nil)
		var aerr *api.Error
		switch {
		case err == nil:
			return doneMsg{text: "saved the prompt of " + id, reload: true}
		case errors.As(err, &aerr) && aerr.Code == api.CodeVersionMismatch:
			return doneMsg{err: fmt.Errorf("prompt not saved: %s changed on disk while you edited; press E to edit the current version", id)}
		case errors.As(err, &aerr) && aerr.Code == api.CodeValidation:
			fields := validationFields(aerr)
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				parts = append(parts, k+": "+strings.Join(fields[k], ", "))
			}
			if len(parts) == 0 {
				parts = []string{aerr.Message}
			}
			return doneMsg{err: fmt.Errorf("prompt not saved: %s", strings.Join(parts, "; "))}
		}
		return doneMsg{err: fmt.Errorf("prompt not saved: %w", err)}
	}
}

// editorMsg handles the editor's result messages.
func (m Model) editorMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case editorReadyMsg:
		return m, m.runEditor(msg), true
	case promptGotMsg:
		if msg.err != nil {
			m.setMessage("", msg.err)
			return m, nil, true
		}
		det := msg.det
		return m, m.openEditor(det.Prompt, func(text string, err error) tea.Msg {
			return promptEditedMsg{det: det, text: text, err: err}
		}), true
	case promptEditedMsg:
		switch {
		case msg.err != nil:
			m.setMessage("", msg.err)
			return m, nil, true
		case msg.text == msg.det.Prompt:
			m.setMessage("prompt unchanged", nil)
			return m, nil, true
		}
		return m, m.savePrompt(msg.det, msg.text), true
	case formPromptMsg:
		if msg.seq != m.form.seq {
			return m, nil, true
		}
		if msg.err != nil {
			m.setMessage("", msg.err)
			return m, nil, true
		}
		if msg.text != m.form.prompt {
			m.form.edited("prompt")
		}
		m.form.prompt = msg.text
		return m, nil, true
	}
	return m, nil, false
}
