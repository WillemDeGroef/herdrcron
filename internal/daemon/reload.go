package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
)

// reload serves "reload": it re-reads config.yaml and every task file.
//
// A changed config.yaml is applied to the running daemon (applyConfig),
// and the tasks are validated against the result. The one part that can
// wait is a target with active runs: see api.ReloadResult. A config.yaml
// that does not parse changes nothing in the running config; the task
// files still reload, and the call then fails with that error.
func (d *daemon) reload(ctx context.Context, _ json.RawMessage) (any, error) {
	d.mu.Lock()
	res, err := d.reloadLocked(ctx)
	d.mu.Unlock()
	// A higher max_parallel leaves room for fires that waited. Not under
	// d.mu: a run that starts calls back into the daemon.
	d.disp.Drain(ctx)
	return res, err
}

// reloadLocked is reload with d.mu held, without the drain. The result is
// nil when err is not.
func (d *daemon) reloadLocked(ctx context.Context) (any, error) {
	cfg := d.config()
	var deferred []string
	fileCfg, cfgErr := config.Load(d.configFile)
	if cfgErr == nil && !reflect.DeepEqual(fileCfg, cfg) {
		cfg, deferred = d.applyConfig(cfg, fileCfg)
	}
	entries, err := config.LoadTasks(d.configDir, cfg)
	if err != nil {
		return nil, err
	}
	old := map[string]string{}
	for _, e := range d.tasks {
		old[e.ID] = d.versions[e.ID]
	}
	d.tasks = entries
	d.rescheduleLocked(ctx)
	res := api.ReloadResult{
		Tasks:           make([]api.TaskSummary, 0, len(entries)),
		RestartRequired: len(deferred) > 0, DeferredTargets: deferred,
	}
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
	d.log.Info("reloaded", "tasks", len(entries), "restart_required", res.RestartRequired, "deferred_targets", deferred)
	if cfgErr != nil {
		d.log.Error("config.yaml not applied", "err", cfgErr)
		return nil, &api.Error{Code: api.CodeInternalError, Message: "config.yaml: " + cfgErr.Error()}
	}
	return res, nil
}

// applyConfig makes next, config.yaml as just loaded, the running config
// in place of cur, and returns the config the daemon runs with from now
// on. Every setting applies at once, except a target with active runs
// whose host, shell or herdr_path changed, or that is gone from next: it
// keeps its old settings (only max_parallel follows next) and is named in
// deferred. The caller holds d.mu and reschedules the tasks afterwards, so
// a new timezone reaches their next fire.
func (d *daemon) applyConfig(cur, next config.Config) (running config.Config, deferred []string) {
	deferred = d.runner.Reconfigure(next, d.newTarget)
	running = next
	running.Targets = maps.Clone(next.Targets)
	for _, name := range deferred {
		kept := cur.Targets[name]
		if t, ok := next.Targets[name]; ok {
			kept.MaxParallel = t.MaxParallel
		}
		running.Targets[name] = kept
	}
	d.cfgMu.Lock()
	d.cfg = running
	d.cfgMu.Unlock()
	d.notifier.SetConfig(running.Notify)
	d.sched.SetConfig(running)
	d.disp.SetConfig(running)

	// A removed or replaced target loses its last check: the report was
	// about another machine or another herdr.
	d.checkMu.Lock()
	for name := range d.lastChecks {
		if t, ok := running.Targets[name]; !ok || !sameMachine(t, cur.Targets[name]) {
			delete(d.lastChecks, name)
		}
	}
	d.checkMu.Unlock()
	// A new or replaced target starts over as unknown; tell subscribers.
	for _, name := range d.targetNames() {
		if before, ok := cur.Targets[name]; !ok || before != running.Targets[name] {
			d.bus.publish(api.EventTargetChanged, api.TargetChangedEvent{Target: d.targetStatus(name)})
		}
	}
	d.log.Info("config applied", "targets", d.targetNames(), "deferred_targets", deferred)
	return running, deferred
}

// sameMachine reports whether a and b differ in max_parallel at most.
func sameMachine(a, b config.Target) bool {
	a.MaxParallel, b.MaxParallel = 0, 0
	return a == b
}
