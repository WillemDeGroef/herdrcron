package target_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// neutralPrefix starts every remote command line: the remote login shell
// only has to run /bin/sh with two single-quoted words.
const neutralPrefix = `/bin/sh -c 'eval "$(printf %b "$0")"' '`

// The remote login shell (bash, zsh, fish, tcsh, …) reads the command line
// ssh sends. Single-quoted words mean the same in all of them only without
// a quote, a newline, "!" (csh history) or a backslash before a quote or a
// backslash (fish escapes). So the line must be two such words, with every
// other byte written as a printf %b octal escape for /bin/sh to decode.
func TestSSHRemoteCommandLineDoesNotDependOnTheLoginShell(t *testing.T) {
	argv, _ := fakeSSH(t)
	t.Setenv("FAKE_SSH_EXIT", "0")
	s, _ := newTestSSH(t, "")
	nasty := "it's a\\'b \\\\ !! $HOME `id` \"q\"\nline two é ~ * ; | & > #"

	check := func(t *testing.T, line string) {
		t.Helper()
		rest, ok := strings.CutPrefix(line, neutralPrefix)
		if !ok {
			t.Fatalf("command line = %q, want prefix %q", line, neutralPrefix)
		}
		payload, ok := strings.CutSuffix(rest, "'")
		if !ok {
			t.Fatalf("command line does not end its second quoted word: %q", line)
		}
		for i := 0; i < len(payload); i++ {
			c := payload[i]
			switch {
			case c == '\\':
				if i+4 >= len(payload) || payload[i+1] != '0' || strings.Trim(payload[i+2:i+5], "01234567") != "" {
					t.Fatalf("backslash at %d is not a \\0ooo escape: %q", i, payload)
				}
				i += 4
			case c < ' ' || c > '~' || c == '\'' || c == '!':
				t.Fatalf("byte %q at %d is not the same in every login shell: %q", c, i, payload)
			}
		}
	}

	t.Run("Exec", func(t *testing.T) {
		if _, err := s.Exec(context.Background(), []string{"herdr", "pane", "run", nasty}, nil); err != nil {
			t.Fatal(err)
		}
		got := argv()
		check(t, got[len(got)-1])
	})
	t.Run("PutFile", func(t *testing.T) {
		if err := s.PutFile(context.Background(), "~/"+nasty, []byte("x")); err != nil {
			t.Fatal(err)
		}
		got := argv()
		check(t, got[len(got)-1])
	})
	t.Run("InteractiveArgv", func(t *testing.T) {
		got := s.InteractiveArgv([]string{"herdr", "agent", "attach", nasty})
		check(t, got[len(got)-1])
	})
}

// A killed ssh takes nothing with it on the remote machine: the command
// there only sees ssh's stdin close. Cancelling a call must still stop it.
func TestSSHCancelStopsTheRemoteCommand(t *testing.T) {
	needSetsid(t)
	for _, mode := range []string{"session", "child"} {
		t.Run(mode, func(t *testing.T) {
			fakeSSH(t)
			t.Setenv("FAKE_SSH_REMOTE", mode)
			s, _ := newTestSSH(t, "sh -c")
			pidfile := filepath.Join(t.TempDir(), "pid")
			// The remote command writes its pid and then runs "forever".
			remote := []string{"sh", "-c", `echo $$ > "$1.tmp" && mv "$1.tmp" "$1" && exec sleep 300`, "sh", pidfile}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := s.Exec(ctx, remote, nil)
				done <- err
			}()
			pid := waitForPid(t, pidfile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Error("cancelled call returned no error")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("cancelled call did not return")
			}
			deadline := time.Now().Add(20 * time.Second)
			for processRuns(pid) {
				if time.Now().After(deadline) {
					t.Fatalf("remote command (pid %d) still runs after the call was cancelled", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// With its own session, as under sshd, everything the remote command
// started goes with it.
func TestSSHCancelStopsWhatTheRemoteCommandStarted(t *testing.T) {
	needSetsid(t)
	fakeSSH(t)
	t.Setenv("FAKE_SSH_REMOTE", "session")
	s, _ := newTestSSH(t, "sh -c")
	pidfile := filepath.Join(t.TempDir(), "pid")
	remote := []string{"sh", "-c", `sleep 300 & echo $! > "$1.tmp" && mv "$1.tmp" "$1" && wait`, "sh", pidfile}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Exec(ctx, remote, nil)
		done <- err
	}()
	pid := waitForPid(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled call returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled call did not return")
	}
	deadline := time.Now().Add(20 * time.Second)
	for processRuns(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the remote command's child (pid %d) still runs after the call was cancelled", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The login shell that started the remote command may lead a process group
// it already had (an interactive user's shell). Cancelling must stop the
// command and what it started, and must not signal that group's other jobs.
func TestSSHCancelDoesNotSignalTheParentGroupsOtherJob(t *testing.T) {
	needSetsid(t)
	_, dir := fakeSSH(t)
	t.Setenv("FAKE_SSH_REMOTE", "login")
	s, _ := newTestSSH(t, "sh -c")
	pidfile := filepath.Join(t.TempDir(), "pid")
	remote := []string{"sh", "-c", `sleep 300 & echo $! > "$1.tmp" && mv "$1.tmp" "$1" && wait`, "sh", pidfile}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Exec(ctx, remote, nil)
		done <- err
	}()
	child := waitForPid(t, pidfile)
	bystander := waitForPid(t, filepath.Join(dir, "bystander"))
	t.Cleanup(func() {
		_ = syscall.Kill(child, syscall.SIGKILL)
		_ = syscall.Kill(bystander, syscall.SIGKILL)
	})

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled call returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled call did not return")
	}
	deadline := time.Now().Add(20 * time.Second)
	for processRuns(child) {
		if time.Now().After(deadline) {
			t.Fatalf("the remote command's child (pid %d) still runs after the call was cancelled", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !processRuns(bystander) {
		t.Fatalf("a job of the parent process group (pid %d) was signalled when the call was cancelled", bystander)
	}
}

// A call that is not cancelled is not cut short: the command outlives the
// end of its stdin data and its exit code comes back.
func TestSSHCommandOutlivesItsStdinData(t *testing.T) {
	for _, mode := range []string{"", "session", "child"} {
		t.Run("remote="+mode, func(t *testing.T) {
			if mode == "session" {
				needSetsid(t)
			}
			fakeSSH(t)
			t.Setenv("FAKE_SSH_REMOTE", mode)
			s, _ := newTestSSH(t, "sh -c")
			out, err := s.Exec(context.Background(), []string{"sh", "-c", "cat; sleep 0.3; echo late"}, strings.NewReader("data\n"))
			if err != nil || string(out) != "data\nlate\n" {
				t.Errorf("with stdin: out = %q, err = %v", out, err)
			}
			out, err = s.Exec(context.Background(), []string{"sh", "-c", "cat; sleep 0.3; echo late"}, nil)
			if err != nil || string(out) != "late\n" {
				t.Errorf("without stdin: out = %q, err = %v", out, err)
			}
		})
	}
}

// needSetsid skips a test whose fake remote needs its own session when
// there is no setsid that can wait for its command (util-linux), or no
// /proc to tell a live process from a zombie.
func needSetsid(t *testing.T) {
	t.Helper()
	if err := exec.Command("setsid", "-w", "true").Run(); err != nil {
		t.Skipf("no setsid -w to give the fake remote its own session: %v", err)
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
}

func waitForPid(t *testing.T, pidfile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(pidfile)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("pid file: %v", err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the remote command never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processRuns reports whether pid is a live process (not gone, not a
// zombie waiting for its parent).
func processRuns(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	// "pid (comm) state …": a zombie has exited.
	if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && strings.HasPrefix(string(stat[i+1:]), " Z") {
		return false
	}
	return true
}
