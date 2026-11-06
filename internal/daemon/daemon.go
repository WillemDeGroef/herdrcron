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
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/scheduler"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// Options configures Run.
type Options struct {
	Version string // reported by "status"
	// OnListening, if set, is called once the socket accepts connections.
	OnListening func(socket string)
	// Clock drives the scheduler; nil means the system clock.
	Clock clock.Clock
}

type daemon struct {
	version string
	started time.Time
	cfg     config.Config
	tasks   []config.TaskEntry
	store   *store.Store
	sched   *scheduler.Scheduler
	log     *slog.Logger
}

// Run loads the configuration and tasks, then serves the API on the socket
// until ctx is done. It logs JSON to daemon.log. An invalid config.yaml stops
// startup; an invalid task file does not.
func Run(ctx context.Context, p paths.Paths, opts Options) error {
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	logFile, err := os.OpenFile(p.Log(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
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
	d := &daemon{version: opts.Version, started: time.Now(), cfg: cfg, tasks: tasks, store: st, log: log}
	log.Info("config loaded", "targets", d.targetNames(), "tasks", len(tasks))
	for _, e := range tasks {
		logTask(log, e)
	}
	// Until the herdr adapter exists, every target is a fake that runs
	// nothing; see target.Fake.
	targets := make(map[string]target.Target, len(cfg.Targets))
	for name := range cfg.Targets {
		targets[name] = target.NewFake(name)
	}
	d.sched = scheduler.New(scheduler.Options{Clock: clk, Store: st, Targets: targets, Config: cfg, Log: log})
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
	err = srv.Serve(ctx, ln)
	cancel()
	<-schedDone
	log.Info("daemon stopped")
	return err
}

// logTask logs one loaded task. Never log prompt text or env values: only
// the prompt file path and the env keys.
func logTask(log *slog.Logger, e config.TaskEntry) {
	if e.Err != nil {
		log.Warn("task invalid", "task_id", e.ID, "file", e.File, "err", e.Err)
		return
	}
	envKeys := make([]string, 0, len(e.Task.Env))
	for k := range e.Task.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	log.Info("task loaded", "task_id", e.ID, "target", e.Task.Target, "file", e.File,
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
	}
	for _, n := range d.targetNames() {
		res.Targets = append(res.Targets, api.TargetStatus{Name: n, Reachability: "unknown"})
	}
	return res, nil
}

func (d *daemon) tasksList(ctx context.Context, _ json.RawMessage) (any, error) {
	res := api.TasksListResult{Tasks: make([]api.TaskSummary, 0, len(d.tasks))}
	for _, e := range d.tasks {
		s := api.TaskSummary{ID: e.ID, Name: e.Task.Name, Target: e.Task.Target, Schedule: e.Task.Schedule}
		var verr *config.ValidationError
		switch {
		case e.Err != nil:
			s.State, s.Error = api.StateInvalid, e.Err.Error()
			if !errors.As(e.Err, &verr) {
				// Unreadable or unparsable file: its fields are not trustworthy.
				s.Name, s.Target, s.Schedule = "", "", ""
			}
		case !e.Task.Enabled:
			s.State = api.StateOff
		default:
			s.State = api.StateIdle
		}
		if next, ok := d.sched.NextFire(e.ID); ok {
			s.NextFireAt = apiTime(next)
		}
		last, ok, err := d.store.LastRun(ctx, e.ID)
		if err != nil {
			return nil, fmt.Errorf("last run of %s: %w", e.ID, err)
		}
		if ok {
			s.LastRun = runSummary(last)
		}
		res.Tasks = append(res.Tasks, s)
	}
	return res, nil
}

func runSummary(r store.Run) *api.RunSummary {
	return &api.RunSummary{
		ID: r.ID, Trigger: r.Trigger, Status: r.Status,
		ScheduledAt: apiTime(r.ScheduledAt), StartedAt: apiTime(r.StartedAt),
		EndedAt: apiTime(r.EndedAt), ExitReason: r.ExitReason,
	}
}

// apiTime is RFC 3339 in UTC, or "" for the zero time.
func apiTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
