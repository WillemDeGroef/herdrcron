// Package until parses pause "until" times shared by the CLI and TUI.
package until

import (
	"errors"
	"fmt"
	"time"
)

// layouts are the zone-less forms accepted alongside RFC 3339; read in loc.
var layouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"}

// Parse turns s into an RFC 3339 time after now. s is RFC 3339 with a zone,
// or one of layouts read in loc.
func Parse(s string, now time.Time, loc *time.Location) (string, error) {
	if s == "" {
		return "", errors.New("empty time")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		parsed := false
		for _, layout := range layouts {
			if lt, lerr := time.ParseInLocation(layout, s, loc); lerr == nil {
				t, parsed = lt, true
				break
			}
		}
		if !parsed {
			return "", fmt.Errorf("cannot read %q as a time; use RFC 3339 (2026-11-10T08:00:00Z) or local 2026-11-10T08:00, \"2026-11-10 08:00\" or 2026-11-10", s)
		}
	}
	if !t.After(now) {
		return "", fmt.Errorf("%s is in the past", t.Format(time.RFC3339))
	}
	return t.Format(time.RFC3339), nil
}
