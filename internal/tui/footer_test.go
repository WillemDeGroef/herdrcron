package tui

import (
	"strings"
	"testing"
)

func TestFooterShowsTheSpecKeysWhenWide(t *testing.T) {
	got := footerKeys(120)
	want := "space on/off  p pause  r run now  n new  e edit  d delete  enter details  a attach  / filter  ? help  q quit"
	if got != want {
		t.Errorf("footer = %q, want %q", got, want)
	}
}

func TestFooterDropsKeysButKeepsHelpAndQuitWhenNarrow(t *testing.T) {
	got := footerKeys(60)
	if len(" "+got) > 60 {
		t.Errorf("footer %q is wider than 60", got)
	}
	if !strings.HasPrefix(got, "space on/off") || !strings.HasSuffix(got, "? help  q quit") {
		t.Errorf("footer = %q, want it to start with space on/off and end with ? help  q quit", got)
	}
}
