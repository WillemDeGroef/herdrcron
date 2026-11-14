package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
)

// reload serves "reload": it re-reads config.yaml and every task file.
//
// A changed config.yaml is applied to the running daemon only after the
// task files have been read against the config that would result. A
// failure to read them leaves the running config as it is. The one part
// that can wait is a target with active runs: see api.ReloadResult. A
// config.yaml that does not parse changes nothing in the running config;
// the task files still reload, and the call then fails with that error.
//
// The files are read without d.mu (readForReload) and the result is
// applied with it (applyReadLocked). A config directory on a filesystem
// that stopped answering, a network mount for one, would otherwise hold
// d.mu for as long as the read hangs, and with it every API call that
// lists or changes tasks, the start of every run (currentTask) and the
// task.changed events: the daemon would look dead to the CLI and the TUI
// and miss its fires. A read that hangs holds up only its own reload.
//
// What happened during the read can make it out of date: a task written
// through the API, a hand edit the watcher reloaded, another reload, a run
// that started or ended on a target config.yaml changes. applyReadLocked
// then applies nothing and the files are read again.
func (d *daemon) reload(ctx context.Context, _ json.RawMessage) (any, error) {
	res, err := d.reloadFiles(ctx)
	// A higher max_parallel leaves room for fires that waited. Not under
	// d.mu: a run that starts calls back into the daemon.
	d.disp.Drain(ctx)
	return res, err
}

// reloadAttempts is how often one "reload" reads the files before it
// gives up. Each further attempt needs another change to land during the
// read before it, so this is not reached in practice; the bound keeps a
// reload from spinning if it ever is.
const reloadAttempts = 5

// reloadFiles is reload without the drain. The result is nil when err is
// not.
func (d *daemon) reloadFiles(ctx context.Context) (any, error) {
	for range reloadAttempts {
		read, err := d.readForReload()
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		res, applied, err := d.applyReadLocked(ctx, read)
		d.mu.Unlock()
		if applied {
			return res, err
		}
		d.log.Info("reload read out of date, reading again")
	}
	return nil, &api.Error{Code: api.CodeInternalError,
		Message: "reload: tasks and runs kept changing while the files were read; try again"}
}

// reloadRead is config.yaml and the task files as one "reload" read them,
// with what the daemon looked like when the read began.
type reloadRead struct {
	// cur is the running config and gen is d.tasksGen when the read began.
	// The read is applied only if gen still holds: no task and no config
	// has changed in the daemon since.
	cur config.Config
	gen uint64

	// next is config.yaml as read and cfgErr why it could not be. replace
	// is set when next is to take the place of cur.
	next    config.Config
	cfgErr  error
	replace bool
	// deferred are the targets that would keep their old settings, as the
	// runner said during the read. resulting is the config the daemon runs
	// with once the read is applied: next with those targets as cur has
	// them, or cur itself when replace is not set. The entries were
	// validated against it.
	deferred  []string
	resulting config.Config

	entries []config.TaskEntry
	// entryVersions[i] is the version of the bytes entries[i] was parsed
	// from, not a later read of the file.
	entryVersions []string
}

// readForReload reads config.yaml and the task files. It changes nothing
// in the daemon and must be called without d.mu: this is the part of a
// reload that can hang on a slow filesystem.
func (d *daemon) readForReload() (reloadRead, error) {
	d.mu.Lock()
	r := reloadRead{cur: d.config(), gen: d.tasksGen}
	d.mu.Unlock()

	r.resulting = r.cur
	r.next, r.cfgErr = config.Load(d.configFile)
	if r.cfgErr == nil && !reflect.DeepEqual(r.next, r.cur) {
		r.replace = true
		r.deferred = d.runner.WouldDefer(r.next)
		r.resulting = overlayConfig(r.cur, r.next, r.deferred)
	}
	var err error
	if d.loadTasks != nil {
		// Test stand-in. It returns the entries; the version is whatever
		// the files hold once it returns. Production hashes the bytes it
		// parsed (SnapshotTasks) so a write during the read cannot be
		// stored under the new hash: the watcher would then skip it.
		if r.entries, err = d.loadTasks(d.configDir, r.resulting); err != nil {
			return reloadRead{}, err
		}
		r.entryVersions = make([]string, len(r.entries))
		for i, e := range r.entries {
			r.entryVersions[i] = d.fileVersion(e)
		}
		return r, nil
	}
	if r.entries, r.entryVersions, err = config.SnapshotTasks(d.configDir, r.resulting); err != nil {
		return reloadRead{}, err
	}
	return r, nil
}

// applyReadLocked makes r the daemon's tasks and, when config.yaml
// changed, its config, all under one hold of d.mu: nobody sees the new
// config with the old tasks or the other way round. Every setting applies
// at once, except a target with active runs whose host, shell or
// herdr_path changed, or that is gone from config.yaml: it keeps its old
// settings (only max_parallel follows) and is named in the result.
//
// applied is false, and nothing has changed, when r is out of date: a
// task or the config changed in the daemon after the read began (tasksGen
// moved), or the runner would no longer defer the targets r was validated
// for. The caller reads again. It reads no config or task file; the caller
// holds d.mu.
func (d *daemon) applyReadLocked(ctx context.Context, r reloadRead) (res any, applied bool, err error) {
	if d.tasksGen != r.gen {
		return nil, false, nil
	}
	if r.replace {
		// The last check, and the one step that can still refuse: after it
		// the runner has the new targets and the rest must follow.
		if _, ok := d.runner.ReconfigureIf(r.next, d.newTarget, r.deferred); !ok {
			return nil, false, nil
		}
		d.installConfig(r.cur, r.resulting)
	}
	old := d.versions
	d.tasks = r.entries
	d.versions = make(map[string]string, len(r.entries))
	d.tasksGen++
	d.rescheduleLocked(ctx)
	out := api.ReloadResult{
		Tasks:           make([]api.TaskSummary, 0, len(r.entries)),
		RestartRequired: len(r.deferred) > 0, DeferredTargets: r.deferred,
	}
	// Versions go in before the events. Publish every change before building
	// the response: a summary error must not skip one, because the versions
	// are already the new ones and the next reload would not send it either.
	for i, e := range r.entries {
		d.versions[e.ID] = r.entryVersions[i]
	}
	for i, e := range r.entries {
		if prev, ok := old[e.ID]; !ok || prev != r.entryVersions[i] {
			logTask(d.log, e)
			d.publishTaskLocked(ctx, i)
		}
	}
	for id := range old {
		if _, ok := d.versions[id]; !ok {
			d.publishDeleted(id)
		}
	}
	for _, e := range r.entries {
		s, err := d.summary(ctx, e)
		if err != nil {
			return nil, true, err
		}
		out.Tasks = append(out.Tasks, s)
	}
	d.log.Info("reloaded", "tasks", len(r.entries), "restart_required", out.RestartRequired, "deferred_targets", r.deferred)
	if r.cfgErr != nil {
		d.log.Error("config.yaml not applied", "err", r.cfgErr)
		return nil, true, &api.Error{Code: api.CodeInternalError, Message: "config.yaml: " + r.cfgErr.Error()}
	}
	return out, true, nil
}

// overlayConfig is next with each deferred target put back as cur had it,
// except max_parallel, which follows next when the target is still there.
func overlayConfig(cur, next config.Config, deferred []string) config.Config {
	running := next
	running.Targets = maps.Clone(next.Targets)
	if running.Targets == nil {
		running.Targets = map[string]config.Target{}
	}
	for _, name := range deferred {
		kept := cur.Targets[name]
		if t, ok := next.Targets[name]; ok {
			kept.MaxParallel = t.MaxParallel
		}
		running.Targets[name] = kept
	}
	return running
}

// installConfig publishes running to the daemon's collaborators. The runner
// already has it (ReconfigureIf). The caller holds d.mu.
func (d *daemon) installConfig(cur, running config.Config) {
	d.cfgMu.Lock()
	d.cfg = running
	d.cfgMu.Unlock()
	d.notifier.SetConfig(running.Notify)
	d.sched.SetConfig(running)
	d.disp.SetConfig(running)
	removed := removedTargets(cur, running)
	d.disp.DropTargets(removed)

	// A removed or replaced target loses its last check: the report was
	// about another machine or another herdr. Bump the epoch too, so a
	// check that started on the old machine cannot store its report after
	// this returns.
	d.checkMu.Lock()
	for name, t := range cur.Targets {
		if nt, ok := running.Targets[name]; !ok || !sameMachine(nt, t) {
			delete(d.lastChecks, name)
			d.checkEpoch[name]++
		}
	}
	d.checkMu.Unlock()
	// A new or replaced target starts over as unknown; tell subscribers.
	for _, name := range d.targetNames() {
		if before, ok := cur.Targets[name]; !ok || before != running.Targets[name] {
			d.bus.publish(api.EventTargetChanged, api.TargetChangedEvent{Target: d.targetStatus(name)})
		}
	}
	d.log.Info("config applied", "targets", d.targetNames(), "removed_targets", removed)
}

// removedTargets are the names in cur that running no longer has.
func removedTargets(cur, running config.Config) []string {
	var names []string
	for name := range cur.Targets {
		if _, ok := running.Targets[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// sameMachine reports whether a and b differ in max_parallel at most.
func sameMachine(a, b config.Target) bool {
	a.MaxParallel, b.MaxParallel = 0, 0
	return a == b
}
