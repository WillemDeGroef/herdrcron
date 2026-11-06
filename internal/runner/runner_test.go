package runner_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

type env struct {
	t         *testing.T
	ctx       context.Context
	clock     *clock.Fake
	store     *store.Store
	herdr     *fakeHerdr
	runner    *runner.Runner
	configDir string
}

const prompt = "Update the deps.\nSay \"done\" and 'stop'."

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "herdrcron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "deps.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, ctx: context.Background(), store: st, herdr: newFakeHerdr(), configDir: dir,
		clock: clock.NewFake(time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)),
	}
	e.runner = runner.New(runner.Options{
		Clock:     e.clock,
		Store:     st,
		Targets:   map[string]target.Target{"local": e.herdr},
		Config:    config.Config{Timezone: "Europe/Brussels", Targets: map[string]config.Target{"local": {}}},
		ConfigDir: dir,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The fake server answers at once or never.
		ServerStartWait: 50 * time.Millisecond,
	})
	return e
}

func deps() config.Task {
	return config.Task{
		ID: "deps", Target: "local", Cwd: "/src/peppol-sh", Schedule: "*/2 * * * *", Enabled: true,
		Agent:      config.Agent{Command: "claude", Args: []string{"--permission-mode", "acceptEdits"}},
		PromptFile: "prompts/deps.md", Timeout: config.Duration{Duration: time.Hour},
		Overlap: "skip", Missed: "run_once", OnDone: "keep_tab", OnBlocked: "notify",
		Env: map[string]string{"FOO": "bar baz"},
	}
}

func (e *env) start(t config.Task) store.Run {
	e.t.Helper()
	run, err := e.runner.Start(e.ctx, t, store.TriggerManual, time.Time{})
	if err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return run
}

func (e *env) lastCall(prefix ...string) []string {
	var last []string
	for _, c := range e.herdr.Calls() {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			last = c
		}
	}
	return last
}

func (e *env) get(id string) store.Run {
	e.t.Helper()
	r, err := e.store.GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// poll runs one poll of the local target and returns the run's stored
// status afterwards.
func (e *env) poll(id string) string {
	e.t.Helper()
	e.clock.Advance(15 * time.Second)
	e.runner.PollTarget(e.ctx, "local")
	return e.get(id).Status
}

// statuses polls n times and returns the status after each poll.
func (e *env) statuses(id string, n int) []string {
	e.t.Helper()
	var out []string
	for range n {
		out = append(out, e.poll(id))
	}
	return out
}

func TestRunGoesFromWorkingToDone(t *testing.T) {
	e := newEnv(t)
	e.herdr.output = "line 1\nline 2\n"
	e.herdr.script(starting, working, working, idle, idle)

	run := e.start(deps())
	if run.Status != store.StatusRunning || run.HerdrPane == "" || run.HerdrTab == "" || run.HerdrWorkspace == "" {
		t.Fatalf("started run = %+v, want running with herdr ids", run)
	}
	got := e.statuses(run.ID, 5)
	want := []string{"running", "running", "running", "running", "done"}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	r := e.get(run.ID)
	if r.EndedAt.IsZero() || r.OutputTail != "line 1\nline 2\n" {
		t.Errorf("ended run = %+v, want ended_at and the pane output as tail", r)
	}
	if n := e.herdr.count("herdr", "pane", "read"); n != 1 {
		t.Errorf("%d pane reads, want 1", n)
	}
	if c := e.lastCall("herdr", "pane", "read"); !slices.Equal(c, []string{"herdr", "pane", "read", r.HerdrPane, "--source", "recent-unwrapped", "--lines", "200"}) {
		t.Errorf("pane read = %q, want the last 200 unwrapped lines", c)
	}
	if !e.herdr.tabOpen(r.HerdrTab) {
		t.Errorf("on_done keep_tab closed the tab")
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target: %v", e.herdr.Files())
	}
	// An ended run is no longer polled.
	before := e.herdr.count("herdr", "pane", "list")
	e.poll(run.ID)
	if after := e.herdr.count("herdr", "pane", "list"); after != before {
		t.Errorf("pane list called with no active runs")
	}
}

func TestRunGoesFromWorkingToBlockedAndBackToDone(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, blocked, blocked, working, doneS, doneS)
	run := e.start(deps())
	got := e.statuses(run.ID, 6)
	want := []string{"running", "blocked", "blocked", "running", "running", "done"}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
}

func TestDoneNeedsTwoPollsInARow(t *testing.T) {
	e := newEnv(t)
	// done and idle count the same; a single one between working polls
	// does not finish the run.
	e.herdr.script(working, idle, working, doneS, working, doneS, idle)
	run := e.start(deps())
	got := e.statuses(run.ID, 7)
	want := []string{"running", "running", "running", "running", "running", "running", "done"}
	if !slices.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
}

func TestIdleWithoutAgentDoesNotFinish(t *testing.T) {
	e := newEnv(t)
	// herdr says idle but sees no agent yet: still starting.
	noAgent := sample{status: "idle"}
	e.herdr.script(noAgent, noAgent, noAgent)
	run := e.start(deps())
	if got := e.statuses(run.ID, 3); !slices.Equal(got, []string{"running", "running", "running"}) {
		t.Fatalf("statuses = %v, want running throughout", got)
	}
}

func TestUnknownStatusStaysRunning(t *testing.T) {
	e := newEnv(t)
	odd := sample{status: "unknown", agent: "claude"}
	e.herdr.script(working, odd, odd, odd, odd)
	run := e.start(deps())
	if got := e.statuses(run.ID, 5); slices.ContainsFunc(got, func(s string) bool { return s != "running" }) {
		t.Fatalf("statuses = %v, want running throughout", got)
	}
}

// An unknown status after blocked is not blocked any more: the spec maps
// unknown or no status to running.
func TestUnknownStatusAfterBlockedIsRunning(t *testing.T) {
	e := newEnv(t)
	odd := sample{status: "unknown", agent: "claude"}
	e.herdr.script(blocked, odd)
	run := e.start(deps())
	if got := e.statuses(run.ID, 2); !slices.Equal(got, []string{"blocked", "running"}) {
		t.Fatalf("statuses = %v, want blocked, running", got)
	}
}

// A poll whose context is cancelled (the daemon stopping) still stores the
// end of a run it finishes, so no row is left running with no tracker.
func TestRunEndIsStoredWhenPollContextIsCancelled(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, vanished)
	run := e.start(deps())
	e.poll(run.ID)
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.runner.PollTarget(ctx, "local")
	if got := e.get(run.ID); got.Status != store.StatusLost || got.EndedAt.IsZero() {
		t.Fatalf("run = %s ended %v, want lost with ended_at", got.Status, got.EndedAt)
	}
}

func TestPaneVanishesRunIsLost(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working, vanished)
	run := e.start(deps())
	got := e.statuses(run.ID, 3)
	if !slices.Equal(got, []string{"running", "running", "lost"}) {
		t.Fatalf("statuses = %v, want running, running, lost", got)
	}
	r := e.get(run.ID)
	if r.EndedAt.IsZero() || r.ExitReason == "" {
		t.Errorf("lost run = %+v, want ended_at and a reason", r)
	}
	if n := e.herdr.count("herdr", "pane", "read"); n != 0 {
		t.Errorf("%d pane reads of a gone pane", n)
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target")
	}
}

func TestCloseTabOnDone(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, idle, idle)
	task := deps()
	task.OnDone = "close_tab"
	run := e.start(task)
	e.statuses(run.ID, 3)
	r := e.get(run.ID)
	if r.Status != store.StatusDone {
		t.Fatalf("status = %s, want done", r.Status)
	}
	if e.herdr.tabOpen(r.HerdrTab) {
		t.Errorf("on_done close_tab left the tab open")
	}
}

// A plain command (no agent integration) ends when the shell is back at its
// prompt, but never on the first sample after the launch.
func TestPlainCommandEndsWhenShellIsBackAtPrompt(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(shell, starting, shell)
	run := e.start(deps())
	got := e.statuses(run.ID, 3)
	if !slices.Equal(got, []string{"running", "running", "done"}) {
		t.Fatalf("statuses = %v, want running, running, done", got)
	}
}

func TestAgentExitingOnItsOwnFailsTheRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, shell)
	run := e.start(deps())
	got := e.statuses(run.ID, 2)
	if !slices.Equal(got, []string{"running", "failed"}) {
		t.Fatalf("statuses = %v, want running, failed", got)
	}
	if r := e.get(run.ID); r.ExitReason != "agent exited" {
		t.Errorf("exit reason = %q, want %q", r.ExitReason, "agent exited")
	}
}

func TestOnePaneListPerPollCoversAllRuns(t *testing.T) {
	e := newEnv(t)
	e.herdr.script(working, working)
	e.herdr.script(working, working)
	e.herdr.script(working, working)
	a, b, c := e.start(deps()), e.start(deps()), e.start(deps())
	before := e.herdr.count("herdr", "pane", "list")
	e.poll(a.ID)
	if n := e.herdr.count("herdr", "pane", "list") - before; n != 1 {
		t.Errorf("%d pane list calls for 3 runs, want 1", n)
	}
	if n := e.herdr.count("herdr", "pane", "process-info"); n != 0 {
		t.Errorf("%d process-info calls while agents are present, want 0", n)
	}
	for _, r := range []store.Run{a, b, c} {
		if s := e.get(r.ID).Status; s != store.StatusRunning {
			t.Errorf("run %s = %s", r.ID, s)
		}
	}
}

func TestLaunchCreatesWorkspaceAndRenamesItsFirstTab(t *testing.T) {
	e := newEnv(t)
	run := e.start(deps())

	calls := e.herdr.Calls()
	var create, rename []string
	for _, c := range calls {
		switch {
		case slices.Equal(c[:3], []string{"herdr", "workspace", "create"}):
			create = c
		case slices.Equal(c[:3], []string{"herdr", "tab", "rename"}):
			rename = c
		case slices.Equal(c[:3], []string{"herdr", "tab", "create"}):
			t.Errorf("tab create in a new workspace: %q", c)
		}
	}
	if flag(create, "--label") != "hc-peppol-sh" || flag(create, "--cwd") != "/src/peppol-sh" {
		t.Errorf("workspace create = %q", create)
	}
	wantEnv := []string{"HERDRCRON_RUN_ID=" + run.ID, "FOO=bar baz"}
	if got := flags(create, "--env"); !slices.Equal(got, wantEnv) {
		t.Errorf("env = %q, want %q", got, wantEnv)
	}
	// 07:00 UTC is 08:00 in Brussels.
	if len(rename) != 5 || rename[3] != run.HerdrTab || rename[4] != "hc-deps-0800" {
		t.Errorf("tab rename = %q, want tab %s labelled hc-deps-0800", rename, run.HerdrTab)
	}

	// The second run reuses the workspace with a new tab.
	second := e.start(deps())
	if second.HerdrWorkspace != run.HerdrWorkspace || second.HerdrTab == run.HerdrTab {
		t.Errorf("second run ws %s tab %s, first ws %s tab %s", second.HerdrWorkspace, second.HerdrTab, run.HerdrWorkspace, run.HerdrTab)
	}
	if n := e.herdr.count("herdr", "tab", "create"); n != 1 {
		t.Errorf("%d tab creates, want 1", n)
	}
}

func TestPromptFileIsWrittenAndCommandReadsIt(t *testing.T) {
	e := newEnv(t)
	run := e.start(deps())
	path := "~/.cache/herdrcron/runs/" + run.ID + ".md"
	if got := string(e.herdr.Files()[path]); got != prompt {
		t.Errorf("prompt file %s = %q, want the prompt", path, got)
	}
	cmd := e.herdr.pane(0).command
	if strings.Contains(cmd, "Update the deps") {
		t.Errorf("prompt text typed into the pane: %q", cmd)
	}
}

func TestPreflightFailureFailsTheRun(t *testing.T) {
	e := newEnv(t)
	e.herdr.preflight = "agent command claude not found on PATH"
	run, err := e.runner.Start(e.ctx, deps(), store.TriggerSchedule, e.clock.Now())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := e.get(run.ID)
	if r.Status != store.StatusFailed || r.EndedAt.IsZero() || !strings.Contains(r.ExitReason, "not found on PATH") {
		t.Errorf("run = %+v, want failed with the preflight reason", r)
	}
	if n := e.herdr.count("herdr", "workspace", "create"); n != 0 {
		t.Errorf("workspace created after a failed preflight")
	}
}

func TestPreflightIsCachedForTenMinutes(t *testing.T) {
	e := newEnv(t)
	preflights := func() int {
		n := 0
		for _, c := range e.herdr.Calls() {
			if c[0] != "herdr" {
				n++
			}
		}
		return n
	}
	e.start(deps())
	e.clock.Advance(9 * time.Minute)
	e.start(deps())
	if n := preflights(); n != 1 {
		t.Errorf("%d preflight checks within 10 min, want 1", n)
	}
	e.clock.Advance(2 * time.Minute)
	e.start(deps())
	if n := preflights(); n != 2 {
		t.Errorf("%d preflight checks after 11 min, want 2", n)
	}
}

func TestPreflightResolvesCwd(t *testing.T) {
	e := newEnv(t)
	e.herdr.resolvedCwd = "/home/u/src/peppol-sh"
	task := deps()
	task.Cwd = "~/src/peppol-sh"
	e.start(task)
	for _, c := range e.herdr.Calls() {
		if slices.Equal(c[:3], []string{"herdr", "workspace", "create"}) && flag(c, "--cwd") != "/home/u/src/peppol-sh" {
			t.Errorf("workspace create cwd = %q, want the resolved path", flag(c, "--cwd"))
		}
	}
}

func TestServerNotRunningIsStartedOnce(t *testing.T) {
	e := newEnv(t)
	e.herdr.serverDown, e.herdr.serverStarts = true, true
	run := e.start(deps())
	if run.Status != store.StatusRunning {
		t.Fatalf("run = %+v, want running after the server was started", run)
	}
	if n := e.herdr.serverStartCalls(); n != 1 {
		t.Errorf("%d server starts, want 1", n)
	}
}

func TestServerNotRunningFailsAfterRetries(t *testing.T) {
	e := newEnv(t)
	e.herdr.serverDown = true
	run := e.start(deps())
	r := e.get(run.ID)
	if r.Status != store.StatusFailed || !strings.Contains(r.ExitReason, "server") {
		t.Errorf("run = %+v, want failed because the herdr server is down", r)
	}
}

// The prompt reaches the agent byte for byte: the typed command line is run
// by a real shell, and a stand-in agent writes the argv it got.
func TestPromptReachesAgentUnchanged(t *testing.T) {
	e := newEnv(t)
	tricky := "Quotes \"double\" and 'single', $HOME, $(id), `uname`,\nnew\nlines\ttab \\ back\\slash *"
	if err := os.WriteFile(filepath.Join(e.configDir, "prompts", "deps.md"), []byte(tricky), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	out := filepath.Join(bin, "argv")
	agent := filepath.Join(bin, "fake agent's")
	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + out + "\n"
	if err := os.WriteFile(agent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	task := deps()
	task.Agent = config.Agent{Command: agent, Args: []string{"--append", "it's $x `y`"}}
	run := e.start(task)

	// Play the target: put the prompt file in a fake home, type the command.
	home := t.TempDir()
	rel := strings.TrimPrefix("~/.cache/herdrcron/runs/"+run.ID+".md", "~/")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(home, rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	data := e.herdr.Files()["~/"+rel]
	if err := os.WriteFile(filepath.Join(home, rel), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", e.herdr.pane(0).command)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("typed command failed: %v\n%s", err, b)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(got), "\x00"), "\x00")
	want := []string{"--append", "it's $x `y`", tricky}
	if !slices.Equal(args, want) {
		t.Errorf("agent argv = %q, want %q", args, want)
	}
}

func TestLaunchFailureClosesTheTabAndDeletesThePromptFile(t *testing.T) {
	e := newEnv(t)
	e.herdr.failRun = true
	run := e.start(deps())
	r := e.get(run.ID)
	if r.Status != store.StatusFailed || r.EndedAt.IsZero() || !strings.Contains(r.ExitReason, "launch agent") {
		t.Fatalf("run = %+v, want failed at launch", r)
	}
	if r.HerdrTab == "" || e.herdr.tabOpen(r.HerdrTab) {
		t.Errorf("tab %q left open after a failed launch", r.HerdrTab)
	}
	if len(e.herdr.Files()) != 0 {
		t.Errorf("prompt file left on the target")
	}
	if e.runner.Active(run.ID) {
		t.Errorf("failed run is still tracked")
	}
}
