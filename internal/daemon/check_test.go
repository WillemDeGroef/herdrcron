package daemon_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

func TestTargetsCheckReportsAndPublishes(t *testing.T) {
	p := setupUTC(t)
	write(t, p.ConfigDir+"/prompts/other.md", "x")
	write(t, p.TasksDir()+"/other.yaml", `id: other
target: local
cwd: ~/src/other
schedule: "@daily"
agent: {command: codex}
timeout: 1h
`)
	h := newFakeHerdr()
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon),
		Targets: map[string]target.Target{"local": h}})
	ch := subscribe(t, p)

	got := call[api.TargetCheck](t, c, api.MethodTargetsCheck, api.TargetCheckParams{Name: "local"})
	if !got.OK || got.Target != "local" || got.HerdrVersion != "0.9.1" || got.WantHerdrVersion != "0.9.1" {
		t.Errorf("check = %+v", got)
	}
	if got.CheckedAt != noon.Format(time.RFC3339) {
		t.Errorf("checked_at = %q, want %q", got.CheckedAt, noon.Format(time.RFC3339))
	}
	var names []string
	for _, it := range got.Items {
		names = append(names, it.Name)
		if it.Result != api.CheckOK {
			t.Errorf("%s = %s %q", it.Name, it.Result, it.Detail)
		}
	}
	wantKinds := []string{
		api.CheckKindReachable, api.CheckKindHerdrServer, api.CheckKindHerdrVersion,
		api.CheckKindAgentCommand, api.CheckKindAgentCommand,
		api.CheckKindCwd, api.CheckKindCwd,
	}
	var gotKinds []string
	for _, it := range got.Items {
		gotKinds = append(gotKinds, it.Kind)
	}
	if !slices.Equal(gotKinds, wantKinds) {
		t.Errorf("kinds = %q\nwant    %q", gotKinds, wantKinds)
	}
	// nightly (/tmp, claude) and other are on local; parked is on vps and
	// broken is invalid.
	want := []string{"reachable", "herdr server", "herdr version", "agent command claude",
		"agent command codex", "cwd /tmp", "cwd ~/src/other"}
	if !slices.Equal(names, want) {
		t.Errorf("items = %q\nwant    %q", names, want)
	}
	if it := got.Items[len(got.Items)-1]; !slices.Equal(it.Tasks, []string{"other"}) {
		t.Errorf("cwd ~/src/other tasks = %q", it.Tasks)
	}

	ev := next(t, ch, api.EventTargetChanged, 2*time.Second, func(e api.TargetChangedEvent) bool {
		return e.Target.Name == "local" && e.Target.LastCheck != nil
	})
	if ev.Target.LastCheck.CheckedAt != got.CheckedAt || len(ev.Target.LastCheck.Items) != len(got.Items) {
		t.Errorf("target.changed last_check = %+v", ev.Target.LastCheck)
	}
}

func TestTargetsCheckUnknownTarget(t *testing.T) {
	c := startWith(t, setupUTC(t), daemon.Options{Version: "test", Targets: fakeTargets()})
	for _, params := range []any{api.TargetCheckParams{Name: "mars"}, api.TargetCheckParams{}, nil} {
		var res api.TargetCheck
		err := c.Call(context.Background(), api.MethodTargetsCheck, params, &res)
		var rpcErr *api.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInvalidParams {
			t.Errorf("params %+v: err = %v, want invalid params", params, err)
		}
	}
}
