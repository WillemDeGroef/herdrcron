package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

var noon = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// setupUTC is setup with the config timezone pinned to UTC, so expected
// fire times do not depend on the machine's zone.
func setupUTC(t *testing.T) paths.Paths {
	t.Helper()
	p := setup(t)
	write(t, p.ConfigFile(), "timezone: UTC\ntargets:\n  vps:\n    host: vps\n")
	return p
}

func call[R any](t *testing.T, c *api.Client, method string, params any) R {
	t.Helper()
	var res R
	if err := c.Call(context.Background(), method, params, &res); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

func taskFile(t *testing.T, p paths.Paths, id string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.TasksDir(), id+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDisableAndEnableWriteTheTaskFile(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon)})

	got := call[api.TaskSummary](t, c, api.MethodTasksSetEnabled, api.SetEnabledParams{ID: "nightly", Enabled: false})
	if got.State != api.StateOff || got.NextFireAt != "" {
		t.Errorf("after disable = %+v, want off without next fire", got)
	}
	if f := taskFile(t, p, "nightly"); !strings.Contains(f, "enabled: false") || !strings.Contains(f, "API_TOKEN") {
		t.Errorf("task file after disable:\n%s", f)
	}
	if got := listByID(t, c)["nightly"]; got.State != api.StateOff {
		t.Errorf("list after disable = %+v", got)
	}

	got = call[api.TaskSummary](t, c, api.MethodTasksSetEnabled, api.SetEnabledParams{ID: "parked", Enabled: true})
	// "0 7 * * 1" with the config timezone (UTC here): Monday 07:00.
	if got.State != api.StateIdle || got.NextFireAt != "2026-11-09T07:00:00Z" {
		t.Errorf("after enable = %+v, want idle with next 2026-11-09T07:00:00Z", got)
	}
	if f := taskFile(t, p, "parked"); !strings.Contains(f, "enabled: true") {
		t.Errorf("task file after enable:\n%s", f)
	}
}

func TestPauseAndResumeWriteTheTaskFile(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon)})

	got := call[api.TaskSummary](t, c, api.MethodTasksPause, api.PauseParams{ID: "nightly", Until: "2026-11-10T08:00:00+01:00"})
	if got.State != api.StatePaused || got.PausedUntil != "2026-11-10T08:00:00+01:00" || got.NextFireAt == "" {
		t.Errorf("after pause = %+v", got)
	}
	if f := taskFile(t, p, "nightly"); !strings.Contains(f, `paused_until: "2026-11-10T08:00:00+01:00"`) {
		t.Errorf("task file after pause:\n%s", f)
	}

	got = call[api.TaskSummary](t, c, api.MethodTasksPause, api.PauseParams{ID: "nightly", Until: api.PauseIndefinite})
	if got.State != api.StatePaused || got.PausedUntil != api.PauseIndefinite {
		t.Errorf("after indefinite pause = %+v", got)
	}

	got = call[api.TaskSummary](t, c, api.MethodTasksResume, api.TaskIDParams{ID: "nightly"})
	if got.State != api.StateIdle || got.PausedUntil != "" || got.NextFireAt != "2026-11-07T00:00:00Z" {
		t.Errorf("after resume = %+v", got)
	}
	if f := taskFile(t, p, "nightly"); !strings.Contains(f, "paused_until: null") {
		t.Errorf("task file after resume:\n%s", f)
	}
}

func TestDaemonClearsPausedUntilWhenItPasses(t *testing.T) {
	p := setupUTC(t)
	clk := clock.NewFake(noon)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clk})
	call[api.TaskSummary](t, c, api.MethodTasksPause, api.PauseParams{ID: "nightly", Until: "2026-11-06T13:00:00Z"})

	clk.Set(time.Date(2026, 11, 6, 13, 0, 1, 0, time.UTC))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(taskFile(t, p, "nightly"), "paused_until: null") {
		if time.Now().After(deadline) {
			t.Fatalf("paused_until not cleared after it passed:\n%s", taskFile(t, p, "nightly"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := listByID(t, c)["nightly"]; got.State != api.StateIdle || got.PausedUntil != "" {
		t.Errorf("after expiry = %+v", got)
	}
}

func TestPauseAllLivesInDaemonStateNotTaskFiles(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon)})
	before := taskFile(t, p, "nightly")

	gp := call[api.GlobalPause](t, c, api.MethodPauseAll, api.TargetParams{Target: "vps"})
	if gp.PausedAll || strings.Join(gp.PausedTargets, ",") != "vps" {
		t.Errorf("pause_all vps = %+v", gp)
	}
	if got := listByID(t, c)["nightly"]; got.State != api.StateIdle {
		t.Errorf("nightly (local) during pause of vps = %+v, want idle", got)
	}

	gp = call[api.GlobalPause](t, c, api.MethodPauseAll, api.TargetParams{})
	if !gp.PausedAll {
		t.Errorf("pause_all = %+v", gp)
	}
	st := call[api.StatusResult](t, c, api.MethodStatus, nil)
	if !st.PausedAll || strings.Join(st.PausedTargets, ",") != "vps" {
		t.Errorf("status during pause_all = %+v", st)
	}
	if got := listByID(t, c)["nightly"]; got.State != api.StatePaused || got.PausedUntil != "" || got.NextFireAt == "" {
		t.Errorf("nightly during pause_all = %+v, want paused, no paused_until, a next fire", got)
	}
	if after := taskFile(t, p, "nightly"); after != before {
		t.Errorf("pause_all changed the task file:\n%s", after)
	}

	gp = call[api.GlobalPause](t, c, api.MethodResumeAll, api.TargetParams{})
	if gp.PausedAll || len(gp.PausedTargets) != 0 {
		t.Errorf("resume_all = %+v, want no global pause", gp)
	}
	if got := listByID(t, c)["nightly"]; got.State != api.StateIdle {
		t.Errorf("nightly after resume_all = %+v", got)
	}
}

func TestPauseAllSurvivesRestart(t *testing.T) {
	p := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, p, daemon.Options{Version: "test"}) }()
	var c *api.Client
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var err error
		if c, err = api.Dial(p.Socket()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	call[api.GlobalPause](t, c, api.MethodPauseAll, api.TargetParams{Target: "vps"})
	_ = c.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	c = start(t, p)
	if st := call[api.StatusResult](t, c, api.MethodStatus, nil); strings.Join(st.PausedTargets, ",") != "vps" {
		t.Errorf("status after restart = %+v, want vps paused", st)
	}
}

func TestPauseRequestErrors(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon)})
	before := taskFile(t, p, "broken")
	for _, tc := range []struct {
		name, method string
		params       any
		want         string
	}{
		{"unknown task", api.MethodTasksResume, api.TaskIDParams{ID: "nope"}, `unknown task "nope"`},
		{"invalid task", api.MethodTasksSetEnabled, api.SetEnabledParams{ID: "broken"}, `task "broken" is invalid`},
		{"bad until", api.MethodTasksPause, api.PauseParams{ID: "nightly", Until: "tomorrow"}, "until"},
		{"missing until", api.MethodTasksPause, api.PauseParams{ID: "nightly"}, "until"},
		{"past until", api.MethodTasksPause, api.PauseParams{ID: "nightly", Until: "2026-11-06T11:00:00Z"}, "in the past"},
		{"unknown target", api.MethodPauseAll, api.TargetParams{Target: "mars"}, `unknown target "mars"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Call(context.Background(), tc.method, tc.params, nil)
			var rpcErr *api.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInvalidParams || !strings.Contains(rpcErr.Message, tc.want) {
				t.Errorf("err = %v, want invalid params containing %q", err, tc.want)
			}
		})
	}
	if after := taskFile(t, p, "broken"); after != before {
		t.Errorf("a refused request changed the task file:\n%s", after)
	}
}
