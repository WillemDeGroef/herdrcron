package config

import "time"

// PauseEnd returns the end of a timed pause: the paused_until time, and
// true. It returns false for no pause, "indefinite" and an unparsable value.
func (t Task) PauseEnd() (time.Time, bool) {
	until, _, err := parsePausedUntil(t.PausedUntil)
	if err != nil || until.IsZero() {
		return time.Time{}, false
	}
	return until, true
}
