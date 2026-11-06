package target

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// finished runs sh -c "exit <code>" and returns its process state and error.
func finished(t *testing.T, code string) (*os.ProcessState, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit "+code)
	err := cmd.Run()
	return cmd.ProcessState, err
}

// A command that already exited keeps that status when the deadline has
// also fired. Classifying from ctx.Err() first would call a slow but
// finished command unreachable.
func TestSSHCallResultPrefersTheExitCodeOverTheDeadline(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatal("deadline not exceeded")
	}

	t.Run("exit 0 rewritten as the deadline", func(t *testing.T) {
		ps, err := finished(t, "0")
		if err != nil {
			t.Fatal(err)
		}
		_, err = sshCallResult(expired, "vps", []string{"true"}, ps, expired.Err(), bytes.NewBufferString("ok\n"), new(bytes.Buffer))
		if err != nil {
			t.Fatalf("err = %v, want success: the command exited 0", err)
		}
	})

	t.Run("exit 3", func(t *testing.T) {
		ps, runErr := finished(t, "3")
		_, err := sshCallResult(expired, "vps", []string{"herdr", "pane", "list"}, ps, runErr, new(bytes.Buffer), bytes.NewBufferString("boom\n"))
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 3 || errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want exit 3, not unreachable", err)
		}
		if string(ee.Stderr) != "boom\n" {
			t.Errorf("stderr = %q", ee.Stderr)
		}
	})

	t.Run("exit 255", func(t *testing.T) {
		ps, runErr := finished(t, "255")
		_, err := sshCallResult(expired, "vps", []string{"herdr", "pane", "list"}, ps, runErr, new(bytes.Buffer), bytes.NewBufferString("Connection refused\n"))
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want unreachable", err)
		}
		var ee *ExitError
		if errors.As(err, &ee) {
			t.Errorf("exit 255 gave an *ExitError: %v", err)
		}
	})
}

// No exit code and a deadline is a hung ssh (killed when the deadline
// fired). Cancellation is not: the daemon stopping says nothing about
// the target.
func TestSSHCallResultDeadlineWithoutAnExitIsUnreachable(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := sshCallResult(expired, "vps", []string{"herdr"}, nil, expired.Err(), new(bytes.Buffer), new(bytes.Buffer))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want unreachable", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	stop()
	_, err = sshCallResult(ctx, "vps", []string{"herdr"}, nil, ctx.Err(), new(bytes.Buffer), new(bytes.Buffer))
	if err == nil || errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want a non-unreachable error", err)
	}
}
