package runner_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/dispatch"
	"github.com/WillemDeGroef/herdrcron/internal/scheduler"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// startLost starts a scheduled run whose `pane run` is accepted by the
// target while its reply is lost: the agent runs, the run is stored as
// host_unreachable, and the target stays unreachable.
func (e *env) startLost() store.Run {
	e.t.Helper()
	e.herdr.mu.Lock()
	e.herdr.loseRunReply = true
	e.herdr.mu.Unlock()
	run, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
	if err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	if run.Status != store.StatusHostUnreachable || run.HerdrPane == "" {
		e.t.Fatalf("run with a lost launch reply = %+v, want host_unreachable with its pane id", run)
	}
	if !e.herdr.tabOpen(run.HerdrTab) {
		e.t.Fatal("the tab was closed although the target did not answer")
	}
	return run
}

// reconnect makes the target answer again, after its retry backoff.
func (e *env) reconnect() {
	e.setUnreachable(false)
	e.clock.Advance(5 * time.Minute)
}

func (e *env) adopt() (store.Run, bool, error) {
	e.t.Helper()
	return e.runner.AdoptLost(e.ctx, deps())
}

// Issue #37: the launch was accepted and its reply lost. When the target
// answers again the run's tab and pane are still there, so the run is
// adopted as running instead of left as host_unreachable, and tracked to
// its end like any run.
func TestRunWithLostLaunchReplyIsAdoptedWhenItsPaneExists(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, idle, idle)
	lost := e.startLost()
	if e.runner.Active(lost.ID) {
		t.Fatal("the host_unreachable run is tracked before the target answers")
	}

	e.reconnect()
	tabs := e.herdr.count("herdr", "tab", "create") + e.herdr.count("herdr", "workspace", "create")
	run, adopted, err := e.adopt()
	if err != nil || !adopted {
		t.Fatalf("AdoptLost = %+v, %v, %v, want the run adopted", run, adopted, err)
	}
	if run.ID != lost.ID || run.Status != store.StatusRunning || !run.EndedAt.IsZero() || run.ExitReason != "" {
		t.Errorf("adopted run = %+v, want run %s running with no end", run, lost.ID)
	}
	if got := e.get(lost.ID); got.Status != store.StatusRunning || !got.EndedAt.IsZero() || got.HerdrPane != lost.HerdrPane {
		t.Errorf("stored run = %+v, want running in pane %s", got, lost.HerdrPane)
	}
	if !e.runner.Active(lost.ID) {
		t.Error("the adopted run is not tracked")
	}
	if n := e.herdr.count("herdr", "tab", "create") + e.herdr.count("herdr", "workspace", "create"); n != tabs {
		t.Errorf("adoption opened %d tabs, want none", n-tabs)
	}

	// Adopting again changes nothing.
	if _, again, err := e.adopt(); err != nil || again {
		t.Errorf("second AdoptLost = %v, %v, want nothing to adopt", again, err)
	}
	for range 4 {
		e.poll(lost.ID)
	}
	if got := e.get(lost.ID); got.Status != store.StatusDone || got.EndedAt.IsZero() {
		t.Errorf("adopted run after its agent went idle = %s ended %v, want done", got.Status, got.EndedAt)
	}
}

// The lost-reply run is found behind the later fires of the same outage,
// which the runner did not even try.
func TestAdoptLostLooksBehindLaterUnreachableFires(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	lost := e.startLost()
	e.clock.Advance(10 * time.Second)
	if later := e.start(deps()); later.Status != store.StatusHostUnreachable {
		t.Fatalf("fire during the outage = %s, want host_unreachable", later.Status)
	}
	e.clock.Advance(10 * time.Second)
	run, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
	if err != nil || run.Status != store.StatusHostUnreachable {
		t.Fatalf("fire during the outage = %+v, %v, want host_unreachable", run, err)
	}

	e.reconnect()
	got, adopted, err := e.adopt()
	if err != nil || !adopted || got.ID != lost.ID {
		t.Fatalf("AdoptLost = %+v, %v, %v, want run %s adopted", got, adopted, err, lost.ID)
	}
}

func TestAdoptLostAdoptsNothing(t *testing.T) {
	t.Run("the pane is gone", func(t *testing.T) {
		e := newEnv(t)
		lost := e.startLost()
		e.reconnect()
		if _, err := e.herdr.Exec(e.ctx, []string{"herdr", "tab", "close", lost.HerdrTab}, nil); err != nil {
			t.Fatal(err)
		}
		if _, adopted, err := e.adopt(); err != nil || adopted {
			t.Fatalf("AdoptLost = %v, %v, want nothing adopted", adopted, err)
		}
		if got := e.get(lost.ID); got.Status != store.StatusHostUnreachable {
			t.Errorf("stored run = %s, want host_unreachable", got.Status)
		}
		// The run is settled: the target is not asked about it again.
		calls := len(e.herdr.Calls())
		if _, adopted, err := e.adopt(); err != nil || adopted {
			t.Fatalf("second AdoptLost = %v, %v, want nothing adopted", adopted, err)
		}
		if n := len(e.herdr.Calls()) - calls; n != 0 {
			t.Errorf("second AdoptLost made %d target calls, want 0", n)
		}
	})
	t.Run("the launch was never sent", func(t *testing.T) {
		e := newEnv(t)
		e.setUnreachable(true)
		run := e.start(deps())
		if run.Status != store.StatusHostUnreachable {
			t.Fatalf("run = %s, want host_unreachable", run.Status)
		}
		e.setUnreachable(false)
		calls := len(e.herdr.Calls())
		if _, adopted, err := e.adopt(); err != nil || adopted {
			t.Fatalf("AdoptLost = %v, %v, want nothing adopted", adopted, err)
		}
		if n := len(e.herdr.Calls()) - calls; n != 0 {
			t.Errorf("AdoptLost made %d target calls with no candidate, want 0", n)
		}
	})
	t.Run("the launch never reached the target", func(t *testing.T) {
		// pane run failed to connect: the tab holds an idle shell. Adopting
		// it would end the run done and swallow the fire.
		e := newEnv(t)
		e.herdr.mu.Lock()
		e.herdr.dropBeforeRun = true
		e.herdr.mu.Unlock()
		run, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
		if err != nil || run.Status != store.StatusHostUnreachable || !e.herdr.tabOpen(run.HerdrTab) {
			t.Fatalf("run = %+v, %v, want host_unreachable with its tab left open", run, err)
		}
		e.reconnect()
		if _, adopted, err := e.adopt(); err != nil || adopted {
			t.Fatalf("AdoptLost = %v, %v, want nothing adopted", adopted, err)
		}
		if e.herdr.tabOpen(run.HerdrTab) {
			t.Error("the tab of the launch that never arrived is still open")
		}
		if got := e.get(run.ID); got.Status != store.StatusHostUnreachable {
			t.Errorf("stored run = %s, want host_unreachable", got.Status)
		}
	})
	t.Run("the pane id belongs to another tab now", func(t *testing.T) {
		// A herdr server that restarted hands out its ids again.
		e := newEnv(t)
		lost := e.startLost()
		e.reconnect()
		e.herdr.mu.Lock()
		e.herdr.tabLabels[lost.HerdrTab] = "notes"
		e.herdr.mu.Unlock()
		if _, adopted, err := e.adopt(); err != nil || adopted {
			t.Fatalf("AdoptLost = %v, %v, want nothing adopted", adopted, err)
		}
	})
}

// While the target still does not answer, AdoptLost cannot tell: it
// returns the unreachable error, so the caller starts no catch-up run.
func TestAdoptLostFailsWhileTheTargetIsUnreachable(t *testing.T) {
	e := newEnv(t)
	lost := e.startLost()
	_, adopted, err := e.adopt()
	if adopted || !errors.Is(err, target.ErrUnreachable) {
		t.Fatalf("AdoptLost = %v, %v, want an unreachable error", adopted, err)
	}
	if got := e.get(lost.ID); got.Status != store.StatusHostUnreachable || !strings.Contains(got.ExitReason, "launch not confirmed") {
		t.Errorf("stored run = %s %q, want host_unreachable, launch not confirmed", got.Status, got.ExitReason)
	}
}

// A herdr server restart reuses the pane id under a new terminal id. Stale
// scrollback that still shows the launch line must not adopt the run; the
// tab is left alone so catch-up can start a fresh agent.
func TestAdoptLostDoesNotAdoptARestoredPaneWhoseTerminalChanged(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(shell)
	lost := e.startLost()
	e.herdr.restartServer()
	e.herdr.mu.Lock()
	e.herdr.output = "$ claude \"$(cat \"$HOME\"/.herdrcron/prompts/" + lost.ID + ".md)\"\n"
	e.herdr.mu.Unlock()
	e.reconnect()
	closes := e.herdr.count("herdr", "tab", "close")
	if _, adopted, err := e.adopt(); err != nil || adopted {
		t.Fatalf("AdoptLost = %v, %v, want nothing adopted", adopted, err)
	}
	if !e.herdr.tabOpen(lost.HerdrTab) {
		t.Error("the restored pane's tab was closed")
	}
	if got := e.get(lost.ID); got.Status != store.StatusHostUnreachable || !strings.Contains(got.ExitReason, "no agent found") {
		t.Errorf("stored run = %s %q, want host_unreachable settled as no agent", got.Status, got.ExitReason)
	}
	if n := e.herdr.count("herdr", "tab", "close") - closes; n != 0 {
		t.Errorf("%d tab closes during adoption, want none", n)
	}
}

// The agent of the lost launch has exited by the time the target answers:
// the pane is a shell at its prompt, and its output shows the launch line.
// The run is adopted, so the fire is not run a second time, and the next
// polls end it.
func TestAdoptLostAdoptsARunWhoseAgentAlreadyExited(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(shell)
	lost := e.startLost()
	e.herdr.mu.Lock()
	e.herdr.output = "$ claude \"$(cat \"$HOME\"/.herdrcron/prompts/" + lost.ID + ".md)\"\nall done\n$ \n"
	e.herdr.mu.Unlock()
	e.reconnect()
	if _, adopted, err := e.adopt(); err != nil || !adopted {
		t.Fatalf("AdoptLost = %v, %v, want the run adopted", adopted, err)
	}
	e.poll(lost.ID)
	e.poll(lost.ID)
	if got := e.get(lost.ID); got.Status != store.StatusDone {
		t.Errorf("adopted run = %s, want done", got.Status)
	}
}

// A scheduled fire was the first call to reach the target again and
// started a run before anyone asked about the lost one. The lost run is
// still adopted, so its agent is tracked and its tab closed at its end.
func TestAdoptLostAdoptsBehindALaterRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	lost := e.startLost()
	e.reconnect()
	e.herdr.script(working)
	later, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
	if err != nil || later.Status != store.StatusRunning {
		t.Fatalf("later run = %+v, %v, want running", later, err)
	}
	got, adopted, err := e.adopt()
	if err != nil || !adopted || got.ID != lost.ID {
		t.Fatalf("AdoptLost = %+v, %v, %v, want run %s adopted", got, adopted, err, lost.ID)
	}
}

// The first thing after the outage is an on-time fire, not catch-up.
// Overlap skip must not start a second agent beside the one whose launch
// reply was lost.
func TestOnTimeFireAdoptsLostLaunchAndDoesNotStartAnother(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working)
	lost := e.startLost()
	e.reconnect()

	cfg := config.Config{
		Timezone: "Europe/Brussels", HistoryDays: config.DefaultHistoryDays,
		Targets: map[string]config.Target{"local": {}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := dispatch.New(dispatch.Options{Runs: e.runner, Store: e.store, Config: cfg, Log: log})
	sched := scheduler.New(scheduler.Options{
		Clock: e.clock, Store: e.store, Runs: disp, Adopt: e.runner, Config: cfg, Log: log,
	})
	tk := deps()
	if err := sched.SetTasks(e.ctx, []config.TaskEntry{{ID: tk.ID, Task: tk}}); err != nil {
		t.Fatal(err)
	}
	// reconnect left the clock at 08:05 Brussels. The next */2 fire is
	// 08:06, and ten seconds later it is still on time.
	e.clock.Advance(70 * time.Second)
	launches := e.herdr.count("herdr", "pane", "run")
	sched.Tick(e.ctx)
	if n := e.herdr.count("herdr", "pane", "run"); n != launches {
		t.Fatalf("%d launches after the on-time fire, want %d", n, launches)
	}
	if got := e.get(lost.ID); got.Status != store.StatusRunning || !got.EndedAt.IsZero() || !got.StartedAt.Equal(lost.StartedAt) {
		t.Errorf("lost run = %+v, want %s still running from %v", got, lost.ID, lost.StartedAt)
	}
	if !e.runner.Active(lost.ID) {
		t.Error("the adopted run is not tracked")
	}
}
