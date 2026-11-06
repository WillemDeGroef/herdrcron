package runner_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
)

// checkTask is a task on the local target with an agent command and cwd.
func checkTask(id, command, cwd string) config.Task {
	return config.Task{ID: id, Target: "local", Cwd: cwd, Agent: config.Agent{Command: command}}
}

// hostShell makes the fake run the check scripts in a real sh with a temp
// HOME that has ~/src/app, and a PATH that has only an executable claude
// (plus the system dirs sh needs).
func hostShell(t *testing.T, e *env) string {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	for _, dir := range []string{bin, filepath.Join(home, "src", "app")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.herdr.hostEnv = []string{"HOME=" + home, "PATH=" + bin}
	return home
}

// items maps item name to result and detail.
func items(c runner.Check) map[string]runner.CheckItem {
	m := map[string]runner.CheckItem{}
	for _, it := range c.Items {
		m[it.Name] = it
	}
	return m
}

func names(c runner.Check) []string {
	var out []string
	for _, it := range c.Items {
		out = append(out, it.Name)
	}
	return out
}

func TestCheckTargetAllOK(t *testing.T) {
	e := newEnv(t)
	home := hostShell(t, e)
	tasks := []config.Task{
		checkTask("b", "claude", "~/src/app"),
		checkTask("a", "claude", filepath.Join(home, "src", "app")),
		checkTask("c", "claude", "~/src/app"),
	}
	c, err := e.runner.CheckTarget(e.ctx, "local", tasks)
	if err != nil {
		t.Fatal(err)
	}
	if !c.OK || c.Target != "local" || c.HerdrVersion != "0.9.1" || c.WantHerdrVersion != config.DefaultHerdrVersion {
		t.Errorf("check = %+v", c)
	}
	if !c.CheckedAt.Equal(e.clock.Now()) {
		t.Errorf("CheckedAt = %v, want %v", c.CheckedAt, e.clock.Now())
	}
	want := []string{"reachable", "herdr server", "herdr version", "agent command claude",
		"cwd " + filepath.Join(home, "src", "app"), "cwd ~/src/app"}
	if got := names(c); !slices.Equal(got, want) {
		t.Errorf("items = %q\nwant    %q", got, want)
	}
	for _, it := range c.Items {
		if it.Result != runner.CheckOK {
			t.Errorf("%s = %s %q, want ok", it.Name, it.Result, it.Detail)
		}
	}
	m := items(c)
	if got := m["agent command claude"].Tasks; !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("agent command tasks = %q", got)
	}
	if got := m["cwd ~/src/app"].Tasks; !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("cwd tasks = %q", got)
	}
	if st, _ := e.runner.TargetState("local"); st.Reachability != runner.ReachReachable {
		t.Errorf("reachability = %s, want reachable", st.Reachability)
	}
}

func TestCheckTargetMissingCommandAndCwdFail(t *testing.T) {
	e := newEnv(t)
	hostShell(t, e)
	c, err := e.runner.CheckTarget(e.ctx, "local", []config.Task{
		checkTask("a", "codex", "~/src/gone"),
		checkTask("b", "claude", "~/src/app"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.OK {
		t.Error("OK with a missing command and cwd")
	}
	m := items(c)
	if it := m["agent command codex"]; it.Result != runner.CheckFail || it.Detail != "not found on PATH" || !slices.Equal(it.Tasks, []string{"a"}) {
		t.Errorf("agent command codex = %+v", it)
	}
	if it := m["cwd ~/src/gone"]; it.Result != runner.CheckFail || it.Detail != "does not exist" {
		t.Errorf("cwd ~/src/gone = %+v", it)
	}
	if it := m["agent command claude"]; it.Result != runner.CheckOK {
		t.Errorf("agent command claude = %+v", it)
	}
}

func TestCheckTargetVersionMismatchWarns(t *testing.T) {
	e := newEnv(t)
	e.herdr.version = "0.9.0"
	c, err := e.runner.CheckTarget(e.ctx, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	it := items(c)["herdr version"]
	if it.Result != runner.CheckWarn || it.Detail != "got 0.9.0, want 0.9.1" {
		t.Errorf("herdr version = %+v", it)
	}
	if !c.OK || c.HerdrVersion != "0.9.0" {
		t.Errorf("check = %+v; a warning keeps it OK", c)
	}
}

func TestCheckTargetServerNotRunningWarnsAndDoesNotStartIt(t *testing.T) {
	e := newEnv(t)
	e.herdr.serverDown = true
	e.herdr.serverStarts = true
	c, err := e.runner.CheckTarget(e.ctx, "local", []config.Task{checkTask("a", "claude", "/src")})
	if err != nil {
		t.Fatal(err)
	}
	m := items(c)
	if it := m["herdr server"]; it.Result != runner.CheckWarn || it.Detail != "not running; a run starts it" {
		t.Errorf("herdr server = %+v", it)
	}
	if it := m["reachable"]; it.Result != runner.CheckOK {
		t.Errorf("reachable = %+v", it)
	}
	if it := m["herdr version"]; it.Result != runner.CheckOK {
		t.Errorf("herdr version = %+v", it)
	}
	if !c.OK {
		t.Error("not OK with only a warning")
	}
	if n := e.herdr.serverStartCalls(); n != 0 {
		t.Errorf("server started %d times", n)
	}
}

func TestCheckTargetUnreachableSkipsTheRest(t *testing.T) {
	e := newEnv(t)
	e.herdr.unreachable = true
	c, err := e.runner.CheckTarget(e.ctx, "local", []config.Task{checkTask("a", "claude", "/src")})
	if err != nil {
		t.Fatal(err)
	}
	if c.OK {
		t.Error("OK on an unreachable target")
	}
	if c.Items[0].Name != "reachable" || c.Items[0].Result != runner.CheckFail || c.Items[0].Detail == "" {
		t.Errorf("first item = %+v", c.Items[0])
	}
	for _, it := range c.Items[1:] {
		if it.Result != runner.CheckSkip {
			t.Errorf("%s = %s, want skip", it.Name, it.Result)
		}
	}
	if len(c.Items) != 5 {
		t.Errorf("items = %q", names(c))
	}
	if n := len(e.herdr.Calls()); n != 1 {
		t.Errorf("%d calls after the unreachable one: %q", n, e.herdr.Calls())
	}
	if st, _ := e.runner.TargetState("local"); st.Reachability != runner.ReachUnreachable {
		t.Errorf("reachability = %s, want unreachable", st.Reachability)
	}
}

func TestCheckTargetUnknownTarget(t *testing.T) {
	e := newEnv(t)
	if _, err := e.runner.CheckTarget(e.ctx, "nope", nil); err == nil {
		t.Error("no error for an unknown target")
	}
}
