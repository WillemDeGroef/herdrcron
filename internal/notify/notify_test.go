package notify_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/notify"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// recorder writes a script that appends its arguments, one per line and
// a blank line after each call, to a file, and returns both paths.
func recorder(t *testing.T) (script, out string) {
	t.Helper()
	dir := t.TempDir()
	script, out = filepath.Join(dir, "rec"), filepath.Join(dir, "calls")
	body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + out + "; done\necho >> " + out + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, out
}

// calls returns the recorded calls, each as its argv lines.
func calls(t *testing.T, out string) [][]string {
	t.Helper()
	data, err := os.ReadFile(out)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var res [][]string
	for _, c := range strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n") {
		if c != "" {
			res = append(res, strings.Split(c, "\n"))
		}
	}
	return res
}

func newLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func TestCommandGetsMessageAppendedWithoutAShell(t *testing.T) {
	script, out := recorder(t)
	log, _ := newLog()
	n := notify.New(config.Notify{Command: []string{script, "publish", "herd rcron"}, On: config.NotifyEvents}, log)
	msg := `deps: "blocked" $(touch /nonexistent) ; echo hi`
	n.Notify(config.EventBlocked, msg, log)
	n.Wait()
	got := calls(t, out)
	want := []string{"publish", "herd rcron", msg}
	if len(got) != 1 || strings.Join(got[0], "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want one call with argv %q", got, want)
	}
}

func TestOnlyListedEventsNotify(t *testing.T) {
	script, out := recorder(t)
	log, _ := newLog()
	n := notify.New(config.Notify{Command: []string{script}, On: []string{config.EventLost}}, log)
	n.Notify(config.EventBlocked, "blocked", log)
	n.Notify(config.EventFailed, "failed", log)
	n.Notify(config.EventLost, "lost", log)
	n.Wait()
	got := calls(t, out)
	if len(got) != 1 || got[0][0] != "lost" {
		t.Fatalf("calls = %q, want only the lost one", got)
	}
}

func TestNoCommandNotifiesNothing(t *testing.T) {
	log, buf := newLog()
	n := notify.New(config.Notify{On: config.NotifyEvents}, log)
	n.Notify(config.EventFailed, "failed", log)
	n.Wait()
	if strings.Contains(buf.String(), "notify") {
		t.Errorf("log = %s, want nothing without a command", buf)
	}
}

func TestFailingCommandIsLogged(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho no route >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	log, buf := newLog()
	n := notify.New(config.Notify{Command: []string{script}, On: config.NotifyEvents}, log)
	n.Notify(config.EventFailed, "failed", log.With("run_id", "R1"))
	n.Wait()
	s := buf.String()
	for _, want := range []string{`"level":"WARN"`, `"run_id":"R1"`, `"event":"failed"`, "no route", "exit status 3"} {
		if !strings.Contains(s, want) {
			t.Errorf("log = %s, want it to contain %s", s, want)
		}
	}
}

func TestMissingCommandIsLogged(t *testing.T) {
	log, buf := newLog()
	n := notify.New(config.Notify{Command: []string{filepath.Join(t.TempDir(), "nope")}, On: config.NotifyEvents}, log)
	n.Notify(config.EventLost, "lost", log)
	n.Wait()
	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("log = %s, want a warning", buf)
	}
}

func TestHangingCommandIsKilledAfterTheTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hang")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	log, buf := newLog()
	n := notify.New(config.Notify{Command: []string{script}, On: config.NotifyEvents}, log)
	n.Timeout = 100 * time.Millisecond
	start := time.Now()
	n.Notify(config.EventLost, "lost", log)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Notify blocked for %s, want it to return at once", d)
	}
	n.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Wait took %s, want the command killed after the timeout", d)
	}
	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("log = %s, want a warning", buf)
	}
}

func TestRunMessage(t *testing.T) {
	r := store.Run{ID: "01JRUN", TaskID: "deps", Target: "local", Status: store.StatusFailed, ExitReason: "agent exited"}
	if got, want := notify.RunMessage(r), "herdrcron: deps failed: agent exited (target local, run 01JRUN)"; got != want {
		t.Errorf("RunMessage = %q, want %q", got, want)
	}
	r.Status, r.ExitReason = store.StatusBlocked, ""
	if got, want := notify.RunMessage(r), "herdrcron: deps blocked (target local, run 01JRUN)"; got != want {
		t.Errorf("RunMessage = %q, want %q", got, want)
	}
	r.ExitReason = "stale reason must not appear in blocked notification"
	if got, want := notify.RunMessage(r), "herdrcron: deps blocked (target local, run 01JRUN)"; got != want {
		t.Errorf("RunMessage blocked with exit_reason = %q, want %q (no exit_reason)", got, want)
	}
}
