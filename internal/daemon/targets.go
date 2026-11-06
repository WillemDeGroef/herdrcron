package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
)

// targetStatus is the API view of a target: its reachability from the
// runner and its last "targets.check" report.
func (d *daemon) targetStatus(name string) api.TargetStatus {
	ts := api.TargetStatus{Name: name, Reachability: api.ReachUnknown}
	if st, ok := d.runner.TargetState(name); ok {
		ts.Reachability = st.Reachability
		ts.UnreachableSince = rfc3339(st.UnreachableSince)
		ts.NextRetryAt = rfc3339(st.NextRetryAt)
		ts.LastError = st.LastError
		ts.LastSeenAt = rfc3339(st.LastSeenAt)
	}
	d.checkMu.Lock()
	if c, ok := d.lastChecks[name]; ok {
		ts.LastCheck = &c
	}
	d.checkMu.Unlock()
	return ts
}

// rfc3339 formats t, or "" for the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// targetChanged is the runner's OnTargetChange: it publishes
// "target.changed", notifies target_unreachable (if notify.on lists it),
// and when the target answers again applies the missed policy to the
// fires it missed.
func (d *daemon) targetChanged(st runner.TargetState) {
	d.bus.publish(api.EventTargetChanged, api.TargetChangedEvent{Target: d.targetStatus(st.Name)})
	switch st.Reachability {
	case runner.ReachUnreachable:
		d.notifier.Notify(config.EventTargetUnreachable,
			fmt.Sprintf("herdrcron: target %s unreachable: %s", st.Name, st.LastError), nil)
	case runner.ReachReachable:
		d.catchUp(st.Name)
	}
}

// setCtx sets the context of background work started from callbacks.
func (d *daemon) setCtx(ctx context.Context) {
	d.checkMu.Lock()
	d.ctx = ctx
	d.checkMu.Unlock()
}

// stopCatchUps makes catchUp do nothing from now on, so bg.Wait cannot
// race a bg.Add.
func (d *daemon) stopCatchUps() {
	d.checkMu.Lock()
	d.ctx = nil
	d.checkMu.Unlock()
}

// catchUp starts, in the background, the catch_up runs of the tasks on
// target whose last fire found it unreachable. Before the daemon serves
// (during Recover) and once it is stopping it does nothing: Run calls it
// again once the scheduler has its tasks.
func (d *daemon) catchUp(target string) {
	d.checkMu.Lock()
	defer d.checkMu.Unlock()
	ctx := d.ctx
	if ctx == nil || ctx.Err() != nil {
		return
	}
	d.bg.Add(1) // under checkMu, so it cannot follow Run's setCtx(nil)
	go func() {
		defer d.bg.Done()
		d.sched.CatchUpTarget(ctx, target)
	}()
}

// checkTimeout bounds one "targets.check".
const checkTimeout = 60 * time.Second

// targetsCheck serves "targets.check": the preflight report of one target
// for the valid tasks on it. The report is kept as the target's last
// check and published with "target.changed".
func (d *daemon) targetsCheck(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.TargetCheckParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, api.InvalidParams(errors.New("name is required"))
	}
	if _, ok := d.cfg.Targets[p.Name]; !ok {
		return nil, api.InvalidParams(fmt.Errorf("unknown target %q", p.Name))
	}
	d.mu.Lock()
	var tasks []config.Task
	for _, e := range d.tasks {
		if e.Err == nil && e.Task.Target == p.Name {
			tasks = append(tasks, e.Task)
		}
	}
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	c, err := d.runner.CheckTarget(ctx, p.Name, tasks)
	if err != nil {
		return nil, api.InvalidParams(err)
	}
	res := api.TargetCheck{
		Target: c.Target, CheckedAt: rfc3339(c.CheckedAt), OK: c.OK,
		HerdrVersion: c.HerdrVersion, WantHerdrVersion: c.WantHerdrVersion,
		Items: make([]api.TargetCheckItem, 0, len(c.Items)),
	}
	for _, it := range c.Items {
		res.Items = append(res.Items, api.TargetCheckItem{Name: it.Name, Result: it.Result, Detail: it.Detail, Tasks: it.Tasks})
	}
	d.checkMu.Lock()
	d.lastChecks[p.Name] = res
	d.checkMu.Unlock()
	d.bus.publish(api.EventTargetChanged, api.TargetChangedEvent{Target: d.targetStatus(p.Name)})
	return res, nil
}
