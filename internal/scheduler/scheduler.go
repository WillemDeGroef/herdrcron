// Package scheduler fires tasks at their schedule's fire times. It keeps
// next_fire_at per task in the store, starts a run for every fire through
// a Starter, and prunes run history older than history_days.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
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
// fire time further in the past (the host slept, the daemon stalled) is a
// missed fire and does not start a scheduled run.
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
	Config config.Config // as loaded: timezone fallback and history_days
	Log    *slog.Logger
}

// Scheduler fires tasks. Its methods are safe for concurrent use.
type Scheduler struct {
	clock  clock.Clock
	store  *store.Store
	runs   Starter
	cfg    config.Config
	log    *slog.Logger
	reload chan struct{}

	mu        sync.Mutex
	tasks     map[string]*entry // scheduled tasks: valid and enabled
	nextPrune time.Time
}

type entry struct {
	task  config.Task
	loc   string // time zone name
	sched schedule.Schedule
	next  time.Time
}

// New returns a scheduler with no tasks.
func New(o Options) *Scheduler {
	return &Scheduler{
		clock: o.Clock, store: o.Store, runs: o.Runs, cfg: o.Config, log: o.Log,
		reload: make(chan struct{}, 1),
		tasks:  map[string]*entry{},
	}
}

// SetTasks replaces the task list. Valid, enabled tasks get a next_fire_at
// in their time zone; off and invalid tasks get none. A task that was
// already scheduled with the same schedule and time zone keeps its next
// fire time; any other task's is computed from now. The result is written
// to task_state.
//
// SetTasks overwrites the next_fire_at stored by an earlier daemon, so the
// missed-fire check on start must read task_state before calling it.
func (s *Scheduler) SetTasks(ctx context.Context, entries []config.TaskEntry) error {
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make(map[string]*entry, len(entries))
	for _, e := range entries {
		var next time.Time
		if e.Err == nil && e.Task.Enabled {
			en, err := s.newEntry(e.Task, now, s.tasks[e.ID])
			if err != nil {
				// Validation parses the schedule too, so this is a bug or a
				// race with a hand edit; the task just does not fire.
				s.log.Error("cannot schedule task", "task_id", e.ID, "err", err)
			} else {
				tasks[e.ID] = en
				next = en.next
			}
		}
		if err := s.store.SetNextFire(ctx, e.ID, next); err != nil {
			return err
		}
	}
	s.tasks = tasks
	select {
	case s.reload <- struct{}{}:
	default:
	}
	return nil
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

// Tick fires every task whose fire time has come, prunes old runs when due,
// and returns the earliest next fire time (zero if no task is scheduled).
// A task fires at most once per Tick, however late the Tick is; its next
// fire time is then computed from now.
func (s *Scheduler) Tick(ctx context.Context) time.Time {
	now := s.clock.Now()
	s.prune(ctx, now)

	type due struct {
		e           *entry
		scheduledAt time.Time
		next        time.Time
	}
	var fire []due
	s.mu.Lock()
	for _, e := range s.tasks {
		if !e.next.IsZero() && !e.next.After(now) {
			d := due{e: e, scheduledAt: e.next, next: e.sched.Next(now)}
			e.next = d.next
			fire = append(fire, d)
		}
	}
	s.mu.Unlock()

	for _, d := range fire {
		s.fire(ctx, d.e, d.scheduledAt, d.next, now)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var wake time.Time
	for _, e := range s.tasks {
		if !e.next.IsZero() && (wake.IsZero() || e.next.Before(wake)) {
			wake = e.next
		}
	}
	return wake
}

// fire handles one fire time of e: it starts a scheduled run unless the
// task is paused or the fire is missed, and records it in task_state.
func (s *Scheduler) fire(ctx context.Context, e *entry, scheduledAt, next, now time.Time) {
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
	case now.Sub(scheduledAt) > lateGrace:
		// The missed policy (skip or run_once with trigger catch_up) is
		// not implemented yet; for now a missed fire is only logged.
		log.Warn("fire missed", "scheduled_at", scheduledAt, "late_by", now.Sub(scheduledAt).String())
	default:
		run, err := s.runs.Start(ctx, t, store.TriggerSchedule, scheduledAt)
		if run.ID != "" {
			log = logging.ForRun(s.log, run.ID, t.ID, t.Target)
		}
		if err != nil {
			log.Error("cannot start run", "scheduled_at", scheduledAt, "err", err)
		}
		// Record the fire even when the finishing write failed: the row
		// exists, and last_run_id should point at it.
		if run.ID != "" {
			if err := s.store.RecordFire(wctx, t.ID, run.ID, scheduledAt); err != nil {
				log.Error("cannot write task state", "err", err)
			}
		}
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
