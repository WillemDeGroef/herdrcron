package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Run shows the TUI on the terminal until the user quits or ctx is done.
// It uses the alternate screen and no mouse, so it works the same over
// `ssh -t`. The subscription ends with it; the daemon is left as it is.
func Run(ctx context.Context, b Backend) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := tea.NewProgram(New(Options{Backend: b, Context: ctx}), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	if ctx.Err() != nil {
		return nil // interrupted (SIGINT/SIGTERM): a normal way out
	}
	return err
}
