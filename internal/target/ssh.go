package target

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/shellquote"
)

// ErrUnreachable is (wrapped by) the error of a call on an SSH target that
// ssh could not reach: ssh exited 255.
var ErrUnreachable = errors.New("target unreachable")

// UnreachableError is returned when ssh exits 255: it could not connect or
// authenticate, so the remote command did not run (or its result is
// unknown). It matches ErrUnreachable with errors.Is.
type UnreachableError struct {
	Target string
	Stderr []byte
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("target %s unreachable: %s", e.Target, firstLine(e.Stderr))
}

// Is makes errors.Is(err, ErrUnreachable) true.
func (e *UnreachableError) Is(target error) bool { return target == ErrUnreachable }

// sshUnreachableCode is the exit code ssh uses for its own errors.
const sshUnreachableCode = 255

// sshWaitDelay bounds the wait for ssh's stdio after it exits: a
// ControlPersist master forked by the call can hold them open.
const sshWaitDelay = 2 * time.Second

// SSHConfig describes one SSH target.
type SSHConfig struct {
	// Name is the target name in config.yaml.
	Name string
	// Host is the ssh destination, normally an alias from ~/.ssh/config.
	Host string
	// Shell is the remote command prefix every command runs through,
	// e.g. "bash -lc". Empty means "bash -lc".
	Shell string
	// ControlDir holds the ControlMaster sockets
	// ($XDG_STATE_HOME/herdrcron/ssh).
	ControlDir string
}

// SSH is a Target on a remote machine, reached with the system ssh binary
// so ~/.ssh/config, agents, keys and ProxyJump work as for the user. Every
// command runs through the target's shell, each argument shell-quoted.
type SSH struct {
	cfg SSHConfig
}

var _ Target = (*SSH)(nil)

// NewSSH returns the SSH target for cfg.
func NewSSH(cfg SSHConfig) *SSH {
	if strings.TrimSpace(cfg.Shell) == "" {
		cfg.Shell = "bash -lc"
	}
	return &SSH{cfg: cfg}
}

// Name returns the target name.
func (s *SSH) Name() string { return s.cfg.Name }

// Exec runs argv on the remote machine through the target shell, feeding
// it stdin if non-nil, and returns its stdout. ssh exiting 255 gives an
// *UnreachableError; any other non-zero exit an *ExitError with the
// remote argv and stderr.
func (s *SSH) Exec(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("ssh exec: empty argv")
	}
	return s.run(ctx, argv, shellquote.Join(argv...), stdin)
}

// PutFile writes data to path on the remote machine with mode 0600,
// creating parent directories, from ssh's stdin so the content is never in
// an argv. It writes a temp file next to path and renames it into place. A
// leading "~/" means the remote user's home directory.
func (s *SSH) PutFile(ctx context.Context, p string, data []byte) error {
	dst := remotePath(p)
	tmp := remotePath(p + ".tmp")
	dir := remotePath(path.Dir(p))
	// On cat failure, remove the temp so we do not leave a truncated file; we
	// avoid trap because odd paths can contain quotes that break trap parsing.
	script := "umask 077 && mkdir -p " + dir + " && cat > " + tmp + " || { rm -f " + tmp + "; exit 1; } && mv -f " + tmp + " " + dst
	if _, err := s.run(ctx, []string{"put-file", p}, script, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	return nil
}

// RemoveFile removes path on the remote machine; a missing file is not an
// error. A leading "~/" means the remote user's home directory.
func (s *SSH) RemoveFile(ctx context.Context, p string) error {
	if _, err := s.run(ctx, []string{"rm", p}, "rm -f "+remotePath(p), nil); err != nil {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// InteractiveArgv is the local argv that runs remoteArgv on the target
// with a TTY (ssh -t), through the target shell. It is for a user at a
// terminal, so it leaves out BatchMode: ssh may ask for a passphrase. It
// creates the ControlMaster directory if it can.
func (s *SSH) InteractiveArgv(remoteArgv []string) []string {
	_ = s.ensureControlDir()
	argv := append([]string{"ssh", "-t"}, s.options()...)
	return append(argv, "--", s.cfg.Host, s.commandLine(shellquote.Join(remoteArgv...)))
}

// run runs the shell command line cmd on the remote machine. name is the
// argv reported in an *ExitError; keep prompt text and env values out of
// its first three elements.
func (s *SSH) run(ctx context.Context, name []string, cmd string, stdin io.Reader) ([]byte, error) {
	if err := s.ensureControlDir(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w", s.cfg.Name, err)
	}
	argv := append([]string{"-o", "BatchMode=yes"}, s.options()...)
	argv = append(argv, "--", s.cfg.Host, s.commandLine(cmd))
	c := exec.CommandContext(ctx, "ssh", argv...)
	c.Env = environ()
	c.WaitDelay = sshWaitDelay
	if stdin != nil {
		c.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case errors.Is(err, exec.ErrWaitDelay):
		// ssh exited 0 (a non-zero exit gives *exec.ExitError instead) but
		// a ControlPersist master it forked still holds stderr: older
		// OpenSSH does not detach it from the caller's stdio.
		return stdout.Bytes(), nil
	case errors.As(err, &ee) && ee.ExitCode() == sshUnreachableCode:
		return stdout.Bytes(), &UnreachableError{Target: s.cfg.Name, Stderr: stderr.Bytes()}
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return stdout.Bytes(), &ExitError{Argv: slices.Clone(name), Code: ee.ExitCode(), Stderr: stderr.Bytes()}
	default:
		return stdout.Bytes(), fmt.Errorf("ssh %s: %w", s.cfg.Name, err)
	}
}

// options are the ssh options of every call (herdrcron.md, Target adapter)
// but BatchMode, which only non-interactive calls set.
func (s *SSH) options() []string {
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + SSHControlSocketPath(s.cfg.ControlDir, s.cfg.Host),
		"-o", "ControlPersist=10m",
		"-o", "ConnectTimeout=10",
	}
}

// SSHControlSocketPath is the ControlMaster socket path for host under
// controlDir (same value ssh uses via -o ControlPath=…). OpenSSH's default
// %C expands to a long hex string that, with a long state directory, can
// exceed the Unix domain socket path limit (~108 bytes on Linux); we use a
// short hash of the destination instead.
func SSHControlSocketPath(controlDir, host string) string {
	h := sha256.Sum256([]byte(host))
	short := hex.EncodeToString(h[:8])
	return filepath.Join(controlDir, "cm-"+short)
}

// commandLine is the one string ssh hands to the remote login shell: the
// target shell (the user's config, used as is) and cmd as one quoted word.
func (s *SSH) commandLine(cmd string) string {
	return s.cfg.Shell + " " + shellquote.Quote(cmd)
}

func (s *SSH) ensureControlDir() error {
	if s.cfg.ControlDir == "" {
		return errors.New("no ControlMaster directory")
	}
	return os.MkdirAll(s.cfg.ControlDir, 0o700)
}

// remotePath quotes p for the remote shell, with a leading "~/" as
// "$HOME"/ so the shell still expands it.
func remotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return `"$HOME"/` + shellquote.Quote(rest)
	}
	return shellquote.Quote(p)
}

func firstLine(b []byte) []byte {
	msg := bytes.TrimSpace(b)
	if i := bytes.IndexByte(msg, '\n'); i >= 0 {
		msg = bytes.TrimSpace(msg[:i])
	}
	if len(msg) > maxStderrInError {
		msg = msg[:maxStderrInError]
	}
	return msg
}
