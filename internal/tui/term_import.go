package tui

// Importing term runs its init before Bubble Tea's, so opening the TUI
// does not wait on a terminal colour query. See package term.
import _ "github.com/WillemDeGroef/herdrcron/internal/tui/term"
