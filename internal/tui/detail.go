package tui

import tea "github.com/charmbracelet/bubbletea"

// detailState is the state of the task detail screen.
type detailState struct{}

// openDetail opens the detail screen of task id.
func (m Model) openDetail(id string) (Model, tea.Cmd) { return m, nil }

// detailMsg handles the detail screen's result messages.
func (m Model) detailMsg(msg tea.Msg) (Model, tea.Cmd, bool) { return m, nil, false }

// detailKey handles a key on the detail screen.
func (m Model) detailKey(msg tea.KeyMsg) (Model, tea.Cmd) { return m, nil }

// detailView renders the detail screen.
func (m Model) detailView() string { return "" }
