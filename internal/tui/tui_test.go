package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"
)

// start runs m in a teatest program and waits until the task list shows.
func start(t *testing.T, m Model) *teatest.TestModel {
	t.Helper()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 16))
	waitText(t, tm, "factory-coordinator")
	return tm
}

func waitText(t *testing.T, tm *teatest.TestModel, s string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
		teatest.WithDuration(3*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
}

func key(s string) tea.KeyMsg {
	switch s {
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// finalView quits tm (ctrl+c works in every mode) and returns the last frame.
func finalView(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	return tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second)).View()
}

func TestMainScreenGolden(t *testing.T) {
	tm := start(t, newTestModel(fixture(), plain()))
	golden.RequireEqual(t, []byte(finalView(t, tm)))
}

func TestSortBlockedThenRunningThenNextFire(t *testing.T) {
	tm := start(t, newTestModel(fixture(), plain()))
	view := finalView(t, tm)
	want := []string{"release-notes", "factory-coordinator", "e2e-timeout", "nightly-e2e", "peppol-sh-deps", "peppolnews-ingest"}
	assertOrder(t, view, want)
}

func assertOrder(t *testing.T, view string, ids []string) {
	t.Helper()
	last := -1
	for _, id := range ids {
		i := strings.Index(view, id)
		if i < 0 {
			t.Fatalf("view lacks %s:\n%s", id, view)
		}
		if i < last {
			t.Fatalf("%s out of order (want %v):\n%s", id, ids, view)
		}
		last = i
	}
}
