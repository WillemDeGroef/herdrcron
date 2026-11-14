package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// droppedRuns returns the task's dropped rows from runs.list.
func (l *lifecycle) droppedRuns() []api.RunSummary {
	l.t.Helper()
	var dropped []api.RunSummary
	for _, r := range l.runs() {
		if r.Status == store.StatusDropped {
			dropped = append(dropped, r)
		}
	}
	return dropped
}

// storedQueuedFires reads queued_fires; the daemon must be stopped.
func (l *lifecycle) storedQueuedFires() []store.QueuedFire {
	l.t.Helper()
	st, err := store.Open(l.p.DB())
	if err != nil {
		l.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	fires, err := st.ListQueuedFires(context.Background())
	if err != nil {
		l.t.Fatal(err)
	}
	return fires
}

// Issue #60: a fire that waits for a target which reload then removes is
// dropped, and the drop shows in the task's history.
//
// Reload keeps a target that has an active run, so the fire has to wait on
// a target without one: the task runs on local, is then moved to old, and
// its next fire waits (overlap queue) for the run on local, bound to old.
func TestReloadRemovingATargetRecordsItsWaitingFireAsDropped(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\noverlap: queue\n")
	l.withConfig("targets:\n  old:\n    host: old\n")
	old := newFakeHerdr()
	old.name = "old"
	l.c, l.stop = runDaemon(t, l.p, daemon.Options{
		Version: "test", Clock: l.clk,
		Targets: map[string]target.Target{"local": l.herdr, "old": old},
	})
	first := l.mustRunNow()

	write(t, filepath.Join(l.p.TasksDir(), "job.yaml"),
		"id: job\ntarget: old\ncwd: /tmp/repo\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\noverlap: queue\n")
	reload(t, l.c)
	queued, err := l.runNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if !queued.Queued {
		t.Fatalf("second run_now = %+v, want queued behind the run on local", queued)
	}

	events := subscribe(t, l.p)
	l.withConfig("")
	if res := reload(t, l.c); res.RestartRequired || len(res.DeferredTargets) != 0 {
		t.Fatalf("reload = %+v, want old removed: it has no active run", res)
	}

	dropped := l.droppedRuns()
	if len(dropped) != 1 {
		t.Fatalf("runs = %+v, want one dropped row", l.runs())
	}
	got := dropped[0]
	if got.Target != "old" || got.Trigger != store.TriggerManual || got.EndedAt == "" ||
		!strings.Contains(got.ExitReason, "removed from config") {
		t.Errorf("dropped row = %+v, want a manual fire on old, ended, with reason \"... removed from config\"", got)
	}
	next(t, events, api.EventRunChanged, 5*time.Second, func(e api.RunChangedEvent) bool {
		return e.Run.ID == got.ID && e.Run.Status == store.StatusDropped
	})
	if r := l.run(first.ID); r.Status != store.StatusRunning {
		t.Errorf("first run = %+v, want it still running on local", r)
	}

	l.kill()
	if fires := l.storedQueuedFires(); len(fires) != 0 {
		t.Errorf("stored queued fires after the drop = %+v, want none", fires)
	}
}

// Issue #60: a task deleted while its fire waits. When the active run ends
// the fire cannot start; it gets a dropped row, and no notification.
func TestQueuedFireOfADeletedTaskIsRecordedAsDropped(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1m\noverlap: queue\n")
	dir := t.TempDir()
	script, out := filepath.Join(dir, "rec"), filepath.Join(dir, "calls")
	write(t, script, "#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+out+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	l.withConfig("notify:\n  command: [" + script + "]\n")
	l.start()
	first := l.mustRunNow()
	queued, err := l.runNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if !queued.Queued {
		t.Fatalf("second run_now = %+v, want queued", queued)
	}

	if err := os.Remove(filepath.Join(l.p.TasksDir(), "job.yaml")); err != nil {
		t.Fatal(err)
	}
	reload(t, l.c)
	if _, ok := listByID(t, l.c)["job"]; ok {
		t.Fatal("job still listed after its file was deleted")
	}
	if n := len(l.runs()); n != 1 {
		t.Fatalf("%d runs while the first run is active, want 1: the fire still waits", n)
	}

	// The first run times out, which is the one notification of this test.
	l.advanceUntil("the first run ends", pollStep, l.ended(first.ID))
	eventually(t, "the dropped row", func() bool { return len(l.droppedRuns()) == 1 })
	runs := l.runs()
	got := l.droppedRuns()[0]
	if len(runs) != 2 || got.Trigger != store.TriggerManual || got.EndedAt == "" ||
		!strings.HasPrefix(got.ExitReason, "not started:") {
		t.Errorf("runs = %+v, want the first run and a dropped manual fire with reason \"not started: ...\"", runs)
	}

	// Stopping the daemon waits for the notify commands it started.
	l.kill()
	if fires := l.storedQueuedFires(); len(fires) != 0 {
		t.Errorf("stored queued fires after the drop = %+v, want none", fires)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify calls: %v, want the timeout of the first run", err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(calls) != 1 || !strings.Contains(calls[0], "timeout") || !strings.Contains(calls[0], first.ID) {
		t.Errorf("notify calls = %q, want only the timeout of run %s: a dropped fire does not notify", calls, first.ID)
	}
}

// Issue #60: a stored fire whose task is gone when the daemon starts is
// dropped at start, with a row in the history.
func TestRestartRecordsAQueuedFireOfAMissingTaskAsDropped(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\noverlap: queue\n")
	l.start()
	first := l.mustRunNow()
	queued, err := l.runNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if !queued.Queued {
		t.Fatalf("second run_now = %+v, want queued", queued)
	}

	l.kill()
	if fires := l.storedQueuedFires(); len(fires) != 1 {
		t.Fatalf("stored queued fires after the stop = %+v, want the waiting fire", fires)
	}
	if err := os.Remove(filepath.Join(l.p.TasksDir(), "job.yaml")); err != nil {
		t.Fatal(err)
	}
	l.start()

	dropped := l.droppedRuns()
	if len(dropped) != 1 {
		t.Fatalf("runs after the restart = %+v, want one dropped row", l.runs())
	}
	if got := dropped[0]; got.ExitReason != "task removed or invalid" || got.Trigger != store.TriggerManual ||
		got.EndedAt == "" || got.ID == first.ID {
		t.Errorf("dropped row = %+v, want a manual fire with reason %q", got, "task removed or invalid")
	}

	l.kill()
	if fires := l.storedQueuedFires(); len(fires) != 0 {
		t.Errorf("stored queued fires after the restart = %+v, want none", fires)
	}
	st, err := store.Open(l.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	row, err := st.GetRun(context.Background(), dropped[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.StatusDropped || row.TaskID != "job" || !row.StartedAt.Equal(row.EndedAt) || !row.EndedAt.Equal(noon) {
		t.Errorf("stored row = %+v, want dropped for job, started and ended at %v", row, noon)
	}
}
