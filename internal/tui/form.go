package tui

import tea "github.com/charmbracelet/bubbletea"

// formState is the state of the new/edit task form.
type formState struct{}

// openNewForm opens the form for a new task.
func (m Model) openNewForm() (Model, tea.Cmd) { return m, nil }

// openEditForm opens the form on task id.
func (m Model) openEditForm(id string) (Model, tea.Cmd) { return m, nil }

// editPrompt opens task id's prompt in the editor and saves it.
func (m Model) editPrompt(id string) (Model, tea.Cmd) { return m, nil }

// deleteTask asks to delete task id (offering stop and delete with an active run).
func (m Model) deleteTask(id string) (Model, tea.Cmd) { return m, nil }

// formMsg handles the form's, the editor's and delete's result messages.
func (m Model) formMsg(msg tea.Msg) (Model, tea.Cmd, bool) { return m, nil, false }

// formKey handles a key on the form.
func (m Model) formKey(msg tea.KeyMsg) (Model, tea.Cmd) { return m, nil }

// formView renders the form.
func (m Model) formView() string { return "" }
