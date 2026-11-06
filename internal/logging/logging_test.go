package logging_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/logging"
)

func openSize(t *testing.T, path string, maxBytes int64, keep int) *logging.RotatingFile {
	t.Helper()
	f, err := logging.OpenSize(path, maxBytes, keep)
	if err != nil {
		t.Fatalf("OpenSize: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func write(t *testing.T, f *logging.RotatingFile, s string) {
	t.Helper()
	n, err := f.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%q) = %d, %v", s, n, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestDefaultsMatchSpec(t *testing.T) {
	if logging.MaxBytes != 10<<20 || logging.Keep != 5 {
		t.Fatalf("MaxBytes=%d Keep=%d, want 10 MB and 5", logging.MaxBytes, logging.Keep)
	}
}

func TestOpenCreatesFileWithMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f, err := logging.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	write(t, f, "hello\n")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	if got := read(t, path); got != "hello\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestRotatesWhenWriteWouldExceedMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f := openSize(t, path, 10, 5)
	write(t, f, "aaaa\n")
	write(t, f, "bbbb\n") // exactly 10: fits
	if exists(path + ".1") {
		t.Fatal("rotated before reaching the threshold")
	}
	write(t, f, "c\n") // 12 > 10: rotate first
	if got := read(t, path+".1"); got != "aaaa\nbbbb\n" {
		t.Fatalf("daemon.log.1 = %q", got)
	}
	if got := read(t, path); got != "c\n" {
		t.Fatalf("daemon.log = %q", got)
	}
}

func TestNoFileExceedsMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f := openSize(t, path, 20, 5)
	for i := range 30 {
		write(t, f, fmt.Sprintf("line %03d\n", i)) // 9 bytes
	}
	for _, p := range logging.FilesKeep(path, 5) {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 20 {
			t.Fatalf("%s is %d bytes, max 20", p, st.Size())
		}
	}
}

func TestOversizedWriteIsNotSplit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f := openSize(t, path, 10, 5)
	write(t, f, "x\n")
	big := strings.Repeat("y", 25) + "\n"
	write(t, f, big)
	if got := read(t, path); got != big {
		t.Fatalf("daemon.log = %q, want the whole oversized write", got)
	}
	if got := read(t, path+".1"); got != "x\n" {
		t.Fatalf("daemon.log.1 = %q", got)
	}
}

func TestKeepsAtMostKeepFilesAndDropsOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	f := openSize(t, path, 6, 3)
	for i := range 10 {
		write(t, f, fmt.Sprintf("l%02d\n", i)) // 4 bytes: one line per file
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d files, want 3", len(entries))
	}
	want := map[string]string{path: "l09\n", path + ".1": "l08\n", path + ".2": "l07\n"}
	for p, w := range want {
		if got := read(t, p); got != w {
			t.Errorf("%s = %q, want %q", filepath.Base(p), got, w)
		}
	}
}

func TestReopenContinuesExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("12345678\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := openSize(t, path, 10, 5)
	write(t, f, "ab\n") // 9+3 > 10: rotate
	if got := read(t, path+".1"); got != "12345678\n" {
		t.Fatalf("daemon.log.1 = %q", got)
	}
	if got := read(t, path); got != "ab\n" {
		t.Fatalf("daemon.log = %q", got)
	}
}

func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f := openSize(t, path, 100, 5)
	write(t, f, "one\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	g := openSize(t, path, 100, 5)
	write(t, g, "two\n")
	if got := read(t, path); got != "one\ntwo\n" {
		t.Fatalf("daemon.log = %q", got)
	}
}

func TestConcurrentWritesStayWholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	f := openSize(t, path, 4096, 50)
	const writers, perWriter = 8, 200
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				_, _ = fmt.Fprintf(f, "writer=%d line=%04d %s\n", w, i, strings.Repeat("z", 20))
			}
		}()
	}
	wg.Wait()
	lines := 0
	for _, p := range logging.FilesKeep(path, 50) {
		sc := bufio.NewScanner(strings.NewReader(read(t, p)))
		for sc.Scan() {
			var w, i int
			var pad string
			if n, err := fmt.Sscanf(sc.Text(), "writer=%d line=%d %s", &w, &i, &pad); n != 3 || err != nil || len(pad) != 20 {
				t.Fatalf("torn line in %s: %q", filepath.Base(p), sc.Text())
			}
			lines++
		}
	}
	if lines != writers*perWriter {
		t.Fatalf("got %d lines, want %d", lines, writers*perWriter)
	}
}

func TestFilesOldestFirstAndOnlyExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	if got := logging.Files(path); len(got) != 0 {
		t.Fatalf("Files with no log = %v, want none", got)
	}
	for _, n := range []string{"daemon.log", "daemon.log.1", "daemon.log.3", "daemon.log.9"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{path + ".3", path + ".1", path}
	if got := logging.Files(path); !slices.Equal(got, want) {
		t.Fatalf("Files = %v, want %v", got, want)
	}
}

func TestForRunAddsRunAttributes(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	logging.ForRun(log, "r-1", "nightly", "vps").Info("run started", "attempt", 1)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	want := map[string]any{
		logging.KeyRunID: "r-1", logging.KeyTaskID: "nightly", logging.KeyTarget: "vps",
		"msg": "run started", "attempt": float64(1),
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %v", k, rec[k], v)
		}
	}
	if logging.KeyRunID != "run_id" || logging.KeyTaskID != "task_id" || logging.KeyTarget != "target" {
		t.Fatal("attribute keys do not match the spec")
	}
}
