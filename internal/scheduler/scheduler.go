// Package scheduler fires tasks at their schedule's fire times. It keeps
// next_fire_at per task in the store, starts a run for every fire through
// the task's Target, and prunes run history older than history_days.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/schedule"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// maxWait caps one sleep of the Run loop, so a wall-clock jump (suspend,
// NTP step) delays a fire by at most this much.
const maxWait = time.Minute

// pruneEvery is how often run history is pruned.
const pruneEvery = time.Hour

// Options configures New.
type Options struct {
	Clock   clock.Clock
	Store   *store.Store
	Targets map[string]target.Target // by target name
	Config  config.Config            // timezone fallback and history_days
	Log     *slog.Logger
}

// Scheduler fires tasks. Its methods are safe for concurrent use.
type Scheduler struct {
	clock   clock.Clock
	store   *store.Store
	targets map[string]target.Target
	cfg     config.Config
	log     *slog.Logger
	reload  chan struct{}

	mu        sync.Mutex
	tasks     map[string]*entry // scheduled tasks: valid and enabled
	nextPrune time.Time
}

type entry struct {
	task  config.Task
	sched schedule.Schedule
	next  time.Time
}

// New returns a scheduler with no tasks.
func New(o Options) *Scheduler {
	return &Scheduler{
		clock: o.Clock, store: o.Store, targets: o.Targets, cfg: o.Config, log: o.Log,
		reload: make(chan struct{}, 1),
		tasks:  map[string]*entry{},
	}
}

// SetTasks replaces the task list. Valid, enabled tasks get a next_fire_at
// computed from now in their time zone; off and invalid tasks get none.
// The result is written to task_state.
func (s *Scheduler) SetTasks(ctx context.Context, entries []config.TaskEntry) error {
	now := s.clock.Now()
	tasks := make(map[string]*entry, len(entries))
	for _, e := range entries {
		var next time.Time
		if e.Err == nil && e.Task.Enabled {
			en, err := s.newEntry(e.Task, now)
			if err != nil {
				// Validation parses the schedule too, so this is a bug or a
				// race with a hand edit; the task just does not fire.
				s.log.Error("cannot schedule task", "task_id", e.ID, "err", err)
			} else {
				tasks[e.ID] = en
				next = en.next
			}
		}
		if err := s.putNext(ctx, e.ID, next); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.tasks = tasks
	s.mu.Unlock()
	select {
	case s.reload <- struct{}{}:
	default:
	}
	return nil
}

func (s *Scheduler) newEntry(t config.Task, now time.Time) (*entry, error) {
	loc, err := s.cfg.Location(t)
	if err != nil {
		return nil, err
	}
	sched, err := schedule.Parse(t.Schedule, loc)
	if err != nil {
		return nil, fmt.Errorf("schedule %q: %w", t.Schedule, err)
	}
	return &entry{task: t, sched: sched, next: sched.Next(now)}, nil
}

// putNext stores next as the task's next_fire_at and keeps the rest of its
// task_state.
func (s *Scheduler) putNext(ctx context.Context, id string, next time.Time) error {
	st, _, err := s.store.GetTaskState(ctx, id)
	if err != nil {
		return fmt.Errorf("task %s: %w", id, err)
	}
	st.TaskID, st.NextFireAt = id, next
	if err := s.store.PutTaskState(ctx, st); err != nil {
		return fmt.Errorf("task %s: %w", id, err)
	}
	return nil
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
	}
	var fire []due
	s.mu.Lock()
	for _, e := range s.tasks {
		if !e.next.IsZero() && !e.next.After(now) {
			fire = append(fire, due{e, e.next})
			e.next = e.sched.Next(now)
		}
	}
	s.mu.Unlock()

	for _, d := range fire {
		s.fire(ctx, d.e.task, d.scheduledAt, d.e.next, now)
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

// fire starts one scheduled run of t, unless t is paused, and records the
// fire in task_state.
func (s *Scheduler) fire(ctx context.Context, t config.Task, scheduledAt, next, now time.Time) {
	log := s.log.With("task_id", t.ID, "target", t.Target)
	st, _, err := s.store.GetTaskState(ctx, t.ID)
	if err != nil {
		log.Error("cannot read task state", "err", err)
	}
	st.TaskID, st.NextFireAt = t.ID, next
	if paused(t, now) {
		log.Info("fire skipped: task paused", "scheduled_at", scheduledAt, "paused_until", t.PausedUntil)
	} else {
		run, err := s.start(ctx, t, store.TriggerSchedule, scheduledAt)
		if err != nil {
			log.Error("cannot start run", "err", err)
		} else {
			st.LastRunID, st.LastFireAt = run.ID, scheduledAt
		}
	}
	if err := s.store.PutTaskState(ctx, st); err != nil {
		log.Error("cannot write task state", "err", err)
	}
}

// paused reports whether t is paused at now: paused_until is "indefinite"
// or a time after now.
func paused(t config.Task, now time.Time) bool {
	switch t.PausedUntil {
	case "":
		return false
	case "indefinite":
		return true
	}
	until, err := time.Parse(time.RFC3339, t.PausedUntil)
	return err == nil && until.After(now)
}

// start reserves a run row and launches the run on the task's target.
//
// Until the herdr adapter exists, a launch is one Target.Exec of the agent
// argv, and the run ends when Exec returns: done on success, failed with
// the error as exit reason.
func (s *Scheduler) start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error) {
	tg, ok := s.targets[t.Target]
	if !ok {
		return store.Run{}, fmt.Errorf("unknown target %q", t.Target)
	}
	now := s.clock.Now()
	run := store.Run{
		ID: store.NewRunID(now), TaskID: t.ID, Target: t.Target, Trigger: trigger,
		ScheduledAt: scheduledAt, StartedAt: now, Status: store.StatusStarting,
	}
	if err := s.store.InsertRun(ctx, run); err != nil {
		return store.Run{}, err
	}
	log := s.log.With("run_id", run.ID, "task_id", t.ID, "target", t.Target)
	log.Info("run starting", "trigger", trigger, "scheduled_at", scheduledAt)

	argv := append([]string{t.Agent.Command}, t.Agent.Args...)
	_, err := tg.Exec(ctx, argv, nil)
	run.EndedAt, run.Status = s.clock.Now(), store.StatusDone
	if err != nil {
		run.Status, run.ExitReason = store.StatusFailed, err.Error()
	}
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return run, err
	}
	log.Info("run ended", "status", run.Status, "exit_reason", run.ExitReason)
	return run, nil
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

	days := s.cfg.HistoryDays
	if days <= 0 {
		days = config.DefaultHistoryDays
	}
	cutoff := now.AddDate(0, 0, -days)
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
