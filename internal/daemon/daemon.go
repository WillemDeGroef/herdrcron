// Package daemon is `herdrcron daemon`: it loads config.yaml and the task
// files, and serves the daemon API on daemon.sock.
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
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

// Options configures Run.
type Options struct {
	Version string // reported by "status"
	// OnListening, if set, is called once the socket accepts connections.
	OnListening func(socket string)
}

type daemon struct {
	version string
	started time.Time
	cfg     config.Config
	tasks   []config.TaskEntry
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
	d := &daemon{version: opts.Version, started: time.Now(), cfg: cfg, tasks: tasks}
	log.Info("config loaded", "targets", d.targetNames(), "tasks", len(tasks))
	for _, e := range tasks {
		logTask(log, e)
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
	err = srv.Serve(ctx, ln)
	log.Info("daemon stopped")
	return err
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
	}
	for _, n := range d.targetNames() {
		res.Targets = append(res.Targets, api.TargetStatus{Name: n, Reachability: "unknown"})
	}
	return res, nil
}

func (d *daemon) tasksList(context.Context, json.RawMessage) (any, error) {
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
		res.Tasks = append(res.Tasks, s)
	}
	return res, nil
}
