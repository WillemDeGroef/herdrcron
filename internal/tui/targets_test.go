package tui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// targetsFixture is fixture with preflight reports in the status: local
// passed, vps runs an older herdr, buildbox is down and never checked.
func targetsFixture() *fakeBackend {
	f := fixture()
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 3*24*3600 + 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: api.ReachReachable, LastSeenAt: ts(-time.Minute), LastCheck: localCheck()},
			{Name: "vps", Reachability: api.ReachReachable, LastSeenAt: ts(-time.Minute), LastCheck: vpsCheck()},
			{Name: "buildbox", Reachability: api.ReachUnreachable, UnreachableSince: ts(-10 * time.Minute),
				NextRetryAt: ts(time.Minute), LastError: "ssh: connect to host buildbox port 22: Connection refused",
				LastSeenAt: ts(-11 * time.Minute)},
		},
		GlobalPause: api.GlobalPause{PausedTargets: []string{}},
	})
	return f
}

func localCheck() *api.TargetCheck {
	return &api.TargetCheck{Target: "local", CheckedAt: ts(-5 * time.Minute), OK: true,
		HerdrVersion: "0.9.1", WantHerdrVersion: "0.9.1",
		Items: []api.TargetCheckItem{
			{Kind: api.CheckKindReachable, Name: "reachable", Result: api.CheckOK},
			{Kind: api.CheckKindHerdrServer, Name: "herdr server", Result: api.CheckOK},
			{Kind: api.CheckKindHerdrVersion, Name: "herdr version", Result: api.CheckOK, Detail: "0.9.1"},
		}}
}

func vpsCheck() *api.TargetCheck {
	return &api.TargetCheck{Target: "vps", CheckedAt: ts(-time.Hour), OK: false,
		HerdrVersion: "0.8.0", WantHerdrVersion: "0.9.1",
		Items: []api.TargetCheckItem{
			{Kind: api.CheckKindReachable, Name: "reachable", Result: api.CheckOK},
			{Kind: api.CheckKindHerdrServer, Name: "herdr server", Result: api.CheckOK},
			{Kind: api.CheckKindHerdrVersion, Name: "herdr version", Result: api.CheckWarn, Detail: "0.8.0, want 0.9.1"},
			{Kind: api.CheckKindAgentCommand, Name: "agent command claude", Result: api.CheckOK, Tasks: []string{"factory-coordinator"}},
			{Kind: api.CheckKindCwd, Name: "cwd /srv/factory", Result: api.CheckFail, Detail: "no such directory",
				Tasks: []string{"factory-coordinator", "nightly-e2e"}},
		}}
}

// openTargets starts the TUI with room for the targets screen and presses t.
func openTargets(t *testing.T, f *fakeBackend, m Model) *teatest.TestModel {
	t.Helper()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 24))
	waitText(t, tm, "factory-coordinator")
	tm.Send(key("t"))
	waitText(t, tm, "LAST CHECK")
	return tm
}

func TestTargetsGolden(t *testing.T) {
	f := targetsFixture()
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("down"))
	waitText(t, tm, "cwd /srv/factory")
	golden.RequireEqual(t, []byte(finalView(t, tm)))
}

func TestTargetsShowReachabilityVersionAndLastCheck(t *testing.T) {
	f := targetsFixture()
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("down"))
	tm.Send(key("down"))
	waitText(t, tm, "unreachable since")
	view := finalView(t, tm)
	for _, want := range [][]string{
		{"local", "● reachable", "0.9.1", "✓ ok", "5m ago"},
		{"vps", "● reachable", "0.8.0 (want 0.9.1)", "✗ failed", "1h ago"},
		{"buildbox", "○ unreachable", "10m", "never"},
	} {
		if !lineWith(view, want...) {
			t.Errorf("no line with %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "buildbox · unreachable since Nov 5 09:50 · retry Nov 5 10:01") || !strings.Contains(view, "Connection refused") {
		t.Errorf("selected unreachable target does not show its error:\n%s", view)
	}
}

func TestTargetsHerdrVersionMismatchIsAWarning(t *testing.T) {
	f := targetsFixture()
	tm := openTargets(t, f, newTestModel(f, coloured()))
	view := finalView(t, tm)
	const warning = "33"
	for _, l := range strings.Split(view, "\n") {
		switch {
		case strings.Contains(l, "0.8.0 (want 0.9.1)"):
			if !hasSGR(l, warning) {
				t.Errorf("version mismatch is not a warning: %q", l)
			}
		case strings.Contains(l, "local") && strings.Contains(l, "0.9.1"):
			if hasSGR(l, warning) {
				t.Errorf("matching version is a warning: %q", l)
			}
		}
	}
}

func TestTargetsCheckRunsTargetsCheckOnTheSelectedTarget(t *testing.T) {
	f := targetsFixture()
	f.set(api.MethodTargetsCheck, api.TargetCheck{Target: "vps", CheckedAt: ts(0), OK: true,
		HerdrVersion: "0.9.1", WantHerdrVersion: "0.9.1",
		Items: []api.TargetCheckItem{
			{Kind: api.CheckKindReachable, Name: "reachable", Result: api.CheckOK},
			{Kind: api.CheckKindHerdrVersion, Name: "herdr version", Result: api.CheckOK},
		}})
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("down"))
	tm.Send(key("c"))
	got := waitCall(t, f, api.MethodTargetsCheck, 1)
	if want := (api.TargetCheckParams{Name: "vps"}); got[0] != want {
		t.Fatalf("targets.check params = %+v, want %+v", got[0], want)
	}
	waitText(t, tm, "vps: check ok")
	view := finalView(t, tm)
	if !lineWith(view, "vps", "0.9.1", "✓ ok", "just now") {
		t.Errorf("check result not shown:\n%s", view)
	}
	if strings.Contains(view, "0.8.0") {
		t.Errorf("old check still shown:\n%s", view)
	}
}

func TestTargetsCheckErrorShowsInTheMessageLine(t *testing.T) {
	f := targetsFixture()
	f.fail(api.MethodTargetsCheck, errors.New("unknown target"))
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("c"))
	waitText(t, tm, "unknown target")
	view := finalView(t, tm)
	if !lineWith(view, "local", "✓ ok", "5m ago") {
		t.Errorf("failed call replaced the last check:\n%s", view)
	}
}

func TestTargetsScreenFollowsTargetChanged(t *testing.T) {
	f := targetsFixture()
	tm := openTargets(t, f, newTestModel(f, plain()))
	f.push(t, api.EventTargetChanged, api.TargetChangedEvent{Target: api.TargetStatus{
		Name: "buildbox", Reachability: api.ReachReachable, LastSeenAt: ts(0),
		LastCheck: &api.TargetCheck{Target: "buildbox", CheckedAt: ts(0), OK: true, HerdrVersion: "0.9.1", WantHerdrVersion: "0.9.1"}}})
	waitText(t, tm, "buildbox  ● reachable")
	view := finalView(t, tm)
	if !lineWith(view, "buildbox", "● reachable", "0.9.1", "✓ ok") {
		t.Errorf("target.changed not applied:\n%s", view)
	}
}

func TestHeaderShowsTargetGoingUnreachable(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	f.push(t, api.EventTargetChanged, api.TargetChangedEvent{Target: api.TargetStatus{
		Name: "vps", Reachability: api.ReachUnreachable, UnreachableSince: ts(0)}})
	waitText(t, tm, "vps ○ unreachable")

	// The 30s refresh returns a snapshot that still says vps is reachable
	// and has no observation time. Uptime changes so the test can see that
	// the snapshot was applied. The header keeps the later target.changed.
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 5 * 24 * 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: api.ReachReachable},
			{Name: "vps", Reachability: api.ReachReachable},
			{Name: "buildbox", Reachability: api.ReachUnreachable},
		},
		GlobalPause: api.GlobalPause{PausedTargets: []string{}},
	})
	tm.Send(refreshMsg{})
	waitText(t, tm, "daemon up 5d")
	view := finalView(t, tm)
	if !strings.Contains(view, "local ●  vps ○ unreachable  buildbox ○ unreachable") {
		t.Errorf("stale status read restored the old reachability:\n%s", view)
	}
}

func TestInFlightStatusKeepsTargetChanged(t *testing.T) {
	f := fixture()
	m := newTestModel(f, plain())
	msg := m.load()()
	loaded, ok := msg.(loadedMsg)
	if !ok {
		t.Fatalf("load = %T", msg)
	}
	m.applyLoad(loaded)
	// The read starts while buildbox is unreachable and returns that
	// snapshot. target.changed lands before the snapshot is applied, and
	// the event carries no observation time.
	read := m.loadStatus()
	data, err := json.Marshal(api.TargetChangedEvent{Target: api.TargetStatus{
		Name: "buildbox", Reachability: api.ReachReachable}})
	if err != nil {
		t.Fatal(err)
	}
	m.applyEvent(api.Notification{Method: api.EventTargetChanged, Params: data})
	updated, _ := m.Update(read())
	view := updated.View()
	if !strings.Contains(view, "buildbox ●") {
		t.Errorf("in-flight status read undid target.changed:\n%s", view)
	}
}

func TestHeaderTakesANewerStatusReachability(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	f.push(t, api.EventTargetChanged, api.TargetChangedEvent{Target: api.TargetStatus{
		Name: "vps", Reachability: api.ReachUnreachable, UnreachableSince: ts(-time.Minute)}})
	waitText(t, tm, "vps ○ unreachable")
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 5 * 24 * 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: api.ReachReachable},
			{Name: "vps", Reachability: api.ReachReachable, LastSeenAt: ts(0)},
			{Name: "buildbox", Reachability: api.ReachUnreachable},
		},
		GlobalPause: api.GlobalPause{PausedTargets: []string{}},
	})
	tm.Send(refreshMsg{})
	waitText(t, tm, "daemon up 5d")
	view := finalView(t, tm)
	if strings.Contains(view, "vps ○ unreachable") || !strings.Contains(view, "vps ●") {
		t.Errorf("newer status reachability did not land:\n%s", view)
	}
}

func TestTargetsBackKeysReturnToTheList(t *testing.T) {
	for _, k := range []string{"esc", "q", "t"} {
		t.Run(k, func(t *testing.T) {
			f := targetsFixture()
			tm := openTargets(t, f, newTestModel(f, plain()))
			tm.Send(key(k))
			waitText(t, tm, "space on/off")
			view := finalView(t, tm)
			if strings.Contains(view, "LAST CHECK") {
				t.Errorf("still on the targets screen:\n%s", view)
			}
			if strings.Contains(view, "targets: reachability") {
				t.Errorf("help is open; %s should leave the targets screen:\n%s", k, view)
			}
		})
	}
}

func TestTargetsScrollsCheckItemsOnAShortTerminal(t *testing.T) {
	f := targetsFixture()
	tm := teatest.NewTestModel(t, newTestModel(f, plain()), teatest.WithInitialTermSize(80, 10))
	waitText(t, tm, "factory-coordinator")
	tm.Send(key("t"))
	waitText(t, tm, "LAST CHECK")
	tm.Send(key("down")) // vps, whose check items sit below the fold
	tm.Send(tea.KeyMsg{Type: tea.KeyPgDown})
	waitText(t, tm, "cwd /srv/factory")
	view := finalView(t, tm)
	if strings.Contains(view, "LAST CHECK") {
		t.Errorf("pgdown did not scroll:\n%s", view)
	}
	if !strings.Contains(view, "pgup/pgdn scroll") || !strings.Contains(view, "esc back") {
		t.Errorf("footer scrolled away:\n%s", view)
	}
	if n := strings.Count(view, "\n") + 1; n > 10 {
		t.Errorf("targets screen is %d lines, taller than the terminal:\n%s", n, view)
	}
}

func TestStatusRefreshKeepsANewerCheck(t *testing.T) {
	f := targetsFixture()
	f.set(api.MethodTargetsCheck, api.TargetCheck{Target: "vps", CheckedAt: ts(0), OK: true,
		HerdrVersion: "0.9.1", WantHerdrVersion: "0.9.1",
		Items: []api.TargetCheckItem{
			{Kind: api.CheckKindHerdrVersion, Name: "herdr version", Result: api.CheckOK},
		}})
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("down"))
	tm.Send(key("c"))
	waitText(t, tm, "vps: check ok")
	// The snapshot still carries the pre-check report. "spare" is only in
	// this snapshot, so seeing it means the snapshot landed.
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 3*24*3600 + 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: api.ReachReachable, LastSeenAt: ts(-time.Minute), LastCheck: localCheck()},
			{Name: "vps", Reachability: api.ReachReachable, LastSeenAt: ts(-time.Minute), LastCheck: vpsCheck()},
			{Name: "buildbox", Reachability: api.ReachUnreachable, UnreachableSince: ts(-10 * time.Minute)},
			{Name: "spare", Reachability: api.ReachReachable},
		},
		GlobalPause: api.GlobalPause{PausedTargets: []string{}},
	})
	tm.Send(refreshMsg{})
	waitText(t, tm, "spare")
	view := finalView(t, tm)
	if strings.Contains(view, "0.8.0") || !lineWith(view, "vps", "0.9.1", "✓ ok") {
		t.Errorf("stale status read put the old check back:\n%s", view)
	}
}

func TestStatusRefreshAppliesANewerCheck(t *testing.T) {
	f := targetsFixture()
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("down"))
	waitText(t, tm, "last check Nov 5 09:00")
	newer := vpsCheck()
	newer.CheckedAt = ts(0)
	newer.OK = true
	newer.HerdrVersion = "0.9.1"
	newer.Items = []api.TargetCheckItem{
		{Kind: api.CheckKindHerdrVersion, Name: "herdr version", Result: api.CheckOK, Detail: "0.9.1"},
	}
	f.set(api.MethodStatus, api.StatusResult{
		Version: "dev", UptimeSeconds: 3*24*3600 + 3600,
		Targets: []api.TargetStatus{
			{Name: "local", Reachability: api.ReachReachable, LastSeenAt: ts(-time.Minute), LastCheck: localCheck()},
			{Name: "vps", Reachability: api.ReachReachable, LastSeenAt: ts(-time.Minute), LastCheck: newer},
			{Name: "buildbox", Reachability: api.ReachUnreachable, UnreachableSince: ts(-10 * time.Minute)},
		},
		GlobalPause: api.GlobalPause{PausedTargets: []string{}},
	})
	tm.Send(refreshMsg{})
	waitText(t, tm, "last check Nov 5 10:00")
	view := finalView(t, tm)
	if strings.Contains(view, "0.8.0") || !lineWith(view, "vps", "0.9.1", "✓ ok", "just now") {
		t.Errorf("newer status check did not land:\n%s", view)
	}
}

func TestTargetsVersionWarningFollowsTheItemKind(t *testing.T) {
	f := targetsFixture()
	f.set(api.MethodTargetsCheck, api.TargetCheck{Target: "local", CheckedAt: ts(0), OK: true,
		WantHerdrVersion: "0.9.1",
		Items: []api.TargetCheckItem{
			{Kind: api.CheckKindHerdrVersion, Name: "version of herdr", Result: api.CheckWarn, Detail: "cannot read: exit 127"},
		}})
	tm := openTargets(t, f, newTestModel(f, plain()))
	tm.Send(key("c"))
	waitText(t, tm, "local: check ok with warnings")
	view := finalView(t, tm)
	if !lineWith(view, "local", "! unreadable", "! warnings", "just now") {
		t.Errorf("unreadable version is not a warning:\n%s", view)
	}
}

func TestTargetsLateCheckReplyDoesNotHideANewerCheck(t *testing.T) {
	f := targetsFixture()
	f.set(api.MethodTargetsCheck, api.TargetCheck{Target: "vps", CheckedAt: ts(-30 * time.Minute), OK: false,
		HerdrVersion: "0.8.0", WantHerdrVersion: "0.9.1"})
	m := newTestModel(f, plain())
	tm := openTargets(t, f, m)
	f.push(t, api.EventTargetChanged, api.TargetChangedEvent{Target: api.TargetStatus{
		Name: "vps", Reachability: api.ReachReachable,
		LastCheck: &api.TargetCheck{Target: "vps", CheckedAt: ts(0), OK: true, HerdrVersion: "0.9.1", WantHerdrVersion: "0.9.1"}}})
	waitText(t, tm, "✓ ok just now")
	tm.Send(key("down"))
	tm.Send(key("c"))
	waitText(t, tm, "vps: check failed")
	view := finalView(t, tm)
	if !lineWith(view, "vps", "0.9.1", "✓ ok just now") {
		t.Errorf("late reply replaced the newer check:\n%s", view)
	}
}

func TestTargetsCheckResultStaysOffOtherScreens(t *testing.T) {
	f := targetsFixture()
	f.fail(api.MethodTargetsCheck, errors.New("boom from check"))
	m := newTestModel(f, plain())
	m.status = api.StatusResult{Targets: []api.TargetStatus{{Name: "vps"}}}
	m.screen = screenMain
	cmd := m.checkTarget("vps")
	m.setMessage("", nil) // the user left the targets screen
	m2, _ := m.Update(cmd())
	if got := m2.(Model).message; got != "" {
		t.Errorf("message on the main screen = %q, want none", got)
	}
}
