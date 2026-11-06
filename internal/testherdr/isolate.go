// Package testherdr keeps integration tests off the user's herdr server.
// A throwaway session lives under a temporary HOME and XDG tree, and every
// herdr command that stops or deletes it carries that environment itself,
// so cleanup cannot follow HERDR_SOCKET_PATH back to the user.
package testherdr

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Dirs is a temporary home and XDG tree for one throwaway herdr session.
type Dirs struct {
	Home       string
	ConfigHome string
	StateHome  string
	CacheHome  string
}

// UnsetUserEnv removes every HERDR_* variable for the rest of the test
// (t.Setenv restores them afterwards). HERDR_SOCKET_PATH, set inside a
// herdr pane, would otherwise point every herdr command at the user's server.
func UnsetUserEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "HERDR_") {
			continue
		}
		t.Setenv(k, "") // restores the value after the test
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

// Fresh creates a temp tree and points the process at it. The local target
// writes prompts under Home/.cache; herdr stores named sessions under
// ConfigHome/herdr/sessions.
func Fresh(t *testing.T) Dirs {
	t.Helper()
	root := t.TempDir()
	d := Dirs{
		Home:       filepath.Join(root, "home"),
		ConfigHome: filepath.Join(root, "config"),
		StateHome:  filepath.Join(root, "state"),
		CacheHome:  filepath.Join(root, "cache"),
	}
	d.Apply(t)
	return d
}

// Apply points HOME and the XDG dirs at d and creates the directories.
// Call it before starting herdr, and register RemoveSession after it so
// cleanup runs while this environment is still in place.
func (d Dirs) Apply(t *testing.T) {
	t.Helper()
	for _, dir := range []string{d.Home, d.ConfigHome, d.StateHome, d.CacheHome} {
		if dir == "" {
			t.Fatal("testherdr: empty directory")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", d.Home)
	t.Setenv("XDG_CONFIG_HOME", d.ConfigHome)
	t.Setenv("XDG_STATE_HOME", d.StateHome)
	t.Setenv("XDG_CACHE_HOME", d.CacheHome)
}

// RemoveSession stops and deletes a session this test created. herdr is run
// with an explicit environment (no HERDR_* variable, temp HOME and XDG), so
// this still cannot dial the user's server if it runs after the test has
// restored the process environment. A session that was never created is
// left alone. The test fails if the session directory is still there.
func (d Dirs) RemoveSession(t *testing.T, bin, session string) {
	t.Helper()
	sessionDir := filepath.Join(d.ConfigHome, "herdr", "sessions", session)
	if _, err := os.Stat(sessionDir); errors.Is(err, os.ErrNotExist) {
		return
	}
	env := d.env()
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	out, err := run("--session", session, "server", "stop")
	if err != nil {
		killSession(session)
		out = append(out, []byte("\n(sent SIGTERM to the leftover server)")...)
	}
	time.Sleep(500 * time.Millisecond)
	if delOut, delErr := run("session", "delete", session); delErr != nil {
		out = append(out, delOut...)
		err = errors.Join(err, delErr)
	}
	if _, statErr := os.Stat(sessionDir); errors.Is(statErr, os.ErrNotExist) {
		return
	}
	t.Errorf("throwaway herdr session %s still present at %s: %v: %s", session, sessionDir, err, out)
}

// env is the process environment with every HERDR_* variable removed and
// these dirs pinned.
func (d Dirs) env() []string {
	skip := map[string]bool{
		"HOME": true, "XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HERDR_") || skip[k] {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+d.Home,
		"XDG_CONFIG_HOME="+d.ConfigHome,
		"XDG_STATE_HOME="+d.StateHome,
		"XDG_CACHE_HOME="+d.CacheHome,
	)
}

// killSession signals a herdr server whose argv contains this exact session
// name. The user's default server has no such argument, so it is not matched.
func killSession(session string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(data), "\x00")
		if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), "herdr") {
			continue
		}
		if !slices.Contains(args, "--session") || !slices.Contains(args, session) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}
