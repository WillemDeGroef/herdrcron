// Package term marks the terminal background as already known so Bubble
// Tea does not query it.
//
// Bubble Tea's init calls lipgloss.HasDarkBackground, which asks the
// terminal for its background colour (OSC 11) and waits termenv.OSCTimeout
// (5s) when nothing answers. That stalls the first frame on a terminal
// that ignores the query, including some `ssh -t` sessions. The TUI paints
// with a fixed ANSI palette, so the answer is never used.
//
// This init runs before Bubble Tea's: the package imports only lipgloss,
// while Bubble Tea waits on further packages, and the linker then schedules
// ready packages lexicographically (this module's path sorts before
// charmbracelet).
package term

import "github.com/charmbracelet/lipgloss"

func init() {
	lipgloss.SetHasDarkBackground(true)
}
