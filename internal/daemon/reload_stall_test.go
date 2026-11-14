package daemon_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// hungReader stands in for a config directory on a filesystem that stops
// answering: once hang is called, the next read of the task files blocks
// until release, and then returns the files as they were when it began.
// Reads before that, and after release, are the real ones.
type hungReader struct {
	mu      sync.Mutex
	armed   bool
	calls   int
	entered chan struct{} // closed when the armed read has started
	gate    chan struct{} // closed by release
	once    sync.Once
}

func newHungReader() *hungReader {
	return &hungReader{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (h *hungReader) load(configDir string, cfg config.Config) ([]config.TaskEntry, error) {
	h.mu.Lock()
	h.calls++
	block := h.armed
	h.armed = false
	h.mu.Unlock()
	entries, err := config.LoadTasks(configDir, cfg)
	if block {
		close(h.entered)
		<-h.gate
	}
	return entries, err
}

func (h *hungReader) hang() {
	h.mu.Lock()
	h.armed = true
	h.mu.Unlock()
}

func (h *hungReader) release() { h.once.Do(func() { close(h.gate) }) }

// reads is how many times the task files were read so far.
func (h *hungReader) reads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// stalledReload is a "reload" whose read of the task files hangs.
type stalledReload struct {
	t    *testing.T
	fs   *hungReader
	done chan struct{}
	res  api.ReloadResult
	err  error
}

// startStalled runs a daemon on l whose task file reads go through a
// hungReader.
func (l *lifecycle) startStalled() *hungReader {
	l.t.Helper()
	fs := newHungReader()
	l.c, l.stop = runDaemon(l.t, l.p, daemon.Options{
		Version: "test", Clock: l.clk, Targets: map[string]target.Target{"local": l.herdr}, LoadTasks: fs.load,
	})
	// Registered after runDaemon's own cleanup, so it runs before it: a
	// daemon whose reload still hangs cannot stop.
	l.t.Cleanup(fs.release)
	return fs
}

// stallReload calls "reload" on its own connection and returns once that
// reload is stuck reading the task files.
func (l *lifecycle) stallReload(fs *hungReader) *stalledReload {
	l.t.Helper()
	c, err := api.Dial(l.p.Socket())
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { _ = c.Close() })
	s := &stalledReload{t: l.t, fs: fs, done: make(chan struct{})}
	fs.hang()
	go func() {
		defer close(s.done)
		s.err = c.Call(context.Background(), api.MethodReload, nil, &s.res)
	}()
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		l.t.Fatal("reload did not start reading the task files")
	}
	return s
}

// finish lets the read return and gives the reload's result.
func (s *stalledReload) finish() api.ReloadResult {
	s.t.Helper()
	s.fs.release()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.t.Fatal("reload did not return after the read did")
	}
	if s.err != nil {
		s.t.Fatalf("reload: %v", s.err)
	}
	return s.res
}

// within is call with a deadline: a call that waits on a lock the stuck
// reload holds fails the test instead of hanging it.
func within[R any](t *testing.T, c *api.Client, method string, params any) R {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var res R
	if err := c.Call(ctx, method, params, &res); err != nil {
		t.Fatalf("%s while a reload is stuck reading the task files: %v", method, err)
	}
	return res
}

// A reload that hangs on reading the task files (a network filesystem that
// stopped answering) holds up only itself: the socket API answers (which
// is all the TUI needs), a scheduled fire starts its run, and so does a
// manual one.
func TestReloadStuckReadingTaskFilesDoesNotStallTheDaemon(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\noverlap: parallel\n")
	fs := l.startStalled()
	stuck := l.stallReload(fs)

	if got := within[api.StatusResult](t, l.c, api.MethodStatus, nil); got.Timezone != "UTC" {
		t.Errorf("status = %+v", got)
	}
	if got := within[api.TasksListResult](t, l.c, api.MethodTasksList, nil); len(got.Tasks) != 1 || got.Tasks[0].ID != "job" {
		t.Errorf("tasks.list = %+v, want the one task", got)
	}
	if got := within[api.TaskDetail](t, l.c, api.MethodTasksGet, api.TaskIDParams{ID: "job"}); got.Prompt != "do the job" {
		t.Errorf("tasks.get prompt = %q", got.Prompt)
	}

	scheduled := func() []api.RunSummary {
		return within[api.RunsListResult](t, l.c, api.MethodRunsList, api.RunsListParams{TaskID: "job"}).Runs
	}
	l.advanceUntil("the scheduled fire starts a run", time.Hour, func() bool {
		runs := scheduled()
		return len(runs) == 1 && runs[0].Status == store.StatusRunning
	})
	if got := scheduled()[0].Trigger; got != store.TriggerSchedule {
		t.Errorf("trigger = %q, want %q", got, store.TriggerSchedule)
	}
	manual := within[api.RunNowResult](t, l.c, api.MethodTasksRunNow, api.RunNowParams{ID: "job"})
	if manual.Queued || manual.Status != store.StatusRunning {
		t.Errorf("run_now = %+v, want a running run", manual)
	}

	if res := stuck.finish(); len(res.Tasks) != 1 || res.Tasks[0].ID != "job" {
		t.Errorf("reload = %+v, want the one task", res)
	}
}

// A config.yaml change is not half applied while the task files are being
// read: the daemon runs on the old config until the read is back, and on
// the new one from then on.
func TestReloadAppliesConfigOnlyAfterTheTaskFilesAreRead(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	fs := l.startStalled()
	write(t, l.p.ConfigFile(), "timezone: Europe/Brussels\nattach_mode: full\n")
	stuck := l.stallReload(fs)

	if got := within[api.StatusResult](t, l.c, api.MethodStatus, nil).Timezone; got != "UTC" {
		t.Errorf("timezone during the read = %q, want UTC still", got)
	}
	stuck.finish()
	if got := call[api.StatusResult](t, l.c, api.MethodStatus, nil).Timezone; got != "Europe/Brussels" {
		t.Errorf("timezone after the reload = %q, want Europe/Brussels", got)
	}
}

// A task written through the API while a reload reads the task files is
// not undone by that reload's older read: the reload reads again.
func TestReloadDoesNotApplyAReadOlderThanATaskWrite(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	fs := l.startStalled()
	stuck := l.stallReload(fs)
	before := fs.reads()

	paused := within[api.TaskSummary](t, l.c, api.MethodTasksPause, api.PauseParams{ID: "job", Until: api.PauseIndefinite})
	if paused.State != api.StatePaused {
		t.Fatalf("tasks.pause = %+v, want paused", paused)
	}
	// The stuck read began before the pause was written. Its result, once
	// it returns, is of files that have changed since.
	res := stuck.finish()
	if len(res.Tasks) != 1 || res.Tasks[0].State != api.StatePaused {
		t.Errorf("reload = %+v, want job paused", res.Tasks)
	}
	if got := listByID(t, l.c)["job"].State; got != api.StatePaused {
		t.Errorf("state after the reload = %q, want paused", got)
	}
	if got := fs.reads(); got <= before {
		t.Errorf("task files read %d times before the write and %d after, want another read", before, got)
	}
}

// The task files are validated against the config the reload ends up
// applying. A run that ends during the read frees its target, so the
// target's change no longer waits: the reload applies it in full.
func TestReloadFollowsARunThatEndsWhileTheTaskFilesAreRead(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	fs := l.startStalled()
	run := l.mustRunNow()
	l.withConfig("targets:\n  local:\n    herdr_path: /opt/bin/herdr\n")
	stuck := l.stallReload(fs)

	l.herdr.set(run.HerdrPane, paneIdle)
	l.advanceUntil("the run ends", pollStep, l.ended(run.ID))
	l.herdr.setBin("/opt/bin/herdr")
	if res := stuck.finish(); res.RestartRequired || len(res.DeferredTargets) != 0 {
		t.Fatalf("reload = %+v, want the target change applied: its run ended", res)
	}
	next := l.mustRunNow()
	got, err := l.attachCmd(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/bin/herdr", "agent", "attach", next.HerdrPane}; !slices.Equal(got.Argv, want) {
		t.Errorf("attach argv after the change = %q, want %q", got.Argv, want)
	}
}

// A reload whose every read is overtaken by a task write gives up with an
// error after a few reads instead of reading for ever. It has applied
// nothing: the writes themselves stand.
func TestReloadGivesUpWhenEveryReadIsOvertaken(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	var mu sync.Mutex
	var writer *api.Client // set once the daemon runs: the reads of this test
	reads := 0
	l.c, l.stop = runDaemon(t, l.p, daemon.Options{
		Version: "test", Clock: l.clk, Targets: map[string]target.Target{"local": l.herdr},
		LoadTasks: func(configDir string, cfg config.Config) ([]config.TaskEntry, error) {
			entries, err := config.LoadTasks(configDir, cfg)
			mu.Lock()
			defer mu.Unlock()
			if writer == nil {
				return entries, err
			}
			reads++
			method, params := api.MethodTasksPause, any(api.PauseParams{ID: "job", Until: api.PauseIndefinite})
			if reads%2 == 0 {
				method, params = api.MethodTasksResume, api.TaskIDParams{ID: "job"}
			}
			if werr := writer.Call(context.Background(), method, params, nil); werr != nil {
				t.Errorf("%s during the read: %v", method, werr)
			}
			return entries, err
		},
	})
	w, err := api.Dial(l.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	mu.Lock()
	writer = w
	mu.Unlock()

	err = l.c.Call(context.Background(), api.MethodReload, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("reload err = %v, want it to give up and say to try again", err)
	}
	mu.Lock()
	n := reads
	writer = nil
	mu.Unlock()
	if n < 2 {
		t.Fatalf("task files read %d times, want several attempts", n)
	}
	want := api.StatePaused
	if n%2 == 0 {
		want = api.StateIdle
	}
	if got := listByID(t, l.c)["job"].State; got != want {
		t.Errorf("state after %d writes = %q, want %q: the last write stands", n, got, want)
	}
	if res := reload(t, l.c); len(res.Tasks) != 1 || res.Tasks[0].State != want {
		t.Errorf("reload once the writes stopped = %+v, want job %s", res.Tasks, want)
	}
}
