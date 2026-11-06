package until

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "2026-11-10T08:00", want: "2026-11-10T08:00:00+01:00"},
		{in: "2026-11-10T08:00:30", want: "2026-11-10T08:00:30+01:00"},
		{in: "2026-11-10 08:00", want: "2026-11-10T08:00:00+01:00"},
		{in: "2026-11-10", want: "2026-11-10T00:00:00+01:00"},
		{in: "2026-11-10T08:00:00Z", want: "2026-11-10T08:00:00Z"},
		{in: "2026-11-10T08:00:00+05:30", want: "2026-11-10T08:00:00+05:30"},
		{in: "2026-11-06T12:30", err: "past"}, // 11:30Z in Brussels
		{in: "2026-11-06T14:00", want: "2026-11-06T14:00:00+01:00"},
		{in: "2026-11-06T12:00:00Z", err: "past"},
		{in: "2026-11-06", err: "past"},
		{in: "2025-01-01T00:00:00Z", err: "past"},
		{in: "tomorrow", err: "tomorrow"},
		{in: "2026-13-01", err: "2026-13-01"},
		{in: "", err: "empty"},
	} {
		got, err := Parse(tc.in, now, loc)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("Parse(%q) = %q, %v; want error containing %q", tc.in, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
