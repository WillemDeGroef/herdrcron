package tui

import (
	"fmt"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/schedule"
)

// fireLayout shows a fire time, in the TUI's zone with its label, on the
// detail screen and in the form's schedule preview alike.
const fireLayout = "Mon Jan 2 15:04 MST"

// nextFires returns the next n fire times of expr after after, in the task
// time zone tz, or in defaultTZ (the daemon's config timezone) when tz is
// empty, or in fallback when both are empty. It stops early when the
// schedule never fires again. The TUI computes these itself: they are a
// preview of the schedule, not the daemon's plan (pauses are not applied).
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
