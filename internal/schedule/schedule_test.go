package schedule_test

import (
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/schedule"
)

func brussels(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// fires returns the next n fire times after start, formatted in loc.
func fires(t *testing.T, expr string, loc *time.Location, start time.Time, n int) []string {
	t.Helper()
	s, err := schedule.Parse(expr, loc)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	var out []string
	at := start
	for range n {
		next := s.Next(at)
		if !next.After(at) {
			t.Fatalf("Next(%v) = %v, not after", at, next)
		}
		out = append(out, next.In(loc).Format("2006-01-02 15:04 MST"))
		at = next
	}
	return out
}

func assertFires(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d fires %q, want %q", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fire %d = %s, want %s (all: %q)", i, got[i], want[i], got)
		}
	}
}

func TestCronInTaskTimezone(t *testing.T) {
	loc := brussels(t)
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, loc) // a Friday
	assertFires(t, fires(t, "0 7 * * 1", loc, start, 2),
		"2026-11-09 07:00 CET", "2026-11-16 07:00 CET")
}

func TestDescriptors(t *testing.T) {
	loc := brussels(t)
	start := time.Date(2026, 11, 6, 12, 10, 0, 0, loc)
	assertFires(t, fires(t, "@daily", loc, start, 2), "2026-11-07 00:00 CET", "2026-11-08 00:00 CET")
	assertFires(t, fires(t, "@hourly", loc, start, 2), "2026-11-06 13:00 CET", "2026-11-06 14:00 CET")
	assertFires(t, fires(t, "@every 2h", loc, start, 2), "2026-11-06 14:10 CET", "2026-11-06 16:10 CET")
}

func TestSameExpressionDiffersByTimezone(t *testing.T) {
	start := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := schedule.Parse("0 7 * * *", brussels(t))
	n, _ := schedule.Parse("0 7 * * *", ny)
	if got, want := b.Next(start).UTC(), time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Brussels: %v, want %v", got, want)
	}
	if got, want := n.Next(start).UTC(), time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("New York: %v, want %v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{"", "* * *", "61 * * * *", "@every nope", "@weekly-ish", "CRON_TZ=UTC 0 7 * * *"} {
		if _, err := schedule.Parse(expr, time.UTC); err == nil {
			t.Errorf("Parse(%q): want error", expr)
		}
	}
	if _, err := schedule.Parse("@daily", nil); err == nil {
		t.Error("Parse with nil location: want error")
	}
}

// Brussels switches from CET to CEST on the last Sunday of March: on
// 2026-03-29 the wall clock jumps from 02:00 to 03:00.
func TestSpringForwardBrussels(t *testing.T) {
	loc := brussels(t)
	start := time.Date(2026, 3, 28, 12, 0, 0, 0, loc)

	t.Run("a fixed time in the gap fires once, when the gap ends", func(t *testing.T) {
		assertFires(t, fires(t, "30 2 * * *", loc, start, 3),
			"2026-03-29 03:00 CEST", "2026-03-30 02:30 CEST", "2026-03-31 02:30 CEST")
	})
	t.Run("several gap minutes still fire only once", func(t *testing.T) {
		assertFires(t, fires(t, "*/20 2 * * *", loc, start, 4),
			"2026-03-29 03:00 CEST", "2026-03-30 02:00 CEST", "2026-03-30 02:20 CEST", "2026-03-30 02:40 CEST")
	})
	t.Run("hourly skips the hour that does not exist", func(t *testing.T) {
		assertFires(t, fires(t, "@hourly", loc, time.Date(2026, 3, 29, 0, 30, 0, 0, loc), 3),
			"2026-03-29 01:00 CET", "2026-03-29 03:00 CEST", "2026-03-29 04:00 CEST")
	})
	t.Run("times outside the gap are unaffected", func(t *testing.T) {
		assertFires(t, fires(t, "30 1 * * *", loc, start, 2), "2026-03-29 01:30 CET", "2026-03-30 01:30 CEST")
		assertFires(t, fires(t, "0 3 * * *", loc, start, 2), "2026-03-29 03:00 CEST", "2026-03-30 03:00 CEST")
		assertFires(t, fires(t, "@daily", loc, start, 2), "2026-03-29 00:00 CET", "2026-03-30 00:00 CEST")
	})
	t.Run("a gap time on another weekday does not fire on the switch day", func(t *testing.T) {
		// 02:30 on Mondays only; the switch is on a Sunday.
		assertFires(t, fires(t, "30 2 * * 1", loc, start, 1), "2026-03-30 02:30 CEST")
	})
	t.Run("@every counts real elapsed time", func(t *testing.T) {
		assertFires(t, fires(t, "@every 1h", loc, time.Date(2026, 3, 29, 1, 30, 0, 0, loc), 2),
			"2026-03-29 03:30 CEST", "2026-03-29 04:30 CEST")
	})
}

// Brussels switches back from CEST to CET on the last Sunday of October: on
// 2026-10-25 the wall clock goes from 03:00 back to 02:00, so 02:00-02:59
// happens twice.
func TestFallBackBrussels(t *testing.T) {
	loc := brussels(t)
	start := time.Date(2026, 10, 24, 12, 0, 0, 0, loc)

	t.Run("a fixed time in the repeated hour fires once", func(t *testing.T) {
		assertFires(t, fires(t, "30 2 * * *", loc, start, 2), "2026-10-25 02:30 CEST", "2026-10-26 02:30 CET")
	})
	t.Run("several minutes in the repeated hour fire once each", func(t *testing.T) {
		assertFires(t, fires(t, "*/30 2 * * *", loc, start, 3),
			"2026-10-25 02:00 CEST", "2026-10-25 02:30 CEST", "2026-10-26 02:00 CET")
	})
	t.Run("hourly fires in both copies of the hour", func(t *testing.T) {
		assertFires(t, fires(t, "@hourly", loc, time.Date(2026, 10, 25, 1, 30, 0, 0, loc), 4),
			"2026-10-25 02:00 CEST", "2026-10-25 02:00 CET", "2026-10-25 03:00 CET", "2026-10-25 04:00 CET")
	})
	t.Run("times outside the repeated hour are unaffected", func(t *testing.T) {
		assertFires(t, fires(t, "0 7 * * *", loc, start, 2), "2026-10-25 07:00 CET", "2026-10-26 07:00 CET")
		assertFires(t, fires(t, "@daily", loc, start, 2), "2026-10-25 00:00 CEST", "2026-10-26 00:00 CET")
	})
	t.Run("starting inside the second copy does not refire", func(t *testing.T) {
		// 02:10 CET is after the CEST 02:30 fire already happened.
		inSecond := time.Date(2026, 10, 25, 1, 10, 0, 0, time.UTC) // 02:10 CET
		assertFires(t, fires(t, "30 2 * * *", loc, inSecond, 1), "2026-10-26 02:30 CET")
	})
	t.Run("@every counts real elapsed time", func(t *testing.T) {
		assertFires(t, fires(t, "@every 1h", loc, time.Date(2026, 10, 25, 1, 30, 0, 0, loc), 3),
			"2026-10-25 02:30 CEST", "2026-10-25 02:30 CET", "2026-10-25 03:30 CET")
	})
}
