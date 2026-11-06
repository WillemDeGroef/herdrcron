package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func xdg(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "s"))
	p, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListWithoutDaemonFails(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"list"}, &out, &errOut)
	if code == 0 {
		t.Fatal("want non-zero exit when the daemon is not running")
	}
	if msg := errOut.String(); !strings.Contains(msg, "daemon is not running") || !strings.Contains(msg, "herdrcron daemon") {
		t.Fatalf("stderr = %q, want a clear message that says how to start it", msg)
	}
}

func TestDaemonAndList(t *testing.T) {
	p := xdg(t)
	write(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md"), "go")
	write(t, filepath.Join(p.TasksDir(), "nightly.yaml"),
		"id: nightly\ntarget: local\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\n")
	write(t, filepath.Join(p.ConfigDir, "prompts", "parked.md"), "go")
	write(t, filepath.Join(p.TasksDir(), "parked.yaml"),
		"id: parked\ntarget: local\nschedule: \"*/2 * * * *\"\nenabled: false\nagent: {command: claude}\ntimeout: 1h\n")
	write(t, filepath.Join(p.TasksDir(), "broken.yaml"),
		"id: broken\ntarget: mars\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\n")

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

	var out, errOut bytes.Buffer
	for i := 0; ; i++ {
		out.Reset()
		errOut.Reset()
		if run(context.Background(), []string{"list"}, &out, &errOut) == 0 {
			break
		}
		if i > 500 {
			t.Fatalf("list never succeeded: %s", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 tasks, got:\n%s", out.String())
	}
	for _, col := range []string{"ID", "TARGET", "SCHEDULE", "STATE"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks %s", lines[0], col)
		}
	}
	wantRows := [][]string{
		{"broken", "mars", "@daily", `invalid: unknown target "mars"`},
		{"nightly", "local", "@daily", "idle"},
		{"parked", "local", "*/2 * * * *", "off"},
	}
	for i, want := range wantRows {
		for _, cell := range want {
			if !strings.Contains(lines[i+1], cell) {
				t.Errorf("row %d %q lacks %q", i+1, lines[i+1], cell)
			}
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage") {
		t.Errorf("stderr = %q", errOut.String())
	}
}
