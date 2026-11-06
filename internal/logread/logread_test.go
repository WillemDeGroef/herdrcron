package logread_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/logread"
)

// syncBuffer is a goroutine-safe writer for follow tests.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func printAll(t *testing.T, logPath string, opts logread.Options) string {
	t.Helper()
	var buf bytes.Buffer
	if err := logread.Print(context.Background(), logPath, &buf, opts); err != nil {
		t.Fatalf("Print: %v", err)
	}
	return buf.String()
}

func rec(msg, taskID string) string {
	return `{"time":"2026-11-06T10:00:00.000+01:00","level":"INFO","msg":"` + msg + `","task_id":"` + taskID + `"}` + "\n"
}

func TestFormatsRecordWithAttrsInJSONOrder(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, `{"time":"2026-11-06T10:00:00.123+01:00","level":"INFO","msg":"run started","run_id":"r1","task_id":"nightly","target":"local","attempt":2,"ok":true}`+"\n")

	got := printAll(t, logPath, logread.Options{})

	want := "2026-11-06T10:00:00.123+01:00 INFO run started run_id=r1 task_id=nightly target=local attempt=2 ok=true\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestQuotesStringsWithSpacesQuotesOrEquals(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, `{"time":"T","level":"WARN","msg":"m","a":"two words","b":"say \"hi\"","c":"k=v","d":"","e":"plain"}`+"\n")

	got := printAll(t, logPath, logread.Options{})

	want := `T WARN m a="two words" b="say \"hi\"" c="k=v" d="" e=plain` + "\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPrintsNestedValuesAsCompactJSON(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, `{"time":"T","level":"INFO","msg":"m","env":{ "keys": ["A", "B"] },"list":[1, 2],"none":null}`+"\n")

	got := printAll(t, logPath, logread.Options{})

	want := `T INFO m env={"keys":["A","B"]} list=[1,2] none=null` + "\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPrintsNonJSONLinesVerbatim(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, "panic: boom\n"+rec("ok", "x"))

	got := printAll(t, logPath, logread.Options{})

	want := "panic: boom\n2026-11-06T10:00:00.000+01:00 INFO ok task_id=x\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestFiltersByTaskID(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, rec("one", "a")+rec("two", "b")+"not json\n"+`{"msg":"nested","attrs":{"task_id":"a"}}`+"\n"+rec("three", "a"))

	got := printAll(t, logPath, logread.Options{TaskID: "a"})

	want := "2026-11-06T10:00:00.000+01:00 INFO one task_id=a\n" +
		"2026-11-06T10:00:00.000+01:00 INFO three task_id=a\n"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPrintsRotatedFilesOldestFirst(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath+".4", rec("oldest", "t"))
	writeFile(t, logPath+".2", rec("older", "t"))
	writeFile(t, logPath+".1", rec("newer", "t"))
	writeFile(t, logPath, rec("live", "t"))

	got := printAll(t, logPath, logread.Options{})

	want := line("oldest") + line("older") + line("newer") + line("live")
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestMissingLogReturnsNotExist(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")

	err := logread.Print(context.Background(), logPath, &bytes.Buffer{}, logread.Options{})

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), logPath) {
		t.Errorf("err %q does not name %s", err, logPath)
	}
}

// follow starts Print in follow mode and returns the output buffer and a
// stop function that cancels it and returns Print's error.
func follow(t *testing.T, logPath string, opts logread.Options) (*syncBuffer, func() error) {
	t.Helper()
	opts.Follow = true
	opts.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- logread.Print(ctx, logPath, out, opts) }()
	stopped := false
	var stopErr error
	stop := func() error {
		if !stopped {
			stopped = true
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Print did not return after cancel")
			}
		}
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	return out, stop
}

func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out.String() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output never became\n%q\ngot\n%q", want, out.String())
}

func line(msg string) string {
	return "2026-11-06T10:00:00.000+01:00 INFO " + msg + " task_id=t\n"
}

func TestFollowPrintsAppendedLines(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, rec("first", "t"))
	out, stop := follow(t, logPath, logread.Options{})
	waitFor(t, out, line("first"))

	appendFile(t, logPath, rec("second", "t"))

	waitFor(t, out, line("first")+line("second"))
	if err := stop(); err != nil {
		t.Errorf("Print returned %v on cancel, want nil", err)
	}
}

func TestFollowWaitsForMissingLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	out, _ := follow(t, logPath, logread.Options{})
	time.Sleep(20 * time.Millisecond)

	appendFile(t, logPath, rec("hello", "t"))

	waitFor(t, out, line("hello"))
}

func TestFollowAppliesTaskFilter(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, "")
	out, _ := follow(t, logPath, logread.Options{TaskID: "t"})

	appendFile(t, logPath, rec("other", "u")+rec("mine", "t"))

	waitFor(t, out, line("mine"))
}

func TestFollowHoldsPartialLineUntilComplete(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, "")
	out, _ := follow(t, logPath, logread.Options{})
	full := rec("split", "t")

	appendFile(t, logPath, full[:20])
	time.Sleep(50 * time.Millisecond)
	if got := out.String(); got != "" {
		t.Fatalf("partial line printed early: %q", got)
	}
	appendFile(t, logPath, full[20:])

	waitFor(t, out, line("split"))
}

func TestFollowSurvivesRotation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, rec("a", "t"))
	out, _ := follow(t, logPath, logread.Options{})
	waitFor(t, out, line("a"))

	// Write to the old file, rotate, and write to the new one before the
	// follower has a chance to see the last old line.
	appendFile(t, logPath, rec("b", "t"))
	if err := os.Rename(logPath, logPath+".1"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, logPath, rec("c", "t"))
	appendFile(t, logPath, rec("d", "t"))

	waitFor(t, out, line("a")+line("b")+line("c")+line("d"))
}

func TestFollowRereadsTruncatedLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, logPath, rec("long-first-record", "t"))
	out, _ := follow(t, logPath, logread.Options{})
	waitFor(t, out, line("long-first-record"))

	writeFile(t, logPath, rec("x", "t"))

	waitFor(t, out, line("long-first-record")+line("x"))
}
