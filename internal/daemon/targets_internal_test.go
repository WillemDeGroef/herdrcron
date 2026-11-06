package daemon

import (
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func TestRealTargetsMakesAnSSHTargetForEveryRemoteTarget(t *testing.T) {
	cfg := config.Config{Targets: map[string]config.Target{
		config.LocalTarget: {},
		"vps":              {Host: "vps-alias", Shell: "bash -lc"},
	}}
	got := realTargets(cfg, t.TempDir())
	if _, ok := got[config.LocalTarget].(*target.Local); !ok {
		t.Errorf("local = %T, want *target.Local", got[config.LocalTarget])
	}
	ssh, ok := got["vps"].(*target.SSH)
	if !ok {
		t.Fatalf("vps = %T, want *target.SSH", got["vps"])
	}
	if ssh.Name() != "vps" {
		t.Errorf("vps Name = %q", ssh.Name())
	}
	if len(got) != 2 {
		t.Errorf("got %d targets, want 2", len(got))
	}
}
