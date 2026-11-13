package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/schedule"
)

// fireLayout shows a fire time, in the TUI's zone with its label, on the
// detail screen and in the form's schedule preview alike.
const fireLayout = "Mon Jan 2 15:04 MST"

// nextFires returns the next n fire times of expr after after, in the task
// time zone tz, or in defaultTZ (the daemon's config timezone) when tz is
// empty, or in fallback when both are empty. It stops early when the
// schedule never fires again. The TUI computes these itself: they are a
// preview of the schedule, not the daemon's plan. Callers apply pauses with
// hold: they show no fires under a pause without an end, and pass
// hold.from as after for a timed one.
func nextFires(expr, tz, defaultTZ string, fallback *time.Location, after time.Time, n int) ([]time.Time, error) {
	loc := fallback
	if tz == "" {
		tz = defaultTZ
	}
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("unknown time zone %q", tz)
		}
		loc = l
	}
	s, err := schedule.Parse(expr, loc)
	if err != nil {
		return nil, err
	}
	var out []time.Time
	for t := after; len(out) < n; {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

// hold is what keeps a task's schedule from firing: a global pause, or the
// task's own paused_until. The zero hold is no pause.
type hold struct {
	// global is the scope of a global pause that covers the task: "all
	// targets" or "target <name>". A global pause has no end.
	global string
	// indefinite is a paused_until without an end.
	indefinite bool
	// until is the end of a timed pause; zero without one.
	until time.Time
}

// holdOn returns the hold on a task on target with the given paused_until,
// read from the daemon status and the clock. A global pause wins over the
// task's own pause: it lasts until someone resumes it. A paused_until that
// has passed or does not parse is no hold.
func (m Model) holdOn(target, pausedUntil string) hold {
	switch {
	case m.status.PausedAll:
		return hold{global: "all targets"}
	case target != "" && contains(m.status.PausedTargets, target):
		return hold{global: "target " + target}
	case pausedUntil == api.PauseIndefinite:
		return hold{indefinite: true}
	}
	until, err := time.Parse(time.RFC3339Nano, pausedUntil)
	if err != nil || !until.After(m.now()) {
		return hold{}
	}
	return hold{until: until}
}

// none reports whether nothing holds the schedule.
func (h hold) none() bool { return h == hold{} }

// endless reports whether the hold has no end: no fire can be previewed.
func (h hold) endless() bool { return h.global != "" || h.indefinite }

// from is the time after which the schedule expr fires again: the end of a
// timed pause, else now. A cron fire at the very end of the pause runs, so
// the result is just before it. An @every interval counts from the end of
// the pause.
func (h hold) from(expr string, now time.Time) time.Time {
	switch {
	case h.until.IsZero():
		return now
	case strings.HasPrefix(strings.TrimSpace(expr), "@every"):
		return h.until
	}
	return h.until.Add(-time.Nanosecond)
}

// text says what holds the schedule, in loc: a full line for an endless
// hold, and the lead-in to the fire times after a timed pause. It is empty
// for no hold.
func (h hold) text(loc *time.Location) string {
	switch {
	case h.global == "all targets":
		return "all targets are paused: no fires until resumed"
	case h.global != "":
		return h.global + " is paused: no fires until resumed"
	case h.indefinite:
		return "paused indefinitely: no fires until resumed"
	case !h.until.IsZero():
		return "paused until " + h.until.In(loc).Format(fireLayout) + ", then"
	}
	return ""
}
