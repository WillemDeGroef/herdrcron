package shellquote_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/shellquote"
)

func TestQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"abc", "abc"},
		{"A-Za_z0.9/@%+=:,", "A-Za_z0.9/@%+=:,"},
		{"-n", "-n"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"'", `''\'''`},
		{"$HOME", "'$HOME'"},
		{"~", "'~'"},
		{"a\nb", "'a\nb'"},
		{"é", "'é'"},
	}
	for _, c := range cases {
		if got := shellquote.Quote(c.in); got != c.want {
			t.Errorf("Quote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJoin(t *testing.T) {
	if got, want := shellquote.Join("echo", "a b", "", "c"), "echo 'a b' '' c"; got != want {
		t.Errorf("Join = %q, want %q", got, want)
	}
	if got := shellquote.Join(); got != "" {
		t.Errorf("Join() = %q, want empty", got)
	}
}

func TestRoundTripThroughShell(t *testing.T) {
	args := []string{
		"plain",
		"single ' quote",
		`double " quote`,
		"$HOME",
		"$(id)",
		"`id`",
		"new\nline",
		"tab\there",
		"*",
		"~",
		"~/x",
		"!",
		`back\slash\\`,
		"-n",
		"--flag=1",
		"unicode é 日本 🙂",
		"",
		"''",
		`'\''`,
		"; rm -rf / #",
		"a b  c",
	}
	out, err := exec.Command("/bin/sh", "-c", "printf '%s\\0' "+shellquote.Join(args...)).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	got := strings.Split(string(out), "\x00")
	if len(got) == 0 || got[len(got)-1] != "" {
		t.Fatalf("output not NUL-terminated: %q", out)
	}
	got = got[:len(got)-1]
	if !slices.Equal(got, args) {
		t.Errorf("round trip mismatch:\n got %q\nwant %q", got, args)
	}
}
