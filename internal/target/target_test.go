package target_test

import (
	"context"
	"errors"
	"slices"
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
