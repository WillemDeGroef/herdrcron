// Package dispatch sits between the fire sources (the scheduler and the
// run_now API) and the runner. It applies each task's overlap mode and each
// target's max_parallel.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// Runner is what the dispatcher starts runs with; runner.Runner implements it.
type Runner interface {
	// Start stores and launches a run. A run that fails to launch comes
	// back ended; the runner then has already called RunEnded for it.
	Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error)
	// ActiveRuns returns every tracked, not-ended run on any target,
	// recovered runs included. A run is gone from it by the time the
	// runner calls RunEnded for it.
	ActiveRuns() []store.Run
}

// Options configures New.
type Options struct {
	Runs Runner
	// Store records deferred schedule and catch_up fires and keeps the
	// waiting fires for Restore. It may be nil: nothing is stored then.
	Store  *store.Store
	Config config.Config
	Log    *slog.Logger
	// Now gives the time a dropped fire's history row carries. It may be
	// nil: time.Now then.
	Now func() time.Time
	// OnDropped is called with the history row of each fire that was
	// dropped, after the row is stored. It may be nil. It can be called
	// with the dispatcher's lock held: it must not call the Dispatcher.
	OnDropped func(store.Run)
}

// ErrActive is returned by Submit for a manual fire of an overlap skip task
// that is busy, unless the fire forces a parallel run.
var ErrActive = errors.New("task has an active run")

// DefaultMaxParallel applies when a target has no max_parallel.
const DefaultMaxParallel = 4

// Outcome is what Submit did with a fire. At most one of its parts is set.
type Outcome struct {
	// Run is the run that started now, or the history row of a fire that
	// was dropped (status dropped). It may come back ended (a failed launch).
	Run store.Run
	// Queued: the fire waits, as the task's one pending overlap fire or in
	// its target's max_parallel queue.
	Queued bool
	// Skipped: overlap skip, or a queue fire dropped because one is already
	// pending. Nothing was recorded.
	Skipped bool
	// Neither set and no error: the fire was dropped and its history row
	// could not be stored. It is not queued.
}

// fire is one request to start a run of a task.
type fire struct {
	task        config.Task
	trigger     string
	scheduledAt time.Time
	// id is the fire's row in the store while it waits; 0 if it has none.
	id int64
}

// Dispatcher applies overlap and max_parallel. It is safe for concurrent use.
// It never calls Runs.Start while holding its mutex, because the runner calls
// RunEnded synchronously, also from inside Start.
type Dispatcher struct {
	runs      Runner
	store     *store.Store
	log       *slog.Logger
	now       func() time.Time
	onDropped func(store.Run)
	// stopCtx is cancelled by Stop. drain takes no fire out of the store
	// under a cancelled stopCtx.
	stopCtx    context.Context
	cancelStop context.CancelFunc

	mu  sync.Mutex
	cfg config.Config // max_parallel per target
	// gen counts finished Start calls and RunEnded calls. A snapshot of
	// ActiveRuns taken while gen stayed the same agrees with the launching
	// counts, and no run ended without seeing the state built on it.
	gen           uint64
	launching     map[string]int    // target -> Start calls in flight
	launchingTask map[string]int    // task id -> Start calls in flight
	pending       map[string]fire   // task id -> its one pending overlap fire
	queue         map[string][]fire // target -> fires waiting for capacity, FIFO
	draining      map[string]bool   // target -> a drain loop is running
	drainAgain    map[string]bool   // target -> the running drain must look again
	gone          map[string]bool   // target was removed; its waiting fires must not start
}

// New returns a Dispatcher.
func New(o Options) *Dispatcher {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	stopCtx, cancelStop := context.WithCancel(context.Background())
	return &Dispatcher{
		runs: o.Runs, store: o.Store, cfg: o.Config, log: log,
		now: now, onDropped: o.OnDropped, stopCtx: stopCtx, cancelStop: cancelStop,
		launching:     map[string]int{},
		launchingTask: map[string]int{},
		pending:       map[string]fire{},
		queue:         map[string][]fire{},
		draining:      map[string]bool{},
		drainAgain:    map[string]bool{},
		gone:          map[string]bool{},
	}
}

// SetConfig replaces the config max_parallel is read from. It starts
// nothing: call Drain afterwards, so fires that waited for capacity use
// the room a higher max_parallel gives. A target DropTargets marked gone
// is runnable again when cfg lists it.
func (d *Dispatcher) SetConfig(cfg config.Config) {
	d.mu.Lock()
	d.cfg = cfg
	for name := range d.gone {
		if _, ok := cfg.Targets[name]; ok {
			delete(d.gone, name)
		}
	}
	d.mu.Unlock()
}

// Stop makes the dispatcher start no more waiting fires: the daemon is
// shutting down. They stay in the store, and the next daemon's Restore
// brings them back. A launch already in flight finishes. Stop does not
// wait for it.
func (d *Dispatcher) Stop() { d.cancelStop() }

// DropTargets discards waiting fires for targets reload removed: the
// max_parallel queue and a task's pending overlap fire. A fire already
// handed to Start is dropped there too. Each dropped fire gets a row in
// its task's history. A later SetConfig that lists the target again lets
// new fires run. The drops are written even when ctx is cancelled: see
// recordDrop.
func (d *Dispatcher) DropTargets(ctx context.Context, names []string) {
	if len(names) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	gone := make(map[string]bool, len(names))
	for _, name := range names {
		gone[name] = true
	}
	for _, name := range names {
		d.gone[name] = true
		if n := len(d.queue[name]); n > 0 {
			d.log.Info("dropping queued fires: target is gone", logging.KeyTarget, name, "fires", n)
		}
		for _, f := range d.queue[name] {
			d.recordDrop(ctx, f, reasonTargetGone(name))
		}
		delete(d.queue, name)
	}
	for id, f := range d.pending {
		if gone[f.task.Target] {
			d.log.Info("dropping pending fire: target is gone",
				logging.KeyTaskID, id, logging.KeyTarget, f.task.Target)
			d.recordDrop(ctx, f, reasonTargetGone(f.task.Target))
			delete(d.pending, id)
		}
	}
}

// Drain starts the waiting fires of every target that has capacity. Like
// Submit, it may start runs: do not call it with a lock the runner's
// callbacks take.
func (d *Dispatcher) Drain(ctx context.Context) {
	d.mu.Lock()
	targets := make([]string, 0, len(d.queue))
	for name := range d.queue {
		targets = append(targets, name)
	}
	d.mu.Unlock()
	for _, name := range targets {
		d.drain(ctx, name)
	}
}

// Start satisfies scheduler.Starter: Submit without forcing a parallel run.
// A skipped or queued fire gives a zero Run and a nil error.
func (d *Dispatcher) Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error) {
	out, err := d.Submit(ctx, t, trigger, scheduledAt, false)
	return out.Run, err
}

// Submit handles one fire with trigger schedule, catch_up or manual.
// forceParallel treats the task as overlap parallel (the user confirmed a
// parallel run). A manual fire of a busy overlap skip task returns ErrActive.
func (d *Dispatcher) Submit(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time, forceParallel bool) (Outcome, error) {
	f := fire{task: t, trigger: trigger, scheduledAt: scheduledAt}
	active := d.lockWithActive()
	overlap := t.Overlap
	if forceParallel {
		overlap = "parallel"
	}
	if overlap != "parallel" && d.busyLocked(t.ID, active) {
		if overlap == "queue" {
			if _, ok := d.pending[t.ID]; ok {
				d.mu.Unlock()
				d.log.Info("overlap queue: a fire is already pending, dropping this one",
					logging.KeyTaskID, t.ID, logging.KeyTarget, t.Target,
					"trigger", trigger, "scheduled_at", scheduledAt)
				return Outcome{Skipped: true}, nil
			}
			d.storeLocked(ctx, &f, true)
			d.pending[t.ID] = f
			d.mu.Unlock()
			return Outcome{Queued: true}, nil
		}
		// skip, and any unknown value: validation only lets skip through.
		d.mu.Unlock()
		if trigger == store.TriggerManual {
			return Outcome{}, ErrActive
		}
		d.log.Info("overlap skip: task has an active run",
			logging.KeyTaskID, t.ID, logging.KeyTarget, t.Target, "scheduled_at", scheduledAt)
		return Outcome{Skipped: true}, nil
	}
	target := t.Target
	if len(d.queue[target]) > 0 || !d.hasCapacityLocked(target, active) {
		d.storeLocked(ctx, &f, false)
		d.queue[target] = append(d.queue[target], f)
		d.mu.Unlock()
		return Outcome{Queued: true}, nil
	}
	d.launching[target]++
	d.launchingTask[t.ID]++
	d.mu.Unlock()

	run, _, err := d.launch(ctx, f)
	d.drain(ctx, target)
	return Outcome{Run: run}, err
}

// RunEnded is called by the runner after a run's end is stored. The ended
// run is already gone from ActiveRuns. The task's pending overlap fire
// starts only once nothing else of that task is in flight: a forced
// parallel run ending must not start it while the run it queued behind is
// still active, and that run ending must not start it while the forced
// run is still active. Waiting fires then start while the target has
// capacity.
func (d *Dispatcher) RunEnded(ctx context.Context, run store.Run) {
	d.mu.Lock()
	d.gen++
	d.mu.Unlock()
	d.releasePendingIfIdle(ctx, run.TaskID)
	d.drain(ctx, run.Target)
}

// releasePendingIfIdle moves the task's pending overlap fire onto its
// target queue when the task has no run launching and no run still active.
// A pending fire stays put while any run of the task is in flight, so
// queue mode keeps its one waiting fire until the task is actually idle.
func (d *Dispatcher) releasePendingIfIdle(ctx context.Context, taskID string) {
	active := d.lockWithActive()
	if !d.taskInFlightLocked(taskID, active) {
		d.releasePendingLocked(ctx, taskID)
	}
	d.mu.Unlock()
}

// taskInFlightLocked reports whether a run of the task is being launched
// or is still active. It does not count the pending overlap fire or a
// fire waiting in a target queue.
func (d *Dispatcher) taskInFlightLocked(taskID string, active []store.Run) bool {
	if d.launchingTask[taskID] > 0 {
		return true
	}
	for _, r := range active {
		if r.TaskID == taskID && r.EndedAt.IsZero() {
			return true
		}
	}
	return false
}

// releasePendingLocked moves the task's pending overlap fire, if any, to the
// end of its target's queue.
//
// The store write follows ctx: a cancelled caller does not wait for it. No
// fire is lost that way. The fire moves in memory either way and keeps its
// old row, still marked pending, which the launch removes. If the daemon
// stops first, Restore releases that row again, as it does for any pending
// fire whose run ended with the previous daemon.
func (d *Dispatcher) releasePendingLocked(ctx context.Context, taskID string) {
	f, ok := d.pending[taskID]
	if !ok {
		return
	}
	delete(d.pending, taskID)
	if d.store != nil && f.id != 0 {
		id, err := d.store.ReleaseQueuedFire(ctx, f.id)
		switch {
		case err != nil && ctx.Err() != nil:
			d.log.Info("queued fire not released in the store: cancelled", logging.KeyTaskID, taskID, "err", err)
		case err != nil:
			d.log.Error("cannot release queued fire", logging.KeyTaskID, taskID, "err", err)
		default:
			f.id = id
		}
	}
	d.queue[f.task.Target] = append(d.queue[f.task.Target], f)
}

// launch calls Runs.Start for a fire already counted in launching, then
// uncounts it. The caller must not hold d.mu and must drain the target
// afterwards. A launch that fails before the run exists never gets
// RunEnded; one that fails inside Start gets RunEnded before this count
// drops, so the pending fire is released here, after the decrement, and
// only when the task is idle. dropped reports that the fire was not handed
// to Start because its target is gone; launch recorded that drop.
func (d *Dispatcher) launch(ctx context.Context, f fire) (run store.Run, dropped bool, err error) {
	d.mu.Lock()
	gone := d.gone[f.task.Target]
	d.mu.Unlock()
	if gone {
		// Reload removed the target after this fire was counted. Starting
		// it would launch on a machine that is no longer configured.
		d.log.Info("dropping fire: target is gone",
			logging.KeyTaskID, f.task.ID, logging.KeyTarget, f.task.Target, "trigger", f.trigger)
		run = d.recordDrop(ctx, f, reasonTargetGone(f.task.Target))
	} else {
		run, err = d.runs.Start(ctx, f.task, f.trigger, f.scheduledAt)
	}
	d.mu.Lock()
	d.gen++
	d.launching[f.task.Target]--
	d.launchingTask[f.task.ID]--
	if d.launchingTask[f.task.ID] == 0 {
		delete(d.launchingTask, f.task.ID)
	}
	d.mu.Unlock()
	d.releasePendingIfIdle(ctx, f.task.ID)
	return run, gone, err
}

// drain starts queued fires on target while it has capacity. Only one drain
// runs per target; a call during a running drain makes that one look again.
// The fires it starts are not the caller's (Submit's ctx may be an API
// request's), so they do not inherit ctx's cancellation. A drain that
// stopped because one caller went away would also strand the fires of every
// caller it was asked to look again for.
//
// What does stop a drain is Stop, at daemon shutdown: once stopCtx is
// cancelled, the next fire is not taken out of the store and not launched,
// so it waits there for the next daemon. stopCtx is checked before the
// delete, not passed to it. A delete that started must complete: the
// driver can report a cancellation for a statement that was committed, and
// the fire would then be gone from the store without being launched.
func (d *Dispatcher) drain(ctx context.Context, target string) {
	ctx = context.WithoutCancel(ctx)
	d.mu.Lock()
	if d.draining[target] {
		d.drainAgain[target] = true
		d.mu.Unlock()
		return
	}
	d.draining[target] = true
	d.mu.Unlock()
	for {
		d.mu.Lock()
		d.drainAgain[target] = false
		d.mu.Unlock()
		active := d.lockWithActive()
		if d.gone[target] {
			// Fires that reached the queue after DropTargets emptied it.
			for _, f := range d.queue[target] {
				d.recordDrop(ctx, f, reasonTargetGone(target))
			}
			delete(d.queue, target)
			if d.drainAgain[target] {
				d.mu.Unlock()
				continue
			}
			delete(d.draining, target)
			delete(d.drainAgain, target)
			d.mu.Unlock()
			return
		}
		if d.stopCtx.Err() != nil {
			delete(d.draining, target)
			delete(d.drainAgain, target)
			d.mu.Unlock()
			return
		}
		if len(d.queue[target]) == 0 || !d.hasCapacityLocked(target, active) {
			if d.drainAgain[target] {
				d.mu.Unlock()
				continue
			}
			delete(d.draining, target)
			delete(d.drainAgain, target)
			if len(d.queue[target]) == 0 {
				delete(d.queue, target)
			}
			d.mu.Unlock()
			return
		}
		f := d.queue[target][0]
		// Removed before the launch: a daemon that stops during it leaves
		// the run row, and must not start the fire a second time. A failed
		// delete leaves the row, so the fire stays queued and is not
		// launched; the next drain tries again.
		if err := d.unstoreLocked(ctx, f); err != nil {
			d.log.Error("cannot remove queued fire from the store",
				logging.KeyTaskID, f.task.ID, logging.KeyTarget, f.task.Target, "err", err)
			delete(d.draining, target)
			delete(d.drainAgain, target)
			d.mu.Unlock()
			return
		}
		d.queue[target] = d.queue[target][1:]
		d.launching[target]++
		d.launchingTask[f.task.ID]++
		d.mu.Unlock()
		d.startDeferred(ctx, f)
	}
}

// startDeferred starts a fire that waited in a queue. Its errors have no
// caller to go to, so they are logged. A fire that Start did not make a
// run for, with an error (its task was deleted or made invalid while it
// waited) or without one, is gone from the queue and has no run row: it
// gets a dropped row instead.
// A schedule or catch_up fire is recorded in task_state, as the scheduler
// records only fires it started.
func (d *Dispatcher) startDeferred(ctx context.Context, f fire) {
	run, dropped, err := d.launch(ctx, f)
	log := d.log.With(logging.KeyTaskID, f.task.ID, logging.KeyTarget, f.task.Target)
	if run.ID != "" {
		log = logging.ForRun(d.log, run.ID, f.task.ID, f.task.Target)
	}
	if err != nil {
		log.Error("cannot start queued run", "trigger", f.trigger, "scheduled_at", f.scheduledAt, "err", err)
	}
	if run.ID == "" && !dropped {
		reason := "not started"
		if err != nil {
			reason += ": " + err.Error()
		}
		d.recordDrop(ctx, f, reason)
	}
	// A dropped fire did not run. Leaving task_state alone keeps it
	// from counting as the last fire, which would hide an older missed one.
	if dropped || run.Status == store.StatusDropped || run.ID == "" || f.trigger == store.TriggerManual || d.store == nil {
		return
	}
	if err := d.store.RecordFire(ctx, f.task.ID, run.ID, f.scheduledAt); err != nil {
		log.Error("cannot write task state", "err", err)
	}
}

// storeLocked writes a fire that starts to wait to the store, so that
// Restore finds it after a daemon restart. A fire that cannot be stored
// still waits in memory. The write happens under d.mu to keep the stored
// order equal to the queue order.
//
// This write must complete, so it does not follow ctx's cancellation. The
// caller is told the fire is queued and the fire waits in memory whatever
// happens to ctx (an API client that went away, or the scheduler's context
// at shutdown). Without the row, the next daemon would not know about it.
func (d *Dispatcher) storeLocked(ctx context.Context, f *fire, pending bool) {
	if d.store == nil {
		return
	}
	id, err := d.store.AddQueuedFire(context.WithoutCancel(ctx), store.QueuedFire{
		TaskID: f.task.ID, Target: f.task.Target, Trigger: f.trigger,
		ScheduledAt: f.scheduledAt, Pending: pending,
	})
	if err != nil {
		d.log.Error("cannot store queued fire: a daemon restart would lose it",
			logging.KeyTaskID, f.task.ID, logging.KeyTarget, f.task.Target, "err", err)
		return
	}
	f.id = id
}

// unstoreLocked removes a fire that is about to start from the store. The
// caller logs a failure. The fire must not launch when the delete fails,
// or a restart would start it again from the row left behind.
func (d *Dispatcher) unstoreLocked(ctx context.Context, f fire) error {
	if d.store == nil || f.id == 0 {
		return nil
	}
	return d.store.DeleteQueuedFire(ctx, f.id)
}

func reasonTargetGone(target string) string {
	return "target " + target + " removed from config"
}

// recordDrop removes a fire that will not run from the store and adds the
// row that shows the drop, and reason, in its task's history. The two are
// one transaction. A failure is logged: the fire is dropped all the same.
// Without a store nothing is recorded. It may be called with d.mu held.
// The stored row is returned. A zero run means nothing was stored.
//
// This write must complete, so it does not follow ctx's cancellation. The
// fire is already gone from memory. If its row stayed in queued_fires, the
// next daemon would restore it and run a fire that was dropped.
func (d *Dispatcher) recordDrop(ctx context.Context, f fire, reason string) store.Run {
	if d.store == nil {
		return store.Run{}
	}
	now := d.now()
	run := store.Run{
		ID: store.NewRunID(now), TaskID: f.task.ID, Target: f.task.Target, Trigger: f.trigger,
		ScheduledAt: f.scheduledAt, StartedAt: now, EndedAt: now,
		Status: store.StatusDropped, ExitReason: reason,
	}
	if err := d.store.DropQueuedFire(context.WithoutCancel(ctx), f.id, run); err != nil {
		d.log.Error("cannot record dropped fire",
			logging.KeyTaskID, f.task.ID, logging.KeyTarget, f.task.Target, "reason", reason, "err", err)
		return store.Run{}
	}
	if d.onDropped != nil {
		d.onDropped(run)
	}
	return run
}

// Restore loads the fires a previous daemon left waiting, in their queue
// order. Call it once at daemon start, after the runner recovered its open
// runs and before any Submit. tasks are the valid tasks as they are now: a
// fire waits on its task's current target, and a fire of a task that is
// gone is dropped. So is a schedule or catch_up fire planned longer than
// catch_up_window before now: like a missed fire of that age, it no longer
// runs. A pending overlap fire whose run ended with the previous daemon
// moves to the back of its target's queue, behind fires that were
// already waiting for a slot. Each dropped fire gets a row in its task's
// history. Restore starts nothing; Resume does.
func (d *Dispatcher) Restore(ctx context.Context, tasks []config.Task, now time.Time) error {
	if d.store == nil {
		return nil
	}
	stored, err := d.store.ListQueuedFires(ctx)
	if err != nil {
		return fmt.Errorf("restore queued fires: %w", err)
	}
	byID := make(map[string]config.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	active := d.lockWithActive()
	defer d.mu.Unlock()
	// Idle pending fires are released only after every capacity-queue row
	// is loaded. Releasing one inside the loop would put it ahead of rows
	// that were already waiting, which is the opposite of a live release.
	var idlePending []string
	for _, s := range stored {
		t, ok := byID[s.TaskID]
		if !ok {
			// Only what the history row of the drop needs.
			t = config.Task{ID: s.TaskID, Target: s.Target}
		}
		f := fire{task: t, trigger: s.Trigger, scheduledAt: s.ScheduledAt, id: s.ID}
		_, dup := d.pending[s.TaskID]
		switch {
		case !ok:
			d.log.Warn("queued fire dropped: its task is gone or invalid",
				logging.KeyTaskID, s.TaskID, logging.KeyTarget, s.Target,
				"trigger", s.Trigger, "scheduled_at", s.ScheduledAt)
			d.recordDrop(ctx, f, "task removed or invalid")
			continue
		case s.Trigger != store.TriggerManual && d.cfg.CatchUpWindow.Duration > 0 &&
			now.Sub(s.ScheduledAt) > d.cfg.CatchUpWindow.Duration:
			d.log.Warn("queued fire dropped: older than catch_up_window",
				logging.KeyTaskID, s.TaskID, logging.KeyTarget, s.Target, "trigger", s.Trigger,
				"scheduled_at", s.ScheduledAt, "catch_up_window", d.cfg.CatchUpWindow.String())
			d.recordDrop(ctx, f, "older than catch_up_window ("+d.cfg.CatchUpWindow.String()+")")
			continue
		case s.Pending && dup:
			// Submit keeps one pending fire per task, so this is not stored.
			d.recordDrop(ctx, f, "the task already has a pending fire")
			continue
		case s.Pending:
			d.pending[t.ID] = f
			if !d.taskInFlightLocked(t.ID, active) {
				idlePending = append(idlePending, t.ID)
			}
		default:
			d.queue[t.Target] = append(d.queue[t.Target], f)
		}
		d.log.Info("queued fire restored", logging.KeyTaskID, t.ID, logging.KeyTarget, t.Target,
			"trigger", s.Trigger, "scheduled_at", s.ScheduledAt)
	}
	for _, taskID := range idlePending {
		d.releasePendingLocked(ctx, taskID)
	}
	return nil
}

// Resume starts the waiting fires that fit on their targets now. The
// daemon calls it after Restore; while it runs, fires are also started by
// Submit and RunEnded as usual.
func (d *Dispatcher) Resume(ctx context.Context) {
	d.mu.Lock()
	targets := make([]string, 0, len(d.queue))
	for target := range d.queue {
		targets = append(targets, target)
	}
	d.mu.Unlock()
	slices.Sort(targets)
	for _, target := range targets {
		d.drain(ctx, target)
	}
}

// lockWithActive locks d.mu and returns the active runs, taken so that they
// agree with the launching counts: no Start finished between the snapshot
// and the lock. ActiveRuns is called without d.mu held, so a runner that
// holds its own lock while calling RunEnded cannot deadlock with it.
func (d *Dispatcher) lockWithActive() []store.Run {
	for {
		d.mu.Lock()
		gen := d.gen
		d.mu.Unlock()
		active := d.runs.ActiveRuns()
		d.mu.Lock()
		if d.gen == gen {
			return active
		}
		d.mu.Unlock()
	}
}

// busyLocked reports whether the task has an active run, a run being
// launched, a pending overlap fire, or a fire waiting in its target's queue.
// A pending fire counts because the runner drops an ending run from
// ActiveRuns before it calls RunEnded, which releases the pending fire:
// in between, the task must not look idle.
func (d *Dispatcher) busyLocked(taskID string, active []store.Run) bool {
	if d.taskInFlightLocked(taskID, active) {
		return true
	}
	if _, ok := d.pending[taskID]; ok {
		return true
	}
	for _, q := range d.queue {
		for _, f := range q {
			if f.task.ID == taskID {
				return true
			}
		}
	}
	return false
}

// hasCapacityLocked reports whether one more run fits on target.
func (d *Dispatcher) hasCapacityLocked(target string, active []store.Run) bool {
	n := d.launching[target]
	for _, r := range active {
		if r.Target == target && r.EndedAt.IsZero() {
			n++
		}
	}
	return n < d.maxParallel(target)
}

func (d *Dispatcher) maxParallel(target string) int {
	if m := d.cfg.Targets[target].MaxParallel; m > 0 {
		return m
	}
	return DefaultMaxParallel
}
