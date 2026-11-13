package daemon

import (
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func TestRealTargetIsLocalOrAnSSHTargetForARemoteTarget(t *testing.T) {
	d := &daemon{sshControlDir: t.TempDir()}
	if got, ok := d.realTarget(config.LocalTarget, config.Target{}).(*target.Local); !ok {
		t.Errorf("local = %T, want *target.Local", got)
	}
	ssh, ok := d.realTarget("vps", config.Target{Host: "vps-alias", Shell: "bash -lc"}).(*target.SSH)
	if !ok {
		t.Fatalf("vps is not a *target.SSH")
	}
	if ssh.Name() != "vps" {
		t.Errorf("vps Name = %q", ssh.Name())
	}
}
