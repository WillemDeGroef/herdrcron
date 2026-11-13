package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func reload(t *testing.T, c *api.Client) api.ReloadResult {
	t.Helper()
	return call[api.ReloadResult](t, c, api.MethodReload, nil)
}

func targetNames(t *testing.T, c *api.Client) []string {
	t.Helper()
	var names []string
	for _, ts := range call[api.StatusResult](t, c, api.MethodStatus, nil).Targets {
		names = append(names, ts.Name)
	}
	return names
}

// A notify section added to config.yaml notifies after "reload", with the
// daemon still running.
func TestReloadAppliesNotifyWithoutARestart(t *testing.T) {
	p := setup(t)
	dir := t.TempDir()
	script, out := filepath.Join(dir, "rec"), filepath.Join(dir, "calls")
	write(t, script, "#!/bin/sh\nprintf '%s|' \"$@\" >> "+out+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	c := startWith(t, p, daemon.Options{Version: "test", Targets: fakeTargets()})

	write(t, p.ConfigFile(), "targets:\n  vps:\n    host: vps\nnotify:\n  command: ["+script+", --title, herdrcron]\n  on: [failed]\n")
	if res := reload(t, c); res.RestartRequired {
		t.Fatalf("reload = %+v, want notify applied without a restart", res)
	}
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

// "reload" adds a target that is new in config.yaml and drops one that is
// gone from it: a task on the new target becomes valid and runs there, a
// task on the removed one becomes invalid.
func TestReloadAddsAndRemovesTargets(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.withConfig("targets:\n  old:\n    host: old\n")
	onTarget := func(id, name string) {
		write(t, filepath.Join(l.p.ConfigDir, "prompts", id+".md"), "x")
		write(t, filepath.Join(l.p.TasksDir(), id+".yaml"),
			"id: "+id+"\ntarget: "+name+"\ncwd: /tmp/repo\nschedule: \"@daily\"\ntimeout: 1h\nagent: {command: claude}\n")
	}
	onTarget("oldjob", "old")
	onTarget("boxjob", "box")
	old, box := newFakeHerdr(), newFakeHerdr()
	old.name, box.name = "old", "box"
	l.c, l.stop = runDaemon(t, l.p, daemon.Options{
		Version: "test", Clock: l.clk,
		Targets: map[string]target.Target{"local": l.herdr, "old": old, "box": box},
	})
	if got, want := targetNames(t, l.c), []string{"local", "old"}; !slices.Equal(got, want) {
		t.Fatalf("targets before reload = %q, want %q", got, want)
	}
	before := listByID(t, l.c)
	if before["oldjob"].State != api.StateIdle || before["boxjob"].State != api.StateInvalid {
		t.Fatalf("before reload: oldjob %q, boxjob %q; want idle and invalid", before["oldjob"].State, before["boxjob"].State)
	}

	l.withConfig("targets:\n  box:\n    host: box\n")
	res := reload(t, l.c)
	if res.RestartRequired || len(res.DeferredTargets) != 0 {
		t.Fatalf("reload = %+v, want the targets applied without a restart", res)
	}
	if got, want := targetNames(t, l.c), []string{"local", "box"}; !slices.Equal(got, want) {
		t.Errorf("targets after reload = %q, want %q", got, want)
	}
	after := listByID(t, l.c)
	if after["boxjob"].State != api.StateIdle {
		t.Errorf("boxjob = %+v, want it valid on the new target", after["boxjob"])
	}
	if s := after["oldjob"]; s.State != api.StateInvalid || !strings.Contains(s.Error, "old") {
		t.Errorf("oldjob = %+v, want it invalid: its target is gone", s)
	}

	run, err := runNow(l.c, "boxjob")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.StatusRunning || run.Target != "box" || !box.tabOpen(run.HerdrTab) {
		t.Errorf("run on the new target = %+v, want it running in a tab on box", run)
	}
	err = l.c.Call(context.Background(), api.MethodTargetsCheck, api.TargetCheckParams{Name: "old"}, nil)
	wantInvalidParams(t, "targets.check of the removed target", err, "unknown target")
}

// A target with an active run keeps its settings: the change waits, and
// the result says so. A reload after the run has ended applies it.
func TestReloadDefersATargetChangeWhileTheTargetHasActiveRuns(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()

	l.withConfig("targets:\n  local:\n    herdr_path: /opt/bin/herdr\n")
	res := reload(t, l.c)
	if !res.RestartRequired || !slices.Equal(res.DeferredTargets, []string{"local"}) {
		t.Fatalf("reload with an active run = %+v, want local deferred and restart_required", res)
	}
	got, err := l.attachCmd(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"herdr", "agent", "attach", run.HerdrPane}; !slices.Equal(got.Argv, want) {
		t.Errorf("attach argv of the active run = %q, want %q (the old herdr_path)", got.Argv, want)
	}

	l.herdr.set(run.HerdrPane, paneIdle)
	l.advanceUntil("the run ends", pollStep, l.ended(run.ID))
	l.herdr.setBin("/opt/bin/herdr")
	if res := reload(t, l.c); res.RestartRequired || len(res.DeferredTargets) != 0 {
		t.Fatalf("reload after the run ended = %+v, want the change applied", res)
	}
	next := l.mustRunNow()
	got, err = l.attachCmd(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/bin/herdr", "agent", "attach", next.HerdrPane}; !slices.Equal(got.Argv, want) {
		t.Errorf("attach argv after the change = %q, want %q", got.Argv, want)
	}
}

// max_parallel applies at once, active runs or not: a fire that waited for
// capacity starts when the reload makes room.
func TestReloadAppliesMaxParallelAndStartsQueuedFires(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.withConfig("targets:\n  local:\n    max_parallel: 1\n")
	write(t, filepath.Join(l.p.ConfigDir, "prompts", "second.md"), "x")
	write(t, filepath.Join(l.p.TasksDir(), "second.yaml"),
		"id: second\ntarget: local\ncwd: /tmp/repo\nschedule: \"@daily\"\ntimeout: 1h\nagent: {command: claude}\n")
	l.start()
	l.mustRunNow()
	queued := call[api.RunNowResult](t, l.c, api.MethodTasksRunNow, api.RunNowParams{ID: "second"})
	if !queued.Queued {
		t.Fatalf("run_now with max_parallel 1 and a run active = %+v, want queued", queued)
	}

	l.withConfig("targets:\n  local:\n    max_parallel: 2\n")
	if res := reload(t, l.c); res.RestartRequired || len(res.DeferredTargets) != 0 {
		t.Fatalf("reload = %+v, want max_parallel applied while a run is active", res)
	}
	eventually(t, "the queued fire to start", func() bool {
		runs := call[api.RunsListResult](t, l.c, api.MethodRunsList, api.RunsListParams{TaskID: "second"}).Runs
		return len(runs) == 1 && runs[0].Status == store.StatusRunning
	})
}

// Settings the daemon itself reads apply too: attach_mode here.
func TestReloadAppliesAttachMode(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()
	l.withConfig("attach_mode: full\n")
	if res := reload(t, l.c); res.RestartRequired {
		t.Fatalf("reload = %+v, want attach_mode applied without a restart", res)
	}
	got, err := l.attachCmd(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != api.AttachFull {
		t.Errorf("attach mode after reload = %q, want full", got.Mode)
	}
}

// A config.yaml change is not applied when the task files cannot be read:
// the call fails and the daemon keeps the config it had.
func TestReloadLeavesConfigWhenTasksCannotBeRead(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	tasks := l.p.TasksDir()
	bak := tasks + ".dir"
	if err := os.Rename(tasks, bak); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tasks, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = os.Remove(tasks)
		if err := os.Rename(bak, tasks); err != nil {
			t.Errorf("restore tasks dir: %v", err)
		}
	}
	defer restore()

	write(t, l.p.ConfigFile(), "timezone: Europe/Brussels\n")
	err := l.c.Call(context.Background(), api.MethodReload, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "tasks") {
		t.Fatalf("reload err = %v, want a tasks directory error", err)
	}
	if got := call[api.StatusResult](t, l.c, api.MethodStatus, nil).Timezone; got != "UTC" {
		t.Errorf("timezone = %q, want UTC: config was applied though the task files could not be read", got)
	}
}

// A targets.check that started before reload replaced the target does not
// become the new target's last check: the report describes the old machine.
func TestReloadDropsACheckStartedBeforeTheTargetChanged(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	release := l.herdr.holdExec()
	defer release()
	c2, err := api.Dial(l.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c2.Call(context.Background(), api.MethodTargetsCheck, api.TargetCheckParams{Name: "local"}, nil)
	}()
	select {
	case <-l.herdr.execHeldCh():
	case <-time.After(5 * time.Second):
		t.Fatal("targets.check did not reach the target")
	}

	write(t, l.p.ConfigFile(), "timezone: UTC\ntargets:\n  local:\n    herdr_path: /opt/bin/herdr\n")
	if res := reload(t, l.c); res.RestartRequired || len(res.DeferredTargets) != 0 {
		t.Fatalf("reload = %+v, want the herdr_path applied", res)
	}
	release()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	for _, ts := range call[api.StatusResult](t, l.c, api.MethodStatus, nil).Targets {
		if ts.Name == "local" && ts.LastCheck != nil {
			t.Errorf("last check = %+v, want none: it ran against the target before herdr_path changed", ts.LastCheck)
		}
	}
}
