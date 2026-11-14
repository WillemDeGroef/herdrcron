package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// envConfig is the config newRunner starts with, plus targets.
func envConfig(targets map[string]config.Target) config.Config {
	return config.Config{Timezone: "Europe/Brussels", Targets: targets}
}

// A target added by Reconfigure is polled like one the runner started
// with; a removed one is gone and its poll worker stops.
func TestReconfigureAddsAndRemovesATargetWhileRunning(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); e.runner.Run(ctx) }()
	defer func() { cancel(); <-done }()
	e.waitClock()

	box := newFakeHerdr()
	box.name = "box"
	built := 0
	newTarget := func(name string, _ config.Target) target.Target {
		built++
		if name != "box" {
			t.Errorf("newTarget(%q), want it called for box only", name)
		}
		return box
	}
	withBox := envConfig(map[string]config.Target{"local": {}, "box": {Host: "box"}})
	if deferred := e.runner.Reconfigure(withBox, newTarget); len(deferred) != 0 {
		t.Fatalf("deferred = %q, want none", deferred)
	}
	if st, ok := e.runner.TargetState("box"); !ok || st.Reachability != runner.ReachUnknown {
		t.Fatalf("box after Reconfigure = %+v, %v; want it known and not probed yet", st, ok)
	}
	e.waitFor("the poll workers of local and box", func() bool { return e.clock.Waiters() == 2 })
	e.clock.Advance(15 * time.Second)
	e.waitFor("box to be probed", func() bool {
		st, _ := e.runner.TargetState("box")
		return st.Reachability == runner.ReachReachable
	})

	// The same config again builds nothing and keeps what is known.
	e.runner.Reconfigure(withBox, newTarget)
	if st, _ := e.runner.TargetState("box"); built != 1 || st.Reachability != runner.ReachReachable {
		t.Errorf("after an unchanged Reconfigure: built %d targets, box %+v; want 1 and still reachable", built, st)
	}

	// Both workers wait for their next poll before box goes.
	e.waitFor("local to be probed", func() bool { return e.targetState().Reachability == runner.ReachReachable })
	e.waitFor("the poll workers to wait again", func() bool { return e.clock.Waiters() == 2 })
	e.runner.Reconfigure(envConfig(map[string]config.Target{"local": {}}), newTarget)
	if _, ok := e.runner.TargetState("box"); ok {
		t.Error("box still has a state after it was removed")
	}
	// Past the idle probe interval local is probed again, box never.
	boxCalls, localCalls := len(box.Calls()), len(e.herdr.Calls())
	e.clock.Advance(6 * time.Minute)
	e.waitFor("local to be probed again", func() bool { return len(e.herdr.Calls()) > localCalls })
	if n := len(box.Calls()); n != boxCalls {
		t.Errorf("box got %d calls after it was removed", n-boxCalls)
	}
}

// A target with an active run keeps its settings and is reported as
// deferred; max_parallel alone defers nothing. Once the run has ended the
// same Reconfigure replaces the target.
func TestReconfigureDefersATargetWithAnActiveRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)
	run := e.start(deps())
	other := newFakeHerdr()
	newTarget := func(string, config.Target) target.Target { return other }

	parallel := envConfig(map[string]config.Target{"local": {MaxParallel: 9}})
	if deferred := e.runner.Reconfigure(parallel, newTarget); len(deferred) != 0 {
		t.Errorf("max_parallel change: deferred = %q, want none", deferred)
	}
	moved := envConfig(map[string]config.Target{"local": {HerdrPath: "/opt/bin/herdr"}})
	if deferred := e.runner.Reconfigure(moved, newTarget); !slices.Equal(deferred, []string{"local"}) {
		t.Fatalf("herdr_path change with an active run: deferred = %q, want local", deferred)
	}
	removed := envConfig(map[string]config.Target{})
	if deferred := e.runner.Reconfigure(removed, newTarget); !slices.Equal(deferred, []string{"local"}) {
		t.Fatalf("removal with an active run: deferred = %q, want local", deferred)
	}
	// The run is still tracked on the target it started on.
	if got := e.poll(run.ID); got != store.StatusRunning {
		t.Fatalf("run after the deferred changes = %s, want running", got)
	}
	if n := len(other.Calls()); n != 0 {
		t.Errorf("the replacement target got %d calls while the change was deferred", n)
	}

	e.poll(run.ID)
	if got := e.poll(run.ID); got != store.StatusDone {
		t.Fatalf("run = %s, want done after two done polls", got)
	}
	if deferred := e.runner.Reconfigure(moved, newTarget); len(deferred) != 0 {
		t.Fatalf("after the run ended: deferred = %q, want none", deferred)
	}
	e.runner.PollTarget(e.ctx, "local")
	viaNewPath := slices.ContainsFunc(other.Calls(), func(c []string) bool {
		return slices.Equal(c[:min(len(c), 3)], []string{"/opt/bin/herdr", "pane", "list"})
	})
	if !viaNewPath {
		t.Errorf("replacement target calls = %q, want a pane list through the new herdr_path", other.Calls())
	}
}

// Reload can remove a target after a waiting fire has left the queue and
// before the dispatcher marks the target gone. Start then finds no target.
// The fire is dropped, not failed, and nothing is launched or notified.
func TestStartAfterTargetRemovedIsDroppedNotFailed(t *testing.T) {
	e := newEnv(t)
	before := len(e.herdr.Calls())
	removed := envConfig(map[string]config.Target{})
	newTarget := func(string, config.Target) target.Target { return e.herdr }
	if deferred := e.runner.Reconfigure(removed, newTarget); len(deferred) != 0 {
		t.Fatalf("deferred = %q, want none: local has no active run", deferred)
	}
	task := deps()
	scheduled := e.clock.Now()
	run, err := e.runner.Start(e.ctx, task, store.TriggerSchedule, scheduled)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.StatusDropped {
		t.Fatalf("status = %s, want dropped", run.Status)
	}
	if !strings.Contains(run.ExitReason, "target local removed from config") {
		t.Fatalf("reason = %q, want the removal", run.ExitReason)
	}
	if !run.StartedAt.Equal(run.EndedAt) || !run.EndedAt.Equal(e.clock.Now()) {
		t.Fatalf("start %v end %v, want both at the drop", run.StartedAt, run.EndedAt)
	}
	if run.HerdrWorkspace != "" || run.HerdrTab != "" || run.HerdrPane != "" {
		t.Fatalf("herdr ids set on a dropped run: %+v", run)
	}
	stored := e.get(run.ID)
	if stored.Status != store.StatusDropped || !stored.StartedAt.Equal(stored.EndedAt) || stored.ExitReason != run.ExitReason {
		t.Fatalf("stored = %+v, want the dropped row", stored)
	}
	if notes := e.notifications(); len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none", notes)
	}
	if n := len(e.herdr.Calls()); n != before {
		t.Fatalf("herdr calls = %d, was %d: a dropped fire launches nothing", n, before)
	}
	if n := len(e.runner.ActiveRuns()); n != 0 {
		t.Fatalf("%d active runs, want none", n)
	}
	if ended := e.endedRuns(); len(ended) != 1 || ended[0].ID != run.ID || ended[0].Status != store.StatusDropped {
		t.Fatalf("OnEnd = %+v, want the dropped run", ended)
	}
}
