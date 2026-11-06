package daemon_test

import (
	"context"
	"errors"
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

var outputNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func (l *lifecycle) output(runID string, lines int) (api.RunsOutputResult, error) {
	var res api.RunsOutputResult
	err := l.c.Call(context.Background(), api.MethodRunsOutput, api.RunsOutputParams{RunID: runID, Lines: lines}, &res)
	return res, err
}

func (l *lifecycle) attachCmd(runID string) (api.AttachCmdResult, error) {
	var res api.AttachCmdResult
	err := l.c.Call(context.Background(), api.MethodRunsAttachCmd, api.RunIDParams{RunID: runID}, &res)
	return res, err
}

// wantInvalidParams checks that err is an InvalidParams error whose message
// contains msg.
func wantInvalidParams(t *testing.T, what string, err error, msg string) {
	t.Helper()
	var rpcErr *api.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInvalidParams || !strings.Contains(rpcErr.Message, msg) {
		t.Errorf("%s: err = %v, want invalid params with %q", what, err, msg)
	}
}

func TestRunsOutputReadsTheLivePaneWithColours(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.herdr.setANSIOutput("\x1b[32mworking\x1b[0m\n")
	l.start()
	run := l.mustRunNow()

	got, err := l.output(run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != run.ID || got.Status != store.StatusRunning || !got.Live || got.Output != "\x1b[32mworking\x1b[0m\n" {
		t.Errorf("runs.output = %+v, want the live ANSI pane text", got)
	}
	want := []string{"herdr", "pane", "read", run.HerdrPane, "--source", "recent-unwrapped", "--lines", "200", "--format", "ansi"}
	if c := l.herdr.lastCall("herdr", "pane", "read"); !slices.Equal(c, want) {
		t.Errorf("pane read = %q, want %q", c, want)
	}

	if _, err := l.output(run.ID, 30); err != nil {
		t.Fatal(err)
	}
	if c := l.herdr.lastCall("herdr", "pane", "read"); flag(c, "--lines") != "30" {
		t.Errorf("pane read = %q, want --lines 30", c)
	}
}

func TestRunsOutputOfAnEndedRunIsItsStoredTail(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.herdr.setOutput("final words\n")
	l.herdr.setANSIOutput("\x1b[1mlive\x1b[0m")
	l.start()
	run := l.mustRunNow()
	l.herdr.set(run.HerdrPane, paneIdle)
	l.advanceUntil("the run ends", pollStep, l.ended(run.ID))

	got, err := l.output(run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != run.ID || got.Status != store.StatusDone || got.Live || got.Output != "final words\n" {
		t.Errorf("runs.output = %+v, want the stored tail of the done run", got)
	}
}

func TestRunsOutputErrors(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.start()
	_, err := l.output("", 0)
	wantInvalidParams(t, "no run_id", err, "run_id is required")
	_, err = l.output("01NOSUCHRUN", 0)
	wantInvalidParams(t, "unknown run", err, "run 01NOSUCHRUN not found")
}

// withConfig replaces config.yaml before the daemon starts.
func (l *lifecycle) withConfig(yaml string) {
	write(l.t, l.p.ConfigFile(), "timezone: UTC\n"+yaml)
}

func TestAttachCmdAttachesToTheAgentPane(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()

	got, err := l.attachCmd(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"herdr", "agent", "attach", run.HerdrPane}; !slices.Equal(got.Argv, want) || got.Mode != api.AttachPane || got.Note != "" {
		t.Errorf("runs.attach_cmd = %+v, want argv %q in pane mode", got, want)
	}
}

func TestAttachCmdUsesTheTargetsHerdrPath(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.withConfig("targets:\n  local:\n    herdr_path: /opt/bin/herdr\n")
	l.herdr.setBin("/opt/bin/herdr")
	l.start()
	run := l.mustRunNow()

	got, err := l.attachCmd(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/bin/herdr", "agent", "attach", run.HerdrPane}; !slices.Equal(got.Argv, want) {
		t.Errorf("argv = %q, want %q", got.Argv, want)
	}
}

func TestAttachCmdFullModeOpensTheFullUI(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.withConfig("attach_mode: full\ntargets:\n  local:\n    herdr_path: /opt/bin/herdr\n")
	l.herdr.setBin("/opt/bin/herdr")
	l.start()
	run := l.mustRunNow()

	got, err := l.attachCmd(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Argv, []string{"/opt/bin/herdr"}) || got.Mode != api.AttachFull || got.Note != "" {
		t.Errorf("runs.attach_cmd = %+v, want the full UI", got)
	}
}

func TestAttachCmdFallsBackToTheFullUIWhenTheAgentHasExited(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()
	l.herdr.set(run.HerdrPane, paneNoAgent)

	got, err := l.attachCmd(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Argv, []string{"herdr"}) || got.Mode != api.AttachFull || !strings.Contains(got.Note, "agent has exited") {
		t.Errorf("runs.attach_cmd = %+v, want the full UI with a note", got)
	}
}

func TestAttachCmdErrors(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.start()
	_, err := l.attachCmd("")
	wantInvalidParams(t, "no run_id", err, "run_id is required")
	_, err = l.attachCmd("01NOSUCHRUN")
	wantInvalidParams(t, "unknown run", err, "run 01NOSUCHRUN is not active")

	run := l.mustRunNow()
	l.herdr.set(run.HerdrPane, paneIdle)
	l.advanceUntil("the run ends", pollStep, l.ended(run.ID))
	_, err = l.attachCmd(run.ID)
	wantInvalidParams(t, "ended run", err, "run "+run.ID+" is not active")
}

func TestAttachCmdRefusesRemoteTargetsForNow(t *testing.T) {
	l := newLifecycle(t, outputNow, "timeout: 1h\n")
	l.withConfig("targets:\n  vps:\n    host: vps\n")
	write(t, filepath.Join(l.p.TasksDir(), "job.yaml"), `id: job
target: vps
cwd: /tmp/repo
schedule: "@daily"
agent: {command: claude}
timeout: 1h
`)
	l.c, l.stop = runDaemon(t, l.p, daemon.Options{
		Version: "test", Clock: l.clk, Targets: map[string]target.Target{"vps": l.herdr},
	})
	run := l.mustRunNow()

	_, err := l.attachCmd(run.ID)
	wantInvalidParams(t, "remote run", err, "comes with SSH targets")
}
