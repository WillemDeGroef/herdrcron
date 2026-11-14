package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
)

// decode unmarshals params into v; a failure is an invalid-params error.
func decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return api.InvalidParams(errors.New("params are missing"))
	}
	if err := json.Unmarshal(params, v); err != nil {
		return api.InvalidParams(err)
	}
	return nil
}

func (d *daemon) tasksSetEnabled(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.SetEnabledParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	// A missing "enabled" decodes as false; never disable a task by accident.
	var present struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(params, &present); err != nil || present.Enabled == nil {
		return nil, api.InvalidParams(errors.New("enabled is missing"))
	}
	return d.editTask(ctx, p.ID, "set enabled", func(path string) error { return config.SetEnabled(path, p.Enabled) })
}

func (d *daemon) tasksPause(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.PauseParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Until != api.PauseIndefinite {
		until, err := time.Parse(time.RFC3339Nano, p.Until)
		if err != nil {
			return nil, api.InvalidParams(fmt.Errorf("until %q must be an RFC 3339 time or %q", p.Until, api.PauseIndefinite))
		}
		if !until.After(d.clk.Now()) {
			return nil, api.InvalidParams(fmt.Errorf("until %s is in the past", p.Until))
		}
	}
	return d.editTask(ctx, p.ID, "pause", func(path string) error { return config.SetPausedUntil(path, p.Until) })
}

func (d *daemon) tasksResume(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.TaskIDParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return d.editTask(ctx, p.ID, "resume", func(path string) error { return config.SetPausedUntil(path, "") })
}

// pauseExpired is the scheduler's OnPauseExpired: it clears the task's
// paused_until in the task file, unless the file no longer holds a pause
// that has ended (a resume or a new pause came first). A write error is
// returned so the scheduler tries again. An invalid file is cleared too
// when its paused_until can be read; a file that cannot be edited fails
// the write and is retried.
func (d *daemon) pauseExpired(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.find(id)
	if i < 0 {
		return nil
	}
	if end, ok := d.tasks[i].Task.PauseEnd(); !ok || end.After(d.clk.Now()) {
		return nil
	}
	_, err := d.editLocked(ctx, i, "pause ended", func(path string) error { return config.SetPausedUntil(path, "") })
	return err
}

// editTask applies edit to the task file of a valid task, reloads the file,
// reschedules the task and returns its summary.
func (d *daemon) editTask(ctx context.Context, id, action string, edit func(path string) error) (api.TaskSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.find(id)
	if i < 0 {
		return api.TaskSummary{}, api.InvalidParams(fmt.Errorf("unknown task %q", id))
	}
	if err := d.tasks[i].Err; err != nil {
		return api.TaskSummary{}, api.InvalidParams(fmt.Errorf("task %q is invalid: %v", id, err))
	}
	return d.editLocked(ctx, i, action, edit)
}

// editLocked is editTask for d.tasks[i], with d.mu held.
func (d *daemon) editLocked(ctx context.Context, i int, action string, edit func(path string) error) (api.TaskSummary, error) {
	old := d.tasks[i]
	if err := edit(old.File); err != nil && !errors.Is(err, config.ErrReplaced) {
		return api.TaskSummary{}, fmt.Errorf("write task file %s: %w", old.File, err)
	} else if err != nil {
		// The new bytes are already at the path. Skipping the reload would
		// leave memory on the old task.
		d.log.Error("task file replaced but directory sync failed", logging.KeyTaskID, old.ID, "err", err)
	}
	// Reload rather than patch the entry, so memory matches the file.
	e := config.LoadTask(d.configDir, old.ID, d.config())
	d.tasks[i] = e
	logTask(d.log, e)
	d.log.Info("task file written", logging.KeyTaskID, e.ID, "action", action,
		"enabled", e.Task.Enabled, "paused_until", e.Task.PausedUntil)
	// The file is written; a failed reschedule must not report the edit
	// as failed. The next reload schedules the task again.
	if err := d.sched.SetTask(ctx, e); err != nil {
		d.log.Error("cannot reschedule task", logging.KeyTaskID, e.ID, "err", err)
	}
	d.setVersionLocked(e.ID, d.fileVersion(e))
	d.publishTaskLocked(ctx, i)
	return d.summary(ctx, e)
}

// find returns the index of task id in d.tasks, or -1.
func (d *daemon) find(id string) int {
	for i, e := range d.tasks {
		if e.ID == id {
			return i
		}
	}
	return -1
}

func (d *daemon) pauseAll(ctx context.Context, params json.RawMessage) (any, error) {
	return d.setGlobalPause(ctx, params, true)
}

func (d *daemon) resumeAll(ctx context.Context, params json.RawMessage) (any, error) {
	return d.setGlobalPause(ctx, params, false)
}

// setGlobalPause pauses or resumes every task on one target, or on all
// targets when params name none. It writes the daemon state, never a task
// file.
func (d *daemon) setGlobalPause(ctx context.Context, params json.RawMessage, paused bool) (any, error) {
	var p api.TargetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, api.InvalidParams(err)
		}
	}
	if _, ok := d.config().Targets[p.Target]; p.Target != "" && !ok {
		return nil, api.InvalidParams(fmt.Errorf("unknown target %q", p.Target))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.store.SetGlobalPause(ctx, p.Target, paused); err != nil {
		return nil, err
	}
	if err := d.loadGlobalPauseLocked(ctx); err != nil {
		return nil, err
	}
	d.log.Info("global pause changed", logging.KeyTarget, p.Target, "paused", paused,
		"paused_all", d.globalPause.PausedAll, "paused_targets", d.globalPause.PausedTargets)
	// The display state of the tasks covered changes (paused or idle), and
	// subscribers learn of it only through task.changed. Invalid tasks keep
	// their state, so they are skipped, and so are tasks on other targets
	// when one target changed.
	for i, e := range d.tasks {
		if e.Err == nil && (p.Target == "" || e.Task.Target == p.Target) {
			d.publishTaskLocked(ctx, i)
		}
	}
	return d.globalPause, nil
}

// loadGlobalPause reads the global pause from the daemon state and hands it
// to the scheduler.
func (d *daemon) loadGlobalPause(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loadGlobalPauseLocked(ctx)
}

func (d *daemon) loadGlobalPauseLocked(ctx context.Context) error {
	all, targets, err := d.store.GlobalPause(ctx)
	if err != nil {
		return err
	}
	if targets == nil {
		targets = []string{} // JSON [] rather than null
	}
	d.globalPause = api.GlobalPause{PausedAll: all, PausedTargets: targets}
	d.sched.SetGlobalPause(all, targets)
	return nil
}
