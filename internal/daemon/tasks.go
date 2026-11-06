package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

func (d *daemon) tasksDir() string   { return filepath.Join(d.configDir, "tasks") }
func (d *daemon) promptsDir() string { return filepath.Join(d.configDir, "prompts") }

// promptPathOf is the prompt file of an entry, also for an invalid one
// (from a lenient read; prompts/<id>.md if the file does not parse).
func (d *daemon) promptPathOf(e config.TaskEntry) string {
	pf := e.Task.PromptFile
	if e.Err != nil {
		if t, err := config.ReadTaskLenient(e.File); err == nil {
			pf = t.PromptFile
		}
	}
	if pf == "" {
		pf = config.DefaultTask(e.ID).PromptFile
	}
	return config.PromptPath(pf, d.configDir)
}

// fileVersion is the current version of an entry's files; "" if the task file
// is gone.
func (d *daemon) fileVersion(e config.TaskEntry) string {
	v, err := config.Version(e.File, d.promptPathOf(e))
	if err != nil {
		return ""
	}
	return v
}

// liveState is the display state from the task's active runs: blocked,
// running, or "" with none.
func (d *daemon) liveState(id string) string {
	state := ""
	for _, r := range d.runner.LiveRuns() {
		if r.TaskID != id {
			continue
		}
		if r.Status == store.StatusBlocked {
			return api.StateBlocked
		}
		state = api.StateRunning
	}
	return state
}

func (d *daemon) tasksGet(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.TaskIDParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Catch up with a hand edit the watcher has not reloaded yet, so the
	// definition and version returned match the files.
	d.reloadOneLocked(ctx, p.ID)
	i := d.find(p.ID)
	if i < 0 {
		return nil, api.InvalidParams(fmt.Errorf("unknown task %q", p.ID))
	}
	return d.detailLocked(ctx, d.tasks[i])
}

// detailLocked describes one task with its definition, prompt, version and
// last runs. Call it with d.mu held.
func (d *daemon) detailLocked(ctx context.Context, e config.TaskEntry) (api.TaskDetail, error) {
	s, err := d.summary(ctx, e)
	if err != nil {
		return api.TaskDetail{}, err
	}
	t := e.Task
	if e.Err != nil {
		// Show what can be read, so the user can fix it in the form.
		t, _ = config.ReadTaskLenient(e.File)
		t.ID = e.ID
	}
	det := api.TaskDetail{Summary: s, Task: taskDef(t), Version: d.versions[e.ID], Runs: []api.RunSummary{}}
	if prompt, err := os.ReadFile(d.promptPathOf(e)); err == nil {
		det.Prompt = string(prompt)
	}
	runs, err := d.store.ListRuns(ctx, e.ID, api.TaskRunsLimit)
	if err != nil {
		return api.TaskDetail{}, fmt.Errorf("runs of %s: %w", e.ID, err)
	}
	for _, r := range runs {
		det.Runs = append(det.Runs, *runSummary(r))
	}
	return det, nil
}

// fieldErrors collects validation problems by field.
type fieldErrors map[string][]string

func (f fieldErrors) add(field, msg string) { f[field] = append(f[field], msg) }

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	var parts []string
	for _, field := range slices.Sorted(func(yield func(string) bool) {
		for k := range f {
			if !yield(k) {
				return
			}
		}
	}) {
		parts = append(parts, field+": "+strings.Join(f[field], ", "))
	}
	return &api.Error{Code: api.CodeValidation, Message: "task is invalid: " + strings.Join(parts, "; "),
		Data: api.ValidationErrorData{Fields: f}}
}

// draft turns write params into a task and checks it, without the prompt
// file having to exist. Problems are collected by field.
func (d *daemon) draft(def api.TaskDef) (config.Task, fieldErrors) {
	fe := fieldErrors{}
	t, timeoutErr := taskFromDef(def)
	if timeoutErr != nil {
		fe.add("timeout", timeoutErr.Error())
	}
	var verr *config.ValidationError
	if err := config.ValidateDraft(t, d.configDir, d.cfg); errors.As(err, &verr) {
		for field, msgs := range verr.ByField() {
			if field == "timeout" && timeoutErr != nil {
				continue // already said why
			}
			for _, m := range msgs {
				fe.add(field, m)
			}
		}
	} else if err != nil {
		fe.add("task", err.Error())
	}
	return t, fe
}

func (d *daemon) tasksCreate(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.TaskWriteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, fe := d.draft(p.Task)
	path := filepath.Join(d.tasksDir(), t.ID+".yaml")
	if t.ID != "" && len(fe["id"]) == 0 {
		if _, err := os.Lstat(path); d.find(t.ID) >= 0 || err == nil {
			fe.add("id", fmt.Sprintf("task %q already exists", t.ID))
		}
	}
	if err := fe.err(); err != nil {
		return nil, err
	}
	promptPath := config.PromptPath(t.PromptFile, d.configDir)
	_, statErr := os.Stat(promptPath)
	newPrompt := errors.Is(statErr, fs.ErrNotExist)
	// The prompt first: a task file that appears without it is invalid.
	if err := config.WritePrompt(promptPath, p.Prompt); err != nil && !errors.Is(err, config.ErrReplaced) {
		return nil, fmt.Errorf("write prompt file: %w", err)
	}
	if err := config.WriteTask(path, t, true); err != nil && !errors.Is(err, config.ErrReplaced) {
		if newPrompt {
			_ = os.Remove(promptPath)
		}
		if errors.Is(err, fs.ErrExist) {
			fe.add("id", fmt.Sprintf("task %q already exists", t.ID))
			return nil, fe.err()
		}
		return nil, fmt.Errorf("write task file: %w", err)
	}
	e := config.LoadTask(d.configDir, t.ID, d.cfg)
	d.tasks = append(d.tasks, e)
	slices.SortFunc(d.tasks, func(a, b config.TaskEntry) int { return strings.Compare(a.ID, b.ID) })
	d.log.Info("task created", logging.KeyTaskID, e.ID)
	return d.storedLocked(ctx, e)
}

func (d *daemon) tasksUpdate(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.TaskWriteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.InvalidParams(errors.New("id is required"))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.find(p.ID)
	if i < 0 {
		return nil, api.InvalidParams(fmt.Errorf("unknown task %q", p.ID))
	}
	old := d.tasks[i]
	// Compare with the files, not memory: a hand edit the watcher has not
	// reloaded yet must still refuse the write.
	if cur := d.fileVersion(old); p.Version == "" || p.Version != cur {
		return nil, &api.Error{Code: api.CodeVersionMismatch,
			Message: fmt.Sprintf("task %s changed since it was read; read it again", p.ID)}
	}
	t, fe := d.draft(p.Task)
	if t.ID != p.ID {
		fe["id"] = []string{fmt.Sprintf("id cannot change (task %q)", p.ID)}
	}
	if err := fe.err(); err != nil {
		return nil, err
	}
	if err := config.WritePrompt(config.PromptPath(t.PromptFile, d.configDir), p.Prompt); err != nil && !errors.Is(err, config.ErrReplaced) {
		return nil, fmt.Errorf("write prompt file: %w", err)
	}
	if err := config.WriteTask(old.File, t, false); err != nil && !errors.Is(err, config.ErrReplaced) {
		return nil, fmt.Errorf("write task file: %w", err)
	}
	e := config.LoadTask(d.configDir, p.ID, d.cfg)
	d.tasks[i] = e
	d.log.Info("task updated", logging.KeyTaskID, e.ID)
	return d.storedLocked(ctx, e)
}

// storedLocked finishes a create or update: version, schedule, event and
// result. The edit applies to the next run; active runs keep the task they
// started with.
func (d *daemon) storedLocked(ctx context.Context, e config.TaskEntry) (api.TaskDetail, error) {
	logTask(d.log, e)
	d.versions[e.ID] = d.fileVersion(e)
	// SetTasks keeps the next fire of an unchanged schedule.
	if err := d.sched.SetTasks(ctx, d.tasks); err != nil {
		d.log.Error("cannot reschedule tasks", logging.KeyTaskID, e.ID, "err", err)
	}
	d.publishTaskLocked(ctx, d.find(e.ID))
	return d.detailLocked(ctx, e)
}

func (d *daemon) tasksDelete(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.DeleteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	d.mu.Lock()
	i := d.find(p.ID)
	if i < 0 {
		d.mu.Unlock()
		return nil, api.InvalidParams(fmt.Errorf("unknown task %q", p.ID))
	}
	if !p.StopActive && d.liveState(p.ID) != "" {
		d.mu.Unlock()
		return nil, &api.Error{Code: api.CodeRunActiveDelete,
			Message: fmt.Sprintf("task %s has an active run; delete with stop_active to stop it", p.ID)}
	}
	// Unschedule first, so no new run starts while the active ones stop,
	// and keep hand-edit reloads away until the files are gone.
	d.deleting[p.ID] = true
	defer func() {
		d.mu.Lock()
		delete(d.deleting, p.ID)
		d.mu.Unlock()
	}()
	rest := slices.Delete(slices.Clone(d.tasks), i, i+1)
	if err := d.sched.SetTasks(ctx, rest); err != nil {
		d.log.Error("cannot unschedule task", logging.KeyTaskID, p.ID, "err", err)
	}
	d.mu.Unlock()

	// Stopping takes seconds and ends runs, which calls back into the
	// dispatcher and this daemon: never hold d.mu here.
	res := api.DeleteResult{ID: p.ID, StoppedRuns: []api.RunSummary{}}
	stopped, err := d.stopRunsOf(ctx, p.ID)
	for _, r := range stopped {
		res.StoppedRuns = append(res.StoppedRuns, *runSummary(r))
	}
	if err != nil {
		d.restoreSchedule(ctx)
		return nil, fmt.Errorf("stop active runs of %s: %w", p.ID, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	i = d.find(p.ID)
	if i < 0 {
		return res, nil // a reload removed it meanwhile
	}
	e := d.tasks[i]
	promptPath := d.promptPathOf(e)
	if err := os.Remove(e.File); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = d.sched.SetTasks(ctx, d.tasks)
		return nil, fmt.Errorf("remove task file: %w", err)
	}
	d.tasks = slices.Delete(d.tasks, i, i+1)
	delete(d.versions, p.ID)
	// A prompt file another task points at stays.
	shared := slices.ContainsFunc(d.tasks, func(o config.TaskEntry) bool {
		return filepath.Clean(d.promptPathOf(o)) == filepath.Clean(promptPath)
	})
	if !shared {
		if err := os.Remove(promptPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.log.Warn("cannot remove prompt file", logging.KeyTaskID, p.ID, "err", err)
		}
	}
	if err := d.store.DeleteTaskState(ctx, p.ID); err != nil {
		d.log.Warn("cannot remove task state", logging.KeyTaskID, p.ID, "err", err)
	}
	d.log.Info("task deleted", logging.KeyTaskID, p.ID, "stopped_runs", len(res.StoppedRuns))
	d.publishDeleted(p.ID)
	return res, nil
}

// restoreSchedule schedules d.tasks again after a delete gave up.
func (d *daemon) restoreSchedule(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.sched.SetTasks(ctx, d.tasks); err != nil {
		d.log.Error("cannot reschedule tasks", "err", err)
	}
}

// stopRunsOf stops every live run of the task, in parallel, and returns
// them as stopped. A run still starting is waited for first.
func (d *daemon) stopRunsOf(ctx context.Context, id string) ([]store.Run, error) {
	var (
		mu      sync.Mutex
		stopped []store.Run
		errs    []error
	)
	for {
		var ids []string
		starting := false
		for _, r := range d.runner.LiveRuns() {
			if r.TaskID != id {
				continue
			}
			if d.runner.Active(r.ID) {
				ids = append(ids, r.ID)
			} else {
				starting = true
			}
		}
		if len(ids) == 0 && !starting {
			break
		}
		if len(ids) == 0 {
			// Wait for the launch to finish; it then shows up as active or ends.
			select {
			case <-ctx.Done():
				return stopped, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		var wg sync.WaitGroup
		for _, runID := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := d.runner.Stop(ctx, runID)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				stopped = append(stopped, r)
			}()
		}
		wg.Wait()
		if len(errs) > 0 {
			return stopped, errors.Join(errs...)
		}
	}
	return stopped, nil
}

// reload serves "reload": it re-reads config.yaml and every task file.
// Tasks are validated against the config the daemon runs with; a changed
// config.yaml is reported, and applies after a restart.
func (d *daemon) reload(ctx context.Context, _ json.RawMessage) (any, error) {
	cfg, err := config.Load(d.configFile)
	if err != nil {
		return nil, &api.Error{Code: api.CodeInternalError, Message: "config.yaml: " + err.Error()}
	}
	entries, err := config.LoadTasks(d.configDir, d.cfg)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	old := map[string]string{}
	for _, e := range d.tasks {
		old[e.ID] = d.versions[e.ID]
	}
	d.tasks = entries
	if err := d.sched.SetTasks(ctx, d.tasks); err != nil {
		d.log.Error("cannot reschedule tasks", "err", err)
	}
	res := api.ReloadResult{Tasks: make([]api.TaskSummary, 0, len(entries)), RestartRequired: !reflect.DeepEqual(cfg, d.cfg)}
	for i, e := range entries {
		v := d.fileVersion(e)
		d.versions[e.ID] = v
		if prev, ok := old[e.ID]; !ok || prev != v {
			logTask(d.log, e)
			d.publishTaskLocked(ctx, i)
		}
		delete(old, e.ID)
		s, err := d.summary(ctx, e)
		if err != nil {
			return nil, err
		}
		res.Tasks = append(res.Tasks, s)
	}
	for id := range old {
		delete(d.versions, id)
		d.publishDeleted(id)
	}
	d.log.Info("reloaded", "tasks", len(entries), "restart_required", res.RestartRequired)
	return res, nil
}

// reloadOne re-reads one task file after a change on disk (a hand edit, a
// removal, a prompt edit). Nothing happens when its files are unchanged,
// which is the case after the daemon's own writes.
func (d *daemon) reloadOne(ctx context.Context, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reloadOneLocked(ctx, id)
}

// reloadOneLocked is reloadOne with d.mu held.
func (d *daemon) reloadOneLocked(ctx context.Context, id string) {
	// id comes from a file name or an API call: keep it inside tasks/.
	if id == "" || strings.HasPrefix(id, ".") || strings.ContainsAny(id, `/\`) || d.deleting[id] {
		return
	}
	i := d.find(id)
	path := filepath.Join(d.tasksDir(), id+".yaml")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if i < 0 {
			return
		}
		d.tasks = slices.Delete(d.tasks, i, i+1)
		delete(d.versions, id)
		if err := d.sched.SetTasks(ctx, d.tasks); err != nil {
			d.log.Error("cannot reschedule tasks", "err", err)
		}
		d.log.Info("task file removed", logging.KeyTaskID, id)
		d.publishDeleted(id)
		return
	}
	e := config.LoadTask(d.configDir, id, d.cfg)
	v := d.fileVersion(e)
	if i >= 0 && d.versions[id] == v {
		return
	}
	if i >= 0 {
		d.tasks[i] = e
	} else {
		d.tasks = append(d.tasks, e)
		slices.SortFunc(d.tasks, func(a, b config.TaskEntry) int { return strings.Compare(a.ID, b.ID) })
	}
	d.versions[id] = v
	d.log.Info("task file changed on disk", logging.KeyTaskID, id)
	logTask(d.log, e)
	if err := d.sched.SetTasks(ctx, d.tasks); err != nil {
		d.log.Error("cannot reschedule tasks", "err", err)
	}
	d.publishTaskLocked(ctx, d.find(id))
}

// currentTask is the dispatcher's view of a task when a run starts: the
// definition as it is now, so an edit applies to the next run, also to a
// fire that was queued before the edit.
func (d *daemon) currentTask(t config.Task) (config.Task, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.find(t.ID)
	switch {
	case i < 0:
		return config.Task{}, fmt.Errorf("task %s was deleted", t.ID)
	case d.tasks[i].Err != nil:
		return config.Task{}, fmt.Errorf("task %s is invalid: %v", t.ID, d.tasks[i].Err)
	}
	return d.tasks[i].Task, nil
}

// taskDef is the API form of a task.
func taskDef(t config.Task) api.TaskDef {
	def := api.TaskDef{
		ID: t.ID, Name: t.Name, Target: t.Target, Cwd: t.Cwd, Schedule: t.Schedule, Timezone: t.Timezone,
		Enabled: t.Enabled, PausedUntil: t.PausedUntil,
		Agent:      api.AgentDef{Command: t.Agent.Command, Args: t.Agent.Args},
		PromptFile: t.PromptFile, Overlap: t.Overlap, Missed: t.Missed, OnDone: t.OnDone, OnBlocked: t.OnBlocked,
		Env: t.Env,
	}
	if t.Timeout.Duration > 0 {
		def.Timeout = config.FormatDuration(t.Timeout.Duration)
	}
	return def
}

// taskFromDef is the task an API definition describes. A timeout that
// does not parse is returned as an error, with the timeout left zero.
func taskFromDef(def api.TaskDef) (config.Task, error) {
	t := config.Task{
		ID: def.ID, Name: def.Name, Target: def.Target, Cwd: def.Cwd, Schedule: def.Schedule, Timezone: def.Timezone,
		Enabled: def.Enabled, PausedUntil: def.PausedUntil,
		Agent:      config.Agent{Command: def.Agent.Command, Args: def.Agent.Args},
		PromptFile: def.PromptFile, Overlap: def.Overlap, Missed: def.Missed, OnDone: def.OnDone, OnBlocked: def.OnBlocked,
		Env: def.Env,
	}
	if t.PromptFile == "" {
		t.PromptFile = config.DefaultTask(t.ID).PromptFile
	}
	if def.Timeout == "" {
		return t, nil
	}
	timeout, err := time.ParseDuration(def.Timeout)
	if err != nil {
		return t, fmt.Errorf("%q is not a Go duration (for example 2h or 30m)", def.Timeout)
	}
	t.Timeout = config.Duration{Duration: timeout}
	return t, nil
}
