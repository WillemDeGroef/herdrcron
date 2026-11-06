//go:build integration

package target_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// TestSSHIntegration runs the SSH target against a real sshd. Set
// HERDRCRON_SSH_TEST_HOST to an alias in ~/.ssh/config with key access
// (for example a local sshd container named sshd-<name> on a spare port,
// such as linuxserver/openssh-server); the test is skipped otherwise.
//
//	HERDRCRON_SSH_TEST_HOST=hc-sshd go test -tags integration ./internal/target -run SSHIntegration
func TestSSHIntegration(t *testing.T) {
	host := os.Getenv("HERDRCRON_SSH_TEST_HOST")
	if host == "" {
		t.Skip("HERDRCRON_SSH_TEST_HOST is not set")
	}
	ctx := context.Background()
	s := target.NewSSH(target.SSHConfig{Name: "it", Host: host, ControlDir: t.TempDir()})

	nasty := "it's $HOME `id` \"q\"\nline two"
	out, err := s.Exec(ctx, []string{"printf", "%s", nasty}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != nasty {
		t.Errorf("stdout = %q, want %q", out, nasty)
	}

	path := "~/.cache/herdrcron/runs/hc-integration.md"
	if err := s.PutFile(ctx, path, []byte(nasty)); err != nil {
		t.Fatal(err)
	}
	out, err = s.Exec(ctx, []string{"sh", "-c", `cat "$HOME/.cache/herdrcron/runs/hc-integration.md"`}, nil)
	if err != nil || string(out) != nasty {
		t.Errorf("remote file = %q, %v", out, err)
	}
	if err := s.RemoveFile(ctx, path); err != nil {
		t.Fatal(err)
	}

	_, err = s.Exec(ctx, []string{"sh", "-c", "exit 7"}, nil)
	var ee *target.ExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Errorf("exit 7 gave %v", err)
	}

	bad := target.NewSSH(target.SSHConfig{Name: "bad", Host: "herdrcron-no-such-host.invalid", ControlDir: t.TempDir()})
	if _, err := bad.Exec(ctx, []string{"true"}, nil); !errors.Is(err, target.ErrUnreachable) || !strings.Contains(err.Error(), "bad") {
		t.Errorf("unknown host gave %v, want ErrUnreachable", err)
	}
}
