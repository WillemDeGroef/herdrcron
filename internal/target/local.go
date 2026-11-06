package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Local is the Target for the machine herdrcron itself runs on. It runs
// commands directly, never through a shell.
type Local struct{}

var (
	_ Target  = (*Local)(nil)
	_ Spawner = (*Local)(nil)
)

// NewLocal returns the local target.
func NewLocal() *Local { return &Local{} }

// Name returns "local".
func (*Local) Name() string { return "local" }

// Exec runs argv without a shell, feeding it stdin if non-nil, and returns
// its stdout. A non-zero exit gives an *ExitError carrying stderr.
func (*Local) Exec(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("local exec: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = environ()
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return stdout.Bytes(), &ExitError{Argv: append([]string(nil), argv...), Code: ee.ExitCode(), Stderr: stderr.Bytes()}
	default:
		return stdout.Bytes(), fmt.Errorf("local exec %s: %w", argv[0], err)
	}
}

// PutFile writes data to path with mode 0600, creating parent directories
// with mode 0700. It writes a temp file in the same directory and renames
// it into place. A leading "~/" means the user's home directory.
func (*Local) PutFile(_ context.Context, path string, data []byte) error {
	p, err := expandHome(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("put file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("put file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	return nil
}

// RemoveFile removes path; a missing file is not an error. A leading "~/"
// means the user's home directory.
func (*Local) RemoveFile(_ context.Context, path string) error {
	p, err := expandHome(path)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// Spawn starts argv detached in a new session with stdio to /dev/null and
// returns without waiting; a goroutine reaps it when it exits.
func (*Local) Spawn(argv []string) error {
	if len(argv) == 0 {
		return errors.New("local spawn: empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("local spawn %s: %w", argv[0], err)
	}
	go cmd.Wait() //nolint:errcheck // reap only; the exit status is not used
	return nil
}

// environ is the daemon's environment without HERDR_* variables. A daemon
// started inside a herdr pane inherits HERDR_SOCKET_PATH and the pane ids;
// herdrcron must always talk to the user's default session
// (docs/herdr-findings.md), as it does under systemd.
func environ() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "HERDR_")
	})
}

func expandHome(path string) (string, error) {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %s: %w", path, err)
	}
	return filepath.Join(home, rest), nil
}
