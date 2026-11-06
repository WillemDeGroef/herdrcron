// Package shellquote quotes strings so a POSIX shell reads them back as
// exactly one word each, with no expansion.
package shellquote

import "strings"

// Quote returns s quoted for a POSIX shell. A non-empty s made only of
// [A-Za-z0-9_@%+=:,./-] is returned unchanged; anything else is wrapped in
// single quotes, with each single quote written as close-quote,
// backslash-quote, open-quote:
//
//	it's  ->  'it'\''s'
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if isSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes each arg and joins them with single spaces.
func Join(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = Quote(a)
	}
	return strings.Join(quoted, " ")
}

func isSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("_@%+=:,./-", c) >= 0:
		default:
			return false
		}
	}
	return true
}
