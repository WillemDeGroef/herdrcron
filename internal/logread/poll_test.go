package logread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rotateBeforeStat appends line to path and renames it away on the nth
// statPath call, which is the window between drain and the inode check.
func rotateBeforeStat(t *testing.T, path, line string, n int, createNew bool) {
	t.Helper()
	orig := statPath
	calls := 0
	statPath = func(name string) (os.FileInfo, error) {
		calls++
		if calls == n {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Errorf("append: %v", err)
			} else if _, err := f.WriteString(line); err != nil {
				t.Errorf("write: %v", err)
				f.Close()
			} else if err := f.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			if err := os.Rename(path, path+".1"); err != nil {
				t.Errorf("rename: %v", err)
			}
			if createNew {
				if err := os.WriteFile(path, []byte("ccc\n"), 0o644); err != nil {
					t.Errorf("new file: %v", err)
				}
			}
		}
		return os.Stat(name)
	}
	t.Cleanup(func() { statPath = orig })
}

func newTailer(path string, got *[]string) *tailer {
	return &tailer{path: path, emit: func(line []byte) error {
		*got = append(*got, string(line))
		return nil
	}}
}

func TestPollReadsLineWrittenBeforeRotationIsObserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("aaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := newTailer(path, &got)
	// The second stat is the one inside the second poll, after it has
	// already drained what it could see.
	rotateBeforeStat(t, path, "bbb\n", 2, true)

	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "aaa|bbb|ccc" {
		t.Fatalf("got %q, want aaa, bbb, ccc", got)
	}
}

func TestPollReadsLineWhenLiveFileDisappears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("aaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	tr := newTailer(path, &got)
	rotateBeforeStat(t, path, "bbb\n", 2, false)

	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "aaa|bbb" {
		t.Fatalf("got %q, want aaa, bbb", got)
	}
}
