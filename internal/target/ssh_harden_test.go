package target_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/shellquote"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// writePid is the start of a remote sh -c command: it writes the pid in $2
// to the file $1 in one step, so a reader never sees half a pid.
const writePid = `pidfile() { echo "$2" > "$1.tmp" && mv "$1.tmp" "$1"; }; `

// remoteScript decodes the /bin/sh script inside a remote command line.
func remoteScript(t *testing.T, line string) string {
	t.Helper()
	payload := strings.TrimSuffix(strings.TrimPrefix(line, neutralPrefix), "'")
	var b strings.Builder
	for i := 0; i < len(payload); i++ {
		if payload[i] != '\\' {
			b.WriteByte(payload[i])
			continue
		}
		c, err := strconv.ParseUint(payload[i+2:i+5], 8, 8)
		if err != nil {
			t.Fatalf("escape at %d: %v", i, err)
		}
		b.WriteByte(byte(c))
		i += 4
	}
	return b.String()
}

// newGraceSSH is newTestSSH with a short wait between SIGTERM and
// SIGKILL. The remote sleep accepts a fraction, so the suite does not
// sit on the production 5s default.
func newGraceSSH(t *testing.T) *target.SSH {
	t.Helper()
	return newSSHWithGrace(t, 400*time.Millisecond)
}

func newSSHWithGrace(t *testing.T, grace time.Duration) *target.SSH {
	t.Helper()
	return target.NewSSH(target.SSHConfig{
		Name: "vps", Host: "vps-alias", Shell: "sh -c",
		ControlDir: filepath.Join(t.TempDir(), "ssh"),
		KillGrace:  grace,
	})
}

// poorRemote makes the fake remote machine one without setsid and with a
// BusyBox ps: it can list pid, ppid and pgid, and it rejects -p.
func poorRemote(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"sh", "sleep", "cat", "rm", "mv", "mkdir", "mktemp"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	// BusyBox ps: `ps -A -o pid= -o ppid= -o pgid=` works, `-p` does not.
	// A wrapper that still passes -p gets no process list from this fake.
	real, err := exec.LookPath("ps")
	if err != nil {
		t.Skipf("no ps: %v", err)
	}
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"\tcase $a in\n" +
		"\t-p*) echo \"ps: invalid option -- p\" >&2; exit 1;;\n" +
		"\tesac\n" +
		"done\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SSH_REMOTE_PATH", bin)
}

// blindRemote is a machine with no setsid and a ps that ignores its options
// and prints a default header. That listing must not be parsed as processes.
func blindRemote(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"sh", "sleep", "cat", "rm", "mv", "mkdir", "mktemp"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	ps := "#!/bin/sh\necho 'ps: invalid option' >&2\necho '  PID TTY          TIME CMD'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(ps), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SSH_REMOTE_PATH", bin)
	t.Setenv("FAKE_SSH_NO_PROC", "1")
}

// noSetsidRemote makes the fake remote machine one without setsid.
func noSetsidRemote(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"sh", "sleep", "cat", "rm", "mv", "mkdir", "mktemp", "ps"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKE_SSH_REMOTE_PATH", bin)
}

// remotes are the fake remote machines a cancel must work on.
var remotes = []struct {
	name  string
	setup func(t *testing.T)
}{
	{"own session", func(t *testing.T) { t.Setenv("FAKE_SSH_REMOTE", "session") }},
	{"shared group", func(t *testing.T) { t.Setenv("FAKE_SSH_REMOTE", "child") }},
	{"shared group, no setsid", func(t *testing.T) {
		t.Setenv("FAKE_SSH_REMOTE", "child")
		noSetsidRemote(t)
	}},
	{"shared group, no setsid, no /proc", func(t *testing.T) {
		t.Setenv("FAKE_SSH_REMOTE", "child")
		t.Setenv("FAKE_SSH_NO_PROC", "1")
		noSetsidRemote(t)
	}},
	{"own session, no /proc", func(t *testing.T) {
		t.Setenv("FAKE_SSH_REMOTE", "session")
		t.Setenv("FAKE_SSH_NO_PROC", "1")
	}},
	{"own session, BusyBox ps, no /proc", func(t *testing.T) {
		t.Setenv("FAKE_SSH_REMOTE", "session")
		t.Setenv("FAKE_SSH_NO_PROC", "1")
		poorRemote(t)
	}},
	{"shared group, BusyBox ps, no setsid, no /proc", func(t *testing.T) {
		t.Setenv("FAKE_SSH_REMOTE", "child")
		t.Setenv("FAKE_SSH_NO_PROC", "1")
		poorRemote(t)
	}},
}

// startExec runs Exec in the background and returns what cancels it and
// the channel its error arrives on.
func startExec(t *testing.T, s *target.SSH, remote []string, stdin io.Reader) (cancel func(), done <-chan error) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	ch := make(chan error, 1)
	go func() {
		_, err := s.Exec(ctx, remote, stdin)
		ch <- err
	}()
	return stop, ch
}

// wantCancelled fails unless the call returns an error soon.
func wantCancelled(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled call returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled call did not return")
	}
}

// wantGone fails unless the process ends soon.
func wantGone(t *testing.T, what string, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for processRuns(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) still runs after the call was cancelled", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// remotePid waits for the pid a remote command wrote, and kills that
// process when the test ends.
func remotePid(t *testing.T, pidfile string) int {
	t.Helper()
	pid := waitForPid(t, pidfile)
	t.Cleanup(func() { _ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run() })
	return pid
}

// A command that ignores SIGTERM, and what it started, gets SIGKILL after
// the grace period.
func TestSSHCancelKillsACommandThatIgnoresSIGTERM(t *testing.T) {
	needSetsid(t)
	for _, r := range remotes {
		t.Run(r.name, func(t *testing.T) {
			fakeSSH(t)
			r.setup(t)
			s := newSSHWithGrace(t, 600*time.Millisecond)
			dir := t.TempDir()
			script := writePid + `trap "" TERM; sleep 300 & pidfile "$1/child" $!; pidfile "$1/cmd" $$; wait`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, nil)
			cmd := remotePid(t, filepath.Join(dir, "cmd"))
			child := remotePid(t, filepath.Join(dir, "child"))

			cancel()
			time.Sleep(150 * time.Millisecond)
			if !processRuns(cmd) || !processRuns(child) {
				t.Error("the command was killed before the grace period ended")
			}
			wantCancelled(t, done)
			wantGone(t, "the remote command", cmd)
			wantGone(t, "the remote command's child", child)
		})
	}
}

// SIGTERM comes first: a command that handles it gets to clean up.
func TestSSHCancelSendsSIGTERMBeforeSIGKILL(t *testing.T) {
	needSetsid(t)
	for _, r := range remotes {
		t.Run(r.name, func(t *testing.T) {
			fakeSSH(t)
			r.setup(t)
			s := newGraceSSH(t)
			dir := t.TempDir()
			script := writePid + `trap 'echo bye > "$1/cleaned"; exit 0' TERM; pidfile "$1/cmd" $$; while :; do sleep 0.1; done`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, nil)
			cmd := remotePid(t, filepath.Join(dir, "cmd"))

			cancel()
			wantCancelled(t, done)
			wantGone(t, "the remote command", cmd)
			if _, err := os.Stat(filepath.Join(dir, "cleaned")); err != nil {
				t.Errorf("the command did not get to handle SIGTERM: %v", err)
			}
		})
	}
}

// A shell command that is at its first step must not get to its next one:
// stopping the step alone would let the shell go on.
func TestSSHCancelStopsACommandBetweenItsSteps(t *testing.T) {
	needSetsid(t)
	for _, r := range remotes {
		t.Run(r.name, func(t *testing.T) {
			fakeSSH(t)
			r.setup(t)
			s := newGraceSSH(t)
			dir := t.TempDir()
			script := writePid + `pidfile "$1/cmd" $$; sleep 300; echo next > "$1/next"; sleep 300`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, nil)
			cmd := remotePid(t, filepath.Join(dir, "cmd"))
			time.Sleep(50 * time.Millisecond)

			cancel()
			wantCancelled(t, done)
			wantGone(t, "the remote command", cmd)
			if _, err := os.Stat(filepath.Join(dir, "next")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the command went on to its next step after the cancel: %v", err)
			}
		})
	}
}

// With setsid on the remote machine the command has its own process group
// whether or not the wrapper leads one, and the cancel signals that group.
// So it reaches a process whose parent already exited, which no walk down
// the process tree finds, and it cannot hit a PID that was reused since a
// listing.
func TestSSHCancelStopsAnOrphanTheCommandLeftBehind(t *testing.T) {
	needSetsid(t)
	for _, mode := range []string{"session", "child", "login"} {
		t.Run(mode, func(t *testing.T) {
			_, fakeDir := fakeSSH(t)
			t.Setenv("FAKE_SSH_REMOTE", mode)
			s := newGraceSSH(t)
			dir := t.TempDir()
			script := writePid + `sh -c 'sleep 300 & echo $! > "$1/orphan.tmp"' sh "$1"; mv "$1/orphan.tmp" "$1/orphan"; pidfile "$1/cmd" $$; exec sleep 300`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, nil)
			cmd := remotePid(t, filepath.Join(dir, "cmd"))
			orphan := remotePid(t, filepath.Join(dir, "orphan"))

			cancel()
			wantCancelled(t, done)
			wantGone(t, "the remote command", cmd)
			wantGone(t, "the orphan of the remote command", orphan)
			if mode == "login" {
				if bystander := remotePid(t, filepath.Join(fakeDir, "bystander")); !processRuns(bystander) {
					t.Errorf("a job of the parent process group (pid %d) was signalled", bystander)
				}
			}
		})
	}
}

// Without setsid the wrapper signals the command's process tree one by
// one. It must still leave the other jobs of its process group alone.
func TestSSHCancelWithoutSetsidDoesNotSignalTheGroupsOtherJob(t *testing.T) {
	needSetsid(t)
	for _, noProc := range []string{"", "1"} {
		t.Run("no /proc="+noProc, func(t *testing.T) {
			_, fakeDir := fakeSSH(t)
			t.Setenv("FAKE_SSH_REMOTE", "login")
			t.Setenv("FAKE_SSH_NO_PROC", noProc)
			noSetsidRemote(t)
			s := newGraceSSH(t)
			dir := t.TempDir()
			script := writePid + `trap "" TERM; sleep 300 & pidfile "$1/child" $!; wait`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, nil)
			child := remotePid(t, filepath.Join(dir, "child"))
			bystander := remotePid(t, filepath.Join(fakeDir, "bystander"))

			cancel()
			wantCancelled(t, done)
			wantGone(t, "the remote command's child", child)
			if !processRuns(bystander) {
				t.Errorf("a job of the parent process group (pid %d) was signalled", bystander)
			}
		})
	}
}

// A ps that cannot list pid, ppid and pgid, and no /proc, is reported.
// Cancel still SIGKILLs the command itself and does not guess at other pids.
func TestSSHCancelReportsWhenPsCannotListProcesses(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_REMOTE", "child")
	blindRemote(t)
	s := newGraceSSH(t)

	_, err := s.Exec(context.Background(), []string{"sh", "-c", "echo boom >&2; exit 3"}, nil)
	var ee *target.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || !strings.Contains(string(ee.Stderr), "will not see processes the command started") {
		t.Fatalf("Exec err = %v", err)
	}
	if !strings.Contains(string(ee.Stderr), "boom") {
		t.Errorf("stderr = %q, want the command's own stderr as well", ee.Stderr)
	}

	dir := t.TempDir()
	// The process that ignores SIGTERM has to be the wrapper's direct child
	// (the target shell command). A nested sh -c is a grandchild, and this
	// remote cannot list grandchildren.
	body := writePid + `trap "" TERM; sh -c 'trap "" HUP; exec sleep 300' & pidfile "$1/child" $!; pidfile "$1/cmd" $$; wait`
	s2 := target.NewSSH(target.SSHConfig{
		Name: "vps", Host: "vps-alias",
		Shell:      "sh -c " + shellquote.Quote(body) + " sh",
		ControlDir: filepath.Join(t.TempDir(), "ssh"),
		KillGrace:  400 * time.Millisecond,
	})
	cancel, done := startExec(t, s2, []string{dir}, nil)
	cmd := remotePid(t, filepath.Join(dir, "cmd"))
	child := remotePid(t, filepath.Join(dir, "child"))
	cancel()
	wantCancelled(t, done)
	wantGone(t, "the remote command", cmd)
	if !processRuns(child) {
		t.Error("a child was signalled even though the process list was unusable")
	}
}

// The wrapper is a /bin/sh script, and /bin/sh is not dash everywhere.
func TestSSHWrapperRunsUnderOtherShells(t *testing.T) {
	needSetsid(t)
	for _, sh := range []string{"busybox sh", "bash --posix"} {
		t.Run(sh, func(t *testing.T) {
			if _, err := exec.LookPath(strings.Fields(sh)[0]); err != nil {
				t.Skip(err)
			}
			fakeSSH(t)
			t.Setenv("FAKE_SSH_SH", sh)
			t.Setenv("FAKE_SSH_REMOTE", "session")
			home := t.TempDir()
			t.Setenv("HOME", home)
			s := newGraceSSH(t)

			data := anyBytes(20_000)
			out, err := s.Exec(context.Background(), []string{"sh", "-c", "cat; exit 7"}, bytes.NewReader(data))
			var exit *target.ExitError
			if !errors.As(err, &exit) || exit.Code != 7 || !bytes.Equal(out, data) {
				t.Errorf("Exec: %d bytes out, err = %v; want the %d bytes of stdin and exit 7", len(out), err, len(data))
			}
			if err := s.PutFile(context.Background(), "~/d/p.md", data); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(home, "d", "p.md")); err != nil || !bytes.Equal(got, data) {
				t.Errorf("PutFile wrote %d bytes, %v", len(got), err)
			}

			dir := t.TempDir()
			script := writePid + `trap "" TERM; sleep 300 & pidfile "$1/child" $!; wait`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, nil)
			child := remotePid(t, filepath.Join(dir, "child"))
			cancel()
			wantCancelled(t, done)
			wantGone(t, "the remote command's child", child)
		})
	}
}

// anyBytes is n bytes that hold every byte value, with runs of NULs,
// newlines, backslashes and quotes.
func anyBytes(n int) []byte {
	b := make([]byte, 0, n)
	for i := 0; len(b) < n; i++ {
		switch i % 7 {
		case 0:
			b = append(b, byte(i/7))
		case 1:
			b = append(b, "\\.\n\n\\0101'\"%s\x00\x00 \r\n"...)
		default:
			b = append(b, byte(i*31))
		}
	}
	return b[:n]
}

// Stdin of any size and content reaches the command as it is.
func TestSSHExecStdinCarriesAnyBytes(t *testing.T) {
	fakeSSH(t)
	s, _ := newTestSSH(t, "sh -c")
	for _, n := range []int{0, 1, 255, 256, 257, 100_000} {
		data := anyBytes(n)
		out, err := s.Exec(context.Background(), []string{"cat"}, bytes.NewReader(data))
		if err != nil || !bytes.Equal(out, data) {
			t.Errorf("%d bytes of stdin: got %d bytes back, err = %v", n, len(out), err)
		}
	}
}

// Exec does not read stdin to its end before it starts: ssh runs and gets
// the data while the reader is still open.
func TestSSHExecStreamsStdin(t *testing.T) {
	_, fakeDir := fakeSSH(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	s, _ := newTestSSH(t, "sh -c")
	r, w := io.Pipe()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := s.Exec(context.Background(), []string{"cat"}, r)
		done <- result{out, err}
	}()
	if _, err := w.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	// The data is on the remote machine before stdin ends.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(fakeDir, "argv")); err == nil && spooled(t, tmp) == "one\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first stdin data did not reach the remote machine while stdin was open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := w.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	select {
	case got := <-done:
		if got.err != nil || string(got.out) != "one\ntwo\n" {
			t.Errorf("out = %q, err = %v", got.out, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return after stdin ended")
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("the remote temp directory still has %d entries", len(entries))
	}
}

// spooled is the content of the files in the remote temp directory.
func spooled(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		b.Write(data)
	}
	return b.String()
}

// A cancel does not wait for a stdin that never ends, the command does not
// run on half its stdin, and nothing stays behind on the remote machine.
func TestSSHExecCancelDoesNotWaitForStdin(t *testing.T) {
	needSetsid(t)
	fakeSSH(t)
	t.Setenv("FAKE_SSH_REMOTE", "session")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	s, _ := newTestSSH(t, "sh -c")
	marker := filepath.Join(t.TempDir(), "ran")
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	cancel, done := startExec(t, s, []string{"sh", "-c", `cat > "$1"`, "sh", marker}, r)
	if _, err := w.Write(anyBytes(100_000)); err != nil {
		t.Fatal(err)
	}

	cancel()
	wantCancelled(t, done)
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the remote temp directory still has %d entries after the cancel", len(entries))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command ran on a stdin that was cut short: %v", err)
	}
}

// Stdin data the command never reads, more than a pipe holds, does not
// keep a cancel from stopping the command.
func TestSSHCancelStopsACommandThatLeavesItsStdinUnread(t *testing.T) {
	needSetsid(t)
	for _, mode := range []string{"session", "child"} {
		t.Run(mode, func(t *testing.T) {
			fakeSSH(t)
			t.Setenv("FAKE_SSH_REMOTE", mode)
			s := newGraceSSH(t)
			dir := t.TempDir()
			script := writePid + `pidfile "$1/cmd" $$; exec sleep 300`
			cancel, done := startExec(t, s, []string{"sh", "-c", script, "sh", dir}, bytes.NewReader(anyBytes(100_000)))
			cmd := remotePid(t, filepath.Join(dir, "cmd"))

			cancel()
			wantCancelled(t, done)
			wantGone(t, "the remote command", cmd)
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read(p []byte) (int, error) { return copy(p, "partial"), r.err }

// A stdin that fails is an error of the call, and the command does not run
// on the part that was read.
func TestSSHExecStdinReadError(t *testing.T) {
	fakeSSH(t)
	s, _ := newTestSSH(t, "sh -c")
	marker := filepath.Join(t.TempDir(), "ran")
	boom := errors.New("boom")
	_, err := s.Exec(context.Background(), []string{"sh", "-c", `cat > "$1"`, "sh", marker}, failingReader{boom})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the reader's error", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command ran on a stdin that failed: %v", err)
	}
}

// An upload whose connection drops, as when the call is cancelled, leaves
// neither a truncated file nor a temp file, and keeps the file that was
// there.
func TestSSHPutFileCutShortLeavesNoFile(t *testing.T) {
	for _, mode := range []string{"", "session"} {
		t.Run("remote="+mode, func(t *testing.T) {
			if mode == "session" {
				needSetsid(t)
			}
			fakeSSH(t)
			t.Setenv("FAKE_SSH_REMOTE", mode)
			home := t.TempDir()
			t.Setenv("HOME", home)
			s, _ := newTestSSH(t, "sh -c")
			ctx := context.Background()
			if err := s.PutFile(ctx, "~/runs/old.md", []byte("old")); err != nil {
				t.Fatal(err)
			}

			t.Setenv("FAKE_SSH_STDIN_CUT", "5000")
			if err := s.PutFile(ctx, "~/runs/old.md", anyBytes(50_000)); err == nil {
				t.Error("PutFile over a file: no error for an upload that was cut short")
			}
			if err := s.PutFile(ctx, "~/runs/new.md", anyBytes(50_000)); err == nil {
				t.Error("PutFile of a new file: no error for an upload that was cut short")
			}
			entries, err := os.ReadDir(filepath.Join(home, "runs"))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if len(names) != 1 || names[0] != "old.md" {
				t.Errorf("remote directory holds %q, want only old.md", names)
			}
			if data, _ := os.ReadFile(filepath.Join(home, "runs", "old.md")); string(data) != "old" {
				t.Errorf("old.md = %q, want it unchanged", data)
			}
		})
	}
}

// An upload cancelled half way returns and leaves no temp file.
func TestSSHPutFileCancelled(t *testing.T) {
	needSetsid(t)
	fakeSSH(t)
	t.Setenv("FAKE_SSH_REMOTE", "session")
	t.Setenv("FAKE_SSH_STDIN_STALL", "5000")
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := newTestSSH(t, "sh -c")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.PutFile(ctx, "~/runs/p.md", anyBytes(50_000)) }()

	runs := filepath.Join(home, "runs")
	waitEntries(t, runs, 1, "the upload never started")
	cancel()
	wantCancelled(t, done)
	waitEntries(t, runs, 0, "the temp file is still there after the cancel")
}

// waitEntries waits until dir holds n entries.
func waitEntries(t *testing.T, dir string, n int, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		if len(entries) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d entries in %s, want %d", msg, len(entries), dir, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Uploads of any content arrive whole, with mode 0600.
func TestSSHPutFileCarriesAnyBytes(t *testing.T) {
	fakeSSH(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := newTestSSH(t, "sh -c")
	for _, n := range []int{0, 100_000} {
		data := anyBytes(n)
		if err := s.PutFile(context.Background(), "~/runs/p.md", data); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(home, "runs", "p.md")
		if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, data) {
			t.Errorf("%d bytes: read %d bytes, %v", n, len(got), err)
		}
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	}
}
