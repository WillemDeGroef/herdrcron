package tui

import (
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/until"
)

// pauseAllChoice is one entry of the P menu.
type pauseAllChoice struct {
	label  string
	method string // api.MethodPauseAll or api.MethodResumeAll
	target string // empty: all targets
}

func (m Model) key(msg tea.KeyMsg) (Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeHelp:
		m.mode = modeList
		return m, nil
	case modeFilter:
		return m.filterKey(msg)
	case modePauseMenu:
		return m.pauseMenuKey(msg)
	case modePauseDate:
		return m.pauseDateKey(msg)
	case modePauseAll:
		return m.pauseAllKey(msg)
	case modeConfirm:
		switch msg.String() {
		case "y", "Y":
			m.mode = modeList
			return m, m.confirm.yes
		case "n", "N", "esc", "q":
			m.mode = modeList
			m.setMessage("cancelled", nil)
		}
		return m, nil
	}
	return m.listKey(msg)
}

func (m Model) listKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "?":
		m.mode = modeHelp
		return m, nil
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-max(m.listHeight(), 1))
	case "pgdown":
		m.move(max(m.listHeight(), 1))
	case "home":
		m.move(-len(m.tasks))
	case "end", "G":
		m.move(len(m.tasks))
	case "g":
		m.grouped = !m.grouped
	case "/":
		m.mode = modeFilter
		m.filter.CursorEnd()
		return m, m.filter.Focus()
	case "esc":
		m.filter.SetValue("")
	case "P":
		m.pauseAll = m.pauseAllChoices()
		m.mode = modePauseAll
	}
	t, ok := m.current()
	if !ok {
		return m, nil
	}
	switch msg.String() {
	case " ":
		return m, m.setEnabled(t.ID, !t.Enabled)
	case "p":
		if t.PausedUntil != "" {
			return m, m.resume(t.ID)
		}
		if t.State == api.StatePaused && t.Enabled {
			// Paused only by a global pause: tasks.resume cannot lift it.
			m.setMessage(t.ID+" is paused by a global pause; P resumes it", nil)
			return m, nil
		}
		m.mode = modePauseMenu
	case "r":
		return m, m.runNow(t.ID, false)
	case "s":
		run, ok := m.activeRun(t.ID)
		if !ok {
			m.setMessage(t.ID+" has no active run", nil)
			return m, nil
		}
		m.ask(fmt.Sprintf("Stop run %s of %s?", run.ID, t.ID), m.stop(t.ID, run.ID))
	}
	return m, nil
}

func (m Model) filterKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mode = modeList
		m.filter.Blur()
		return m, nil
	case "esc":
		m.mode = modeList
		m.filter.Blur()
		m.filter.SetValue("")
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	return m, cmd
}

func (m Model) pauseMenuKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	t, ok := m.current()
	if !ok {
		m.mode = modeList
		return m, nil
	}
	now := m.now().In(m.loc)
	switch msg.String() {
	case "1":
		m.mode = modeList
		return m, m.pause(t.ID, now.Add(time.Hour).Format(time.RFC3339))
	case "2":
		m.mode = modeList
		y, mo, d := now.AddDate(0, 0, 1).Date()
		return m, m.pause(t.ID, time.Date(y, mo, d, 8, 0, 0, 0, m.loc).Format(time.RFC3339))
	case "3":
		m.mode = modePauseDate
		m.dateIn.SetValue("")
		return m, m.dateIn.Focus()
	case "4":
		m.mode = modeList
		return m, m.pause(t.ID, api.PauseIndefinite)
	case "esc", "q":
		m.mode = modeList
	}
	return m, nil
}

func (m Model) pauseDateKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.dateIn.Blur()
		return m, nil
	case "enter":
		t, ok := m.current()
		if !ok {
			m.mode = modeList
			return m, nil
		}
		until, err := until.Parse(m.dateIn.Value(), m.now(), m.loc)
		if err != nil {
			m.setMessage("", err)
			return m, nil // stay in the prompt to correct it
		}
		m.mode = modeList
		m.dateIn.Blur()
		return m, m.pause(t.ID, until)
	}
	var cmd tea.Cmd
	m.dateIn, cmd = m.dateIn.Update(msg)
	return m, cmd
}

// pauseAllChoices are the P menu entries for the selected task's target
// and for all targets: pause, or resume where a pause is on.
func (m Model) pauseAllChoices() []pauseAllChoice {
	var out []pauseAllChoice
	if t, ok := m.current(); ok && t.Target != "" {
		if contains(m.status.PausedTargets, t.Target) {
			out = append(out, pauseAllChoice{"resume all on " + t.Target, api.MethodResumeAll, t.Target})
		} else {
			out = append(out, pauseAllChoice{"pause all on " + t.Target, api.MethodPauseAll, t.Target})
		}
	}
	if m.status.PausedAll {
		out = append(out, pauseAllChoice{"resume all targets", api.MethodResumeAll, ""})
	} else {
		out = append(out, pauseAllChoice{"pause all targets", api.MethodPauseAll, ""})
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (m Model) pauseAllKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	s := msg.String()
	if s == "esc" || s == "q" {
		m.mode = modeList
		return m, nil
	}
	for i, c := range m.pauseAll {
		if s == fmt.Sprint(i+1) {
			m.mode = modeList
			return m, m.globalPause(c)
		}
	}
	return m, nil
}

// Actions. Each calls the daemon and reports the outcome as a doneMsg.

func (m Model) setEnabled(id string, enabled bool) tea.Cmd {
	return m.taskOutcome(api.MethodTasksSetEnabled, api.SetEnabledParams{ID: id, Enabled: enabled}, func(t api.TaskSummary) string {
		verb := "disabled"
		if t.Enabled {
			verb = "enabled"
		}
		return verb + " " + t.ID
	})
}

func (m Model) pause(id, until string) tea.Cmd {
	return m.taskOutcome(api.MethodTasksPause, api.PauseParams{ID: id, Until: until}, func(t api.TaskSummary) string {
		if until == api.PauseIndefinite || t.PausedUntil == api.PauseIndefinite {
			return "paused " + t.ID + " indefinitely"
		}
		if t.PausedUntil != "" {
			return "paused " + t.ID + " until " + m.when(t.PausedUntil)
		}
		return "paused " + t.ID
	})
}

func (m Model) resume(id string) tea.Cmd {
	return m.taskOutcome(api.MethodTasksResume, api.TaskIDParams{ID: id}, func(t api.TaskSummary) string {
		return "resumed " + t.ID
	})
}

// taskOutcome makes a call and builds the message line from the task the daemon returns.
func (m Model) taskOutcome(method string, params any, line func(api.TaskSummary) string) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var t api.TaskSummary
		if err := b.Call(ctx, method, params, &t); err != nil {
			return doneMsg{err: fmt.Errorf("%s: %w", method, err)}
		}
		return doneMsg{text: line(t)}
	}
}

func (m Model) globalPause(c pauseAllChoice) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var gp api.GlobalPause
		if err := b.Call(ctx, c.method, api.TargetParams{Target: c.target}, &gp); err != nil {
			return doneMsg{err: fmt.Errorf("%s: %w", c.method, err)}
		}
		return doneMsg{text: c.label + ": done", reload: true}
	}
}

func (m Model) runNow(id string, parallel bool) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var res api.RunNowResult
		err := b.Call(ctx, api.MethodTasksRunNow, api.RunNowParams{ID: id, ForceParallel: parallel}, &res)
		var aerr *api.Error
		if errors.As(err, &aerr) && aerr.Code == api.CodeRunActive && !parallel {
			return runActiveMsg{id}
		}
		if err != nil {
			return doneMsg{err: fmt.Errorf("run %s: %w", id, err)}
		}
		switch {
		case res.Queued:
			return doneMsg{text: "queued a run of " + id}
		case res.Status == runFailed:
			return doneMsg{err: fmt.Errorf("run %s failed to start: %s", id, res.ExitReason)}
		}
		return doneMsg{text: "started run " + res.ID + " of " + id}
	}
}

func (m Model) stop(id, runID string) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		if err := b.Call(ctx, api.MethodRunsStop, api.RunsStopParams{RunID: runID}, nil); err != nil {
			return doneMsg{err: fmt.Errorf("stop %s: %w", runID, err)}
		}
		return doneMsg{text: "stopped run " + runID + " of " + id}
	}
}
