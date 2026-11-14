package runner

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// lostCreate is a tab create or workspace create that got no answer. herdr
// may have made the tab, and no run knows its id: it is an orphan until
// closeOrphanTabs finds and closes it.
type lostCreate struct {
	c store.LostCreate
	// gen is the generation of the targetState the create was sent to. A
	// replaced target is another machine: the create is dropped, not looked
	// for there.
	gen uint64
}

func tabIDs(tabs []herdr.TabInfo) []string {
	ids := make([]string, len(tabs))
	for i, t := range tabs {
		ids[i] = t.TabID
	}
	return ids
}

func workspaceIDs(workspaces []herdr.WorkspaceInfo) []string {
	ids := make([]string, len(workspaces))
	for i, w := range workspaces {
		ids[i] = w.WorkspaceID
	}
	return ids
}

// herdrAnswered reports whether err is an error herdr itself returned. It
// then did not do what was asked, and asking again gives the same answer.
// Any other error (the target did not answer, a timeout, a cancelled
// context, output that cannot be read) leaves open what herdr did.
func herdrAnswered(err error) bool {
	var he *herdr.Error
	return errors.As(err, &he)
}

// createLost records that the create of a's tab failed with err, unless
// herdr answered with an error and so created nothing. c says where the tab
// would be and which ids were there before. The caller holds ts.openMu.
//
// A daemon that dies between sending the create and this record leaves the
// tab unrecorded.
func (r *Runner) createLost(ctx context.Context, ts *targetState, a *activeRun, err error, c store.LostCreate) {
	if herdrAnswered(err) {
		return
	}
	c.RunID, c.TaskID, c.Target, c.Host = a.run.ID, a.run.TaskID, a.run.Target, ts.conf.Host
	a.log.Warn("no answer to the create; its tab may exist, and is closed when the target answers again",
		"label", c.Label, "err", err)
	r.mu.Lock()
	r.lostCreates[c.RunID] = &lostCreate{c: c, gen: ts.gen}
	r.mu.Unlock()
	// The launch has failed by now; the record must not be lost with ctx.
	if err := r.store.AddLostCreate(context.WithoutCancel(ctx), c); err != nil {
		a.log.Error("cannot store the lost create; it is forgotten if the daemon restarts", "err", err)
	}
}

// tabOfLiveRun reports whether a run that is starting or active has the tab.
func (r *Runner) tabOfLiveRun(tab string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range []map[string]*activeRun{r.starting, r.active} {
		for _, a := range m {
			if a.run.HerdrTab == tab {
				return true
			}
		}
	}
	return false
}

// lostCreatesOf returns the lost creates sent to ts, oldest run first.
func (r *Runner) lostCreatesOf(ts *targetState, name string) []store.LostCreate {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.LostCreate
	for _, lc := range r.lostCreates {
		if lc.c.Target == name && lc.gen == ts.gen {
			out = append(out, lc.c)
		}
	}
	slices.SortFunc(out, func(a, b store.LostCreate) int { return strings.Compare(a.RunID, b.RunID) })
	return out
}

// closeOrphanTabs settles the lost creates on the target: the tab each one
// left behind is closed, and the create is forgotten. The caller holds
// ts.openMu, so no launch opens a tab meanwhile. An error means the target
// could not be asked, or its herdr server is not running: that create and
// the ones after it stay recorded.
func (r *Runner) closeOrphanTabs(ctx context.Context, ts *targetState, name string) error {
	for _, c := range r.lostCreatesOf(ts, name) {
		log := logging.ForRun(r.log, c.RunID, c.TaskID, c.Target)
		tab, err := r.findOrphanTab(ctx, ts, c, log)
		if err == nil && tab != "" && r.tabOfLiveRun(tab) {
			// Two targets on one herdr server, for example: not an orphan.
			log.Warn("the new tab with the label of a lost create belongs to a run; leaving it open", "herdr_tab", tab)
			tab = ""
		}
		if err == nil && tab != "" {
			log.Info("closing the tab of a create whose reply was lost", "herdr_tab", tab, "label", c.Label)
			err = ts.herdr.TabClose(ctx, tab)
		}
		switch {
		case err == nil, herdr.IsCode(err, herdr.CodeTabNotFound), herdr.IsCode(err, herdr.CodeWorkspaceNotFound):
			// Closed, or there is nothing to close.
		case herdr.IsCode(err, herdr.CodeServerNotRunning):
			// A server that starts again restores its tabs: ask it then.
			return err
		case herdrAnswered(err):
			log.Warn("cannot close the tab of a lost create; giving up on it", "label", c.Label, "err", err)
		default:
			return err
		}
		r.mu.Lock()
		delete(r.lostCreates, c.RunID)
		r.mu.Unlock()
		// Settled on the target: forget it even if ctx has ended since.
		if err := r.store.DeleteLostCreate(context.WithoutCancel(ctx), c.RunID); err != nil {
			log.Error("cannot delete the lost create", "err", err)
		}
	}
	return nil
}

// findOrphanTab returns the id of the tab the lost create c made, or "" if
// there is none to close: the create never arrived, or the tab cannot be
// told from one herdrcron did not create.
//
// The tab of a tab create is the one tab in the workspace that carries the
// run's label and was not there before the create. The label alone is not
// enough: the kept tab of an earlier run of the task can carry the same
// one. With two such tabs, neither is closed.
//
// A workspace create makes the hc- workspace and its first tab together.
// The workspace is the one with the label that was not there before, and
// its tab is closed only while it is the workspace's only tab.
func (r *Runner) findOrphanTab(ctx context.Context, ts *targetState, c store.LostCreate, log *slog.Logger) (string, error) {
	wsID := c.Workspace
	if wsID == "" {
		workspaces, err := ts.herdr.WorkspaceList(ctx)
		if err != nil {
			return "", err
		}
		workspaces = slices.DeleteFunc(workspaces, func(w herdr.WorkspaceInfo) bool {
			return w.Label != c.Label || slices.Contains(c.Known, w.WorkspaceID)
		})
		if len(workspaces) != 1 {
			logOrphanCount(log, "workspace", c.Label, len(workspaces))
			return "", nil
		}
		wsID = workspaces[0].WorkspaceID
	}
	tabs, err := ts.herdr.TabList(ctx, wsID)
	if err != nil {
		return "", err
	}
	if c.Workspace == "" {
		if len(tabs) != 1 {
			log.Warn("the workspace of a lost create has other tabs by now; leaving it open",
				"herdr_workspace", wsID, "label", c.Label, "tabs", len(tabs))
			return "", nil
		}
		return tabs[0].TabID, nil
	}
	tabs = slices.DeleteFunc(tabs, func(t herdr.TabInfo) bool {
		return t.Label != c.Label || slices.Contains(c.Known, t.TabID)
	})
	if len(tabs) != 1 {
		logOrphanCount(log, "tab", c.Label, len(tabs))
		return "", nil
	}
	return tabs[0].TabID, nil
}

// logOrphanCount logs why a lost create has nothing to close: n is 0 or
// more than 1.
func logOrphanCount(log *slog.Logger, kind, label string, n int) {
	if n == 0 {
		log.Info("the create whose reply was lost never arrived: no new "+kind+" has its label", "label", label)
		return
	}
	log.Warn("more than one new "+kind+" has the label of a lost create; leaving them open", "label", label, "count", n)
}

// resumeLostCreates closes the tabs of the lost creates on the target if
// it is reachable. While a launch holds openMu it does nothing: the launch
// closes them itself.
func (r *Runner) resumeLostCreates(ctx context.Context, name string) {
	ts := r.target(name)
	if ts == nil || len(r.lostCreatesOf(ts, name)) == 0 {
		return
	}
	if st, ok := r.TargetState(name); !ok || st.Reachability != ReachReachable {
		return
	}
	if !ts.openMu.TryLock() {
		return
	}
	defer ts.openMu.Unlock()
	err := r.closeOrphanTabs(ctx, ts, name)
	if err != nil && !herdr.IsCode(err, herdr.CodeServerNotRunning) {
		r.log.Warn("cannot close the tab of a lost create; it is tried again", logging.KeyTarget, name, "err", err)
		_ = r.asUnreachable(name, err)
	}
}

// recoverLostCreates takes over the lost creates in the store and returns
// their targets. One whose target is no longer configured, or has another
// host than when the create was sent, is dropped: its tab is not looked for
// on another machine.
func (r *Runner) recoverLostCreates(ctx context.Context) ([]string, error) {
	recs, err := r.store.ListLostCreates(ctx)
	if err != nil {
		return nil, err
	}
	var targets []string
	var gone []store.LostCreate
	for _, c := range recs {
		ts := r.target(c.Target)
		if ts == nil || ts.conf.Host != c.Host {
			gone = append(gone, c)
			continue
		}
		r.mu.Lock()
		if _, ok := r.lostCreates[c.RunID]; !ok {
			r.lostCreates[c.RunID] = &lostCreate{c: c, gen: ts.gen}
		}
		r.mu.Unlock()
		if !slices.Contains(targets, c.Target) {
			targets = append(targets, c.Target)
		}
	}
	r.dropLostCreates(gone)
	return targets, nil
}

// staleLostCreatesLocked takes out the lost creates whose target was
// removed or replaced, and returns them for dropLostCreates. The caller
// holds r.mu.
func (r *Runner) staleLostCreatesLocked() []store.LostCreate {
	var stale []store.LostCreate
	for id, lc := range r.lostCreates {
		if ts := r.target(lc.c.Target); ts == nil || ts.gen != lc.gen {
			stale = append(stale, lc.c)
			delete(r.lostCreates, id)
		}
	}
	return stale
}

// dropLostCreates forgets lost creates whose target is gone or changed.
// Their tabs, if they exist, stay open on the old target.
func (r *Runner) dropLostCreates(gone []store.LostCreate) {
	for _, c := range gone {
		log := logging.ForRun(r.log, c.RunID, c.TaskID, c.Target)
		log.Warn("target removed or changed; the tab of a create whose reply was lost may still be open there", "label", c.Label)
		if err := r.store.DeleteLostCreate(context.Background(), c.RunID); err != nil {
			log.Error("cannot delete the lost create", "err", err)
		}
	}
}
