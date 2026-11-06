//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/testherdr"
)

// TestRealHerdrDaemonRunsTask is the spec's integration test: the real
// daemon, a real herdr on localhost and a task with agent command
// `bash -c 'echo hi; sleep 5'`, started with `herdrcron run` and tracked
// until it ends done.
//
// herdr runs in an isolated throwaway session under a temporary HOME
// (targets.local.herdr_path points at a wrapper that adds --session), so
// the user's own herdr server is never touched.
//
//	go test -tags integration -run RealHerdrDaemon ./cmd/herdrcron/
func TestRealHerdrDaemonRunsTask(t *testing.T) {
	herdr, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not on PATH")
	}
	testherdr.UnsetUserEnv(t)
	// Temporary HOME and full XDG tree (including XDG_CACHE_HOME) so the
	// daemon, local PutFile and herdr agree on ~/.cache without touching
	// the user's config or server.
	dirs := testherdr.Fresh(t)
	p, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("hc-it-daemon-%d", os.Getpid())
	wrapper := filepath.Join(t.TempDir(), "herdr")
	write(t, wrapper, "#!/bin/sh\nexec "+herdr+" --session "+session+" \"$@\"\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	// The daemon cleanup is registered later, so it runs before this one;
	// both run before HOME and HERDR_* are restored. RemoveSession also
	// pins the temp dirs and strips HERDR_* on the herdr commands themselves.
	t.Cleanup(func() { dirs.RemoveSession(t, herdr, session) })

	repo := filepath.Join(t.TempDir(), "hc-it-repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p.ConfigFile(), "timezone: UTC\npoll_interval: 1s\ntargets:\n  local:\n    herdr_path: "+wrapper+"\n")
	write(t, filepath.Join(p.ConfigDir, "prompts", "hc-it.md"), "integration prompt")
	write(t, filepath.Join(p.TasksDir(), "hc-it.yaml"), `id: hc-it
target: local
schedule: "@daily"
cwd: `+repo+`
agent:
  command: bash
  args: ["-c", "echo hi; sleep 5"]
timeout: 2m
on_done: close_tab
`)

	ctx, cancel := context.WithCancel(context.Background())
	var dOut, dErr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"daemon"}, &dOut, &dErr) }()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("daemon exit %d: %s", code, dErr.String())
		}
	})

	var out, errOut bytes.Buffer
	for i := 0; run(context.Background(), []string{"list"}, &out, &errOut) != 0; i++ {
		if i > 500 {
			t.Fatalf("daemon never answered: %s", errOut.String())
		}
		out.Reset()
		errOut.Reset()
		time.Sleep(10 * time.Millisecond)
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"run", "hc-it"}, &out, &errOut); code != 0 {
		t.Fatalf("herdrcron run: exit %d: %s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), ": running, herdr tab ") {
		t.Fatalf("run did not start in a herdr tab: %s", out.String())
	}

	c, err := api.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var runRow api.RunSummary
	deadline := time.Now().Add(60 * time.Second)
	for {
		var res api.RunsListResult
		if err := c.Call(context.Background(), api.MethodRunsList, api.RunsListParams{TaskID: "hc-it"}, &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Runs) != 1 {
			t.Fatalf("runs = %+v, want exactly one", res.Runs)
		}
		runRow = res.Runs[0]
		if runRow.Status != "running" && runRow.Status != "starting" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run still %s after 60 s", runRow.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if runRow.Status != "done" {
		t.Fatalf("run ended %s (%s), want done", runRow.Status, runRow.ExitReason)
	}
	if runRow.HerdrTab == "" || runRow.EndedAt == "" {
		t.Errorf("run row lacks tab or end time: %+v", runRow)
	}

	var output api.RunsOutputResult
	if err := c.Call(context.Background(), api.MethodRunsOutput, api.RunsOutputParams{RunID: runRow.ID}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, "hi") {
		t.Errorf("output tail lacks the command's output %q:\n%s", "hi", output.Output)
	}

	// on_done: close_tab removed the run's hc- tab from the isolated server.
	tabOut, _ := exec.Command(wrapper, "tab", "get", runRow.HerdrTab).CombinedOutput()
	if !strings.Contains(string(tabOut), "not_found") {
		t.Errorf("on_done close_tab left tab %s: %s", runRow.HerdrTab, tabOut)
	}
}
