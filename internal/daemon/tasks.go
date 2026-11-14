package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
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
	prompt, err := os.ReadFile(d.promptPathOf(e))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return api.TaskDetail{}, fmt.Errorf("read prompt of %s: %w", e.ID, err)
	}
	det.Prompt = string(prompt)
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
	for _, field := range slices.Sorted(maps.Keys(f)) {
		parts = append(parts, field+": "+strings.Join(f[field], ", "))
	}
	return &api.Error{Code: api.CodeValidation, Message: "task is invalid: " + strings.Join(parts, "; "),
		Data: api.ValidationErrorData{Fields: f}}
}

// draft turns write params into a task and checks it and its prompt text,
// without the prompt file having to exist. Problems are collected by field.
func (d *daemon) draft(def api.TaskDef, prompt string) (config.Task, fieldErrors) {
	fe := fieldErrors{}
	if problem := config.CheckPromptSize(int64(len(prompt))); problem != "" {
		fe.add("prompt", problem)
	}
	t, timeoutErr := taskFromDef(def)
	if timeoutErr != nil {
		fe.add("timeout", timeoutErr.Error())
	}
	var verr *config.ValidationError
	if err := config.ValidateDraft(t, d.configDir, d.config()); errors.As(err, &verr) {
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
	if t.PromptFile != "" && !d.promptInside(config.PromptPath(t.PromptFile, d.configDir)) {
		fe.add("prompt_file", "prompt_file must be inside the config directory")
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
	t, fe := d.draft(p.Task, p.Prompt)
	path := d.taskPath(t.ID)
	if t.ID != "" && len(fe["id"]) == 0 {
		if _, err := os.Lstat(path); d.find(t.ID) >= 0 || err == nil {
			fe.add("id", fmt.Sprintf("task %q already exists", t.ID))
		}
	}
	if err := fe.err(); err != nil {
		return nil, err
	}
	promptPath := config.PromptPath(t.PromptFile, d.configDir)
	if !d.promptInside(promptPath) {
		return nil, fmt.Errorf("write prompt file: prompt_file must be inside the config directory")
	}
	_, statErr := os.Stat(promptPath)
	newPrompt := errors.Is(statErr, fs.ErrNotExist)
	// The prompt first: a task file that appears without it is invalid.
	if err := d.noteWrite("prompt file", t.ID, config.WritePrompt(promptPath, p.Prompt)); err != nil {
		return nil, fmt.Errorf("write prompt file: %w", err)
	}
	if err := d.noteWrite("task file", t.ID, config.WriteTask(path, t, true)); err != nil {
		if newPrompt {
			_ = os.Remove(promptPath)
		}
		if errors.Is(err, fs.ErrExist) {
			fe.add("id", fmt.Sprintf("task %q already exists", t.ID))
			return nil, fe.err()
		}
		return nil, fmt.Errorf("write task file: %w", err)
	}
	e := config.LoadTask(d.configDir, t.ID, d.config())
	d.insertLocked(e)
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
	// Before the task file is rewritten: the prompt path to drop is what the
	// task file on disk names now, not the in-memory entry (promptPathOf
	// falls back when the load had a syntax error).
	oldPrompt, oldPromptKnown := d.promptPathInFile(old)
	// Compare with the files, not memory: a hand edit the watcher has not
	// reloaded yet must still refuse the write.
	if cur := d.fileVersion(old); p.Version == "" || p.Version != cur {
		return nil, &api.Error{Code: api.CodeVersionMismatch,
			Message: fmt.Sprintf("task %s changed since it was read; read it again", p.ID)}
	}
	t, fe := d.draft(p.Task, p.Prompt)
	if t.ID != p.ID {
		fe["id"] = []string{fmt.Sprintf("id cannot change (task %q)", p.ID)}
	}
	if err := fe.err(); err != nil {
		return nil, err
	}
	promptPath := config.PromptPath(t.PromptFile, d.configDir)
	if !d.promptInside(promptPath) {
		return nil, fmt.Errorf("write prompt file: prompt_file must be inside the config directory")
	}
	if err := d.noteWrite("prompt file", p.ID, config.WritePrompt(promptPath, p.Prompt)); err != nil {
		return nil, fmt.Errorf("write prompt file: %w", err)
	}
	if err := d.noteWrite("task file", p.ID, config.WriteTask(old.File, t, false)); err != nil {
		return nil, fmt.Errorf("write task file: %w", err)
	}
	e := config.LoadTask(d.configDir, p.ID, d.config())
	d.tasks[i] = e
	// The task points at its new prompt file now; the old one goes unless
	// it is still in use. When the old path is unknown, nothing is removed.
	if oldPromptKnown {
		d.removeUnusedPromptLocked(p.ID, oldPrompt)
	}
	d.log.Info("task updated", logging.KeyTaskID, e.ID)
	return d.storedLocked(ctx, e)
}

// storedLocked finishes a create or update: version, schedule, event and
// result. The edit applies to the next run; active runs keep the task they
// started with.
func (d *daemon) storedLocked(ctx context.Context, e config.TaskEntry) (api.TaskDetail, error) {
	logTask(d.log, e)
	d.versions[e.ID] = d.fileVersion(e)
	d.rescheduleLocked(ctx)
	d.publishTaskLocked(ctx, d.find(e.ID))
	return d.detailLocked(ctx, e)
}

func (d *daemon) tasksDelete(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.DeleteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.InvalidParams(errors.New("id is required"))
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
	d.rescheduleLocked(ctx)
	d.mu.Unlock()

	// Stopping takes seconds and ends runs, which calls back into the
	// dispatcher and this daemon: never hold d.mu here.
	res := api.DeleteResult{ID: p.ID, StoppedRuns: []api.RunSummary{}}
	stopped, err := d.stopRunsOf(ctx, p.ID)
	for _, r := range stopped {
		res.StoppedRuns = append(res.StoppedRuns, *runSummary(r))
	}
	if err != nil {
		d.restoreSchedule(ctx, p.ID)
		return nil, fmt.Errorf("stop active runs of %s: %w", p.ID, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	i = d.find(p.ID)
	if i < 0 {
		return res, nil // a reload removed it meanwhile
	}
	e := d.tasks[i]
	promptPath, promptKnown := d.promptPathInFile(e)
	if err := os.Remove(e.File); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.rescheduleLocked(ctx)
		return nil, fmt.Errorf("remove task file: %w", err)
	}
	d.tasks = slices.Delete(d.tasks, i, i+1)
	delete(d.versions, p.ID)
	if promptKnown {
		d.removeUnusedPromptLocked(p.ID, promptPath)
	}
	if err := d.store.DeleteTaskState(ctx, p.ID); err != nil {
		d.log.Warn("cannot remove task state", logging.KeyTaskID, p.ID, "err", err)
	}
	d.log.Info("task deleted", logging.KeyTaskID, p.ID, "stopped_runs", len(res.StoppedRuns))
	d.publishDeleted(p.ID)
	return res, nil
}

// removeUnusedPromptLocked removes the prompt file task id used until now,
// after a delete or after an update moved the task to another prompt_file.
// The file stays when a task file still names it, by path or as the same
// file through a link, when a task file does not parse (its prompt_file is
// unknown), and when it is outside the config directory. Task files are
// read from disk, so a hand edit the watcher has not reloaded yet still
// counts. A failure to remove is logged: the task itself is already
// stored. Call it with d.mu held and d.tasks up to date.
func (d *daemon) removeUnusedPromptLocked(id, promptPath string) {
	if !d.inConfigDir(promptPath) {
		return
	}
	info, statErr := os.Stat(promptPath)
	inUse := slices.ContainsFunc(d.tasks, func(o config.TaskEntry) bool {
		other, known := d.promptPathInFile(o)
		if !known {
			return true
		}
		if filepath.Clean(other) == filepath.Clean(promptPath) {
			return true
		}
		otherInfo, otherErr := os.Stat(other)
		return statErr == nil && otherErr == nil && os.SameFile(info, otherInfo)
	})
	if inUse {
		return
	}
	if err := os.Remove(promptPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.log.Warn("cannot remove prompt file", logging.KeyTaskID, id, "err", err)
	}
}

// promptPathInFile is the prompt path the task file names on disk.
// known is false when that file does not parse, so its prompt_file cannot
// be told. A type error still carries the fields that parsed, which
// promptPathOf does not: it falls back to prompts/<id>.md whenever the
// lenient read returns an error, and would both invent a path for a syntax
// error and hide the prompt_file a bad field left intact.
func (d *daemon) promptPathInFile(e config.TaskEntry) (string, bool) {
	t, err := config.ReadTaskLenient(e.File)
	if err != nil && t.PromptFile == "" {
		return "", false
	}
	if t.PromptFile == "" {
		t.PromptFile = config.DefaultTask(e.ID).PromptFile
	}
	return config.PromptPath(t.PromptFile, d.configDir), true
}

// restoreSchedule schedules a task again after its delete gave up.
func (d *daemon) restoreSchedule(ctx context.Context, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.deleting, id)
	d.rescheduleLocked(ctx)
}

// rescheduleLocked hands the scheduler every task but those being deleted.
// SetTasks keeps the next fire of an unchanged schedule. A failure is
// logged: the files are written, and the next reload schedules again.
func (d *daemon) rescheduleLocked(ctx context.Context) {
	tasks := slices.DeleteFunc(slices.Clone(d.tasks), func(e config.TaskEntry) bool { return d.deleting[e.ID] })
	if err := d.sched.SetTasks(ctx, tasks); err != nil {
		d.log.Error("cannot reschedule tasks", "err", err)
	}
}

// insertLocked adds a new entry to d.tasks, which stays sorted by id.
func (d *daemon) insertLocked(e config.TaskEntry) {
	d.tasks = append(d.tasks, e)
	slices.SortFunc(d.tasks, func(a, b config.TaskEntry) int { return strings.Compare(a.ID, b.ID) })
}

// taskPath is the task file of id.
func (d *daemon) taskPath(id string) string { return filepath.Join(d.tasksDir(), id+".yaml") }

// inConfigDir reports whether path is a file of the config directory:
// path-wise inside it, and still inside once the symlinks in its directory
// are followed. The API only removes prompt files there. The last
// component is not followed: os.Remove removes a link, not its target, so
// a link inside the directory is removed, and a path that climbs out or
// passes through a linked directory to the outside is not.
func (d *daemon) inConfigDir(path string) bool {
	if !withinDir(d.configDir, path) {
		return false
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(d.configDir)
	if err != nil {
		root = d.configDir
	}
	return withinDir(root, dir)
}

// promptInside reports whether a prompt path stays inside the config
// directory after symlinks are followed. A missing file is judged from
// its nearest existing ancestor, so a symlinked directory cannot place a
// new file outside. Writes use this; a link whose target is outside is
// refused rather than followed.
func (d *daemon) promptInside(path string) bool {
	resolved, err := resolvePath(path)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(d.configDir)
	if err != nil {
		root = d.configDir
	}
	return withinDir(root, resolved)
}

// withinDir reports whether path is path-wise inside dir.
func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath is path with symlinks followed. When the last component does
// not exist yet, the existing ancestor is resolved and the missing suffix
// is joined back on.
func resolvePath(path string) (string, error) {
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return filepath.EvalSymlinks(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	var missing []string
	cur := path
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fs.ErrNotExist
		}
		missing = append(missing, filepath.Base(cur))
		_, err := os.Lstat(parent)
		if errors.Is(err, fs.ErrNotExist) {
			cur = parent
			continue
		}
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", err
		}
		for i := len(missing) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, missing[i])
		}
		return resolved, nil
	}
}

// noteWrite treats a directory-sync failure after a successful replace as
// success: the new bytes are already at the path. The failure is logged,
// as it is for pause and enable. Any other error is returned.
func (d *daemon) noteWrite(what, id string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, config.ErrReplaced) {
		d.log.Error(what+" replaced but directory sync failed", logging.KeyTaskID, id, "err", err)
		return nil
	}
	return err
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
	path := d.taskPath(id)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if i < 0 {
			return
		}
		d.tasks = slices.Delete(d.tasks, i, i+1)
		delete(d.versions, id)
		d.rescheduleLocked(ctx)
		d.log.Info("task file removed", logging.KeyTaskID, id)
		d.publishDeleted(id)
		return
	}
	e := config.LoadTask(d.configDir, id, d.config())
	v := d.fileVersion(e)
	if i >= 0 && d.versions[id] == v {
		return
	}
	if i >= 0 {
		d.tasks[i] = e
	} else {
		d.insertLocked(e)
	}
	d.versions[id] = v
	d.log.Info("task file changed on disk", logging.KeyTaskID, id)
	logTask(d.log, e)
	d.rescheduleLocked(ctx)
	d.publishTaskLocked(ctx, d.find(id))
}

// currentTask is the dispatcher's view of a task when a run starts: the
// definition as it is now, so an edit applies to the next run, also to a
// fire that was queued before the edit.
//
// The prompt is read here, under d.mu, so it belongs to that definition: an
// update or delete that lands while the run starts may replace or remove
// the prompt file, and the run still has its text. promptErr is why the
// file could not be read; the task is still returned.
func (d *daemon) currentTask(t config.Task) (cur config.Task, prompt []byte, promptErr, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.find(t.ID)
	switch {
	case i < 0 || d.deleting[t.ID]:
		return config.Task{}, nil, nil, fmt.Errorf("task %s was deleted", t.ID)
	case d.tasks[i].Err != nil:
		return config.Task{}, nil, nil, fmt.Errorf("task %s is invalid: %v", t.ID, d.tasks[i].Err)
	}
	cur = d.tasks[i].Task
	prompt, promptErr = os.ReadFile(config.PromptPath(cur.PromptFile, d.configDir))
	return cur, prompt, promptErr, nil
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
