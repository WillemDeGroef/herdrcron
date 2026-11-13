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

// A fire at a target that stopped answering records a host_unreachable
// run, publishes "target.changed", notifies target_unreachable and shows
// in "status"; when the target answers again, the missed policy
// (run_once) starts one catch_up run.
func TestUnreachableTargetEndToEnd(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC), "")
	dir := t.TempDir()
	script, out := filepath.Join(dir, "rec"), filepath.Join(dir, "calls")
	write(t, script, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+out+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, l.p.ConfigFile(), "timezone: UTC\ntargets:\n  vps:\n    host: vps\nnotify:\n  command: ["+script+"]\n  on: [target_unreachable]\n")
	write(t, filepath.Join(l.p.TasksDir(), "job.yaml"), `id: job
target: vps
cwd: /tmp/repo
schedule: "@hourly"
missed: run_once
timeout: 1h
agent: {command: claude}
`)
	vps := newFakeHerdr()
	vps.name = "vps"
	l.c, l.stop = runDaemon(t, l.p, daemon.Options{
		Version: "test", Clock: l.clk,
		Targets: map[string]target.Target{"local": l.herdr, "vps": vps},
	})
	ch := subscribe(t, l.p)
	status := func() api.TargetStatus {
		var res api.StatusResult
		if err := l.c.Call(context.Background(), api.MethodStatus, nil, &res); err != nil {
			t.Fatal(err)
		}
		for _, ts := range res.Targets {
			if ts.Name == "vps" {
				return ts
			}
		}
		t.Fatalf("vps not in status: %+v", res.Targets)
		return api.TargetStatus{}
	}
	l.advanceUntil("vps probed", pollStep, func() bool { return status().Reachability == api.ReachReachable })

	vps.setUnreachable(true)
	l.advanceUntil("the 13:00 fire", time.Minute, func() bool { return len(l.runs()) > 0 })
	fire := l.runs()[0]
	if fire.Status != store.StatusHostUnreachable || fire.Trigger != store.TriggerSchedule || fire.EndedAt == "" {
		t.Fatalf("fire while vps is down = %+v, want an ended host_unreachable schedule run", fire)
	}
	next(t, ch, api.EventTargetChanged, 5*time.Second, func(e api.TargetChangedEvent) bool {
		return e.Target.Name == "vps" && e.Target.Reachability == api.ReachUnreachable
	})
	ts := status()
	if ts.Reachability != api.ReachUnreachable || ts.UnreachableSince == "" || ts.NextRetryAt == "" || !strings.Contains(ts.LastError, "unreachable") {
		t.Errorf("status of vps = %+v, want unreachable with since, next retry and error", ts)
	}
	var calls string
	eventually(t, "the target_unreachable notification", func() bool {
		data, _ := os.ReadFile(out)
		calls = string(data)
		return calls != ""
	})
	if !strings.HasPrefix(calls, "herdrcron: target vps unreachable: ") || strings.Count(calls, "\n") != 1 {
		t.Errorf("notify calls = %q, want one target vps unreachable", calls)
	}

	vps.setUnreachable(false)
	catchUp := func() bool { return len(l.runs()) > 1 }
	l.advanceUntil("the catch-up run", 10*time.Second, catchUp)
	next(t, ch, api.EventTargetChanged, 5*time.Second, func(e api.TargetChangedEvent) bool {
		return e.Target.Name == "vps" && e.Target.Reachability == api.ReachReachable
	})
	eventually(t, "the catch-up run is running", func() bool { return l.runs()[0].Status != store.StatusStarting })
	runs := l.runs()
	if len(runs) != 2 || runs[0].Trigger != store.TriggerCatchUp || runs[0].Status != store.StatusRunning {
		t.Fatalf("runs after vps answers = %+v, want one running catch_up run on top", runs)
	}
	if runs[0].ScheduledAt != fire.ScheduledAt {
		t.Errorf("catch_up scheduled_at = %s, want the missed fire's %s", runs[0].ScheduledAt, fire.ScheduledAt)
	}
	if ts := status(); ts.Reachability != api.ReachReachable || ts.NextRetryAt != "" || ts.LastSeenAt == "" {
		t.Errorf("status of vps after it answers = %+v", ts)
	}
}

// Issue #37: the vps accepts the 13:00 launch and drops before its reply
// arrives, so the run is recorded host_unreachable while its agent runs.
// When the vps answers again, the daemon finds the run's hc- tab and pane
// and adopts the run as running: missed: run_once starts no second run,
// then or at a later reconnect.
func TestLostLaunchReplyIsAdoptedNotCaughtUp(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC), "")
	write(t, l.p.ConfigFile(), "timezone: UTC\ntargets:\n  vps:\n    host: vps\n")
	write(t, filepath.Join(l.p.TasksDir(), "job.yaml"), `id: job
target: vps
cwd: /tmp/repo
schedule: "@daily"
missed: run_once
timeout: 6h
agent: {command: claude}
`)
	vps := newFakeHerdr()
	vps.name = "vps"
	l.c, l.stop = runDaemon(t, l.p, daemon.Options{
		Version: "test", Clock: l.clk,
		Targets: map[string]target.Target{"local": l.herdr, "vps": vps},
	})
	reachability := func() string {
		var res api.StatusResult
		if err := l.c.Call(context.Background(), api.MethodStatus, nil, &res); err != nil {
			t.Fatal(err)
		}
		for _, ts := range res.Targets {
			if ts.Name == "vps" {
				return ts.Reachability
			}
		}
		t.Fatalf("vps not in status: %+v", res.Targets)
		return ""
	}
	launches := func() int { return vps.count("herdr", "pane", "run") }

	vps.setLoseRunReply()
	l.advanceUntil("the midnight fire", 10*time.Minute, func() bool { return len(l.runs()) > 0 })
	eventually(t, "the fire ended host_unreachable", func() bool { return l.runs()[0].EndedAt != "" })
	fire := l.runs()[0]
	if fire.Status != store.StatusHostUnreachable || fire.HerdrPane == "" || launches() != 1 {
		t.Fatalf("fire with a lost launch reply = %+v after %d launches, want host_unreachable with a pane, 1 launch", fire, launches())
	}
	if !vps.tabOpen(fire.HerdrTab) {
		t.Fatal("the agent's tab is gone")
	}

	vps.setUnreachable(false)
	l.advanceUntil("the run adopted", 10*time.Second, func() bool { return l.runs()[0].Status == store.StatusRunning })
	runs := l.runs()
	if len(runs) != 1 || runs[0].ID != fire.ID || runs[0].EndedAt != "" || runs[0].HerdrPane != fire.HerdrPane {
		t.Fatalf("runs after vps answers = %+v, want only run %s, running in its pane", runs, fire.ID)
	}

	// A second outage and reconnect: still that one run, one launch.
	vps.setUnreachable(true)
	l.advanceUntil("vps seen down", 10*time.Second, func() bool { return reachability() == api.ReachUnreachable })
	vps.setUnreachable(false)
	l.advanceUntil("vps seen up", 10*time.Second, func() bool { return reachability() == api.ReachReachable })
	eventually(t, "the catch-up pass of the reconnect", func() bool { return l.logContains("target reachable again") })
	if runs := l.runs(); len(runs) != 1 || runs[0].Status != store.StatusRunning || launches() != 1 {
		t.Errorf("runs after a second reconnect = %+v after %d launches, want the one adopted run, 1 launch", runs, launches())
	}
}
