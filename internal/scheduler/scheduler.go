// Package scheduler fires tasks at their schedule's fire times. It keeps
// next_fire_at per task in the store, starts a run for every fire through
// a Starter (the dispatcher + runner), and prunes run history older than
// history_days.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/schedule"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// maxWait caps one sleep of the Run loop, so a wall-clock jump (suspend,
// NTP step) delays a fire by at most this much.
const maxWait = time.Minute

// lateGrace is how late a fire may start and still count as on time. A
// fire time further in the past (the daemon was down, the host slept) is a
// missed fire: the task's missed policy decides whether it is caught up.
const lateGrace = time.Minute

// pruneEvery is how often run history is pruned.
const pruneEvery = time.Hour

// Starter starts a run of a task; runner.Runner implements it. Start
// stores the run row and returns it. It returns an error only when the
// run could not be stored; a run that fails to launch comes back ended.
// It is called inside Tick, so it must return once the run is launched.
type Starter interface {
	Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error)
}

// Options configures New.
type Options struct {
	Clock  clock.Clock
	Store  *store.Store
	Runs   Starter
	Config config.Config // as loaded: timezone fallback, history_days, catch_up_window
	Log    *slog.Logger
	// OnPauseExpired is called from Tick when a task's timed paused_until
	// has passed, including a task that is off or invalid. The daemon
	// clears it in the task file. It may call SetTask. A non-nil error is
	// retried later; nil means the pause is done (cleared, or already gone).
	// A nil func means nothing is called.
	OnPauseExpired func(ctx context.Context, taskID string) error
}

// Scheduler fires tasks. Its methods are safe for concurrent use.
type Scheduler struct {
	clock  clock.Clock
	store  *store.Store
	runs   Starter
	cfg    config.Config
	log    *slog.Logger
	reload chan struct{}

	onPauseExpired func(ctx context.Context, taskID string) error

	mu        sync.Mutex
	tasks     map[string]*entry // enabled tasks, and any task waiting to clear a timed pause
	nextPrune time.Time
	pausedAll bool                // global pause of every target
	pausedOn  map[string]struct{} // global pause by target name
}

type entry struct {
	task  config.Task
	loc   string // time zone name
	sched schedule.Schedule
	next  time.Time
	// pauseEnd is when a timed pause should be cleared. Zero means there is
	// nothing to clear. A failed clear moves it forward so the next attempt
	// waits instead of spinning: the wake time would otherwise be in the past.
	pauseEnd time.Time
}

// pauseClearRetry is how long to wait before clearing paused_until again
// after the write failed.
const pauseClearRetry = time.Minute

// New returns a scheduler with no tasks.
func New(o Options) *Scheduler {
	if o.Config.CatchUpWindow.Duration <= 0 {
		o.Config.CatchUpWindow.Duration = config.DefaultCatchUpWindow
	}
	return &Scheduler{
		clock: o.Clock, store: o.Store, runs: o.Runs, cfg: o.Config, log: o.Log,
		reload:         make(chan struct{}, 1),
		tasks:          map[string]*entry{},
		pausedOn:       map[string]struct{}{},
		onPauseExpired: o.OnPauseExpired,
	}
}

// SetTasks replaces the task list. Valid, enabled tasks get a next_fire_at
// in their time zone; off and invalid tasks get none. A task that was
// already scheduled with the same schedule and time zone keeps its next
// fire time; any other task's is computed from now. The result is written
// to task_state.
//
// A task that has no entry yet (the first call after the daemon starts, or
// a task new to this daemon) keeps the next_fire_at stored in task_state
// if that is earlier than the one computed from now. A stored time in the
// past is a missed fire, which the next Tick hands to the missed policy.
//
// A task that is off or invalid is not fired, but a timed pause on it is
// still watched: when paused_until passes, Tick reports it so the daemon
// can clear the file.
func (s *Scheduler) SetTasks(ctx context.Context, entries []config.TaskEntry) error {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make(map[string]*entry, len(entries))
	for _, e := range entries {
		old := s.tasks[e.ID]
		en, next, err := s.entryFor(e, now, old)
		if err != nil {
			// Validation parses the schedule too, so this is a bug or a
			// race with a hand edit; the task just does not fire.
			s.log.Error("cannot schedule task", "task_id", e.ID, "err", err)
			en, next = nil, time.Time{}
		}
		if en != nil && old == nil {
			st, _, stErr := s.store.GetTaskState(ctx, e.ID)
			if stErr != nil {
				return stErr
			}
			if !st.NextFireAt.IsZero() && st.NextFireAt.Before(en.next) {
				en.next = st.NextFireAt
				next = en.next
			}
		}
		if en != nil {
			tasks[e.ID] = en
		}
		if err := s.store.SetNextFire(ctx, e.ID, next); err != nil {
			return err
		}
	}
	s.tasks = tasks
	s.wake()
	return nil
}

// SetTask replaces one task after an enable, disable, pause, resume or
// edit. Unlike SetTasks it always computes the next fire time from now. A
// task that is off or invalid gets no next fire time. A timed pause on it
// is still watched until it is cleared.
func (s *Scheduler) SetTask(ctx context.Context, e config.TaskEntry) error {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, e.ID)
	en, next, err := s.entryFor(e, now, nil)
	if err != nil {
		// As in SetTasks: the task does not fire, and has no next fire.
		s.log.Error("cannot schedule task", "task_id", e.ID, "err", err)
		return s.store.SetNextFire(ctx, e.ID, time.Time{})
	}
	if en != nil {
		s.tasks[e.ID] = en
	}
	if err := s.store.SetNextFire(ctx, e.ID, next); err != nil {
		return err
	}
	s.wake()
	return nil
}

// entryFor returns the scheduler entry for e and the next_fire_at to store.
// old is the previous entry, used by SetTasks to keep an unchanged fire
// time; SetTask passes nil so the next fire is computed from now.
// A task that does not fire is returned only when it has a timed pause to
// clear, and then its next fire time is zero so Tick will not start it.
func (s *Scheduler) entryFor(e config.TaskEntry, now time.Time, old *entry) (*entry, time.Time, error) {
	if e.Err == nil && e.Task.Enabled {
		en, err := s.newEntry(e.Task, now, old)
		if err != nil {
			return nil, time.Time{}, err
		}
		return en, en.next, nil
	}
	if end, ok := e.Task.PauseEnd(); ok {
		return &entry{task: e.Task, pauseEnd: end}, time.Time{}, nil
	}
	return nil, time.Time{}, nil
}

// SetGlobalPause sets the global pause: all pauses every task, targets
// pauses every task on those targets. Fire times that pass during a global
// pause start no run and are not caught up.
func (s *Scheduler) SetGlobalPause(all bool, targets []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pausedAll = all
	s.pausedOn = make(map[string]struct{}, len(targets))
	for _, t := range targets {
		s.pausedOn[t] = struct{}{}
	}
}

// GloballyPaused reports whether a global pause covers target.
func (s *Scheduler) GloballyPaused(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.globallyPaused(target)
}

func (s *Scheduler) globallyPaused(target string) bool {
	_, ok := s.pausedOn[target]
	return s.pausedAll || ok
}

// wake makes Run recompute its wake-up time. Call it with s.mu held.
func (s *Scheduler) wake() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// newEntry schedules t. It keeps the next fire time of old, the task's
// current entry, if the schedule and time zone did not change.
func (s *Scheduler) newEntry(t config.Task, now time.Time, old *entry) (*entry, error) {
	loc, err := s.cfg.Location(t)
	if err != nil {
		return nil, err
	}
	sched, err := schedule.Parse(t.Schedule, loc)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: %w", t.Schedule, err)
	}
	en := &entry{task: t, loc: loc.String(), sched: sched, next: sched.Next(now)}
	en.pauseEnd, _ = t.PauseEnd()
	if old != nil && old.task.Schedule == t.Schedule && old.loc == en.loc && !old.next.IsZero() {
		en.next = old.next
	}
	return en, nil
}

// NextFire returns the task's next fire time, or false if it has none (off,
// invalid or unknown).
func (s *Scheduler) NextFire(id string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tasks[id]
	if !ok || e.next.IsZero() {
		return time.Time{}, false
	}
	return e.next, true
}

// Tick fires every task whose fire time has come, reports timed pauses that
// have ended, prunes old runs when due, and returns the earliest next fire
// time or pause end (zero if there is none).
// A task fires at most once per Tick, however late the Tick is, for its
// latest fire time that has passed; its next fire time is then computed
// from now.
func (s *Scheduler) Tick(ctx context.Context) time.Time {
	now := s.clock.Now()
	s.prune(ctx, now)

	type due struct {
		e           *entry
		scheduledAt time.Time
		next        time.Time
		globalPause bool
	}
	var fire []due
	var expired []string
	s.mu.Lock()
	for _, e := range s.tasks {
		if !e.next.IsZero() && !e.next.After(now) {
			d := due{
				e: e, scheduledAt: s.latestFire(e, now), next: e.sched.Next(now),
				globalPause: s.globallyPaused(e.task.Target),
			}
			e.next = d.next
			fire = append(fire, d)
		}
	}
	s.mu.Unlock()

	for _, d := range fire {
		s.fire(ctx, d.e, d.scheduledAt, d.next, now, d.globalPause)
	}

	// After the fires: a fire time equal to paused_until is not paused, and
	// fire has judged it with the entry it had.
	s.mu.Lock()
	for id, e := range s.tasks {
		if !e.pauseEnd.IsZero() && !e.pauseEnd.After(now) {
			expired = append(expired, id)
		}
	}
	s.mu.Unlock()
	slices.Sort(expired)
	for _, id := range expired {
		var err error
		if s.onPauseExpired != nil {
			err = s.onPauseExpired(ctx, id)
		}
		s.finishPause(id, now, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var wake time.Time
	for _, e := range s.tasks {
		for _, t := range []time.Time{e.next, e.pauseEnd} {
			if !t.IsZero() && (wake.IsZero() || t.Before(wake)) {
				wake = t
			}
		}
	}
	return wake
}

// latestFire returns the last fire time of e at or before now; e.next must
// not be after now. Fire times older than catch_up_window are skipped
// without walking them; if none is left, it returns e.next.
func (s *Scheduler) latestFire(e *entry, now time.Time) time.Time {
	latest := e.next
	if floor := now.Add(-s.cfg.CatchUpWindow.Duration); latest.Before(floor) {
		// Next is strictly after its argument; a fire at floor is still
		// inside the window.
		f := e.sched.Next(floor.Add(-time.Nanosecond))
		if f.IsZero() || f.After(now) {
			return latest
		}
		latest = f
	}
	for {
		n := e.sched.Next(latest)
		if n.IsZero() || n.After(now) {
			return latest
		}
		latest = n
	}
}

// fire handles the latest passed fire time of e, scheduledAt. On time, it
// starts a scheduled run. Late by more than lateGrace, it is a missed fire:
// with missed: run_once and inside catch_up_window it starts one catch_up
// run, else nothing. Task pause and global pause skip without catch-up.
// A started run is recorded in task_state.
func (s *Scheduler) fire(ctx context.Context, e *entry, scheduledAt, next, now time.Time, globalPause bool) {
	t := e.task
	log := s.log.With(logging.KeyTaskID, t.ID, logging.KeyTarget, t.Target)
	// The state must be written even when ctx is cancelled mid-fire.
	wctx := context.WithoutCancel(ctx)
	switch {
	case t.PausedAt(scheduledAt):
		// Judge the slot, not the tick. A tick that lands just after
		// paused_until must not start a fire the pause covered. Those
		// slots are not missed fires, so nothing catches them up.
		log.Info("fire skipped: task paused", "scheduled_at", scheduledAt, "paused_until", t.PausedUntil)
	case globalPause:
		// Like a task pause: the slot passes and nothing catches it up.
		log.Info("fire skipped: global pause", "scheduled_at", scheduledAt)
	case now.Sub(scheduledAt) <= lateGrace:
		s.maybeStartScheduled(ctx, wctx, log, t, scheduledAt)
	case t.Missed == "skip":
		log.Info("missed fire skipped", "scheduled_at", scheduledAt, "late_by", now.Sub(scheduledAt).String())
	case now.Sub(scheduledAt) > s.cfg.CatchUpWindow.Duration:
		log.Info("missed fire older than catch_up_window ignored", "scheduled_at", scheduledAt,
			"late_by", now.Sub(scheduledAt).String(), "catch_up_window", s.cfg.CatchUpWindow.String())
	default:
		log.Info("catching up missed fire", "scheduled_at", scheduledAt, "late_by", now.Sub(scheduledAt).String())
		s.maybeStartCatchUp(ctx, wctx, log, t, scheduledAt)
	}
	// A SetTasks during the run may have replaced the entry; its next fire
	// time is then already stored.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks[t.ID] != e {
		return
	}
	if err := s.store.SetNextFire(wctx, t.ID, next); err != nil {
		log.Error("cannot write task state", "err", err)
	}
}

// CatchUpTarget applies the missed policy to the fires that found the
// target unreachable, now that it answers again. A task on the target with
// missed: run_once whose latest non-manual run is a host_unreachable fire
// (trigger schedule or catch_up) inside catch_up_window gets one catch_up
// run for that fire, however many fires were missed: each one after the
// first left a newer host_unreachable run, so that run names the latest.
// The usual suppression applies (disabled, paused, global pause).
func (s *Scheduler) CatchUpTarget(ctx context.Context, target string) {
	now := s.clock.Now()
	s.mu.Lock()
	var tasks []config.Task
	for _, e := range s.tasks {
		if e.task.Target == target && e.task.Enabled && e.task.Missed == "run_once" {
			tasks = append(tasks, e.task)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(tasks, func(a, b config.Task) int { return strings.Compare(a.ID, b.ID) })
	wctx := context.WithoutCancel(ctx)
	for _, t := range tasks {
		log := s.log.With(logging.KeyTaskID, t.ID, logging.KeyTarget, t.Target)
		last, ok, err := s.store.LastNonManualRun(ctx, t.ID)
		if err != nil {
			log.Error("cannot read last run for catch-up", "err", err)
			continue
		}
		if !ok || last.Status != store.StatusHostUnreachable || last.Trigger == store.TriggerManual {
			continue
		}
		if now.Sub(last.ScheduledAt) > s.cfg.CatchUpWindow.Duration {
			log.Info("unreachable fire older than catch_up_window ignored", "scheduled_at", last.ScheduledAt,
				"catch_up_window", s.cfg.CatchUpWindow.String())
			continue
		}
		if t.PausedAt(now) {
			log.Info("unreachable fire not caught up: task paused", "scheduled_at", last.ScheduledAt)
			continue
		}
		log.Info("catching up fire the target missed", "scheduled_at", last.ScheduledAt)
		s.maybeStartCatchUp(ctx, wctx, log, t, last.ScheduledAt)
	}
}

// maybeStartScheduled starts an on-time scheduled run after re-checking
// suppression (global pause, disable, task pause) once the scheduler lock
// is free.
func (s *Scheduler) maybeStartScheduled(ctx, wctx context.Context, log *slog.Logger, t config.Task, scheduledAt time.Time) {
	if why := s.fireSuppressed(t.ID, t.Target, scheduledAt); why != "" {
		log.Info("fire skipped: "+why, "scheduled_at", scheduledAt)
		return
	}
	s.start(ctx, wctx, log, t, store.TriggerSchedule, scheduledAt)
}

// maybeStartCatchUp starts a catch_up run with the same suppression rules.
func (s *Scheduler) maybeStartCatchUp(ctx, wctx context.Context, log *slog.Logger, t config.Task, scheduledAt time.Time) {
	if why := s.fireSuppressed(t.ID, t.Target, scheduledAt); why != "" {
		log.Info("fire skipped: "+why, "scheduled_at", scheduledAt)
		return
	}
	s.start(ctx, wctx, log, t, store.TriggerCatchUp, scheduledAt)
}

// start starts a run of t through the Starter and records the fire in
// task_state. A zero run with no error means the Starter did not start one
// (overlap); nothing is recorded then.
func (s *Scheduler) start(ctx, wctx context.Context, log *slog.Logger, t config.Task, trigger string, scheduledAt time.Time) {
	run, err := s.runs.Start(ctx, t, trigger, scheduledAt)
	if run.ID != "" {
		log = logging.ForRun(s.log, run.ID, t.ID, t.Target)
	}
	if err != nil {
		log.Error("cannot start run", "trigger", trigger, "scheduled_at", scheduledAt, "err", err)
	}
	// Record the fire even when the finishing write failed: the row
	// exists, and last_run_id should point at it.
	if run.ID != "" {
		if err := s.store.RecordFire(wctx, t.ID, run.ID, scheduledAt); err != nil {
			log.Error("cannot write task state", "err", err)
		}
	}
}

// fireSuppressed reports why a queued fire must not start, now that the
// lock is free again. "" means the fire still stands. The task as queued
// was not paused and no global pause covered it; this catches a change
// that landed after that snapshot.
func (s *Scheduler) fireSuppressed(id, targetName string, scheduledAt time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.globallyPaused(targetName) {
		return "global pause"
	}
	cur, ok := s.tasks[id]
	if !ok || !cur.task.Enabled {
		return "task disabled"
	}
	if cur.task.PausedAt(scheduledAt) {
		return "task paused"
	}
	return ""
}

// finishPause records the result of OnPauseExpired for id. A failed clear
// is tried again after pauseClearRetry; a success drops the pause so it
// is reported once. A pause SetTask installed in the meantime (a new end
// still in the future) is left alone.
func (s *Scheduler) finishPause(id string, now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.tasks[id]
	due := e != nil && !e.pauseEnd.IsZero() && !e.pauseEnd.After(now)
	switch {
	case err != nil:
		if due {
			e.pauseEnd = now.Add(pauseClearRetry)
		}
		s.log.Error("cannot clear paused_until", logging.KeyTaskID, id, "err", err)
	case due:
		e.pauseEnd = time.Time{}
		s.log.Info("pause ended", logging.KeyTaskID, id)
	case e == nil || e.pauseEnd.IsZero():
		// The callback cleared the pause and replaced or dropped the entry.
		s.log.Info("pause ended", logging.KeyTaskID, id)
	}
}

// prune deletes ended runs older than history_days, at most once per
// pruneEvery.
func (s *Scheduler) prune(ctx context.Context, now time.Time) {
	s.mu.Lock()
	if now.Before(s.nextPrune) {
		s.mu.Unlock()
		return
	}
	s.nextPrune = now.Add(pruneEvery)
	s.mu.Unlock()

	cutoff := now.AddDate(0, 0, -s.cfg.HistoryDays)
	n, err := s.store.DeleteRunsBefore(ctx, cutoff)
	if err != nil {
		s.log.Error("cannot prune run history", "err", err)
		return
	}
	if n > 0 {
		s.log.Info("pruned run history", "deleted", n, "before", cutoff)
	}
}

// Run calls Tick at every fire time until ctx is done. SetTasks wakes it at
// once.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		wake := s.Tick(ctx)
		wait := maxWait
		if !wake.IsZero() {
			wait = min(wait, wake.Sub(s.clock.Now()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.reload:
		case <-s.clock.After(wait):
		}
	}
}
