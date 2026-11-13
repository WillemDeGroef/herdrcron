//go:build integration

package runner_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
	"github.com/WillemDeGroef/herdrcron/internal/testherdr"
)

// realHerdr returns a herdr wrapper bound to a throwaway session under a
// temporary HOME (never the user's default server or config), and that
// tree. The session is stopped and deleted when the test ends.
func realHerdr(t *testing.T) (string, testherdr.Dirs) {
	t.Helper()
	herdrBin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not on PATH")
	}
	testherdr.UnsetUserEnv(t)
	// herdr keeps named sessions under $XDG_CONFIG_HOME (else ~/.config),
	// and the local target writes the prompt under $HOME/.cache. Both must
	// be temporary before the server starts. Cleanup carries that
	// environment itself, so it cannot dial the user's server.
	dirs := testherdr.Fresh(t)
	session := fmt.Sprintf("hc-it-%d", os.Getpid())
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "herdr")
	script := "#!/bin/sh\nexec " + herdrBin + " --session " + session + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dirs.RemoveSession(t, herdrBin, session) })
	return wrapper, dirs
}

// TestRealHerdrRunEndsDone runs a plain command through a real herdr, in
// a throwaway session under a temporary HOME (never the user's default
// server or config): the runner starts the server, opens an hc- workspace
// and tab, the command prints the prompt, and the run ends done with the
// prompt in its output tail.
//
//	go test -tags integration -run RealHerdr ./internal/runner/
func TestRealHerdrRunEndsDone(t *testing.T) {
	wrapper, dirs := realHerdr(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "herdrcron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	dir := t.TempDir()
	tricky := `Quotes "a" 'b', $HOME and ` + "`uname`" + `, done`
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "it.md"), []byte(tricky), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "hc-it-repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	r := runner.New(runner.Options{
		Clock: clock.Real(), Store: st, ConfigDir: dir,
		Targets: map[string]target.Target{"local": target.NewLocal()},
		Config: config.Config{Timezone: "UTC", Targets: map[string]config.Target{
			"local": {HerdrPath: wrapper},
		}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	task := config.Task{
		ID: "it", Target: "local", Cwd: repo, PromptFile: "prompts/it.md", OnDone: "close_tab",
		Agent: config.Agent{Command: "bash", Args: []string{"-c", `printf 'PROMPT<%s>\n' "$0"; sleep 3`}},
		Env:   map[string]string{"HC_IT": "1"},
	}
	ctx := context.Background()
	run, err := r.Start(ctx, task, store.TriggerManual, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.StatusRunning {
		t.Fatalf("run = %+v, want running", run)
	}
	deadline := time.Now().Add(60 * time.Second)
	for r.Active(run.ID) {
		if time.Now().After(deadline) {
			t.Fatal("run did not end")
		}
		time.Sleep(time.Second)
		r.PollTarget(ctx, "local")
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusDone {
		t.Errorf("status = %s (%s), want done", got.Status, got.ExitReason)
	}
	if !strings.Contains(got.OutputTail, "PROMPT<"+tricky+">") {
		t.Errorf("output tail does not hold the prompt unchanged:\n%s", got.OutputTail)
	}
	if _, err := os.Stat(filepath.Join(dirs.Home, ".cache/herdrcron/runs", run.ID+".md")); !os.IsNotExist(err) {
		t.Errorf("prompt file still on the target: %v", err)
	}
	out, _ := exec.Command(wrapper, "tab", "get", got.HerdrTab).CombinedOutput()
	if !strings.Contains(string(out), "not_found") {
		t.Errorf("on_done close_tab left tab %s: %s", got.HerdrTab, out)
	}
}

// TestRealHerdrRestartIsLost stops the herdr server under a
// running run and starts it again. herdr restores the session, so the
// run's pane id exists again as a fresh shell; the next poll must end the
// run lost instead of tracking that shell.
//
//	go test -tags integration -run RealHerdr ./internal/runner/
//
// The name is short on purpose: the session socket lives under t.TempDir(),
// and a unix socket path holds about 100 bytes.
func TestRealHerdrRestartIsLost(t *testing.T) {
	wrapper, _ := realHerdr(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "herdrcron.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "it.md"), []byte("wait"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "hc-it-repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	r := runner.New(runner.Options{
		Clock: clock.Real(), Store: st, ConfigDir: dir,
		Targets: map[string]target.Target{"local": target.NewLocal()},
		Config: config.Config{Timezone: "UTC", Targets: map[string]config.Target{
			"local": {HerdrPath: wrapper},
		}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	task := config.Task{
		ID: "it", Target: "local", Cwd: repo, PromptFile: "prompts/it.md", OnDone: "keep_tab",
		Agent: config.Agent{Command: "sleep", Args: []string{"300"}},
	}
	ctx := context.Background()
	run, err := r.Start(ctx, task, store.TriggerManual, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	r.PollTarget(ctx, "local")
	if !r.Active(run.ID) {
		got, _ := st.GetRun(ctx, run.ID)
		t.Fatalf("run ended before the restart: %s (%s)", got.Status, got.ExitReason)
	}

	if out, err := exec.Command(wrapper, "server", "stop").CombinedOutput(); err != nil {
		t.Fatalf("server stop: %v: %s", err, out)
	}
	time.Sleep(time.Second)
	server := exec.Command(wrapper, "server")
	server.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Wait() }()
	// Wait until the restarted server lists the restored pane.
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := exec.Command(wrapper, "pane", "list").Output()
		if strings.Contains(string(out), `"`+run.HerdrPane+`"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted server did not restore pane %s: %s", run.HerdrPane, out)
		}
		time.Sleep(200 * time.Millisecond)
	}

	r.PollTarget(ctx, "local")
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusLost || got.ExitReason != "pane replaced: herdr server restarted" {
		t.Errorf("run = %s (%s), want lost (pane replaced: herdr server restarted)", got.Status, got.ExitReason)
	}
	if r.Active(run.ID) {
		t.Errorf("lost run is still tracked")
	}
}
