package tui

import tea "github.com/charmbracelet/bubbletea"

// outputState is the state of the live output screen.
type outputState struct{}

// openOutput opens the live output of task id's active run.
func (m Model) openOutput(id string) (Model, tea.Cmd) { return m, nil }

// attach suspends the TUI and attaches to task id's active run.
func (m Model) attach(id string) (Model, tea.Cmd) { return m, nil }

// outputMsg handles the live output screen's and attach's result messages.
func (m Model) outputMsg(msg tea.Msg) (Model, tea.Cmd, bool) { return m, nil, false }

// outputKey handles a key on the live output screen.
func (m Model) outputKey(msg tea.KeyMsg) (Model, tea.Cmd) { return m, nil }

// outputView renders the live output screen.
func (m Model) outputView() string { return "" }
