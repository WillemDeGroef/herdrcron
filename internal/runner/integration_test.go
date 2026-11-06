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
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// TestRealHerdrRunEndsDone runs a plain command through a real herdr, in
// an isolated session (never the user's default server): the runner
// starts the server, opens an hc- workspace and tab, the command prints
// the prompt, and the run ends done with the prompt in its output tail.
//
//	go test -tags integration -run RealHerdr ./internal/runner/
func TestRealHerdrRunEndsDone(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not on PATH")
	}
	isolateFromUserHerdr(t)
	session := fmt.Sprintf("hc-it-%d", os.Getpid())
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "herdr")
	script := "#!/bin/sh\nexec herdr --session " + session + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("herdr", "--session", session, "server", "stop").Run()
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command("herdr", "session", "delete", session).Run()
	})

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
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".cache/herdrcron/runs", run.ID+".md")); !os.IsNotExist(err) {
		t.Errorf("prompt file still on the target: %v", err)
	}
	out, _ := exec.Command(wrapper, "tab", "get", got.HerdrTab).CombinedOutput()
	if !strings.Contains(string(out), "not_found") {
		t.Errorf("on_done close_tab left tab %s: %s", got.HerdrTab, out)
	}
}

// isolateFromUserHerdr unsets every HERDR_* variable for the test, so that
// no herdr command it runs can reach the user's server through
// HERDR_SOCKET_PATH (set when the tests run inside a herdr pane).
func isolateFromUserHerdr(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "HERDR_") {
			t.Setenv(k, "") // restores the value after the test
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
}
