package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const secretPrompt = "SECRET-PROMPT-TEXT"
const secretEnv = "SECRET-ENV-VALUE"

func setup(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	p := paths.Paths{ConfigDir: filepath.Join(root, "cfg"), StateDir: filepath.Join(root, "st")}
	write(t, p.ConfigFile(), "targets:\n  vps:\n    host: vps\n")
	write(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md"), secretPrompt)
	write(t, filepath.Join(p.TasksDir(), "nightly.yaml"), `id: nightly
name: Nightly deps
target: local
cwd: /tmp
schedule: "@daily"
agent: {command: claude}
timeout: 1h
env:
  API_TOKEN: `+secretEnv+`
`)
	write(t, filepath.Join(p.ConfigDir, "prompts", "parked.md"), "x")
	write(t, filepath.Join(p.TasksDir(), "parked.yaml"), `id: parked
target: vps
schedule: "0 7 * * 1"
enabled: false
agent: {command: claude}
timeout: 30m
`)
	write(t, filepath.Join(p.TasksDir(), "broken.yaml"), `id: broken
target: mars
schedule: "@daily"
agent: {command: claude}
timeout: 1h
`)
	return p
}

// start runs the daemon until the test ends and returns a connected client.
func start(t *testing.T, p paths.Paths) *api.Client {
	t.Helper()
	return startWith(t, p, daemon.Options{Version: "test"})
}

func startWith(t *testing.T, p paths.Paths, opts daemon.Options) *api.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// runErr is stored before done is closed. Cleanup and the early-exit
	// path both wait on done, so a failed start cannot leave Cleanup
	// blocked on a second receive.
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = daemon.Run(ctx, p, opts)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Errorf("daemon.Run: %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-done:
			t.Fatalf("daemon exited early: %v", runErr)
		default:
		}
		c, err := api.Dial(p.Socket())
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTasksListShowsDisplayStates(t *testing.T) {
	c := start(t, setup(t))
	var res api.TasksListResult
	if err := c.Call(context.Background(), api.MethodTasksList, nil, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Tasks) != 3 {
		t.Fatalf("got %d tasks: %+v", len(res.Tasks), res.Tasks)
	}
	byID := map[string]api.TaskSummary{}
	for _, s := range res.Tasks {
		byID[s.ID] = s
	}
	if got := byID["nightly"]; got.State != api.StateIdle || got.Target != "local" || got.Schedule != "@daily" || got.Name != "Nightly deps" {
		t.Errorf("nightly = %+v", got)
	}
	if got := byID["parked"]; got.State != api.StateOff || got.Target != "vps" {
		t.Errorf("parked = %+v", got)
	}
	if got := byID["broken"]; got.State != api.StateInvalid || !strings.Contains(got.Error, `unknown target "mars"`) {
		t.Errorf("broken = %+v", got)
	}
}

func TestStatus(t *testing.T) {
	c := start(t, setup(t))
	var res api.StatusResult
	if err := c.Call(context.Background(), api.MethodStatus, nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Version != "test" || res.UptimeSeconds < 0 || res.PausedAll {
		t.Errorf("status = %+v", res)
	}
	if _, err := time.Parse(time.RFC3339, res.StartedAt); err != nil {
		t.Errorf("started_at %q: %v", res.StartedAt, err)
	}
	var names []string
	for _, tg := range res.Targets {
		names = append(names, tg.Name)
	}
	if strings.Join(names, ",") != "local,vps" {
		t.Errorf("targets = %v, want local,vps", names)
	}
}

func TestLocalTargetWithoutConfigFile(t *testing.T) {
	p := setup(t)
	if err := os.Remove(p.ConfigFile()); err != nil {
		t.Fatal(err)
	}
	c := start(t, p)
	var res api.StatusResult
	if err := c.Call(context.Background(), api.MethodStatus, nil, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Name != "local" {
		t.Errorf("targets = %+v", res.Targets)
	}
}

func TestLogIsJSONAndNeverHoldsPromptOrEnvValues(t *testing.T) {
	p := setup(t)
	c := start(t, p)
	if err := c.Call(context.Background(), api.MethodTasksList, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Log())
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		t.Fatal("daemon.log is empty")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
	}
	log := string(data)
	if strings.Contains(log, secretPrompt) || strings.Contains(log, secretEnv) {
		t.Fatalf("log leaks prompt text or env values:\n%s", log)
	}
	if !strings.Contains(log, "API_TOKEN") || !strings.Contains(log, "nightly.md") {
		t.Errorf("want env keys and prompt file path in the log:\n%s", log)
	}
	if !strings.Contains(log, `"task_id":"broken"`) {
		t.Errorf("want the invalid task logged with task_id:\n%s", log)
	}
}

func TestSocketRemovedOnShutdown(t *testing.T) {
	p := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- daemon.Run(ctx, p, daemon.Options{Version: "test"}) }()
	for i := 0; ; i++ {
		if _, err := os.Stat(p.Socket()); err == nil {
			break
		}
		if i > 500 {
			t.Fatal("socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
		t.Fatalf("socket still present after shutdown: %v", err)
	}
}

func TestStartFailsFastWhenConfigIsInvalid(t *testing.T) {
	if os.Getenv("HERDRCRON_START_EARLY") == "1" {
		p := setup(t)
		write(t, p.ConfigFile(), "timezone: Mars/Olympus\n")
		start(t, p)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartFailsFastWhenConfigIsInvalid$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HERDRCRON_START_EARLY=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child passed, want a fast failure:\n%s", out)
	}
	if !bytes.Contains(out, []byte("daemon exited early")) {
		t.Fatalf("child did not fail fast:\n%s", out)
	}
}

func TestBadConfigFailsStartup(t *testing.T) {
	p := setup(t)
	write(t, p.ConfigFile(), "timezone: Mars/Olympus\n")
	if err := daemon.Run(context.Background(), p, daemon.Options{}); err == nil {
		t.Fatal("want error for invalid config.yaml")
	}
}

func listByID(t *testing.T, c *api.Client) map[string]api.TaskSummary {
	t.Helper()
	var res api.TasksListResult
	if err := c.Call(context.Background(), api.MethodTasksList, nil, &res); err != nil {
		t.Fatal(err)
	}
	byID := map[string]api.TaskSummary{}
	for _, s := range res.Tasks {
		byID[s.ID] = s
	}
	return byID
}

func TestScheduledFireShowsAsLastRunAndMovesNext(t *testing.T) {
	p := setup(t)
	write(t, p.ConfigFile(), "timezone: Europe/Brussels\ntargets:\n  vps:\n    host: vps\n")
	clk := clock.NewFake(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clk})

	byID := listByID(t, c)
	// @daily in Brussels: midnight CET is 23:00 UTC.
	if got := byID["nightly"]; got.NextFireAt != "2026-11-06T23:00:00Z" || got.LastRun != nil {
		t.Errorf("nightly before the fire = %+v, want next 2026-11-06T23:00:00Z and no last run", got)
	}
	for _, id := range []string{"parked", "broken"} {
		if got := byID[id]; got.NextFireAt != "" {
			t.Errorf("%s next_fire_at = %q, want none", id, got.NextFireAt)
		}
	}

	clk.Set(time.Date(2026, 11, 6, 23, 0, 1, 0, time.UTC))
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := listByID(t, c)["nightly"]
		if got.LastRun != nil {
			if got.LastRun.Status != "done" || got.LastRun.Trigger != "schedule" ||
				got.LastRun.ScheduledAt != "2026-11-06T23:00:00Z" || len(got.LastRun.ID) != 26 {
				t.Errorf("last run = %+v", got.LastRun)
			}
			if got.NextFireAt != "2026-11-07T23:00:00Z" {
				t.Errorf("next_fire_at after the fire = %q, want 2026-11-07T23:00:00Z", got.NextFireAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no run recorded after the fire time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, "herdrcron.db")); err != nil {
		t.Errorf("state database: %v", err)
	}
}
