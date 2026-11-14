package target_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// fakeSSHScript stands in for the system ssh binary. It records its argv
// (one element per NUL-terminated record), then acts on
// FAKE_SSH_EXIT: unset runs the last argument (the remote command line)
// with sh -c, as sshd would hand it to the login shell; a number exits
// with that code after printing FAKE_SSH_STDERR to stderr; FAKE_SSH_HANG
// set hangs, as ssh over a dead ControlMaster connection does.
//
// FAKE_SSH_REMOTE says how the remote command line relates to the ssh
// process. Unset, it replaces it. "session" runs it as sshd does: in a new
// session and process group that outlives a killed ssh and only sees its
// stdin close. "child" also outlives ssh but shares its process group, as
// behind a login shell that does not exec its command. "login" is a session
// leader with another job beside the command. The leader execs the remote
// line, as a login shell does with a simple command, so it stays the parent
// and the group leader. That job ignores SIGHUP so the session ending does
// not look like a kill.
//
// The remote machine can be made poorer: FAKE_SSH_REMOTE_PATH is the PATH
// of the remote command line, FAKE_SSH_SH the shell that stands in for
// /bin/sh, and FAKE_SSH_NO_PROC set hides /proc from the wrapper script.
// FAKE_SSH_STDIN_CUT=n drops the connection after n bytes of stdin: the
// remote side reads those and then sees stdin close. FAKE_SSH_STDIN_STALL=n
// is a connection that stalls after n bytes: the remote side reads those
// and then nothing, until the caller closes stdin.
const fakeSSHScript = `#!/bin/sh
dir=$(dirname "$0")
: > "$dir/argv"
for a in "$@"; do printf '%s\0' "$a" >> "$dir/argv"; done
for a in "$@"; do last=$a; done
if [ -n "$FAKE_SSH_HOLD_STDERR" ]; then
	# Like a ControlPersist master forked by old OpenSSH: a child keeps
	# stderr open after ssh exits.
	sleep "$FAKE_SSH_HOLD_STDERR" >&2 &
fi
if [ -n "$FAKE_SSH_HANG" ]; then
	exec sleep 60
fi
if [ -n "$FAKE_SSH_EXIT" ]; then
	printf '%s' "$FAKE_SSH_STDERR" >&2
	exit "$FAKE_SSH_EXIT"
fi
if [ -n "$FAKE_SSH_SH" ]; then
	last="$FAKE_SSH_SH${last#/bin/sh}"
fi
if [ -n "$FAKE_SSH_NO_PROC" ]; then
	last=$(printf '%s' "$last" | sed 's|/proc/|/no-proc/|g')
fi
if [ -n "$FAKE_SSH_STDIN_CUT" ]; then
	head -c "$FAKE_SSH_STDIN_CUT" > "$dir/stdin"
	exec < "$dir/stdin"
fi
if [ -n "$FAKE_SSH_STDIN_STALL" ]; then
	mkfifo "$dir/in"
	exec 5<&0
	{ head -c "$FAKE_SSH_STDIN_STALL" <&5; cat <&5 >/dev/null; } > "$dir/in" &
	exec < "$dir/in" 5<&-
fi
env=$(command -v env)
rp=${FAKE_SSH_REMOTE_PATH:-$PATH}
case "$FAKE_SSH_REMOTE" in
session) setsid -w "$env" "PATH=$rp" sh -c "$last"; exit $? ;;
child) "$env" "PATH=$rp" sh -c "$last"; exit $? ;;
login) setsid -w sh -c 'trap "" HUP; sleep 300 & echo $! > "$1/bystander"; PATH=$3; exec sh -c "$2"' sh "$dir" "$last" "$rp"; exit $? ;;
esac
exec "$env" "PATH=$rp" sh -c "$last"
`

// fakeSSH puts a fake ssh first on PATH and returns a function that reads
// the argv of its last call.
func fakeSSH(t *testing.T) (argv func() []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_EXIT", "")
	t.Setenv("FAKE_SSH_HANG", "")
	t.Setenv("FAKE_SSH_REMOTE", "")
	t.Setenv("FAKE_SSH_REMOTE_PATH", "")
	t.Setenv("FAKE_SSH_SH", "")
	t.Setenv("FAKE_SSH_NO_PROC", "")
	t.Setenv("FAKE_SSH_STDIN_CUT", "")
	t.Setenv("FAKE_SSH_STDIN_STALL", "")
	return func() []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "argv"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	}, dir
}

// newTestSSH returns an SSH target whose shell is plain "sh -c", so the
// fake ssh can run remote commands without reading login profiles.
func newTestSSH(t *testing.T, shell string) (*target.SSH, string) {
	t.Helper()
	control := filepath.Join(t.TempDir(), "ssh")
	return target.NewSSH(target.SSHConfig{Name: "vps", Host: "vps-alias", Shell: shell, ControlDir: control}), control
}

func TestSSHName(t *testing.T) {
	s, _ := newTestSSH(t, "")
	if got := s.Name(); got != "vps" {
		t.Errorf("Name = %q", got)
	}
}

func TestSSHControlPathIsShortHashNotPercentC(t *testing.T) {
	argv, _ := fakeSSH(t)
	t.Setenv("FAKE_SSH_EXIT", "0")
	s, control := newTestSSH(t, "")
	if _, err := s.Exec(context.Background(), []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	var cp string
	for _, a := range argv() {
		if after, ok := strings.CutPrefix(a, "ControlPath="); ok {
			cp = after
			break
		}
	}
	if cp == "" {
		t.Fatal("no ControlPath in argv")
	}
	base := filepath.Base(cp)
	wantBase := filepath.Base(target.SSHControlSocketPath(control, "vps-alias"))
	if base != wantBase || strings.Contains(cp, "%") {
		t.Errorf("ControlPath = %q, want basename %q under %q", cp, wantBase, control)
	}
}

func TestSSHExecArgvUsesTheSpecOptionsAndTheTargetShell(t *testing.T) {
	argv, _ := fakeSSH(t)
	t.Setenv("FAKE_SSH_EXIT", "0")
	s, control := newTestSSH(t, "")
	if _, err := s.Exec(context.Background(), []string{"herdr", "pane", "list"}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + target.SSHControlSocketPath(control, "vps-alias"),
		"-o", "ControlPersist=10m",
		"-o", "ConnectTimeout=10",
		"--", "vps-alias",
	}
	got := argv()
	if len(got) != len(want)+1 || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("argv =\n%q\nwant\n%q and the remote command line", got, want)
	}
	line := got[len(got)-1]
	if !strings.HasPrefix(line, neutralPrefix) || !strings.Contains(remoteScript(t, line), `/bin/sh -c 'bash -lc '\''herdr pane list'\'''`) {
		t.Errorf("remote command line = %q, want the target shell command inside the /bin/sh wrapper", line)
	}
	fi, err := os.Stat(control)
	if err != nil {
		t.Fatalf("control dir: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("control dir mode = %v, want a 0700 directory", fi.Mode())
	}
}

func TestSSHExecQuotesEveryArgument(t *testing.T) {
	fakeSSH(t)
	s, _ := newTestSSH(t, "sh -c")
	nasty := "it's $HOME `id` \"q\" \\ ; rm -rf /\nline two! \\' é \\\\ \\n 100%"
	out, err := s.Exec(context.Background(), []string{"printf", "%s|%s", nasty, ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), nasty+"|"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestSSHExecPassesStdin(t *testing.T) {
	fakeSSH(t)
	s, _ := newTestSSH(t, "sh -c")
	out, err := s.Exec(context.Background(), []string{"cat"}, strings.NewReader("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "hello\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestSSHExit255IsUnreachable(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_EXIT", "255")
	t.Setenv("FAKE_SSH_STDERR", "ssh: connect to host vps port 22: Connection refused\n")
	s, _ := newTestSSH(t, "")
	_, err := s.Exec(context.Background(), []string{"herdr", "pane", "list", "SECRET-ARG"}, nil)
	if !errors.Is(err, target.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	var ee *target.ExitError
	if errors.As(err, &ee) {
		t.Errorf("unreachable gave an *ExitError: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "vps") || !strings.Contains(msg, "Connection refused") || strings.Contains(msg, "SECRET-ARG") {
		t.Errorf("Error() = %q", msg)
	}
	for _, call := range []func() error{
		func() error { return s.PutFile(context.Background(), "~/x", []byte("p")) },
		func() error { return s.RemoveFile(context.Background(), "~/x") },
	} {
		if err := call(); !errors.Is(err, target.ErrUnreachable) {
			t.Errorf("file op err = %v, want ErrUnreachable", err)
		}
	}
}

// ssh over a ControlMaster connection whose network dropped hangs instead
// of exiting 255: a call that does not answer before its deadline is
// unreachable too. A cancelled call says nothing about the target.
func TestSSHHungCallIsUnreachable(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_HANG", "1")
	s, _ := newTestSSH(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.Exec(ctx, []string{"herdr", "pane", "list"}, nil)
	if !errors.Is(err, target.ErrUnreachable) || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("hung call err = %v, want ErrUnreachable saying no answer", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = s.Exec(ctx, []string{"herdr", "pane", "list"}, nil)
	if err == nil || errors.Is(err, target.ErrUnreachable) {
		t.Fatalf("cancelled call err = %v, want an error that is not ErrUnreachable", err)
	}
}

func TestSSHOtherNonZeroExitIsACommandError(t *testing.T) {
	fakeSSH(t)
	s, _ := newTestSSH(t, "sh -c")
	_, err := s.Exec(context.Background(), []string{"sh", "-c", "echo boom >&2; exit 3", "SECRET-ARG"}, nil)
	var ee *target.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want *ExitError", err, err)
	}
	if errors.Is(err, target.ErrUnreachable) {
		t.Error("exit 3 reported as unreachable")
	}
	if ee.Code != 3 || string(ee.Stderr) != "boom\n" {
		t.Errorf("ExitError = code %d stderr %q", ee.Code, ee.Stderr)
	}
	if !slices.Equal(ee.Argv, []string{"sh", "-c", "echo boom >&2; exit 3", "SECRET-ARG"}) {
		t.Errorf("Argv = %q, want the remote argv", ee.Argv)
	}
	if strings.Contains(ee.Error(), "SECRET-ARG") {
		t.Errorf("Error() leaks the 4th argv element: %q", ee.Error())
	}
}

func TestSSHMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s, _ := newTestSSH(t, "")
	_, err := s.Exec(context.Background(), []string{"true"}, nil)
	if err == nil {
		t.Fatal("want an error without ssh on PATH")
	}
	var ee *target.ExitError
	if errors.As(err, &ee) || errors.Is(err, target.ErrUnreachable) {
		t.Errorf("missing ssh gave %v", err)
	}
}

func TestSSHPutFileWritesThePromptViaStdin(t *testing.T) {
	argv, _ := fakeSSH(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := newTestSSH(t, "sh -c")
	ctx := context.Background()
	prompt := "it's a $PROMPT with `ticks`\nand 'quotes'\n"
	path := "~/.cache/herdrcron/runs/01RUN.md"
	if err := s.PutFile(ctx, path, []byte(prompt)); err != nil {
		t.Fatal(err)
	}
	gotArgv := argv()
	last := gotArgv[len(gotArgv)-1]
	if strings.Contains(last, "PROMPT") {
		t.Errorf("prompt text is in the ssh argv: %q", last)
	}
	if !strings.Contains(last, "||") || !strings.Contains(last, "rm -f") {
		t.Errorf("PutFile script should remove temp on cat failure: %q", last)
	}
	p := filepath.Join(home, ".cache", "herdrcron", "runs", "01RUN.md")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != prompt {
		t.Errorf("content = %q, want %q", data, prompt)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// Overwrite works and leaves no temp files behind.
	if err := s.PutFile(ctx, path, []byte("again")); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1", len(entries))
	}
	if err := s.RemoveFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists: %v", err)
	}
	if err := s.RemoveFile(ctx, path); err != nil {
		t.Errorf("removing a missing file: %v", err)
	}
}

func TestSSHPutFileQuotesOddPaths(t *testing.T) {
	fakeSSH(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := newTestSSH(t, "sh -c")
	if err := s.PutFile(context.Background(), "~/we ird/$x'y.md", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(home, "we ird", "$x'y.md")); err != nil || string(data) != "ok" {
		t.Errorf("read = %q, %v", data, err)
	}
	abs := filepath.Join(t.TempDir(), "a b", "p.md")
	if err := s.PutFile(context.Background(), abs, []byte("abs")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(abs); err != nil || string(data) != "abs" {
		t.Errorf("read = %q, %v", data, err)
	}
}

func TestSSHInteractiveArgv(t *testing.T) {
	s, control := newTestSSH(t, "")
	got := s.InteractiveArgv([]string{"/opt/herdr", "agent", "attach", "w1:p2"})
	want := []string{
		"ssh", "-t",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + target.SSHControlSocketPath(control, "vps-alias"),
		"-o", "ControlPersist=10m",
		"-o", "ConnectTimeout=10",
		"--", "vps-alias",
		`/bin/sh -c 'eval "$(printf %b "$0")"' 'bash -lc \0047/opt/herdr agent attach w1:p2\0047'`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
}

func TestSSHExecSucceedsWhenAForkedMasterHoldsStderr(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_HOLD_STDERR", "4")
	s, _ := newTestSSH(t, "sh -c")
	out, err := s.Exec(context.Background(), []string{"printf", "ok"}, nil)
	if err != nil {
		t.Fatalf("err = %v, want success", err)
	}
	if string(out) != "ok" {
		t.Errorf("stdout = %q", out)
	}
}
