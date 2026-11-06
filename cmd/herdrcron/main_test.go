package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
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

func xdg(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "s"))
	p, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListWithoutDaemonFails(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"list"}, &out, &errOut)
	if code == 0 {
		t.Fatal("want non-zero exit when the daemon is not running")
	}
	if msg := errOut.String(); !strings.Contains(msg, "daemon is not running") || !strings.Contains(msg, "herdrcron daemon") {
		t.Fatalf("stderr = %q, want a clear message that says how to start it", msg)
	}
}

func TestDaemonAndList(t *testing.T) {
	p := xdg(t)
	write(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md"), "go")
	write(t, filepath.Join(p.TasksDir(), "nightly.yaml"),
		"id: nightly\ntarget: local\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\n")
	write(t, filepath.Join(p.ConfigDir, "prompts", "parked.md"), "go")
	write(t, filepath.Join(p.TasksDir(), "parked.yaml"),
		"id: parked\ntarget: local\nschedule: \"*/2 * * * *\"\nenabled: false\nagent: {command: claude}\ntimeout: 1h\n")
	write(t, filepath.Join(p.TasksDir(), "broken.yaml"),
		"id: broken\ntarget: mars\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\n")

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
	for i := 0; ; i++ {
		out.Reset()
		errOut.Reset()
		if run(context.Background(), []string{"list"}, &out, &errOut) == 0 {
			break
		}
		if i > 500 {
			t.Fatalf("list never succeeded: %s", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 tasks, got:\n%s", out.String())
	}
	for _, col := range []string{"ID", "TARGET", "SCHEDULE", "STATE", "LAST", "NEXT"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks %s", lines[0], col)
		}
	}
	wantRows := [][]string{
		{"broken", "mars", "@daily", "invalid", `unknown target "mars"`},
		{"nightly", "local", "@daily", "idle"},
		{"parked", "local", "*/2 * * * *", "off"},
	}
	for i, want := range wantRows {
		for _, cell := range want {
			if !strings.Contains(lines[i+1], cell) {
				t.Errorf("row %d %q lacks %q", i+1, lines[i+1], cell)
			}
		}
	}

	// enable, pause, resume and disable change the task file on disk.
	nightly := filepath.Join(p.TasksDir(), "nightly.yaml")
	parked := filepath.Join(p.TasksDir(), "parked.yaml")
	for _, step := range []struct {
		args       []string
		file, want string
	}{
		{[]string{"enable", "parked"}, parked, "enabled: true"},
		{[]string{"pause", "nightly"}, nightly, "paused_until: indefinite"},
		{[]string{"resume", "nightly"}, nightly, "paused_until: null"},
		{[]string{"disable", "nightly"}, nightly, "enabled: false"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(context.Background(), step.args, &out, &errOut); code != 0 {
			t.Fatalf("%v: exit %d: %s", step.args, code, errOut.String())
		}
		data, err := os.ReadFile(step.file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), step.want) {
			t.Errorf("after %v the task file lacks %q:\n%s", step.args, step.want, data)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestPrintTasksShowsLastAndNext(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tasks := []api.TaskSummary{
		{ID: "weekly", Target: "local", Schedule: "0 7 * * 1", State: "idle",
			NextFireAt: "2026-11-09T06:00:00Z",
			LastRun:    &api.RunSummary{Status: "done", StartedAt: "2026-11-03T06:00:00Z"}},
		{ID: "fresh", Target: "local", Schedule: "@hourly", State: "idle", NextFireAt: "2026-11-06T13:00:00Z",
			LastRun: &api.RunSummary{Status: "failed", StartedAt: "2026-11-06T11:58:00Z"}},
		{ID: "parked", Target: "vps", Schedule: "@daily", State: "off"},
		{ID: "broken", State: "invalid", Error: `unknown target "mars"`},
	}
	var out bytes.Buffer
	if err := printTasks(&out, tasks, now, loc); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	wantRows := [][]string{
		{"ID", "TARGET", "SCHEDULE", "STATE", "LAST", "NEXT"},
		{"weekly", "idle", "done 3d ago", "Mon 09 Nov 07:00"},
		{"fresh", "failed 2m ago", "Fri 06 Nov 14:00"},
		{"parked", "off", "-"},
		{"broken", "invalid", `unknown target "mars"`},
	}
	if len(lines) != len(wantRows) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(wantRows), out.String())
	}
	for i, cells := range wantRows {
		for _, c := range cells {
			if !strings.Contains(lines[i], c) {
				t.Errorf("line %d %q lacks %q", i, lines[i], c)
			}
		}
	}
}

func TestLogsPrintsAndFiltersByTask(t *testing.T) {
	p := xdg(t)
	write(t, p.Log(),
		`{"time":"2026-11-06T10:00:00Z","level":"INFO","msg":"daemon starting"}`+"\n"+
			`{"time":"2026-11-06T10:00:01Z","level":"INFO","msg":"run started","run_id":"r1","task_id":"nightly","target":"local"}`+"\n"+
			`{"time":"2026-11-06T10:00:02Z","level":"INFO","msg":"run started","run_id":"r2","task_id":"other","target":"vps"}`+"\n")

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"logs"}, &out, &errOut); code != 0 {
		t.Fatalf("logs exit %d: %s", code, errOut.String())
	}
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("want 3 lines, got:\n%s", out.String())
	}

	out.Reset()
	if code := run(context.Background(), []string{"logs", "--task", "nightly"}, &out, &errOut); code != 0 {
		t.Fatalf("logs --task exit %d: %s", code, errOut.String())
	}
	want := "2026-11-06T10:00:01Z INFO run started run_id=r1 task_id=nightly target=local\n"
	if out.String() != want {
		t.Fatalf("logs --task nightly =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestLogsWithoutLogFails(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"logs"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no log yet") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestLogsRejectsBadFlags(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"logs", "--bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestLogsFollowStopsOnCancel(t *testing.T) {
	p := xdg(t)
	write(t, p.Log(), `{"time":"t","level":"INFO","msg":"hello"}`+"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"logs", "-f"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("out = %q", out.String())
	}
}

func TestInstallServiceRunsServiceManager(t *testing.T) {
	xdg(t)
	var calls []string
	orig := serviceRunner
	serviceRunner = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { serviceRunner = orig })
	// The test binary lives under the temp dir, which install-service
	// refuses; pretend it was installed.
	fakeExecutable(t, "/usr/local/bin/herdrcron")

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"install-service"}, &out, &errOut)
	switch runtime.GOOS {
	case "linux", "darwin":
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		if len(calls) == 0 {
			t.Fatal("no service manager commands run")
		}
	default:
		if code != 1 {
			t.Fatalf("exit %d, want 1 on %s", code, runtime.GOOS)
		}
	}
}

func fakeExecutable(t *testing.T, path string) {
	t.Helper()
	orig := executable
	executable = func() (string, error) { return path, nil }
	t.Cleanup(func() { executable = orig })
}

func TestInstallServiceHelpDoesNotInstall(t *testing.T) {
	xdg(t)
	called := false
	orig := serviceRunner
	serviceRunner = func(context.Context, string, ...string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { serviceRunner = orig })

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"install-service", "--help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "install-service") {
		t.Errorf("stdout = %q, want usage", out.String())
	}
	if called {
		t.Error("service manager was run for --help")
	}
}

func TestInstallServiceRejectsArguments(t *testing.T) {
	xdg(t)
	called := false
	orig := serviceRunner
	serviceRunner = func(context.Context, string, ...string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { serviceRunner = orig })

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"install-service", "now"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "unexpected argument") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if called {
		t.Error("service manager was run for an unexpected argument")
	}
}

func TestInstallServicePropagatesServiceError(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install-service only exercises the service stack on linux and darwin")
	}
	xdg(t)
	boom := errors.New("systemctl boom")
	orig := serviceRunner
	serviceRunner = func(context.Context, string, ...string) error { return boom }
	t.Cleanup(func() { serviceRunner = orig })
	fakeExecutable(t, "/usr/local/bin/herdrcron")

	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"install-service"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	msg := errOut.String()
	if !strings.Contains(msg, "systemctl boom") {
		t.Fatalf("stderr = %q, want the wrapped error text", msg)
	}
	// Avoid "herdrcron install-service: systemctl …: systemctl …" double-prefix noise.
	if strings.Count(msg, "install-service:") != 0 {
		t.Errorf("stderr should not repeat install-service prefix: %q", msg)
	}
}

func TestInstallServiceRefusesTempDirBinary(t *testing.T) {
	xdg(t)
	called := false
	orig := serviceRunner
	serviceRunner = func(context.Context, string, ...string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { serviceRunner = orig })
	fakeExecutable(t, filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "herdrcron"))

	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"install-service"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "temporary directory") {
		t.Errorf("stderr lacks the temp-dir explanation: %q", errOut.String())
	}
	if called {
		t.Error("service manager was run for a temp-dir binary")
	}
}

// fakeDaemon is a JSON-RPC server on the xdg socket that records the params
// of every call and answers with canned results.
type fakeDaemon struct {
	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	method string
	params json.RawMessage
}

// startFakeDaemon serves results (method → result, or an error to return)
// on p.Socket() until the test ends.
func startFakeDaemon(t *testing.T, p paths.Paths, results map[string]any) *fakeDaemon {
	t.Helper()
	fd := &fakeDaemon{}
	srv := api.NewServer(nil)
	for method, res := range results {
		method, res := method, res
		srv.Handle(method, func(_ context.Context, params json.RawMessage) (any, error) {
			fd.mu.Lock()
			fd.calls = append(fd.calls, fakeCall{method: method, params: append(json.RawMessage(nil), params...)})
			fd.mu.Unlock()
			if err, ok := res.(error); ok {
				return nil, err
			}
			return res, nil
		})
	}
	ln, err := api.Listen(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return fd
}

// stubDaemon serves handlers on the daemon socket of p until the test ends.
func stubDaemon(t *testing.T, p paths.Paths, handlers map[string]api.HandlerFunc) {
	t.Helper()
	srv := api.NewServer(nil)
	for m, h := range handlers {
		srv.Handle(m, h)
	}
	ln, err := api.Listen(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// only returns the single recorded call, decoding its params into v.
func (fd *fakeDaemon) only(t *testing.T, method string, v any) {
	t.Helper()
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.calls) != 1 {
		t.Fatalf("got %d calls, want 1: %v", len(fd.calls), fd.calls)
	}
	if fd.calls[0].method != method {
		t.Fatalf("method = %q, want %q", fd.calls[0].method, method)
	}
	if err := json.Unmarshal(fd.calls[0].params, v); err != nil {
		t.Fatalf("decode params %s: %v", fd.calls[0].params, err)
	}
}

func (fd *fakeDaemon) count() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return len(fd.calls)
}

var fakeResults = map[string]any{
	api.MethodTasksSetEnabled: api.TaskSummary{ID: "nightly", State: api.StateIdle, NextFireAt: "2026-11-09T06:00:00Z"},
	api.MethodTasksPause:      api.TaskSummary{ID: "nightly", State: api.StatePaused, PausedUntil: api.PauseIndefinite},
	api.MethodTasksResume:     api.TaskSummary{ID: "nightly", State: api.StateIdle, NextFireAt: "2026-11-09T06:00:00Z"},
	api.MethodPauseAll:        api.GlobalPause{PausedTargets: []string{"vps"}},
	api.MethodResumeAll:       api.GlobalPause{},
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), args, &out, &errOut); code != 0 {
		t.Fatalf("%v: exit %d: %s", args, code, errOut.String())
	}
	return out.String()
}

func TestEnableDisableSendSetEnabled(t *testing.T) {
	for _, tc := range []struct {
		cmd     string
		enabled bool
	}{{"enable", true}, {"disable", false}} {
		t.Run(tc.cmd, func(t *testing.T) {
			p := xdg(t)
			res := map[string]any{api.MethodTasksSetEnabled: api.TaskSummary{ID: "nightly", State: api.StateIdle, NextFireAt: "2026-11-09T06:00:00Z"}}
			if !tc.enabled {
				res[api.MethodTasksSetEnabled] = api.TaskSummary{ID: "nightly", State: api.StateOff}
			}
			fd := startFakeDaemon(t, p, res)
			out := runOK(t, tc.cmd, "nightly")
			var got api.SetEnabledParams
			fd.only(t, api.MethodTasksSetEnabled, &got)
			if want := (api.SetEnabledParams{ID: "nightly", Enabled: tc.enabled}); got != want {
				t.Errorf("params = %+v, want %+v", got, want)
			}
			want := "nightly: off\n"
			if tc.enabled {
				want = "nightly: enabled, next fire " + nextFire("2026-11-09T06:00:00Z", time.Local) + "\n"
			}
			if out != want {
				t.Errorf("out = %q, want %q", out, want)
			}
		})
	}
}

func TestPauseSendsUntil(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	localForm := future.In(time.Local).Format("2006-01-02T15:04")
	wantLocal, err := time.ParseInLocation("2006-01-02T15:04", localForm, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	rfc := future.UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		args []string
		want string // RFC 3339 or "indefinite"
	}{
		{"indefinite", []string{"pause", "nightly"}, api.PauseIndefinite},
		{"until after id", []string{"pause", "nightly", "--until", localForm}, wantLocal.Format(time.RFC3339)},
		{"until before id", []string{"pause", "--until", localForm, "nightly"}, wantLocal.Format(time.RFC3339)},
		{"until equals form", []string{"pause", "--until=" + rfc, "nightly"}, rfc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := xdg(t)
			fd := startFakeDaemon(t, p, fakeResults)
			runOK(t, tc.args...)
			var got api.PauseParams
			fd.only(t, api.MethodTasksPause, &got)
			if got.ID != "nightly" {
				t.Errorf("id = %q", got.ID)
			}
			if tc.want == api.PauseIndefinite {
				if got.Until != tc.want {
					t.Errorf("until = %q, want %q", got.Until, tc.want)
				}
				return
			}
			gotT, err := time.Parse(time.RFC3339, got.Until)
			if err != nil {
				t.Fatalf("until %q is not RFC 3339: %v", got.Until, err)
			}
			wantT, _ := time.Parse(time.RFC3339, tc.want)
			if !gotT.Equal(wantT) {
				t.Errorf("until = %q, want %q", got.Until, tc.want)
			}
		})
	}
}

func TestPausePrintsResult(t *testing.T) {
	p := xdg(t)
	until := "2026-11-10T07:00:00Z"
	startFakeDaemon(t, p, map[string]any{
		api.MethodTasksPause: api.TaskSummary{ID: "nightly", State: api.StatePaused, PausedUntil: until},
	})
	out := runOK(t, "pause", "nightly")
	if want := "nightly: paused until " + nextFire(until, time.Local) + "\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestPauseIndefinitePrintsResult(t *testing.T) {
	p := xdg(t)
	startFakeDaemon(t, p, fakeResults)
	if out := runOK(t, "pause", "nightly"); out != "nightly: paused indefinitely\n" {
		t.Errorf("out = %q", out)
	}
}

func TestResumeSendsID(t *testing.T) {
	p := xdg(t)
	fd := startFakeDaemon(t, p, fakeResults)
	out := runOK(t, "resume", "nightly")
	var got api.TaskIDParams
	fd.only(t, api.MethodTasksResume, &got)
	if got.ID != "nightly" {
		t.Errorf("id = %q", got.ID)
	}
	if want := "nightly: resumed, next fire " + nextFire("2026-11-09T06:00:00Z", time.Local) + "\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestPauseResumeAll(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		method string
		target string
		out    string
	}{
		{[]string{"pause", "--all"}, api.MethodPauseAll, "", "paused targets: vps\n"},
		{[]string{"pause", "--all", "--target", "vps"}, api.MethodPauseAll, "vps", "paused targets: vps\n"},
		{[]string{"pause", "--target=vps", "--all"}, api.MethodPauseAll, "vps", "paused targets: vps\n"},
		{[]string{"resume", "--all"}, api.MethodResumeAll, "", "no global pause\n"},
		{[]string{"resume", "--all", "--target", "vps"}, api.MethodResumeAll, "vps", "no global pause\n"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			p := xdg(t)
			fd := startFakeDaemon(t, p, fakeResults)
			out := runOK(t, tc.args...)
			var got api.TargetParams
			fd.only(t, tc.method, &got)
			if got.Target != tc.target {
				t.Errorf("target = %q, want %q", got.Target, tc.target)
			}
			if out != tc.out {
				t.Errorf("out = %q, want %q", out, tc.out)
			}
		})
	}
}

func TestFormatGlobalPause(t *testing.T) {
	for _, tc := range []struct {
		gp   api.GlobalPause
		want string
	}{
		{api.GlobalPause{}, "no global pause"},
		{api.GlobalPause{PausedAll: true}, "paused: all targets"},
		{api.GlobalPause{PausedAll: true, PausedTargets: []string{"vps"}}, "paused: all targets"},
		{api.GlobalPause{PausedTargets: []string{"local", "vps"}}, "paused targets: local, vps"},
	} {
		if got := formatGlobalPause(tc.gp); got != tc.want {
			t.Errorf("formatGlobalPause(%+v) = %q, want %q", tc.gp, got, tc.want)
		}
	}
}

func TestPauseCommandsUsageErrors(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for i, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"enable"}, "task id"},
		{[]string{"disable", "a", "b"}, "task id"},
		{[]string{"enable", "--bogus", "a"}, "bogus"},
		{[]string{"pause"}, "task id"},
		{[]string{"pause", "a", "b"}, "task id"},
		{[]string{"resume"}, "task id"},
		{[]string{"resume", "nightly", "--until", "2099-01-01"}, "until"},
		{[]string{"pause", "--all", "nightly"}, "--all"},
		{[]string{"resume", "nightly", "--all"}, "--all"},
		{[]string{"pause", "--target", "vps", "nightly"}, "--target"},
		{[]string{"resume", "--target", "vps"}, "--target"},
		{[]string{"pause", "--all", "--until", "2099-01-01"}, "--until"},
		{[]string{"pause", "nightly", "--until", "tomorrow"}, "tomorrow"},
		{[]string{"pause", "nightly", "--until", past}, "past"},
		{[]string{"pause", "nightly", "--until"}, "until"},
	} {
		// Short subtest names: the temp dir, and so the socket path, embeds them.
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Logf("args: %q", tc.args)
			p := xdg(t)
			fd := startFakeDaemon(t, p, fakeResults)
			var out, errOut bytes.Buffer
			if code := run(context.Background(), tc.args, &out, &errOut); code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.msg) {
				t.Errorf("stderr = %q, want it to mention %q", errOut.String(), tc.msg)
			}
			if n := fd.count(); n != 0 {
				t.Errorf("%d calls made for a usage error", n)
			}
		})
	}
}

func TestPauseCommandsReportDaemonErrors(t *testing.T) {
	p := xdg(t)
	startFakeDaemon(t, p, map[string]any{
		api.MethodTasksSetEnabled: &api.Error{Code: api.CodeInvalidParams, Message: `unknown task "nope"`},
	})
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"enable", "nope"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if msg := errOut.String(); !strings.HasPrefix(msg, "herdrcron: tasks.set_enabled: ") || !strings.Contains(msg, `unknown task "nope"`) {
		t.Errorf("stderr = %q", msg)
	}
}

func TestPauseWithoutDaemonFails(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"pause", "nightly"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if msg := errOut.String(); !strings.Contains(msg, "daemon is not running") || !strings.Contains(msg, "herdrcron daemon") {
		t.Errorf("stderr = %q", msg)
	}
}

func TestParseUntil(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "2026-11-10T08:00", want: "2026-11-10T08:00:00+01:00"},
		{in: "2026-11-10T08:00:30", want: "2026-11-10T08:00:30+01:00"},
		{in: "2026-11-10 08:00", want: "2026-11-10T08:00:00+01:00"},
		{in: "2026-11-10", want: "2026-11-10T00:00:00+01:00"},
		{in: "2026-11-10T08:00:00Z", want: "2026-11-10T08:00:00Z"},
		{in: "2026-11-10T08:00:00+05:30", want: "2026-11-10T08:00:00+05:30"},
		{in: "2026-11-06T12:30", err: "past"}, // 11:30Z in Brussels
		{in: "2026-11-06T14:00", want: "2026-11-06T14:00:00+01:00"},
		{in: "2026-11-06T12:00:00Z", err: "past"},
		{in: "2026-11-06", err: "past"},
		{in: "2025-01-01T00:00:00Z", err: "past"},
		{in: "tomorrow", err: "tomorrow"},
		{in: "2026-13-01", err: "2026-13-01"},
		{in: "", err: "empty"},
	} {
		got, err := parseUntil(tc.in, now, loc)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("parseUntil(%q) = %q, %v; want error containing %q", tc.in, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseUntil(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func runNowStub(t *testing.T, res api.RunSummary, gotID *string) map[string]api.HandlerFunc {
	return map[string]api.HandlerFunc{
		api.MethodTasksRunNow: func(_ context.Context, params json.RawMessage) (any, error) {
			var p api.RunNowParams
			if err := json.Unmarshal(params, &p); err != nil {
				t.Errorf("run_now params %s: %v", params, err)
			}
			*gotID = p.ID
			if p.ID == "nope" {
				return nil, api.InvalidParams(errors.New(`unknown task "nope"`))
			}
			return res, nil
		},
	}
}

func TestRunPrintsTheStartedRun(t *testing.T) {
	p := xdg(t)
	var gotID string
	stubDaemon(t, p, runNowStub(t, api.RunSummary{ID: "01JRUN", TaskID: "nightly", Target: "local",
		Trigger: "manual", Status: "running", HerdrWorkspace: "w1", HerdrTab: "w1:t2", HerdrPane: "w1:p3"}, &gotID))
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"run", "nightly"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if gotID != "nightly" {
		t.Errorf("run_now id = %q, want nightly", gotID)
	}
	if got, want := out.String(), "run 01JRUN started for nightly: running, herdr tab w1:t2 pane w1:p3\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunThatFailsToStartExitsNonZero(t *testing.T) {
	p := xdg(t)
	var gotID string
	stubDaemon(t, p, runNowStub(t, api.RunSummary{ID: "01JRUN", TaskID: "nightly", Target: "local",
		Trigger: "manual", Status: "failed", ExitReason: "preflight: cwd /nope does not exist"}, &gotID))
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"run", "nightly"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got, want := out.String(), "run 01JRUN started for nightly: failed: preflight: cwd /nope does not exist\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunUnknownTaskFails(t *testing.T) {
	p := xdg(t)
	var gotID string
	stubDaemon(t, p, runNowStub(t, api.RunSummary{}, &gotID))
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"run", "nope"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if msg := errOut.String(); !strings.Contains(msg, `unknown task "nope"`) {
		t.Errorf("stderr = %q, want the daemon's error", msg)
	}
}

func TestRunWithoutDaemonFails(t *testing.T) {
	xdg(t)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"run", "nightly"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if msg := errOut.String(); !strings.Contains(msg, "daemon is not running") || !strings.Contains(msg, "herdrcron daemon") {
		t.Fatalf("stderr = %q, want a clear message that says how to start it", msg)
	}
}

func TestRunNeedsOneTaskID(t *testing.T) {
	xdg(t)
	for _, args := range [][]string{{"run"}, {"run", "a", "b"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "usage") {
			t.Errorf("%q: stderr = %q, want usage", args, errOut.String())
		}
	}
}
