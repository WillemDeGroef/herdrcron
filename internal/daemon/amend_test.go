package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// Issue #58: a run times out while its target is unreachable, so it ends
// with "tab may still be open". When the target answers again the runner
// finishes the stop and amends the row; the daemon publishes the amended
// row as "run.changed" and the task's display state as "task.changed",
// without a second timeout notification.
func TestAmendedRunReachesSubscribers(t *testing.T) {
	const mayBeOpen = "; target unreachable, tab may still be open"
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1m\n")
	dir := t.TempDir()
	script, out := filepath.Join(dir, "rec"), filepath.Join(dir, "calls")
	write(t, script, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+out+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, l.p.ConfigFile(), "timezone: UTC\nnotify:\n  command: ["+script+"]\n  on: [timeout]\n")
	l.herdr.setOutput("last words of the agent\n")
	l.start()
	ch := subscribe(t, l.p)
	run := l.mustRunNow()

	l.herdr.setUnreachable(true)
	l.advanceUntil("the run times out", pollStep, l.ended(run.ID))
	ended := next(t, ch, api.EventRunChanged, 5*time.Second, func(e api.RunChangedEvent) bool {
		return e.Run.ID == run.ID && e.Run.EndedAt != ""
	}).Run
	if ended.Status != store.StatusTimeout || !strings.HasSuffix(ended.ExitReason, mayBeOpen) {
		t.Fatalf("run ended on the unreachable target = %+v, want timeout with %q", ended, mayBeOpen)
	}
	if !l.herdr.tabOpen(run.HerdrTab) {
		t.Fatalf("tab closed on an unreachable target")
	}

	l.herdr.setUnreachable(false)
	l.advanceUntil("the stop is finished", pollStep, func() bool {
		return !strings.HasSuffix(l.run(run.ID).ExitReason, mayBeOpen)
	})
	want := ended
	want.ExitReason = strings.TrimSuffix(ended.ExitReason, mayBeOpen) + "; tab closed after the target came back"
	// The next run.changed of the run is the amended row: same run, new reason.
	amended := next(t, ch, api.EventRunChanged, 5*time.Second, func(e api.RunChangedEvent) bool {
		return e.Run.ID == run.ID
	}).Run
	if amended != want {
		t.Errorf("amended run.changed = %+v, want %+v", amended, want)
	}
	next(t, ch, api.EventTaskChanged, 5*time.Second, func(e api.TaskChangedEvent) bool {
		return e.ID == "job" && e.Task != nil && e.Task.LastRun != nil && *e.Task.LastRun == want
	})
	if listed := l.run(run.ID); listed != want {
		t.Errorf("runs.list = %+v, want %+v", listed, want)
	}
	if l.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab %s still open after the target came back", run.HerdrTab)
	}

	// Stopping the daemon waits for its notifications.
	l.kill()
	stored := l.storedRun(run.ID)
	if stored.ExitReason != want.ExitReason || stored.Status != store.StatusTimeout || stored.OutputTail != "last words of the agent\n" {
		t.Errorf("stored run = %s %q tail %q, want timeout %q and the output tail", stored.Status, stored.ExitReason, stored.OutputTail, want.ExitReason)
	}
	st, err := store.Open(l.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pending, err := st.ListPendingCleanups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("%d pending cleanups left, want none", len(pending))
	}
	calls, _ := os.ReadFile(out)
	if strings.Count(string(calls), "\n") != 1 || !strings.Contains(string(calls), "timeout") {
		t.Errorf("notify calls = %q, want one timeout", calls)
	}
}
