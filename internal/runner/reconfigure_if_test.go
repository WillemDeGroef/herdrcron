package runner_test

import (
	"slices"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// WouldDefer names a target whose settings change while it has an active
// run, and names nothing without one or for max_parallel alone. Asking
// changes nothing: the target is not replaced.
func TestWouldDeferNamesABusyChangedTargetAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	gen := e.targetState().Gen
	moved := envConfig(map[string]config.Target{"local": {HerdrPath: "/opt/bin/herdr"}})
	if deferred := e.runner.WouldDefer(moved); len(deferred) != 0 {
		t.Fatalf("without a run: deferred = %q, want none", deferred)
	}
	if got := e.targetState().Gen; got != gen {
		t.Fatalf("WouldDefer without a run replaced the target: gen %d, want %d", got, gen)
	}

	e.herdr.script(working, idle, idle)
	run := e.start(deps())
	for range 2 {
		if deferred := e.runner.WouldDefer(moved); !slices.Equal(deferred, []string{"local"}) {
			t.Fatalf("herdr_path change with an active run: deferred = %q, want local", deferred)
		}
	}
	if deferred := e.runner.WouldDefer(envConfig(map[string]config.Target{})); !slices.Equal(deferred, []string{"local"}) {
		t.Errorf("removal with an active run: deferred = %q, want local", deferred)
	}
	parallel := envConfig(map[string]config.Target{"local": {MaxParallel: 9}})
	if deferred := e.runner.WouldDefer(parallel); len(deferred) != 0 {
		t.Errorf("max_parallel change: deferred = %q, want none", deferred)
	}
	// The run is still tracked on the target it started on.
	if got := e.poll(run.ID); got != store.StatusRunning {
		t.Fatalf("run after WouldDefer = %s, want running", got)
	}
	if got := e.targetState().Gen; got != gen {
		t.Errorf("WouldDefer replaced the target: gen %d, want %d", got, gen)
	}
}

// ReconfigureIf applies the config like Reconfigure when the deferred set
// is the expected one: an idle target is replaced and used from then on.
func TestReconfigureIfAppliesWhenTheDeferredSetIsAsExpected(t *testing.T) {
	e := newEnv(t)
	other := newFakeHerdr()
	newTarget := func(string, config.Target) target.Target { return other }
	moved := envConfig(map[string]config.Target{"local": {HerdrPath: "/opt/bin/herdr"}})

	deferred, ok := e.runner.ReconfigureIf(moved, newTarget, e.runner.WouldDefer(moved))
	if !ok || len(deferred) != 0 {
		t.Fatalf("ReconfigureIf = %q, %v; want none and applied", deferred, ok)
	}
	e.runner.PollTarget(e.ctx, "local")
	viaNewPath := slices.ContainsFunc(other.Calls(), func(c []string) bool {
		return slices.Equal(c[:min(len(c), 3)], []string{"/opt/bin/herdr", "pane", "list"})
	})
	if !viaNewPath {
		t.Errorf("replacement target calls = %q, want a pane list through the new herdr_path", other.Calls())
	}
}

// With the expected set naming a busy target, ReconfigureIf applies the
// rest of the config, a new target included, and returns that set; the
// busy target keeps its settings.
func TestReconfigureIfAppliesAroundAnExpectedDeferredTarget(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)
	run := e.start(deps())
	gen := e.targetState().Gen
	var built []string
	newTarget := func(name string, _ config.Target) target.Target {
		built = append(built, name)
		return newFakeHerdr()
	}
	cfg := envConfig(map[string]config.Target{"local": {HerdrPath: "/opt/bin/herdr"}, "box": {Host: "box"}})

	deferred, ok := e.runner.ReconfigureIf(cfg, newTarget, e.runner.WouldDefer(cfg))
	if !ok || !slices.Equal(deferred, []string{"local"}) {
		t.Fatalf("ReconfigureIf = %q, %v; want local and applied", deferred, ok)
	}
	if _, known := e.runner.TargetState("box"); !known || !slices.Equal(built, []string{"box"}) {
		t.Errorf("box known = %v, built %q; want box added and nothing else built", known, built)
	}
	if got := e.targetState().Gen; got != gen {
		t.Errorf("local was replaced under its run: gen %d, want %d", got, gen)
	}
	if got := e.poll(run.ID); got != store.StatusRunning {
		t.Errorf("run after ReconfigureIf = %s, want running", got)
	}
}

// A stale expected set changes nothing: ReconfigureIf returns the set as
// it is now and false, whether a run started or ended since the set was
// read. With the returned set it applies.
func TestReconfigureIfRefusesAStaleDeferredSetAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	gen := e.targetState().Gen
	built := 0
	newTarget := func(string, config.Target) target.Target { built++; return newFakeHerdr() }
	cfg := envConfig(map[string]config.Target{"local": {HerdrPath: "/opt/bin/herdr"}, "box": {Host: "box"}})
	unchanged := func(when string) {
		t.Helper()
		_, known := e.runner.TargetState("box")
		if got := e.targetState().Gen; known || built != 0 || got != gen {
			t.Fatalf("%s: box known = %v, built %d targets, local gen %d; want nothing changed (gen %d)", when, known, built, got, gen)
		}
	}

	// A run started after the set was read as empty.
	idleSet := e.runner.WouldDefer(cfg)
	e.herdr.script(working, idle, idle)
	run := e.start(deps())
	busySet, ok := e.runner.ReconfigureIf(cfg, newTarget, idleSet)
	if ok || !slices.Equal(busySet, []string{"local"}) {
		t.Fatalf("expecting none with an active run: ReconfigureIf = %q, %v; want local and not applied", busySet, ok)
	}
	unchanged("after a run started")

	// The run ended after the set was read as local.
	e.poll(run.ID)
	e.poll(run.ID)
	if got := e.poll(run.ID); got != store.StatusDone {
		t.Fatalf("run = %s, want done after two done polls", got)
	}
	current, ok := e.runner.ReconfigureIf(cfg, newTarget, busySet)
	if ok || len(current) != 0 {
		t.Fatalf("expecting local after the run ended: ReconfigureIf = %q, %v; want none and not applied", current, ok)
	}
	unchanged("after the run ended")

	if deferred, ok := e.runner.ReconfigureIf(cfg, newTarget, current); !ok || len(deferred) != 0 {
		t.Fatalf("with the current set: ReconfigureIf = %q, %v; want none and applied", deferred, ok)
	}
	if _, known := e.runner.TargetState("box"); !known || built != 2 || e.targetState().Gen == gen {
		t.Errorf("box known = %v, built %d targets, local gen still %d = %v; want box added and local replaced",
			known, built, gen, e.targetState().Gen == gen)
	}
}
