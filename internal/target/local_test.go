package target_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func TestLocalName(t *testing.T) {
	if got := target.NewLocal().Name(); got != "local" {
		t.Errorf("Name = %q", got)
	}
}

func TestLocalExecReturnsStdout(t *testing.T) {
	out, err := target.NewLocal().Exec(context.Background(), []string{"printf", "%s|%s", "a b", "$HOME"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "a b|$HOME" {
		t.Errorf("stdout = %q", got)
	}
}

func TestLocalExecPassesStdin(t *testing.T) {
	out, err := target.NewLocal().Exec(context.Background(), []string{"cat"}, strings.NewReader("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "hello\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestLocalExecNonZeroExit(t *testing.T) {
	argv := []string{"sh", "-c", "echo boom >&2; exit 3", "SECRET-ARG"}
	_, err := target.NewLocal().Exec(context.Background(), argv, nil)
	var ee *target.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if ee.Code != 3 {
		t.Errorf("Code = %d, want 3", ee.Code)
	}
	if got := string(ee.Stderr); got != "boom\n" {
		t.Errorf("Stderr = %q", got)
	}
	msg := ee.Error()
	if strings.Contains(msg, "SECRET-ARG") {
		t.Errorf("Error() leaks 4th argv element: %q", msg)
	}
	if !strings.Contains(msg, "exit 3") || !strings.Contains(msg, "boom") {
		t.Errorf("Error() = %q", msg)
	}
}

func TestLocalExecMissingBinary(t *testing.T) {
	_, err := target.NewLocal().Exec(context.Background(), []string{"herdrcron-no-such-binary-xyz"}, nil)
	if err == nil {
		t.Fatal("want error for missing binary")
	}
	var ee *target.ExitError
	if errors.As(err, &ee) {
		t.Errorf("missing binary gave *ExitError: %v", err)
	}
}

func TestLocalPutAndRemoveFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	l := target.NewLocal()
	if err := l.PutFile(ctx, "~/a/b/prompt.md", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, "a", "b", "prompt.md")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi" {
		t.Errorf("content = %q", data)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// Overwrite works and leaves no temp files behind.
	if err := l.PutFile(ctx, "~/a/b/prompt.md", []byte("again")); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1", len(entries))
	}
	if err := l.RemoveFile(ctx, "~/a/b/prompt.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists: %v", err)
	}
	if err := l.RemoveFile(ctx, "~/a/b/prompt.md"); err != nil {
		t.Errorf("second RemoveFile = %v, want nil", err)
	}
}

func TestLocalSpawn(t *testing.T) {
	file := filepath.Join(t.TempDir(), "spawned")
	if err := target.NewLocal().Spawn([]string{"sh", "-c", `echo x > "$1"`, "sh", file}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(file); err == nil && string(data) == "x\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("spawned command did not create file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A daemon started inside a herdr pane inherits HERDR_SOCKET_PATH and
// friends; herdr calls must still reach the default session, so commands
// run without any HERDR_* variable.
func TestLocalExecDropsHerdrEnv(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/nowhere/herdr.sock")
	t.Setenv("HERDR_PANE_ID", "w9:p9")
	t.Setenv("HC_KEEP", "kept")
	out, err := target.NewLocal().Exec(context.Background(), []string{"env"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "HERDR_") {
		t.Errorf("env still holds HERDR_* variables:\n%s", out)
	}
	if !strings.Contains(string(out), "HC_KEEP=kept") {
		t.Errorf("other variables were dropped:\n%s", out)
	}
}
