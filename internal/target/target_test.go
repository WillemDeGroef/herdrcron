package target_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func TestFakeRecordsExecCalls(t *testing.T) {
	var tg target.Target = target.NewFake("local")
	f := tg.(*target.Fake)
	if tg.Name() != "local" {
		t.Errorf("Name = %q", tg.Name())
	}
	if _, err := tg.Exec(context.Background(), []string{"claude", "--x"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tg.PutFile(context.Background(), "/tmp/p.md", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"claude", "--x"}) {
		t.Errorf("Calls = %q", calls)
	}
	if got := string(f.Files()["/tmp/p.md"]); got != "hi" {
		t.Errorf("Files = %q", got)
	}
}

func TestFakeFailsWithSetErr(t *testing.T) {
	f := target.NewFake("vps")
	boom := errors.New("boom")
	f.SetErr(boom)
	if _, err := f.Exec(context.Background(), []string{"x"}, nil); !errors.Is(err, boom) {
		t.Errorf("Exec err = %v, want boom", err)
	}
}

func TestFakeRemoveFile(t *testing.T) {
	ctx := context.Background()
	f := target.NewFake("local")
	if err := f.PutFile(ctx, "/tmp/p.md", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveFile(ctx, "/tmp/p.md"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Files()["/tmp/p.md"]; ok {
		t.Error("file still present after RemoveFile")
	}
	if err := f.RemoveFile(ctx, "/tmp/p.md"); err != nil {
		t.Errorf("second RemoveFile = %v, want nil", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("RemoveFile recorded calls: %q", f.Calls())
	}
	boom := errors.New("boom")
	if err := f.PutFile(ctx, "/tmp/q.md", []byte("x")); err != nil {
		t.Fatal(err)
	}
	f.SetErr(boom)
	if err := f.RemoveFile(ctx, "/tmp/q.md"); !errors.Is(err, boom) {
		t.Errorf("RemoveFile err = %v, want boom", err)
	}
	if _, ok := f.Files()["/tmp/q.md"]; !ok {
		t.Error("failed RemoveFile removed the file")
	}
}

func TestExitErrorOmitsLaterArgv(t *testing.T) {
	e := &target.ExitError{
		Argv:   []string{"herdr", "pane", "run", "SECRET=1", "prompt text"},
		Code:   2,
		Stderr: []byte("  bad thing\nsecond line\n"),
	}
	got := e.Error()
	if want := "herdr pane run: exit 2: bad thing"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	long := &target.ExitError{Argv: []string{"x"}, Code: 1, Stderr: []byte(strings.Repeat("a", 500))}
	if n := len(long.Error()); n > len("x: exit 1: ")+200 {
		t.Errorf("Error() length %d, stderr not capped at 200 bytes", n)
	}
}
