package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// call is one API call the fake backend received.
type call struct {
	Method string
	Params any
}

// fakeBackend answers calls from canned results and lets a test push
// subscribe events.
type fakeBackend struct {
	mu      sync.Mutex
	calls   []call
	results map[string]any   // method → result, marshalled into the caller's result
	errs    map[string]error // method → error, returned once
	events  chan api.Notification
	subs    int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		results: map[string]any{},
		errs:    map[string]error{},
		events:  make(chan api.Notification, 16),
	}
}

func (f *fakeBackend) Call(_ context.Context, method string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{method, params})
	if err, ok := f.errs[method]; ok {
		delete(f.errs, method)
		return err
	}
	res, ok := f.results[method]
	if !ok {
		return fmt.Errorf("fake: no result for %s", method)
	}
	if result == nil {
		return nil
	}
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func (f *fakeBackend) Subscribe(context.Context) (<-chan api.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs++
	return f.events, nil
}

func (f *fakeBackend) set(method string, result any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[method] = result
}

func (f *fakeBackend) fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = err
}

// callsOf returns the params of every call of method so far.
func (f *fakeBackend) callsOf(method string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []any
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

func (f *fakeBackend) push(t *testing.T, method string, params any) {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	f.events <- api.Notification{Method: method, Params: data}
}

// now is the fixed clock of the tests: Thursday 5 Nov 2026, 10:00 UTC.
var now = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

// fixture is the daemon state behind the spec mockup.
func fixture() *fakeBackend {
	f := newFakeBackend()
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 3*24*3600 + 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: "reachable"},
			{Name: "vps", Reachability: "reachable"},
			{Name: "buildbox", Reachability: "unreachable"},
		},
		GlobalPause: api.GlobalPause{PausedTargets: []string{}},
	})
	f.set(api.MethodTasksList, api.TasksListResult{Tasks: []api.TaskSummary{
		{ID: "peppol-sh-deps", Target: "local", Schedule: "0 7 * * 1", State: api.StateIdle, Enabled: true,
			NextFireAt: ts(4*24*time.Hour - 3*time.Hour),
			LastRun:    &api.RunSummary{ID: "r1", TaskID: "peppol-sh-deps", Status: "done", StartedAt: ts(-3 * 24 * time.Hour)}},
		{ID: "nightly-e2e", Target: "vps", Schedule: "0 2 * * *", State: api.StatePaused, Enabled: true,
			PausedUntil: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC).Format(time.RFC3339),
			NextFireAt:  ts(16 * time.Hour),
			LastRun:     &api.RunSummary{ID: "r2", TaskID: "nightly-e2e", Status: "failed", StartedAt: ts(-26 * time.Hour)}},
		{ID: "factory-coordinator", Target: "vps", Schedule: "*/30 * * * *", State: api.StateRunning, Enabled: true,
			NextFireAt: ts(30 * time.Minute),
			LastRun:    &api.RunSummary{ID: "r3", TaskID: "factory-coordinator", Status: "running", StartedAt: ts(-2 * time.Minute)}},
		{ID: "peppolnews-ingest", Target: "vps", Schedule: "@every 2h", State: api.StateOff,
			LastRun: &api.RunSummary{ID: "r4", TaskID: "peppolnews-ingest", Status: "done", StartedAt: ts(-5 * 24 * time.Hour)}},
		{ID: "release-notes", Target: "buildbox", Schedule: "0 16 * * 5", State: api.StateBlocked, Enabled: true,
			NextFireAt: ts(24*time.Hour + 6*time.Hour),
			LastRun:    &api.RunSummary{ID: "r5", TaskID: "release-notes", Status: "blocked", StartedAt: ts(-time.Hour)}},
		{ID: "e2e-timeout", Target: "local", Schedule: "@hourly", State: api.StateIdle, Enabled: true,
			NextFireAt: ts(20 * time.Minute),
			LastRun:    &api.RunSummary{ID: "r6", TaskID: "e2e-timeout", Status: "timeout", StartedAt: ts(-40 * time.Minute)}},
	}})
	f.set(api.MethodRunsList, api.RunsListResult{Runs: []api.RunSummary{
		{ID: "r5", TaskID: "release-notes", Target: "buildbox", Status: "blocked", StartedAt: ts(-time.Hour)},
		{ID: "r3", TaskID: "factory-coordinator", Target: "vps", Status: "running", StartedAt: ts(-2 * time.Minute)},
	}})
	return f
}

// plain is a renderer without colours, for golden files.
func plain() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	return r
}

// coloured is a renderer with ANSI colours.
func coloured() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.ANSI)
	return r
}

func newTestModel(f *fakeBackend, r *lipgloss.Renderer) Model {
	return New(Options{
		Backend:  f,
		Now:      func() time.Time { return now },
		Location: time.UTC,
		Renderer: r,
	})
}
