package tui

import (
	"testing"
	"time"
)

func TestNextFiresUsesTaskZoneThenDefault(t *testing.T) {
	after := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC) // a Thursday
	got, err := nextFires("0 7 * * 1", "Europe/Brussels", "", time.UTC, after, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-11-09T06:00:00Z", "2026-11-16T06:00:00Z"}
	for i, w := range want {
		if got[i].UTC().Format(time.RFC3339) != w {
			t.Errorf("fire %d = %s, want %s", i, got[i].UTC().Format(time.RFC3339), w)
		}
	}
	got, err = nextFires("@daily", "", "Europe/Brussels", time.UTC, after, 1)
	if err != nil || got[0].UTC().Format(time.RFC3339) != "2026-11-05T23:00:00Z" {
		t.Errorf("default zone: %v %v", got, err)
	}
	if _, err := nextFires("61 * * * *", "", "", time.UTC, after, 3); err == nil {
		t.Error("bad schedule: want error")
	}
	if _, err := nextFires("@daily", "Mars/Olympus", "", time.UTC, after, 3); err == nil {
		t.Error("bad zone: want error")
	}
}
