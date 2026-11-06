// Package daemon is `herdrcron daemon`: it loads config.yaml and the task
// files, runs the scheduler, and serves the daemon API on daemon.sock.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/dispatch"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/notify"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/scheduler"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
	"github.com/WillemDeGroef/herdrcron/internal/watch"
)

// Options configures Run.
type Options struct {
	Version string // reported by "status"
	// OnListening, if set, is called once the socket accepts connections.
	OnListening func(socket string)
	// Clock drives the scheduler and the runner; nil means the system clock.
	Clock clock.Clock
	// Targets, if set, replaces the real targets by name; tests use it to
	// run without herdr.
	Targets map[string]target.Target
}

type daemon struct {
	version   string
	started   time.Time
	configDir string
	cfg       config.Config
	tasks     []config.TaskEntry
	store     *store.Store
	sched     *scheduler.Scheduler
	runner    *runner.Runner
	disp      *dispatch.Dispatcher
	clk       clock.Clock
	log       *slog.Logger
	// sshControlDir holds the ssh ControlMaster sockets of SSH targets.
	sshControlDir string
	// configFile is config.yaml, re-read by "reload".
	configFile string
	bus        *bus

	// mu guards tasks, versions, deleting and globalPause, and serialises
	// task file writes. Lock order: mu, then the scheduler's lock. Never
	// call into the runner or dispatcher in a way that can end or start a
	// run with mu held: that path calls back into the daemon.
	mu          sync.Mutex
	globalPause api.GlobalPause
	// versions is the version of each task's files as last loaded; a
	// watcher event whose files still have it (the daemon's own write)
	// changes nothing.
	versions map[string]string
	// deleting holds tasks being deleted; the watcher leaves them alone.
	deleting map[string]bool

	// notifier runs notify.command for configured events; wired from runChanged
	// so run notifications share one path with subscribe run.changed.
	notifier *notify.Notifier

	// dirty holds tasks whose runs changed since their state was last
	// published; dirtyWake wakes publishDirty.
	dirtyMu   sync.Mutex
	dirty     map[string]struct{}
	dirtyWake chan struct{}
}

// Run loads the configuration and tasks, then serves the API on the socket
// until ctx is done. It logs JSON to daemon.log, rotated by size. An invalid config.yaml stops
// startup; an invalid task file does not.
func Run(ctx context.Context, p paths.Paths, opts Options) error {
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	logFile, err := logging.Open(p.Log())
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	log := slog.New(slog.NewJSONHandler(logFile, nil))

	log.Info("daemon starting", "version", opts.Version, "config_dir", p.ConfigDir, "state_dir", p.StateDir)
	cfg, err := config.Load(p.ConfigFile())
	if err != nil {
		log.Error("config invalid", "err", err)
		return err
	}
	tasks, err := config.LoadTasks(p.ConfigDir, cfg)
	if err != nil {
		log.Error("cannot load tasks", "err", err)
		return err
	}
	st, err := store.Open(p.DB())
	if err != nil {
		log.Error("cannot open state database", "err", err)
		return err
	}
	defer func() { _ = st.Close() }()
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real()
	}
	d := &daemon{
		version: opts.Version, started: time.Now(), configDir: p.ConfigDir,
		cfg: cfg, tasks: tasks, store: st, clk: clk, log: log,
		configFile: p.ConfigFile(), bus: newBus(log), sshControlDir: p.SSHControlDir(),
		versions: map[string]string{}, deleting: map[string]bool{},
		dirty: map[string]struct{}{}, dirtyWake: make(chan struct{}, 1),
	}
	for _, e := range tasks {
		d.versions[e.ID] = d.fileVersion(e)
	}
	log.Info("config loaded", "targets", d.targetNames(), "tasks", len(tasks))
	for _, e := range tasks {
		logTask(log, e)
	}
	targets := opts.Targets
	if targets == nil {
		targets = realTargets(cfg, p.SSHControlDir())
	}
	notifier := notify.New(cfg.Notify, log)
	d.notifier = notifier
	// Let notifications already started reach the user before exiting.
	defer notifier.Wait()
	d.runner = runner.New(runner.Options{
		Clock: clk, Store: st, Targets: targets, Config: cfg, ConfigDir: p.ConfigDir, Log: log,
		OnEnd:    func(ctx context.Context, run store.Run) { d.disp.RunEnded(ctx, run) },
		OnChange: d.runChanged,
	})
	d.disp = dispatch.New(dispatch.Options{Runs: latestDef{d}, Store: st, Config: cfg, Log: log})
	d.sched = scheduler.New(scheduler.Options{
		Clock: clk, Store: st, Runs: d.disp, Config: cfg, Log: log,
		OnPauseExpired: d.pauseExpired,
	})
	if err := d.loadGlobalPause(ctx); err != nil {
		log.Error("cannot read global pause", "err", err)
		return err
	}
	// Recover open runs before the scheduler reads task_state: a catch-up
	// fire must see a recovered run as active and follow overlap.
	if err := d.runner.Recover(ctx, validTasks(tasks)); err != nil {
		log.Error("cannot recover open runs", "err", err)
		return err
	}
	if err := d.sched.SetTasks(ctx, tasks); err != nil {
		log.Error("cannot schedule tasks", "err", err)
		return err
	}

	ln, err := api.Listen(p.Socket())
	if err != nil {
		log.Error("cannot listen", "err", err)
		return err
	}
	defer func() { _ = os.Remove(p.Socket()) }()
	srv := api.NewServer(log)
	srv.Handle(api.MethodStatus, d.status)
	srv.Handle(api.MethodTasksList, d.tasksList)
	srv.Handle(api.MethodTasksSetEnabled, d.tasksSetEnabled)
	srv.Handle(api.MethodTasksPause, d.tasksPause)
	srv.Handle(api.MethodTasksResume, d.tasksResume)
	srv.Handle(api.MethodPauseAll, d.pauseAll)
	srv.Handle(api.MethodResumeAll, d.resumeAll)
	srv.Handle(api.MethodTasksRunNow, d.tasksRunNow)
	srv.Handle(api.MethodRunsList, d.runsList)
	srv.Handle(api.MethodRunsStop, d.runsStop)
	srv.Handle(api.MethodRunsOutput, d.runsOutput)
	srv.Handle(api.MethodRunsAttachCmd, d.runsAttachCmd)
	srv.Handle(api.MethodTasksGet, d.tasksGet)
	srv.Handle(api.MethodTasksCreate, d.tasksCreate)
	srv.Handle(api.MethodTasksUpdate, d.tasksUpdate)
	srv.Handle(api.MethodTasksDelete, d.tasksDelete)
	srv.Handle(api.MethodReload, d.reload)
	srv.HandleStream(api.MethodSubscribe, d.subscribe)
	w, err := watch.New([]string{d.tasksDir(), d.promptsDir()}, watchDebounce, log)
	if err != nil {
		log.Error("cannot watch task files", "err", err)
		return err
	}
	log.Info("daemon listening", "socket", p.Socket())
	if opts.OnListening != nil {
		opts.OnListening(p.Socket())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		_ = d.sched.Run(ctx) // returns when ctx is done
	}()
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		d.runner.Run(ctx) // returns when ctx is done
	}()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		d.watchFiles(ctx, w) // returns when ctx is done
	}()
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		d.publishDirty(ctx)
	}()
	err = srv.Serve(ctx, ln)
	cancel()
	<-schedDone
	<-runnerDone
	<-watchDone
	<-publishDone
	log.Info("daemon stopped")
	return err
}

// validTasks returns the tasks whose files loaded without error.
func validTasks(entries []config.TaskEntry) []config.Task {
	var out []config.Task
	for _, e := range entries {
		if e.Err == nil {
			out = append(out, e.Task)
		}
	}
	return out
}

// realTargets returns the targets runs can use: local, and an SSH target
// for every other target in config.yaml.
func realTargets(cfg config.Config, sshControlDir string) map[string]target.Target {
	targets := map[string]target.Target{}
	for name, t := range cfg.Targets {
		if name == config.LocalTarget {
			targets[name] = target.NewLocal()
			continue
		}
		targets[name] = sshTarget(name, t, sshControlDir)
	}
	return targets
}

// sshTarget is the SSH target for the config.yaml target name.
func sshTarget(name string, t config.Target, controlDir string) *target.SSH {
	return target.NewSSH(target.SSHConfig{Name: name, Host: t.Host, Shell: t.Shell, ControlDir: controlDir})
}

// logTask logs one loaded task. Never log prompt text or env values: only
// the prompt file path and the env keys.
func logTask(log *slog.Logger, e config.TaskEntry) {
	if e.Err != nil {
		log.Warn("task invalid", logging.KeyTaskID, e.ID, "file", e.File, "err", e.Err)
		return
	}
	envKeys := make([]string, 0, len(e.Task.Env))
	for k := range e.Task.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	log.Info("task loaded", logging.KeyTaskID, e.ID, logging.KeyTarget, e.Task.Target, "file", e.File,
		"schedule", e.Task.Schedule, "enabled", e.Task.Enabled,
		"prompt_file", e.Task.PromptFile, "env_keys", envKeys)
}

func (d *daemon) targetNames() []string {
	names := make([]string, 0, len(d.cfg.Targets))
	for n := range d.cfg.Targets {
		names = append(names, n)
	}
	// "local" first, then the configured targets by name.
	slices.SortFunc(names, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == config.LocalTarget:
			return -1
		case b == config.LocalTarget:
			return 1
		case a < b:
			return -1
		}
		return 1
	})
	return names
}

func (d *daemon) status(context.Context, json.RawMessage) (any, error) {
	res := api.StatusResult{
		Version:       d.version,
		StartedAt:     d.started.Format(time.RFC3339),
		UptimeSeconds: int64(time.Since(d.started) / time.Second),
		Timezone:      d.cfg.Timezone,
	}
	for _, n := range d.targetNames() {
		res.Targets = append(res.Targets, api.TargetStatus{Name: n, Reachability: "unknown"})
	}
	d.mu.Lock()
	res.GlobalPause = d.globalPause
	d.mu.Unlock()
	return res, nil
}

func (d *daemon) tasksList(ctx context.Context, _ json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res := api.TasksListResult{Tasks: make([]api.TaskSummary, 0, len(d.tasks))}
	for _, e := range d.tasks {
		s, err := d.summary(ctx, e)
		if err != nil {
			return nil, err
		}
		res.Tasks = append(res.Tasks, s)
	}
	return res, nil
}

// summary describes one task for the API. Call it with d.mu held.
func (d *daemon) summary(ctx context.Context, e config.TaskEntry) (api.TaskSummary, error) {
	s := api.TaskSummary{ID: e.ID, Name: e.Task.Name, Target: e.Task.Target, Schedule: e.Task.Schedule, Enabled: e.Task.Enabled}
	var verr *config.ValidationError
	now := d.clk.Now()
	switch {
	case e.Err != nil:
		s.State, s.Error = api.StateInvalid, e.Err.Error()
		if !errors.As(e.Err, &verr) {
			// Unreadable or unparsable file: its fields are not trustworthy.
			s.Name, s.Target, s.Schedule, s.Enabled = "", "", "", false
		}
	case !e.Task.Enabled:
		s.State = api.StateOff
	case e.Task.PausedAt(now):
		s.State, s.PausedUntil = api.StatePaused, e.Task.PausedUntil
	case d.sched.GloballyPaused(e.Task.Target):
		s.State = api.StatePaused
	default:
		s.State = api.StateIdle
	}
	// An active run shows over off, paused and idle: a disabled or paused
	// task can still have a manual run going. Invalid stays invalid, so
	// the error text remains the display state; the run itself is unchanged.
	if live := d.liveState(e.ID); live != "" && s.State != api.StateInvalid {
		s.State = live
	}
	if next, ok := d.sched.NextFire(e.ID); ok {
		s.NextFireAt = apiTime(next)
	}
	last, ok, err := d.store.LastRun(ctx, e.ID)
	if err != nil {
		return api.TaskSummary{}, fmt.Errorf("last run of %s: %w", e.ID, err)
	}
	if ok {
		s.LastRun = runSummary(last)
	}
	return s, nil
}

// tasksRunNow starts a manual run of a valid task, whether or not it is
// enabled or paused (spec rule 6). It follows the task's overlap: with skip
// and an active run it fails with CodeRunActive unless force_parallel is
// set; with queue it may only queue the fire.
func (d *daemon) tasksRunNow(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.RunNowParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.InvalidParams(errors.New("id is required"))
	}
	// Copy the entry under d.mu, but submit without it: Submit may start a
	// run, which calls back into the daemon.
	d.mu.Lock()
	i := d.find(p.ID)
	var e config.TaskEntry
	if i >= 0 {
		e = d.tasks[i]
	}
	d.mu.Unlock()
	if i < 0 {
		return nil, api.InvalidParams(fmt.Errorf("unknown task %q", p.ID))
	}
	if e.Err != nil {
		return nil, &api.Error{Code: api.CodeInvalidParams, Message: fmt.Sprintf("task %s is invalid: %v", e.ID, e.Err)}
	}
	out, err := d.disp.Submit(ctx, e.Task, store.TriggerManual, time.Time{}, p.ForceParallel)
	switch {
	case errors.Is(err, dispatch.ErrActive):
		return nil, &api.Error{Code: api.CodeRunActive,
			Message: fmt.Sprintf("task %s has an active run (overlap: skip)", e.ID)}
	case err != nil:
		return nil, fmt.Errorf("start run of %s: %w", e.ID, err)
	case out.Run.ID != "":
		return api.RunNowResult{RunSummary: *runSummary(out.Run)}, nil
	case out.Skipped:
		// overlap queue with a fire already pending: this one is dropped.
		return nil, &api.Error{Code: api.CodeRunActive,
			Message: fmt.Sprintf("task %s has an active run and a queued fire (overlap: queue)", e.ID)}
	}
	return api.RunNowResult{RunSummary: api.RunSummary{TaskID: e.ID, Target: e.Task.Target, Trigger: store.TriggerManual}, Queued: true}, nil
}

// runsStop stops an active run and returns it once the stop sequence is
// done: status stopped, output tail saved, tab closed.
func (d *daemon) runsStop(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.RunsStopParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.RunID == "" {
		return nil, api.InvalidParams(errors.New("run_id is required"))
	}
	run, err := d.runner.Stop(ctx, p.RunID)
	if errors.Is(err, runner.ErrNotActive) {
		return nil, api.InvalidParams(fmt.Errorf("run %s is not active", p.RunID))
	}
	if err != nil {
		return nil, fmt.Errorf("stop run %s: %w", p.RunID, err)
	}
	return runSummary(run), nil
}

// runsList returns runs newest first, of one task or all.
func (d *daemon) runsList(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.RunsListParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Limit < 0 {
		return nil, api.InvalidParams(errors.New("limit must not be negative"))
	}
	if p.Limit == 0 {
		p.Limit = api.DefaultRunsLimit
	}
	runs, err := d.store.ListRuns(ctx, p.TaskID, p.Limit)
	if err != nil {
		return nil, err
	}
	res := api.RunsListResult{Runs: make([]api.RunSummary, 0, len(runs))}
	for _, r := range runs {
		res.Runs = append(res.Runs, *runSummary(r))
	}
	return res, nil
}

// decodeParams decodes params into v; absent params leave v as is.
func decodeParams(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return api.InvalidParams(err)
	}
	return nil
}

func runSummary(r store.Run) *api.RunSummary {
	return &api.RunSummary{
		ID: r.ID, TaskID: r.TaskID, Target: r.Target, Trigger: r.Trigger, Status: r.Status,
		ScheduledAt: apiTime(r.ScheduledAt), StartedAt: apiTime(r.StartedAt),
		EndedAt: apiTime(r.EndedAt), ExitReason: r.ExitReason,
		HerdrWorkspace: r.HerdrWorkspace, HerdrTab: r.HerdrTab, HerdrPane: r.HerdrPane,
	}
}

// apiTime is RFC 3339 in UTC, or "" for the zero time.
func apiTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// latestDef sits between the dispatcher and the runner. A fire carries the
// task as it was when it was scheduled or queued; latestDef starts the
// definition as it is now, so an edit applies to the next run. A task
// deleted or made invalid meanwhile does not start.
type latestDef struct{ d *daemon }

func (l latestDef) Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error) {
	cur, err := l.d.currentTask(t)
	if err != nil {
		return store.Run{}, err
	}
	return l.d.runner.Start(ctx, cur, trigger, scheduledAt)
}

func (l latestDef) ActiveRuns() []store.Run { return l.d.runner.ActiveRuns() }
