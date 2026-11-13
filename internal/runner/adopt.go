package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// reasonLaunchUnconfirmed starts the exit reason of a host_unreachable run
// whose `pane run` was sent while the target stopped answering: the agent
// may have started. It is how AdoptLost finds such a run in the store, so
// it must not change between versions.
const reasonLaunchUnconfirmed = "launch not confirmed"

// reasonNoAgent replaces reasonLaunchUnconfirmed once the target answered
// and showed that the launch did not leave a running pane behind.
const reasonNoAgent = "launch reply lost, no agent found"

// lostLaunchReason reports whether reason is the marker AdoptLost looks
// up. The stored text is the marker, then ": ", then the wrapped error
// (see launch). A different reason that merely shares the prefix is not
// one, so a later wording cannot be adopted by accident.
func lostLaunchReason(reason string) bool {
	return reason == reasonLaunchUnconfirmed || strings.HasPrefix(reason, reasonLaunchUnconfirmed+": ")
}

// AdoptLost looks for runs of task t that ended host_unreachable because
// the reply to their launch was lost, and whose launch did reach the
// target: the run's pane still exists in its hc- tab, and the pane shows
// that the command was typed. Each one is stored as running again and
// tracked from step 7, like a recovered run. It returns the newest adopted
// run; adopted is false when there is none, and then a missed fire still
// needs its catch-up run.
//
// A candidate the target answers for is settled once: adopted, or its
// reason rewritten (and the tab of a launch that never arrived closed), so
// later calls do not ask again. With no candidate the target is not
// called. An error means the target could not be asked, so nothing is
// known: the caller must not start a run that could be a second one.
func (r *Runner) AdoptLost(ctx context.Context, t config.Task) (run store.Run, adopted bool, err error) {
	ts := r.targets[t.Target]
	if ts == nil {
		return store.Run{}, false, nil
	}
	// One at a time per target: a second call must see what the first
	// stored, not adopt a run again that has ended meanwhile.
	ts.adoptMu.Lock()
	defer ts.adoptMu.Unlock()
	runs, err := r.store.ListRuns(ctx, t.ID, 0)
	if err != nil {
		return store.Run{}, false, fmt.Errorf("adopt lost run of %s: %w", t.ID, err)
	}
	runs = slices.DeleteFunc(runs, func(run store.Run) bool {
		return run.Status != store.StatusHostUnreachable || run.Target != t.Target || run.HerdrPane == "" ||
			!lostLaunchReason(run.ExitReason)
	})
	if len(runs) == 0 {
		return store.Run{}, false, nil
	}
	if err := r.knownUnreachable(ts, t.Target); err != nil {
		return store.Run{}, false, err
	}
	panes, err := ts.herdr.PaneList(ctx)
	r.observe(t.Target, err, true)
	if err != nil && !herdr.IsCode(err, herdr.CodeServerNotRunning) {
		return store.Run{}, false, fmt.Errorf("adopt lost run of %s: %w", t.ID, err)
	}
	// With no herdr server there are no panes: err is nil from here on.
	// A run must not be left half adopted when the daemon stops.
	wctx := context.WithoutCancel(ctx)
	for _, c := range runs {
		log := logging.ForRun(r.log, c.ID, c.TaskID, c.Target)
		launched, err := r.launchArrived(ctx, ts, c, panes, log)
		if err != nil {
			if errors.Is(err, target.ErrUnreachable) {
				r.observe(t.Target, err, true)
			}
			return run, adopted, fmt.Errorf("adopt lost run %s: %w", c.ID, err)
		}
		if !launched {
			c.ExitReason = reasonNoAgent + strings.TrimPrefix(c.ExitReason, reasonLaunchUnconfirmed)
			if err := r.store.UpdateRun(wctx, c); err != nil {
				return run, adopted, fmt.Errorf("adopt lost run %s: %w", c.ID, err)
			}
			r.changed(c)
			continue
		}
		got, err := r.adopt(wctx, t, c, log)
		if err != nil {
			return run, adopted, err
		}
		if !adopted {
			run, adopted = got, true
		}
	}
	return run, adopted, nil
}

// launchArrived reports whether the launch of run reached the target: its
// pane is in panes, still in the run's tab, the tab still carries the
// task's hc- label (a restarted herdr server hands out the same ids
// again), and the pane shows the command: an agent, a foreground process
// that is not the shell, or the launch line in its output. A pane with
// none of these is the shell of a launch that never arrived: its tab is
// closed, as after any failed launch.
func (r *Runner) launchArrived(ctx context.Context, ts *targetState, run store.Run, panes []herdr.PaneInfo, log *slog.Logger) (bool, error) {
	i := slices.IndexFunc(panes, func(p herdr.PaneInfo) bool {
		return p.PaneID == run.HerdrPane && p.TabID == run.HerdrTab
	})
	if i < 0 {
		return false, nil
	}
	if terminalReplaced(run, panes[i]) {
		return false, nil
	}
	tabs, err := ts.herdr.TabList(ctx, run.HerdrWorkspace)
	if herdr.IsCode(err, herdr.CodeWorkspaceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The label ends in the launch's HHMM, which the row does not store.
	prefix := "hc-" + run.TaskID + "-"
	if !slices.ContainsFunc(tabs, func(tab herdr.TabInfo) bool {
		return tab.TabID == run.HerdrTab && strings.HasPrefix(tab.Label, prefix)
	}) {
		return false, nil
	}
	if panes[i].Agent != nil {
		return true, nil
	}
	info, err := ts.herdr.PaneProcessInfo(ctx, run.HerdrPane)
	if err != nil {
		return false, err
	}
	if info.ShellPID == nil || info.ForegroundProcessGroupID == nil || *info.ShellPID != *info.ForegroundProcessGroupID {
		return true, nil
	}
	// The shell is at its prompt: the agent has exited already, or the
	// command was never typed. The launch line names the run's prompt file.
	out, err := ts.herdr.PaneRead(ctx, run.HerdrPane, tailLines)
	if err != nil {
		return false, err
	}
	// The launch line in the scrollback means this is still the terminal we
	// typed into. A herdr server restart restores the pane id as a new
	// shell with another terminal id and an empty scrollback (issue #34):
	// that pane fails this check, so it is not adopted and catch-up still
	// runs. Adopting it would take the fire, and the next poll would then
	// mark the run lost because the terminal id changed. If a restored
	// pane ever keeps the old scrollback, reject it here when the run's
	// stored terminal id differs, and do not close its tab.
	if strings.Contains(out, run.ID+".md") {
		return true, nil
	}
	log.Info("launch never arrived: closing its tab", "herdr_tab", run.HerdrTab)
	if err := ts.herdr.TabClose(ctx, run.HerdrTab); err != nil && !herdr.IsCode(err, herdr.CodeTabNotFound) {
		return false, err
	}
	if err := ts.t.RemoveFile(ctx, promptPath(run.ID)); err != nil {
		log.Warn("cannot delete prompt file", "err", err)
	}
	return false, nil
}

// adopt stores an ended host_unreachable run as running again and tracks
// it. Its start time stays, so the task's timeout counts from the launch.
// The prompt file is still on the target: the end of the run deletes it.
func (r *Runner) adopt(ctx context.Context, t config.Task, run store.Run, log *slog.Logger) (store.Run, error) {
	run.Status, run.ExitReason, run.EndedAt = store.StatusRunning, "", time.Time{}
	a := &activeRun{run: run, task: t, log: log}
	// a.mu keeps a poll off the run until its row says running.
	a.mu.Lock()
	defer a.mu.Unlock()
	r.mu.Lock()
	if _, tracked := r.active[run.ID]; tracked {
		r.mu.Unlock()
		return run, nil
	}
	r.active[run.ID] = a
	r.mu.Unlock()
	if err := r.store.UpdateRun(ctx, run); err != nil {
		r.mu.Lock()
		delete(r.active, run.ID)
		a.ended = true // a poll that waits for a.mu must skip it
		r.mu.Unlock()
		return store.Run{}, fmt.Errorf("adopt run %s: %w", run.ID, err)
	}
	r.changed(run)
	a.log.Info("run adopted: its launch reply was lost, its pane exists",
		"herdr_tab", run.HerdrTab, "herdr_pane", run.HerdrPane, "started_at", run.StartedAt)
	return run, nil
}
