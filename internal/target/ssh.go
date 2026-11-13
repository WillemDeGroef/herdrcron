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
// ssh could not reach: ssh exited 255, or the call hung until its deadline.
var ErrUnreachable = errors.New("target unreachable")

// UnreachableError is returned when ssh could not reach the target: it
// exited 255, or it was still running when the deadline fired and had to
// be killed. A command that already exited is a result, not this error,
// even when the deadline has also fired. It matches ErrUnreachable with
// errors.Is.
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
//
// The remote user's login shell need not be a POSIX shell: it only starts
// /bin/sh (see neutralLine). A call that is cancelled or passes its
// deadline also stops its command on the remote machine (see
// watchedScript); that needs /bin/sh, and head for calls with stdin data.
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
// it stdin if non-nil, and returns its stdout. stdin is read to its end
// before ssh starts. ssh exiting 255 gives an
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
// creates the ControlMaster directory if it can. The TTY already ends the
// remote command with the connection (hangup), so there is no watcher.
func (s *SSH) InteractiveArgv(remoteArgv []string) []string {
	_ = s.ensureControlDir()
	argv := append([]string{"ssh", "-t"}, s.options()...)
	return append(argv, "--", s.cfg.Host, neutralLine(s.commandLine(shellquote.Join(remoteArgv...))))
}

// run runs the shell command line cmd on the remote machine. name is the
// argv reported in an *ExitError; keep prompt text and env values out of
// its first three elements.
//
// ssh's stdin stays open until the call ends, after any data from stdin:
// its closing is what tells the remote side that the call was cancelled
// (see watchedScript).
func (s *SSH) run(ctx context.Context, name []string, cmd string, stdin io.Reader) ([]byte, error) {
	if err := s.ensureControlDir(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w", s.cfg.Name, err)
	}
	var data []byte
	if stdin != nil {
		var err error
		if data, err = io.ReadAll(stdin); err != nil {
			return nil, fmt.Errorf("ssh %s: read stdin: %w", s.cfg.Name, err)
		}
	}
	argv := append([]string{"-o", "BatchMode=yes"}, s.options()...)
	argv = append(argv, "--", s.cfg.Host, neutralLine(watchedScript(s.commandLine(cmd), len(data))))
	c := exec.CommandContext(ctx, "ssh", argv...)
	c.Env = environ()
	c.WaitDelay = sshWaitDelay
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	in, err := c.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", s.cfg.Name, err)
	}
	c.Cancel = func() error {
		_ = in.Close()
		return c.Process.Kill()
	}
	if err := c.Start(); err != nil {
		return sshCallResult(ctx, s.cfg.Name, name, nil, err, &stdout, &stderr)
	}
	if len(data) > 0 {
		// Wait closes the pipe when ssh exits, which ends a blocked Write.
		go func() { _, _ = in.Write(data) }()
	}
	err = c.Wait()
	return sshCallResult(ctx, s.cfg.Name, name, c.ProcessState, err, &stdout, &stderr)
}

// watchedScript is the /bin/sh script that runs the remote command line
// line and stops it when the call is cancelled. Killing the local ssh
// signals nothing on the remote machine (there is no TTY: a TTY would
// alter stdin and stdout); the remote side only sees ssh's stdin close. So
// the local side keeps stdin open for the whole call, and the script:
//
//   - feeds the command the stdinLen bytes of stdin data (head -c), or an
//     empty stdin when there are none;
//   - leaves a watcher reading the rest of stdin, which is nothing until
//     it closes: either ssh is gone, or the command finished and sshd
//     closed the session. The watcher then signals the script (USR1),
//     which has exited in the second case;
//   - on USR1 sends TERM to its whole process group when sshd started the
//     login shell as a group of its own (the group leader is the script
//     or its parent, as under OpenSSH), else to the command and its
//     descendants, found with ps.
//
// The watcher holds neither stdout nor stderr, so it never delays the end
// of the ssh call. It starts once the stdin data has gone to the command:
// a command that does not read more than a pipe buffer of data is not
// stopped by a cancel.
func watchedScript(line string, stdinLen int) string {
	feed := ""
	if stdinLen > 0 {
		feed = fmt.Sprintf("head -c %d <&3; ", stdinLen)
	}
	return `exec 3<&0
k() { for p in $(printf '%s\n' "$s" | while read -r a b; do [ "$b" = "$1" ] && echo "$a"; done); do k "$p"; done; kill -TERM "$1" 2>/dev/null; }
trap 'trap "" TERM; g=$(ps -o pgid= -p $$ 2>/dev/null); case " $(echo $g) " in " $$ "|" $PPID ") kill -TERM 0;; *) s=$(ps -A -o pid= -o ppid= 2>/dev/null); k $!;; esac; exit 143' USR1
{ ` + feed + `{ read -r _ <&3; kill -USR1 $$; } >/dev/null 2>&1 & } | ` + line + ` 3<&- &
wait $!`
}

// neutralLine is the command line ssh sends for the /bin/sh script script.
// sshd hands that line to the remote user's login shell, which may be
// bash, zsh, fish or tcsh, and they do not agree on quoting. They do agree
// on a single-quoted word without a quote, a newline, "!" (csh history) or
// a backslash before a quote or backslash (fish escapes). So the line is
// /bin/sh with two such words: a fixed decoder, and the script with every
// other byte as a printf %b octal escape (\0ooo).
func neutralLine(script string) string {
	var b strings.Builder
	b.WriteString(`/bin/sh -c 'eval "$(printf %b "$0")"' '`)
	for i := 0; i < len(script); i++ {
		c := script[i]
		if c < ' ' || c > '~' || c == '\'' || c == '\\' || c == '!' {
			fmt.Fprintf(&b, `\0%03o`, c)
		} else {
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// sshCallResult classifies one ssh invocation. A process that already
// exited keeps that exit code: CommandContext's deadline can also be
// exceeded (Wait prefers a non-zero exit, and a successful exit can be
// rewritten as the context error when Kill wins a race against it), and
// that must not become "unreachable". Only ssh exiting 255, or a call
// that never exited before the deadline, means the target did not answer.
func sshCallResult(ctx context.Context, targetName string, argv []string, ps *os.ProcessState, err error, stdout, stderr *bytes.Buffer) ([]byte, error) {
	code := -1
	if ps != nil && ps.Exited() {
		code = ps.ExitCode()
	}
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		// WaitDelay: ssh exited 0 but a ControlPersist master it forked
		// still holds stderr; older OpenSSH does not detach it from the
		// caller's stdio.
		return stdout.Bytes(), nil
	case code == sshUnreachableCode:
		return stdout.Bytes(), &UnreachableError{Target: targetName, Stderr: stderr.Bytes()}
	case code > 0:
		return stdout.Bytes(), &ExitError{Argv: slices.Clone(argv), Code: code, Stderr: stderr.Bytes()}
	case code == 0:
		// The command exited 0. A deadline or cancel here is Kill racing
		// a process that had already finished; any other error (a stdio
		// copy failure) is that error, not an unreachable target.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return stdout.Bytes(), nil
		}
		return stdout.Bytes(), fmt.Errorf("ssh %s: %w", targetName, err)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// ssh over a ControlMaster connection whose network dropped hangs
		// rather than exiting 255; killed at the deadline with no exit
		// code, it did not answer.
		return stdout.Bytes(), &UnreachableError{Target: targetName, Stderr: []byte("ssh: no answer before the deadline\n")}
	default:
		return stdout.Bytes(), fmt.Errorf("ssh %s: %w", targetName, err)
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

// commandLine runs cmd through the target shell: the shell (the user's
// config, used as is) and cmd as one quoted word, for /bin/sh to read.
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
