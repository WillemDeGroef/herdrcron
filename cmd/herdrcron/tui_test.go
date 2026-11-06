package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/tui"
)

func TestNoCommandWithoutTerminalExplainsSSH(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if msg := errOut.String(); !strings.Contains(msg, "needs a terminal") || !strings.Contains(msg, "ssh -t") {
		t.Fatalf("stderr = %q", msg)
	}
}

func TestNoCommandWithoutDaemonFails(t *testing.T) {
	xdg(t)
	old := isTerminal
	isTerminal = func() bool { return true }
	t.Cleanup(func() { isTerminal = old })
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if msg := errOut.String(); !strings.Contains(msg, "daemon is not running") {
		t.Fatalf("stderr = %q", msg)
	}
}

// startDaemon runs the real daemon on p until the test ends and waits
// until it answers.
func startDaemon(t *testing.T, p paths.Paths) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var dOut, dErr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"daemon"}, &dOut, &dErr) }()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("daemon exit %d: %s", code, dErr.String())
		}
	})
	b := tui.SocketBackend{Path: p.Socket()}
	for i := 0; ; i++ {
		if b.Call(context.Background(), api.MethodStatus, nil, nil) == nil {
			return
		}
		if i > 500 {
			t.Fatalf("daemon never answered: %s", dErr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTUIShowsHandEditWithin2s opens the TUI on a real daemon, edits a
// task file by hand and expects the open TUI to show it within 2 s.
func TestTUIShowsHandEditWithin2s(t *testing.T) {
	p := xdg(t)
	write(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md"), "go")
	file := filepath.Join(p.TasksDir(), "nightly.yaml")
	write(t, file, "id: nightly\ntarget: local\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\n")
	startDaemon(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := tui.New(tui.Options{Backend: tui.SocketBackend{Path: p.Socket()}, Context: ctx})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 20))
	waitFor := func(s string, d time.Duration) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
			teatest.WithDuration(d), teatest.WithCheckInterval(10*time.Millisecond))
	}
	waitFor("@daily", 5*time.Second)

	write(t, file, "id: nightly\ntarget: local\nschedule: \"17 3 * * *\"\nagent: {command: claude}\ntimeout: 1h\n")
	waitFor("03:17 daily", 2*time.Second)

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}
