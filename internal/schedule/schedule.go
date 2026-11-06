// Package schedule parses task schedules and computes their fire times in
// the task's time zone.
//
// A schedule is a 5-field cron expression or a descriptor: @every <dur>,
// @daily, @hourly (and the other robfig/cron descriptors). Cron expressions
// follow the wall clock of the time zone, with two rules for daylight
// saving time changes:
//
//   - Spring forward: a fire time that falls in the skipped hour fires once,
//     at the first instant after the jump (02:30 fires at 03:00). Several
//     fire times in the skipped hour still fire only once.
//   - Fall back: a fire time in the repeated hour fires once, in the first
//     copy of that hour. Schedules with * in the hour field (such as
//     @hourly) fire in both copies, because each copy is a real hour.
//
// @every counts real elapsed time and ignores the wall clock.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// MinEvery is the shortest @every interval: a run is a whole agent session.
const MinEvery = time.Minute

// starBit is set by robfig/cron in a field bitmask when the field was "*".
const starBit = 1 << 63

// Schedule is a parsed schedule bound to a time zone.
type Schedule struct {
	spec  *cron.SpecSchedule          // cron expressions and @daily, @hourly, ...
	every *cron.ConstantDelaySchedule // @every
}

// Parse parses expr in time zone loc. The time zone comes from the task's
// timezone field, so a CRON_TZ= prefix is refused.
func Parse(expr string, loc *time.Location) (Schedule, error) {
	if loc == nil {
		return Schedule{}, errors.New("no time zone")
	}
	if strings.HasPrefix(expr, "CRON_TZ=") || strings.HasPrefix(expr, "TZ=") {
		return Schedule{}, errors.New("set the time zone with the timezone field, not a CRON_TZ= prefix")
	}
	parsed, err := cron.ParseStandard(expr)
	if err != nil {
		return Schedule{}, err
	}
	switch s := parsed.(type) {
	case *cron.SpecSchedule:
		spec := *s
		spec.Location = loc
		return Schedule{spec: &spec}, nil
	case cron.ConstantDelaySchedule:
		if s.Delay < MinEvery {
			return Schedule{}, fmt.Errorf("@every interval must be at least %v", MinEvery)
		}
		return Schedule{every: &s}, nil
	}
	return Schedule{}, fmt.Errorf("unsupported schedule %q", expr)
}

// Next returns the first fire time strictly after after, or the zero time
// if the schedule never fires again (robfig/cron looks five years ahead).
func (s Schedule) Next(after time.Time) time.Time {
	if s.every != nil {
		return s.every.Next(after)
	}
	after = after.In(s.spec.Location)
	for {
		next := s.spec.Next(after)
		if next.IsZero() {
			return next
		}
		if gap, ok := s.gapFire(after, next); ok {
			return gap
		}
		if !s.repeatedWallTime(next) {
			return next
		}
		after = next
	}
}

// gapFire reports whether a fire time was skipped by a spring-forward
// jump between after and next, and returns the instant the jump ends.
func (s Schedule) gapFire(after, next time.Time) (time.Time, bool) {
	_, end := after.ZoneBounds()
	for !end.IsZero() && !end.After(next) {
		_, before := end.Add(-time.Second).Zone()
		_, since := end.Zone()
		if since > before {
			// Evaluate the wall clock as if the jump had not happened: in a
			// fixed zone with the old offset, the skipped wall times are the
			// instants [end, end+jump).
			old := *s.spec
			old.Location = time.FixedZone("", before)
			jump := time.Duration(since-before) * time.Second
			if m := old.Next(end.Add(-time.Second)); !m.IsZero() && m.Before(end.Add(jump)) {
				return end, true
			}
		}
		_, end = end.ZoneBounds()
	}
	return time.Time{}, false
}

// repeatedWallTime reports whether t is in the second copy of an hour
// repeated by a fall-back change, for a schedule that must fire only once
// in it.
func (s Schedule) repeatedWallTime(t time.Time) bool {
	if s.spec.Hour&starBit != 0 {
		return false
	}
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return false
	}
	_, prev := start.Add(-time.Second).Zone()
	_, cur := t.Zone()
	back := time.Duration(prev-cur) * time.Second
	return back > 0 && t.Sub(start) < back
}
