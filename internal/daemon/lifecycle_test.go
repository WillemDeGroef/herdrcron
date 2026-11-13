package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// pollStep is one poll_interval (the default).
const pollStep = 15 * time.Second

// lifecycle is a daemon test environment whose state (paths, fake clock,
// fake herdr) outlives a daemon, so a test can kill and restart it.
type lifecycle struct {
	t     *testing.T
	p     paths.Paths
	clk   *clock.Fake
	herdr *fakeHerdr
	c     *api.Client
	stop  func()
}

// newLifecycle writes config.yaml (time zone UTC, local target only) and
// one task "job" with the given extra YAML lines, at the fake time now.
func newLifecycle(t *testing.T, now time.Time, taskYAML string) *lifecycle {
	t.Helper()
	root := t.TempDir()
	p := paths.Paths{ConfigDir: filepath.Join(root, "cfg"), StateDir: filepath.Join(root, "st")}
	write(t, p.ConfigFile(), "timezone: UTC\n")
	write(t, filepath.Join(p.ConfigDir, "prompts", "job.md"), "do the job")
	write(t, filepath.Join(p.TasksDir(), "job.yaml"), `id: job
target: local
cwd: /tmp/repo
schedule: "@daily"
agent: {command: claude}
`+taskYAML)
	return &lifecycle{t: t, p: p, clk: clock.NewFake(now), herdr: newFakeHerdr()}
}

// start runs a daemon on the environment and connects to it.
func (l *lifecycle) start() {
	l.t.Helper()
	l.c, l.stop = runDaemon(l.t, l.p, daemon.Options{
		Version: "test", Clock: l.clk, Targets: map[string]target.Target{"local": l.herdr},
	})
}

// kill stops the daemon as a crash would leave things: its runs stay open.
func (l *lifecycle) kill() {
	l.t.Helper()
	l.stop()
}

// runDaemon is startWith with a stop function, so a test can stop the
// daemon midway. stop is idempotent and also runs at cleanup.
func runDaemon(t *testing.T, p paths.Paths, opts daemon.Options) (*api.Client, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = daemon.Run(ctx, p, opts)
		close(done)
	}()
	var c *api.Client
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if c != nil {
				_ = c.Close()
			}
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
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-done:
			t.Fatalf("daemon exited early: %v", runErr)
		default:
		}
		var err error
		c, err = api.Dial(p.Socket())
		if err == nil {
			return c, stop
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// eventually waits on the wall clock for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// advanceUntil moves the fake clock forward step by step until cond holds.
// After each step it gives the daemon's goroutines a moment to act on it.
// A goroutine that registers its wait just after a step is woken by the
// next one, so cond decides, not the number of steps.
func (l *lifecycle) advanceUntil(what string, step time.Duration, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out advancing the clock until %s (now %v)", what, l.clk.Now())
		}
		l.clk.Advance(step)
		settle := time.Now().Add(50 * time.Millisecond)
		for !cond() && time.Now().Before(settle) {
			time.Sleep(time.Millisecond)
		}
	}
}

func (l *lifecycle) runNow(force bool) (api.RunNowResult, error) {
	var res api.RunNowResult
	err := l.c.Call(context.Background(), api.MethodTasksRunNow, api.RunNowParams{ID: "job", ForceParallel: force}, &res)
	return res, err
}

func (l *lifecycle) mustRunNow() api.RunNowResult {
	l.t.Helper()
	res, err := l.runNow(false)
	if err != nil {
		l.t.Fatal(err)
	}
	if res.Queued || res.Status != store.StatusRunning || res.HerdrPane == "" {
		l.t.Fatalf("run_now = %+v, want a running run", res)
	}
	return res
}

// runs returns the task's runs, newest first.
func (l *lifecycle) runs() []api.RunSummary {
	l.t.Helper()
	var res api.RunsListResult
	if err := l.c.Call(context.Background(), api.MethodRunsList, api.RunsListParams{TaskID: "job"}, &res); err != nil {
		l.t.Fatal(err)
	}
	return res.Runs
}

func (l *lifecycle) run(id string) api.RunSummary {
	l.t.Helper()
	for _, r := range l.runs() {
		if r.ID == id {
			return r
		}
	}
	l.t.Fatalf("run %s not in runs.list", id)
	return api.RunSummary{}
}

func (l *lifecycle) ended(id string) func() bool {
	return func() bool { return l.run(id).EndedAt != "" }
}

// storedRun reads a run from the database; the daemon must be stopped.
func (l *lifecycle) storedRun(id string) store.Run {
	l.t.Helper()
	st, err := store.Open(l.p.DB())
	if err != nil {
		l.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	r, err := st.GetRun(context.Background(), id)
	if err != nil {
		l.t.Fatal(err)
	}
	return r
}

func (l *lifecycle) logContains(s string) bool {
	data, err := os.ReadFile(l.p.Log())
	return err == nil && strings.Contains(string(data), s)
}

func TestRecoveryAfterKillKeepsTrackingTheSameRun(t *testing.T) {
	// 23:58: two minutes before the @daily fire, which the daemon misses
	// while it is down. missed: run_once makes it a catch-up fire at restart,
	// which overlap skip must drop because the recovered run is active.
	day := time.Date(2026, 11, 6, 23, 58, 0, 0, time.UTC)
	l := newLifecycle(t, day, "timeout: 1h\nmissed: run_once\noverlap: skip\n")
	l.start()
	run := l.mustRunNow()

	l.kill()
	l.clk.Set(day.Add(7 * time.Minute)) // 00:05, the 00:00 fire missed
	l.start()

	runs := l.runs()
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Status != store.StatusRunning || runs[0].EndedAt != "" {
		t.Fatalf("runs after restart = %+v, want only run %s, still running", runs, run.ID)
	}
	eventually(t, "the catch-up fire to be skipped", func() bool {
		return l.logContains("overlap skip: task has an active run")
	})

	l.herdr.set(run.HerdrPane, paneIdle)
	l.advanceUntil("the recovered run ends", pollStep, l.ended(run.ID))
	runs = l.runs()
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Status != store.StatusDone {
		t.Fatalf("runs after the pane went idle = %+v, want only run %s, done", runs, run.ID)
	}
}

func TestRecoveryMarksARunLostWhenItsPaneIsGone(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()

	l.kill()
	l.herdr.set(run.HerdrPane, paneGone)
	l.start()

	runs := l.runs()
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Status != store.StatusLost || runs[0].EndedAt == "" {
		t.Fatalf("runs after restart = %+v, want only run %s, lost", runs, run.ID)
	}
}

func TestRecoveryKeepsARunOpenWhileTheTargetIsUnreachable(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()

	l.kill()
	l.herdr.setUnreachable(true)
	l.start()
	before := l.herdr.count("herdr", "pane", "list")
	l.advanceUntil("a failed poll", pollStep, func() bool { return l.herdr.count("herdr", "pane", "list") > before })
	if got := l.run(run.ID); got.Status != store.StatusRunning || got.EndedAt != "" {
		t.Fatalf("run while the target is unreachable = %+v, want still running", got)
	}

	l.herdr.setUnreachable(false)
	l.herdr.set(run.HerdrPane, paneDone)
	l.advanceUntil("the run ends", pollStep, l.ended(run.ID))
	if got := l.run(run.ID); got.Status != store.StatusDone {
		t.Errorf("run = %+v, want done", got)
	}
}

func TestRunNowOverlapSkipRefusesUnlessForced(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1h\noverlap: skip\n")
	l.start()
	first := l.mustRunNow()

	_, err := l.runNow(false)
	var rpcErr *api.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeRunActive {
		t.Fatalf("second run_now: err = %v, want code %d", err, api.CodeRunActive)
	}
	if n := len(l.runs()); n != 1 {
		t.Fatalf("%d runs after a refused run_now, want 1", n)
	}

	second, err := l.runNow(true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Queued || second.Status != store.StatusRunning || second.ID == first.ID ||
		second.HerdrTab == "" || second.HerdrTab == first.HerdrTab {
		t.Fatalf("forced run_now = %+v, want a second running run in a new tab (first %+v)", second, first)
	}
	if !l.herdr.tabOpen(first.HerdrTab) || !l.herdr.tabOpen(second.HerdrTab) {
		t.Errorf("want both tabs open")
	}
	if n := len(l.runs()); n != 2 {
		t.Errorf("%d runs, want 2", n)
	}
}

func TestRunNowOverlapQueueStartsTheQueuedRunWhenTheActiveOneEnds(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1h\noverlap: queue\n")
	l.start()
	first := l.mustRunNow()

	queued, err := l.runNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if !queued.Queued || queued.ID != "" || queued.TaskID != "job" {
		t.Fatalf("second run_now = %+v, want queued", queued)
	}
	if n := len(l.runs()); n != 1 {
		t.Fatalf("%d runs while queued, want 1", n)
	}

	l.herdr.set(first.HerdrPane, paneIdle)
	l.advanceUntil("the first run ends", pollStep, l.ended(first.ID))
	// The run row appears while the launch is still going: wait for it to
	// leave starting.
	eventually(t, "the queued run to start", func() bool {
		runs := l.runs()
		return len(runs) == 2 && runs[0].Status != store.StatusStarting
	})
	runs := l.runs()
	second := runs[0]
	if second.ID == first.ID || second.Trigger != store.TriggerManual || second.Status != store.StatusRunning ||
		second.HerdrTab == "" || second.HerdrTab == first.HerdrTab {
		t.Errorf("queued run = %+v, want a running manual run in a new tab", second)
	}
	if got := l.run(first.ID); got.Status != store.StatusDone {
		t.Errorf("first run = %+v, want done", got)
	}
}

func TestRunsStopStopsTheRunAndClosesItsTab(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1h\n")
	l.herdr.setOutput("last words of the agent\n")
	l.start()
	run := l.mustRunNow()

	type result struct {
		res api.RunSummary
		err error
	}
	done := make(chan result, 1)
	go func() {
		var res api.RunSummary
		err := l.c.Call(context.Background(), api.MethodRunsStop, api.RunsStopParams{RunID: run.ID}, &res)
		done <- result{res, err}
	}()
	var got result
	returned := false
	l.advanceUntil("runs.stop returns", time.Second, func() bool {
		if !returned {
			select {
			case got = <-done:
				returned = true
			default:
			}
		}
		return returned
	})
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.res.ID != run.ID || got.res.Status != store.StatusStopped || got.res.ExitReason == "" || got.res.EndedAt == "" {
		t.Errorf("runs.stop = %+v, want the run stopped with a reason", got.res)
	}
	if l.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab %s still open after stop", run.HerdrTab)
	}
	if keys := l.herdr.keysSent(run.HerdrPane); !slices.Equal(keys, []string{"esc", "ctrl+c ctrl+c", "ctrl+c ctrl+c"}) {
		t.Errorf("keys sent = %q, want esc, ctrl+c ctrl+c twice", keys)
	}
	if listed := l.run(run.ID); listed.Status != store.StatusStopped {
		t.Errorf("runs.list = %+v, want stopped", listed)
	}

	err := l.c.Call(context.Background(), api.MethodRunsStop, api.RunsStopParams{RunID: "01NOSUCHRUN"}, nil)
	var rpcErr *api.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInvalidParams {
		t.Errorf("runs.stop unknown run: err = %v, want invalid params", err)
	}

	l.kill()
	if tail := l.storedRun(run.ID).OutputTail; tail != "last words of the agent\n" {
		t.Errorf("output tail = %q", tail)
	}
}

func TestTimeoutStopsTheRun(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1m\n")
	l.start()
	run := l.mustRunNow()

	l.advanceUntil("the run times out", pollStep, l.ended(run.ID))
	got := l.run(run.ID)
	if got.Status != store.StatusTimeout || got.ExitReason != "timeout after 1m" {
		t.Errorf("run = %+v, want timeout %q", got, "timeout after 1m")
	}
	if l.herdr.tabOpen(run.HerdrTab) {
		t.Errorf("tab %s still open after the timeout", run.HerdrTab)
	}
}

// Issue #33: a fire waiting in the queue must survive a daemon restart.
func TestQueuedRunSurvivesADaemonRestart(t *testing.T) {
	l := newLifecycle(t, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), "timeout: 1h\noverlap: queue\n")
	l.start()
	first := l.mustRunNow()
	queued, err := l.runNow(false)
	if err != nil {
		t.Fatal(err)
	}
	if !queued.Queued {
		t.Fatalf("second run_now = %+v, want queued", queued)
	}

	l.kill()
	l.start()
	if n := len(l.runs()); n != 1 {
		t.Fatalf("%d runs after the restart, want 1: the queued fire still waits for the recovered run", n)
	}

	l.herdr.set(first.HerdrPane, paneIdle)
	l.advanceUntil("the first run ends", pollStep, l.ended(first.ID))
	eventually(t, "the queued run to start", func() bool {
		runs := l.runs()
		return len(runs) == 2 && runs[0].Status != store.StatusStarting
	})
	second := l.runs()[0]
	if second.ID == first.ID || second.Trigger != store.TriggerManual || second.Status != store.StatusRunning {
		t.Errorf("queued run = %+v, want a running manual run", second)
	}

	// The fire ran once: nothing is left for another restart to start.
	l.kill()
	st, err := store.Open(l.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if fires, err := st.ListQueuedFires(context.Background()); err != nil || len(fires) != 0 {
		t.Errorf("stored queued fires after the run started = %+v err=%v, want none", fires, err)
	}
}
