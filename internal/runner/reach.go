package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// Target reachability, the same strings as api.ReachUnknown,
// api.ReachReachable and api.ReachUnreachable.
const (
	ReachUnknown     = "unknown"
	ReachReachable   = "reachable"
	ReachUnreachable = "unreachable"
)

const (
	// backoffMin is the wait before the first retry of a target that
	// stopped answering; each failed retry doubles it, up to backoffMax.
	backoffMin = 30 * time.Second
	backoffMax = 5 * time.Minute
	// idleProbeEvery is how often a reachable target with no active runs
	// is probed, so its reachability does not go stale.
	idleProbeEvery = 5 * time.Minute
)

// TargetState is what the runner knows about a target's reachability.
type TargetState struct {
	Name         string
	Reachability string // ReachUnknown, ReachReachable or ReachUnreachable
	// UnreachableSince and NextRetryAt are set while unreachable.
	UnreachableSince time.Time
	NextRetryAt      time.Time
	// LastError is the error that made the target unreachable.
	LastError  string
	LastSeenAt time.Time // when the target last answered; zero if never
}

// reach is the reachability state machine of one target. It is guarded by
// targetState.reachMu.
type reach struct {
	state     string
	since     time.Time
	lastSeen  time.Time
	lastErr   string
	backoff   time.Duration
	nextRetry time.Time
}

// errKnownUnreachable is the launch error of a run on a target that is
// known to be unreachable and not yet due for a retry: the run is not
// tried, so a fire does not wait for ssh's connect timeout.
type errKnownUnreachable struct{ target, lastErr string }

func (e *errKnownUnreachable) Error() string {
	return fmt.Sprintf("target %s unreachable: %s", e.target, e.lastErr)
}

func (e *errKnownUnreachable) Is(t error) bool { return t == target.ErrUnreachable }

// launchObservation is what a launch result says about the target's
// reachability, and whether it says anything. A launch that succeeded
// reached the target; one that got an unreachable answer did not, unless
// it was not tried. Other errors may be local (a prompt file, a context),
// so they leave the state alone.
func launchObservation(err error) (error, bool) {
	var skipped *errKnownUnreachable
	switch {
	case err == nil:
		return nil, true
	case errors.As(err, &skipped):
		return nil, false
	case errors.Is(err, target.ErrUnreachable):
		return err, true
	default:
		return nil, false
	}
}

// observe feeds the result of a call on the target into its reachability.
// An error matching target.ErrUnreachable marks it unreachable and backs
// off: 30 s, doubling to 5 min. Anything else, nil or another error, means
// the target answered. A change of reachability calls OnTargetChange.
func (r *Runner) observe(name string, err error) {
	ts := r.targets[name]
	if ts == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		return // the daemon is stopping; the call says nothing about the target
	}
	// changeMu keeps a change and its OnTargetChange together, so two
	// observers cannot report their changes in the opposite order.
	ts.changeMu.Lock()
	defer ts.changeMu.Unlock()
	now := r.clock.Now()
	ts.reachMu.Lock()
	rs := &ts.reach
	prev := rs.state
	if errors.Is(err, target.ErrUnreachable) {
		if rs.state != ReachUnreachable {
			rs.state, rs.since, rs.backoff = ReachUnreachable, now, backoffMin
		} else {
			rs.backoff = min(2*rs.backoff, backoffMax)
		}
		rs.lastErr = err.Error()
		rs.nextRetry = now.Add(rs.backoff)
	} else {
		rs.state, rs.lastSeen, rs.backoff = ReachReachable, now, 0
		rs.since, rs.nextRetry, rs.lastErr = time.Time{}, time.Time{}, ""
	}
	st := ts.stateLocked(name)
	ts.reachMu.Unlock()

	switch {
	case st.Reachability == prev:
		if st.Reachability == ReachUnreachable {
			r.log.Info("target still unreachable", "target", name, "next_retry_at", st.NextRetryAt, "err", st.LastError)
		}
		return
	case st.Reachability == ReachUnreachable:
		r.log.Warn("target unreachable", "target", name, "next_retry_at", st.NextRetryAt, "err", st.LastError)
	case prev == ReachUnreachable:
		r.log.Info("target reachable again", "target", name)
	default:
		r.log.Info("target reachable", "target", name)
	}
	if r.onTargetChange != nil {
		r.onTargetChange(st)
	}
}

// stateLocked returns the target's state; ts.reachMu must be held.
func (ts *targetState) stateLocked(name string) TargetState {
	rs := ts.reach
	st := TargetState{Name: name, Reachability: rs.state, LastError: rs.lastErr, LastSeenAt: rs.lastSeen}
	if st.Reachability == "" {
		st.Reachability = ReachUnknown
	}
	if rs.state == ReachUnreachable {
		st.UnreachableSince, st.NextRetryAt = rs.since, rs.nextRetry
	}
	return st
}

// TargetState returns the reachability of the named target; false for a
// target the runner does not have.
func (r *Runner) TargetState(name string) (TargetState, bool) {
	ts := r.targets[name]
	if ts == nil {
		return TargetState{}, false
	}
	ts.reachMu.Lock()
	defer ts.reachMu.Unlock()
	return ts.stateLocked(name), true
}

// knownUnreachable returns an error matching target.ErrUnreachable if the
// target is unreachable and its next retry is not due yet.
func (r *Runner) knownUnreachable(ts *targetState, name string) error {
	ts.reachMu.Lock()
	defer ts.reachMu.Unlock()
	if ts.reach.state == ReachUnreachable && r.clock.Now().Before(ts.reach.nextRetry) {
		return &errKnownUnreachable{target: name, lastErr: ts.reach.lastErr}
	}
	return nil
}

// pollWait is how long the target's poll worker waits before its next
// poll: until the next retry while the target is unreachable, else the
// poll interval.
func (r *Runner) pollWait(name string, interval time.Duration) time.Duration {
	ts := r.targets[name]
	ts.reachMu.Lock()
	defer ts.reachMu.Unlock()
	if ts.reach.state != ReachUnreachable {
		return interval
	}
	return max(ts.reach.nextRetry.Sub(r.clock.Now()), 0)
}

// probe checks whether a target with no active runs answers, when its
// reachability is unknown, it is unreachable, or it was last seen more
// than idleProbeEvery ago. `herdr pane list` is the probe: any answer,
// even "server not running", means the target is reachable.
func (r *Runner) probe(ctx context.Context, name string) {
	ts := r.targets[name]
	ts.reachMu.Lock()
	due := ts.reach.state != ReachReachable || !r.clock.Now().Before(ts.reach.lastSeen.Add(idleProbeEvery))
	ts.reachMu.Unlock()
	if !due {
		return
	}
	_, err := ts.herdr.PaneList(ctx)
	r.observe(name, err)
}
