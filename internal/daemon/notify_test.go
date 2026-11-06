package daemon_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/daemon"
)

// A run that fails runs notify.command from config.yaml with the message
// as its last argument.
func TestFailedRunRunsTheNotifyCommand(t *testing.T) {
	p := setup(t)
	dir := t.TempDir()
	script, out := filepath.Join(dir, "rec"), filepath.Join(dir, "calls")
	write(t, script, "#!/bin/sh\nprintf '%s|' \"$@\" >> "+out+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p.ConfigFile(), "notify:\n  command: ["+script+", --title, herdrcron]\n  on: [failed]\n")
	c := startWith(t, p, daemon.Options{Version: "test", Targets: fakeTargets()})

	run, err := runNow(c, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for got == "" && time.Now().Before(deadline) {
		data, _ := os.ReadFile(out)
		got = string(data)
		time.Sleep(10 * time.Millisecond)
	}
	want := "--title|herdrcron|herdrcron: nightly failed: "
	if !strings.HasPrefix(got, want) || !strings.Contains(got, "run "+run.ID) {
		t.Errorf("notify argv = %q, want it to start with %q and name run %s", got, want, run.ID)
	}
}
