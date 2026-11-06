package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// deleteActiveMsg is a delete refused because a run started meanwhile.
type deleteActiveMsg struct{ id string }

// deleteTask asks to delete task id (offering stop and delete with an active run).
func (m Model) deleteTask(id string) (Model, tea.Cmd) {
	if run, ok := m.activeRun(id); ok {
		m.ask(fmt.Sprintf("Stop run %s and delete task %s? Its task and prompt files are removed.", run.ID, id), m.del(id, true))
		return m, nil
	}
	m.ask(fmt.Sprintf("Delete task %s? Its task and prompt files are removed.", id), m.del(id, false))
	return m, nil
}

// del calls tasks.delete.
func (m Model) del(id string, stopActive bool) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var res api.DeleteResult
		err := b.Call(ctx, api.MethodTasksDelete, api.DeleteParams{ID: id, StopActive: stopActive}, &res)
		var aerr *api.Error
		if errors.As(err, &aerr) && aerr.Code == api.CodeRunActiveDelete && !stopActive {
			return deleteActiveMsg{id}
		}
		if err != nil {
			return doneMsg{err: fmt.Errorf("delete %s: %w", id, err)}
		}
		text := "deleted " + id
		if len(res.StoppedRuns) > 0 {
			ids := make([]string, len(res.StoppedRuns))
			for i, r := range res.StoppedRuns {
				ids[i] = r.ID
			}
			text = "stopped run " + strings.Join(ids, ", ") + " and deleted " + id
		}
		return doneMsg{text: text, reload: true}
	}
}

// deleteMsg handles delete's result messages.
func (m Model) deleteMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	if msg, ok := msg.(deleteActiveMsg); ok {
		m.ask(fmt.Sprintf("%s started a run meanwhile. Stop it and delete the task?", msg.id), m.del(msg.id, true))
		return m, nil, true
	}
	return m, nil, false
}
