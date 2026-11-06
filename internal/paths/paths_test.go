package paths_test

import (
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveUsesXDGDirs(t *testing.T) {
	p, err := paths.Resolve(env(map[string]string{
		"HOME":            "/home/u",
		"XDG_CONFIG_HOME": "/cfg",
		"XDG_STATE_HOME":  "/state",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := paths.Paths{
		ConfigDir: "/cfg/herdrcron",
		StateDir:  "/state/herdrcron",
	}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
	if got := p.Socket(); got != "/state/herdrcron/daemon.sock" {
		t.Errorf("Socket() = %q", got)
	}
	if got := p.Log(); got != "/state/herdrcron/daemon.log" {
		t.Errorf("Log() = %q", got)
	}
	if got := p.ConfigFile(); got != "/cfg/herdrcron/config.yaml" {
		t.Errorf("ConfigFile() = %q", got)
	}
	if got := p.TasksDir(); got != "/cfg/herdrcron/tasks" {
		t.Errorf("TasksDir() = %q", got)
	}
}

func TestResolveDefaultsUnderHome(t *testing.T) {
	p, err := paths.Resolve(env(map[string]string{"HOME": "/home/u"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/home/u/.config/herdrcron" || p.StateDir != "/home/u/.local/state/herdrcron" {
		t.Fatalf("got %+v", p)
	}
}

func TestResolveIgnoresRelativeXDG(t *testing.T) {
	// The XDG spec says relative paths are invalid and must be ignored.
	p, err := paths.Resolve(env(map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "rel", "XDG_STATE_HOME": "rel"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/home/u/.config/herdrcron" || p.StateDir != "/home/u/.local/state/herdrcron" {
		t.Fatalf("got %+v", p)
	}
}

func TestResolveFailsWithoutHome(t *testing.T) {
	if _, err := paths.Resolve(env(nil)); err == nil {
		t.Fatal("want error when neither HOME nor XDG dirs are set")
	}
}
