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
	"strconv"
	"strings"
	"sync/atomic"
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
	// KillGrace is how long a cancelled remote command gets to end on
	// SIGTERM before SIGKILL. Zero means 5s. A value below 100ms is raised
	// to 100ms. Whole seconds are passed to sleep as an integer; any other
	// value is seconds with millisecond precision, and a remote sleep that
	// rejects that falls back to one second.
	KillGrace time.Duration
}

// SSH is a Target on a remote machine, reached with the system ssh binary
// so ~/.ssh/config, agents, keys and ProxyJump work as for the user. Every
// command runs through the target's shell, each argument shell-quoted.
//
// The remote user's login shell need not be a POSIX shell: it only starts
// /bin/sh (see neutralLine). A call that is cancelled or passes its
// deadline also stops its command on the remote machine, with SIGTERM and
// then SIGKILL (see watchedScript); that needs /bin/sh, and mktemp for
// calls with stdin data.
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
// it stdin if non-nil, and returns its stdout. stdin is streamed to the
// remote machine as it is read, and the command starts when stdin ends. A
// cancelled call returns without waiting for a stdin.Read that blocks. ssh
// exiting 255 gives an *UnreachableError; any other non-zero exit an
// *ExitError with the remote argv and stderr.
func (s *SSH) Exec(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("ssh exec: empty argv")
	}
	script := watchedScript(s.commandLine(shellquote.Join(argv...)), stdin != nil, s.cfg.KillGrace)
	return s.run(ctx, argv, script, stdin)
}

// PutFile writes data to path on the remote machine with mode 0600,
// creating parent directories, from ssh's stdin so the content is never in
// an argv. The data goes to a temp file next to path that is renamed into
// place once it is complete; a call that is cancelled or loses its
// connection removes the temp file and leaves path as it was. A leading
// "~/" means the remote user's home directory.
func (s *SSH) PutFile(ctx context.Context, p string, data []byte) error {
	if _, err := s.run(ctx, []string{"put-file", p}, putFileScript(p), bytes.NewReader(data)); err != nil {
		return fmt.Errorf("put file: %w", err)
	}
	return nil
}

// RemoveFile removes path on the remote machine; a missing file is not an
// error. A leading "~/" means the remote user's home directory.
func (s *SSH) RemoveFile(ctx context.Context, p string) error {
	script := watchedScript(s.commandLine("rm -f "+remotePath(p)), false, s.cfg.KillGrace)
	if _, err := s.run(ctx, []string{"rm", p}, script, nil); err != nil {
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

// run runs the /bin/sh script on the remote machine. name is the argv
// reported in an *ExitError; keep prompt text and env values out of its
// first three elements.
//
// A non-nil stdin is sent encoded (see feedStdin) for the script to decode
// with receiveStdin. ssh's stdin then stays open until the call ends: its
// closing is what tells the remote side that the call was cancelled.
func (s *SSH) run(ctx context.Context, name []string, script string, stdin io.Reader) ([]byte, error) {
	if err := s.ensureControlDir(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w", s.cfg.Name, err)
	}
	argv := append([]string{"-o", "BatchMode=yes"}, s.options()...)
	argv = append(argv, "--", s.cfg.Host, neutralLine(script))
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
	var readErr atomic.Pointer[error]
	if stdin != nil {
		// Not waited for: a Read that blocks must not hold up a cancelled
		// call. Wait closes the pipe when ssh exits, which ends a blocked
		// Write.
		go func() {
			if err := feedStdin(in, stdin); err != nil {
				// Stored before the close that makes the remote side give
				// up, so it is there when Wait returns.
				readErr.Store(&err)
				_ = in.Close()
			}
		}()
	}
	err = c.Wait()
	if e := readErr.Load(); e != nil {
		return nil, fmt.Errorf("ssh %s: read stdin: %w", s.cfg.Name, *e)
	}
	return sshCallResult(ctx, s.cfg.Name, name, c.ProcessState, err, &stdout, &stderr)
}

// stdinEnd is the line that ends the encoded stdin data. No data line
// starts with it, so a stream cut before it never looks complete.
const stdinEnd = `\.`

// feedStdin copies r to w in the encoding receiveStdin decodes: lines of
// printable ASCII with every other byte, and the backslash, as a printf %b
// octal escape (\0ooo), then the stdinEnd line. The length of r need not be
// known, any byte can be sent, and the remote side needs only sh builtins
// to tell the end of the data from a closed connection. It returns r's
// error; a failed write means ssh is gone, which its exit reports.
func feedStdin(w io.Writer, r io.Reader) error {
	const lineBytes = 256
	buf := make([]byte, 32*1024)
	var out bytes.Buffer
	for {
		n, err := r.Read(buf)
		out.Reset()
		for i, c := range buf[:n] {
			if i > 0 && i%lineBytes == 0 {
				out.WriteByte('\n')
			}
			if c < ' ' || c > '~' || c == '\\' {
				fmt.Fprintf(&out, `\0%03o`, c)
			} else {
				out.WriteByte(c)
			}
		}
		if n > 0 {
			out.WriteByte('\n')
		}
		if err == io.EOF {
			out.WriteString(stdinEnd + "\n")
		} else if err != nil {
			return err
		}
		if _, werr := w.Write(out.Bytes()); werr != nil || err == io.EOF {
			return nil
		}
	}
}

// receiveStdin is the /bin/sh fragment that decodes the stdin data sent by
// feedStdin from fd into the file "$f". It leaves the stdinEnd line in $l
// only when all the data arrived and was written.
func receiveStdin(fd string) string {
	return `l=
while IFS= read -r l <&` + fd + ` && [ "$l" != '` + stdinEnd + `' ]; do printf %b "$l" || { l=; break; }; done >"$f"`
}

// putFileScript is the /bin/sh script behind PutFile. It runs no command
// of the target shell, so it needs no watcher: a cancelled call closes
// stdin before the stdinEnd line, and the script then removes its temp
// file. mktemp gives the file mode 0600.
func putFileScript(p string) string {
	tmp := remotePath(path.Join(path.Dir(p), "."+path.Base(p)+".XXXXXX"))
	return `umask 077
mkdir -p ` + remotePath(path.Dir(p)) + ` || exit 1
f=$(mktemp ` + tmp + `) || exit 1
` + receiveStdin("0") + `
[ "$l" = '` + stdinEnd + `' ] && mv -f "$f" ` + remotePath(p) + ` && exit 0
rm -f "$f"
echo 'herdrcron: upload did not complete' >&2
exit 1`
}

// defaultKillGrace is how long a cancelled remote command gets to end on
// SIGTERM before SIGKILL.
const defaultKillGrace = 5 * time.Second

// watchedScript is the /bin/sh script that runs the remote command line
// line and stops it when the call is cancelled. Killing the local ssh
// signals nothing on the remote machine (there is no TTY: a TTY would
// alter stdin and stdout); the remote side only sees ssh's stdin close. So
// the local side keeps stdin open for the whole call, and the script:
//
//   - with hasStdin, first decodes the stdin data into a temp file, which
//     it opens for the command and removes at once. A file, not a pipe: the
//     script reads all the data whatever the command does with it, so no
//     amount of unread data hides a closed connection. Stdin closing
//     before the end of the data ends the script without running the
//     command;
//   - runs the command with a watcher beside it that reads the rest of
//     stdin, which is nothing until it closes: either ssh is gone, or the
//     command finished and sshd closed the session. The watcher then
//     signals the script (USR1), which has exited in the second case;
//   - on USR1 sends TERM, and KILL after the grace period to what is
//     left, to a process group that holds the command and what it started
//     and nothing else. With setsid on the remote machine that is a group
//     the command gets for itself. Without it, it is the script's own
//     group, but only when the script leads that group (it replaced the
//     login shell sshd started with setsid). A parent that leads the group
//     is not enough: that parent may be an interactive login shell, and
//     the group then holds jobs this command did not start;
//   - with neither, signals the command and its descendants one by one
//     and nothing else. It stops each process (STOP) before it lists that
//     process's children, so the children can neither be replaced nor
//     waited for, and their PIDs not reused, until they have their TERM.
//
// A group signal cannot hit a reused PID while the group's leader has not
// been waited for or the group still has a member. That leaves one case:
// the command ended on TERM, a process it started did not, and that one
// ends by itself in the last moments of the grace period. The one-by-one
// path checks a PID's group again before its KILL.
//
// Process data (i gives the parent and group of a live process, all the
// three of every process) comes from /proc where there is one, else from
// `ps -A -o pid= -o ppid= -o pgid=`. That form has no -p: a BusyBox ps has
// -o and not -p, and a ps that cannot print those three numbers is not
// parsed (its default listing is not a process table). With none of
// setsid, /proc and that ps, the script says so on stderr and a cancel
// signals only the command itself, which this shell has not waited for,
// so that pid cannot have been reused.
//
// The watcher holds neither stdout nor stderr, and the script lets go of
// both before it waits for the grace period, so neither delays the end of
// the ssh call. The script ends as soon as nothing is left; when it
// signals its own group it cannot tell and stays for the grace period.
func watchedScript(line string, hasStdin bool, grace time.Duration) string {
	if grace <= 0 {
		grace = defaultKillGrace
	}
	in := "exec 4</dev/null"
	if hasStdin {
		in = `f=$(mktemp "${TMPDIR:-/tmp}/herdrcron.XXXXXX") || exit 1
` + receiveStdin("3") + `
[ "$l" = '` + stdinEnd + `' ] || { rm -f "$f"; echo 'herdrcron: stdin ended early' >&2; exit 1; }
exec 4<"$f"
rm -f "$f"`
	}
	return `exec 3<&0
` + in + `
f3() {
	case $1 in Z*|"") return 1;; esac
	echo "$2 $3"
}
f4() { echo "$1 $3 $4"; }
# P is proc, ps, or empty. ps is listed without -p: BusyBox has -o and not -p.
# A listing that is not three numbers is ignored, so a ps that prints its
# default table is not taken for a process list.
P=
if [ -r /proc/$$/stat ]; then
	P=proc
else
	line=$(ps -A -o pid= -o ppid= -o pgid= 2>/dev/null | while IFS= read -r x; do
		[ -n "$x" ] || continue
		printf '%s\n' "$x"
		break
	done)
	set -f
	n=0
	ok=1
	for w in $line; do
		n=$((n+1))
		case $w in *[!0-9]*) ok=;; esac
	done
	set +f
	[ "$n" = 3 ] && [ "$ok" = 1 ] && P=ps
fi
i() {
	if [ "$P" = proc ]; then
		[ -r "/proc/$1/stat" ] && read -r a <"/proc/$1/stat" && f3 ${a##*) }
		return
	fi
	[ "$P" = ps ] || return 1
	ps -A -o pid= -o ppid= -o pgid= 2>/dev/null | while read -r p pp g _; do
		[ "$p" = "$1" ] && echo "$pp $g" && exit 0
	done
}
all() {
	if [ "$P" = proc ]; then
		for q in /proc/[0-9]*/stat; do read -r a <"$q" && f4 "${a%% *}" ${a##*) }; done 2>/dev/null
	elif [ "$P" = ps ]; then
		ps -A -o pid= -o ppid= -o pgid= 2>/dev/null
	fi
}
k() {
	[ -n "$2" ] && [ "$(i "$1")" = "$2" ] && kill -STOP "$1" 2>/dev/null || return 0
	L="$L $1:${2#* }"
	for p in $(all | while read -r a b _; do [ "$b" = "$1" ] && echo "$a"; done); do
		j=$(i "$p")
		case $j in "$1 "*) k "$p" "$j";; esac
	done
	kill -TERM "$1" 2>/dev/null
	kill -CONT "$1" 2>/dev/null
}
x() {
	case $m in
	g) kill -$1 0;;
	s) kill -$1 -$c 2>/dev/null;;
	*) false;;
	esac && return
	if [ "$1" = TERM ]; then
		kill -0 $c 2>/dev/null || return 0
		L=
		id=$(i "$c")
		if [ -n "$id" ]; then
			k "$c" "$id"
		else
			# Direct child, not yet waited for: its pid cannot have been reused.
			kill -TERM $c 2>/dev/null
		fi
	elif [ -z "$L" ]; then
		kill -KILL $c 2>/dev/null
	else
		for e in $L; do
			case $(i "${e%%:*}") in *" ${e#*:}") kill -KILL "${e%%:*}" 2>/dev/null;; esac
		done
	fi
}
left() {
	case $m in
	g) return 0;;
	s) kill -0 -$c 2>/dev/null && return 0;;
	esac
	for e in $L; do
		case $(i "${e%%:*}") in *" ${e#*:}") return 0;; esac
	done
	return 1
}
stop() {
	trap '' TERM USR1
	exec >/dev/null 2>&1 3<&-
	x TERM
	( sleep ` + killGraceArg(grace) + ` || sleep 1; x KILL ) &
	t=$!
	wait $c
	while left && kill -0 $t 2>/dev/null; do sleep 0.1 || sleep 1; done
	kill -KILL $t 2>/dev/null
	exit 143
}
m=t
S=
if command -v setsid >/dev/null 2>&1; then
	m=s
	S=setsid
else
	g=$(i $$)
	case ${g#* } in $$) m=g;; esac
fi
if [ "$m" = t ] && [ -z "$P" ]; then
	echo 'herdrcron: no /proc and ps cannot list pid and ppid; a cancel will not see processes the command started' >&2
fi
trap stop USR1
$S /bin/sh -c ` + shellquote.Quote("exec "+line) + ` <&4 3<&- 4<&- &
c=$!
exec 4<&-
{ read -r _ <&3; kill -USR1 $$; } >/dev/null 2>&1 &
w=$!
wait $c
r=$?
trap '' USR1
kill $w 2>/dev/null
exit $r`
}

// killGraceArg is the sleep(1) argument for grace. Whole seconds stay
// integers so a POSIX sleep accepts them. Anything else is milliseconds
// ("0.400"); the script falls back to one second if sleep rejects it.
func killGraceArg(grace time.Duration) string {
	if grace <= 0 {
		grace = defaultKillGrace
	}
	if grace < 100*time.Millisecond {
		grace = 100 * time.Millisecond
	}
	if grace%time.Second == 0 {
		return strconv.Itoa(int(grace / time.Second))
	}
	ms := int(grace.Milliseconds())
	return fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
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
