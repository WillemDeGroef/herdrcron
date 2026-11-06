package herdr_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// reply is a canned answer of fakeTarget.
type reply struct {
	stdout []byte
	err    error
}

// fakeTarget records Exec argv and answers with canned replies in order.
type fakeTarget struct {
	calls     [][]string
	replies   []reply
	deadlines []bool
}

func (f *fakeTarget) Name() string { return "fake" }

func (f *fakeTarget) Exec(ctx context.Context, argv []string, _ io.Reader) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	_, ok := ctx.Deadline()
	f.deadlines = append(f.deadlines, ok)
	if len(f.replies) == 0 {
		return nil, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r.stdout, r.err
}

func (f *fakeTarget) PutFile(context.Context, string, []byte) error { return nil }
func (f *fakeTarget) RemoveFile(context.Context, string) error      { return nil }

// spawnTarget is a fakeTarget that also implements target.Spawner.
type spawnTarget struct {
	fakeTarget
	spawned [][]string
	err     error
}

func (s *spawnTarget) Spawn(argv []string) error {
	s.spawned = append(s.spawned, slices.Clone(argv))
	return s.err
}

var (
	_ target.Target  = (*fakeTarget)(nil)
	_ target.Spawner = (*spawnTarget)(nil)
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ok(stdout []byte) reply { return reply{stdout: stdout} }

func exitErr(argv []string, code int, stderr []byte) reply {
	return reply{err: &target.ExitError{Argv: argv, Code: code, Stderr: stderr}}
}

func checkArgv(t *testing.T, f *fakeTarget, want ...string) {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("got %d Exec calls, want 1: %q", len(f.calls), f.calls)
	}
	if !slices.Equal(f.calls[0], want) {
		t.Errorf("argv = %q\nwant   %q", f.calls[0], want)
	}
	if !f.deadlines[0] {
		t.Error("Exec ctx has no deadline")
	}
}

func TestPaneList(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "pane_list.json"))}}
	panes, err := herdr.New(f, "").PaneList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "pane", "list")
	if len(panes) == 0 || panes[0].PaneID != "w2:p68" || panes[0].AgentStatus != herdr.AgentStatusIdle {
		t.Errorf("panes = %+v", panes)
	}
}

func TestWorkspaceList(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "workspace_list.json"))}}
	ws, err := herdr.New(f, "/opt/herdr").WorkspaceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "/opt/herdr", "workspace", "list")
	if len(ws) == 0 || ws[0].WorkspaceID != "w2" || ws[0].Label != "demo" {
		t.Errorf("workspaces = %+v", ws)
	}
}

func TestWorkspaceCreate(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "workspace_create.json"))}}
	res, err := herdr.New(f, "").WorkspaceCreate(context.Background(), "/src/demo", "hc-demo", []string{"B=2", "A=1"})
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "workspace", "create", "--cwd", "/src/demo", "--label", "hc-demo", "--no-focus",
		"--env", "B=2", "--env", "A=1")
	if res.Workspace.WorkspaceID != "w15" || res.Tab.TabID != "w15:t1" || res.RootPane.PaneID != "w15:p1" {
		t.Errorf("result = %+v", res)
	}
}

func TestWorkspaceCreateNoEnv(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "workspace_create.json"))}}
	if _, err := herdr.New(f, "").WorkspaceCreate(context.Background(), "/src/demo", "hc-demo", nil); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "workspace", "create", "--cwd", "/src/demo", "--label", "hc-demo", "--no-focus")
}

func TestTabCreate(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "tab_create.json"))}}
	res, err := herdr.New(f, "").TabCreate(context.Background(), "w15", "/src/demo", "hc-spike-0930", []string{"X=a b", "Y="})
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "tab", "create", "--workspace", "w15", "--cwd", "/src/demo", "--label", "hc-spike-0930", "--no-focus",
		"--env", "X=a b", "--env", "Y=")
	if res.Tab.TabID != "w15:t2" || res.RootPane.PaneID != "w15:p2" {
		t.Errorf("result = %+v", res)
	}
}

func TestTabRename(t *testing.T) {
	out := []byte(`{"id":"cli:tab:rename","result":{"type":"tab_info","tab":{"agent_status":"unknown","focused":false,"label":"hc-x-0930","number":1,"pane_count":1,"tab_id":"w15:t1","workspace_id":"w15"}}}`)
	f := &fakeTarget{replies: []reply{ok(out)}}
	if err := herdr.New(f, "").TabRename(context.Background(), "w15:t1", "hc-x-0930"); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "tab", "rename", "w15:t1", "hc-x-0930")
}

func TestTabClose(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "tab_close.json"))}}
	if err := herdr.New(f, "").TabClose(context.Background(), "w15:t2"); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "tab", "close", "w15:t2")
}

func TestPaneRun(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(nil)}}
	cmd := `claude "$(cat '/tmp/p q')"`
	if err := herdr.New(f, "").PaneRun(context.Background(), "w15:p2", cmd); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "pane", "run", "w15:p2", cmd)
}

func TestPaneSendKeys(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(nil)}}
	if err := herdr.New(f, "").PaneSendKeys(context.Background(), "w15:p2", "ctrl+c", "ctrl+c"); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "pane", "send-keys", "w15:p2", "ctrl+c", "ctrl+c")
}

func TestPaneSendKeysPaneNotFound(t *testing.T) {
	argv := []string{"herdr", "pane", "send-keys", "w1:p9", "esc"}
	f := &fakeTarget{replies: []reply{exitErr(argv, 1, []byte(`{"error":{"code":"pane_not_found","message":"pane w1:p9 not found"},"id":"cli"}`))}}
	err := herdr.New(f, "").PaneSendKeys(context.Background(), "w1:p9", "esc")
	if !herdr.IsCode(err, herdr.CodePaneNotFound) {
		t.Errorf("err = %v, want pane_not_found", err)
	}
}

func TestPaneRead(t *testing.T) {
	text := "line 1\n  line 2 {\"not\": json}\n$ "
	f := &fakeTarget{replies: []reply{ok([]byte(text))}}
	got, err := herdr.New(f, "").PaneRead(context.Background(), "w15:p2", 200)
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "pane", "read", "w15:p2", "--source", "recent-unwrapped", "--lines", "200")
	if got != text {
		t.Errorf("text = %q, want %q", got, text)
	}
}

func TestPaneReadANSI(t *testing.T) {
	text := "\x1b[32mgreen\x1b[0m\n$ "
	f := &fakeTarget{replies: []reply{ok([]byte(text))}}
	got, err := herdr.New(f, "/opt/herdr").PaneReadANSI(context.Background(), "w15:p2", 50)
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "/opt/herdr", "pane", "read", "w15:p2", "--source", "recent", "--lines", "50", "--format", "ansi")
	if got != text {
		t.Errorf("text = %q, want %q", got, text)
	}
}

func TestPaneProcessInfo(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok(readFixture(t, "pane_process_info.json"))}}
	pi, err := herdr.New(f, "").PaneProcessInfo(context.Background(), "w16:p1")
	if err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "herdr", "pane", "process-info", "--pane", "w16:p1")
	if pi.PaneID != "w16:p1" || pi.ShellPID == nil || *pi.ShellPID != 2072829 ||
		pi.ForegroundProcessGroupID == nil || *pi.ForegroundProcessGroupID != 2073275 {
		t.Errorf("process info = %+v", pi)
	}
}

func TestErrorMapping(t *testing.T) {
	argv := []string{"herdr", "pane", "read", "w15:p99"}
	f := &fakeTarget{replies: []reply{exitErr(argv, 1, readFixture(t, "error_pane_not_found.json"))}}
	_, err := herdr.New(f, "").PaneRead(context.Background(), "w15:p99", 10)
	if err == nil {
		t.Fatal("no error")
	}
	if !herdr.IsCode(err, herdr.CodePaneNotFound) {
		t.Errorf("IsCode(%v, pane_not_found) = false", err)
	}
	if herdr.IsCode(err, herdr.CodeServerNotRunning) {
		t.Errorf("IsCode(%v, server_not_running) = true", err)
	}
	var he *herdr.Error
	if !errors.As(err, &he) || he.Code != "pane_not_found" || he.Message != "pane w15:p99 not found" {
		t.Errorf("errors.As = %+v", he)
	}
	if !strings.Contains(err.Error(), "herdr pane read") || !strings.Contains(err.Error(), "pane w15:p99 not found") {
		t.Errorf("error text = %q", err)
	}
}

func TestServerNotRunning(t *testing.T) {
	stderr := []byte(`{"error":{"code":"server_not_running","message":"herdr server is not running"},"id":"cli:pane:list"}`)
	f := &fakeTarget{replies: []reply{exitErr([]string{"herdr", "pane", "list"}, 1, stderr)}}
	_, err := herdr.New(f, "").PaneList(context.Background())
	if !herdr.IsCode(err, herdr.CodeServerNotRunning) {
		t.Errorf("IsCode(%v, server_not_running) = false", err)
	}
}

func TestNonJSONStderr(t *testing.T) {
	f := &fakeTarget{replies: []reply{exitErr([]string{"herdr", "tab", "close"}, 2, []byte("error: unexpected argument\n"))}}
	err := herdr.New(f, "").TabClose(context.Background(), "w1:t1")
	if err == nil {
		t.Fatal("no error")
	}
	var he *herdr.Error
	if errors.As(err, &he) {
		t.Errorf("got *herdr.Error %+v for non-JSON stderr", he)
	}
	var ee *target.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("error %v does not wrap *target.ExitError", err)
	}
	if !strings.Contains(err.Error(), "herdr tab close") {
		t.Errorf("error text = %q", err)
	}
}

func TestOtherExecError(t *testing.T) {
	boom := errors.New("ssh: connection refused")
	f := &fakeTarget{replies: []reply{{err: boom}}}
	_, err := herdr.New(f, "").WorkspaceList(context.Background())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "herdr workspace list") {
		t.Errorf("err = %v", err)
	}
}

// TestErrorsHideArgs checks that argv beyond the subcommand (env values,
// command text) never shows in error text.
func TestErrorsHideArgs(t *testing.T) {
	secret := "TOKEN=s3cr3t"
	secretArgv := []string{"herdr", "tab", "create", "--workspace", "w1", "--cwd", "/src", "--label", "hc-x", "--no-focus", "--env", secret}
	for _, r := range []reply{
		{err: errors.New("boom")},
		exitErr(secretArgv, 1, []byte("junk")),
		exitErr(secretArgv, 1, readFixture(t, "error_pane_not_found.json")),
		ok([]byte("not json")),
	} {
		f := &fakeTarget{replies: []reply{r}}
		_, err := herdr.New(f, "").TabCreate(context.Background(), "w1", "/src", "hc-x", []string{secret})
		if err == nil {
			t.Fatal("no error")
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "/src") {
			t.Errorf("error leaks argv: %q", err)
		}
	}
	f := &fakeTarget{replies: []reply{{err: errors.New("boom")}}}
	err := herdr.New(f, "").PaneRun(context.Background(), "w1:p1", "echo s3cr3t")
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("PaneRun error = %v", err)
	}
}

func TestWrongResultType(t *testing.T) {
	cases := []struct {
		name string
		call func(c *herdr.Client) error
		out  string
	}{
		{"pane list", func(c *herdr.Client) error { _, err := c.PaneList(context.Background()); return err }, "workspace_list.json"},
		{"workspace list", func(c *herdr.Client) error { _, err := c.WorkspaceList(context.Background()); return err }, "pane_list.json"},
		{"workspace create", func(c *herdr.Client) error {
			_, err := c.WorkspaceCreate(context.Background(), "/", "l", nil)
			return err
		}, "tab_create.json"},
		{"tab create", func(c *herdr.Client) error {
			_, err := c.TabCreate(context.Background(), "w", "/", "l", nil)
			return err
		}, "workspace_create.json"},
		{"tab rename", func(c *herdr.Client) error { return c.TabRename(context.Background(), "t", "l") }, "tab_close.json"},
		{"tab close", func(c *herdr.Client) error { return c.TabClose(context.Background(), "t") }, "pane_list.json"},
		{"pane process-info", func(c *herdr.Client) error {
			_, err := c.PaneProcessInfo(context.Background(), "p")
			return err
		}, "tab_close.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeTarget{replies: []reply{ok(readFixture(t, c.out))}}
			err := c.call(herdr.New(f, ""))
			if err == nil {
				t.Fatal("no error for wrong result type")
			}
			if !strings.Contains(err.Error(), "herdr "+c.name) || !strings.Contains(err.Error(), "result type") {
				t.Errorf("error text = %q", err)
			}
		})
	}
}

func TestInvalidJSON(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok([]byte("hello"))}}
	if _, err := herdr.New(f, "").PaneList(context.Background()); err == nil {
		t.Fatal("no error for non-JSON stdout")
	}
}

func TestTimeout(t *testing.T) {
	// The deadline must be at most 30 s away.
	var got time.Duration
	tg := &deadlineTarget{fn: func(ctx context.Context) {
		d, _ := ctx.Deadline()
		got = time.Until(d)
	}}
	_ = herdr.New(tg, "").TabClose(context.Background(), "t")
	if got <= 0 || got > 30*time.Second {
		t.Errorf("deadline in %v, want (0, 30s]", got)
	}
}

type deadlineTarget struct {
	fakeTarget
	fn func(ctx context.Context)
}

func (d *deadlineTarget) Exec(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	d.fn(ctx)
	return d.fakeTarget.Exec(ctx, argv, stdin)
}

func TestStartServerSpawn(t *testing.T) {
	s := &spawnTarget{}
	if err := herdr.New(s, "/opt/my herdr").StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"/opt/my herdr", "server"}}; !slices.EqualFunc(s.spawned, want, slices.Equal) {
		t.Errorf("spawned = %q, want %q", s.spawned, want)
	}
	if len(s.calls) != 0 {
		t.Errorf("Exec calls = %q, want none", s.calls)
	}

	boom := errors.New("fork failed")
	s = &spawnTarget{err: boom}
	if err := herdr.New(s, "").StartServer(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

func TestStartServerShell(t *testing.T) {
	f := &fakeTarget{}
	if err := herdr.New(f, "").StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "sh", "-c", "setsid herdr server </dev/null >/dev/null 2>&1 &")

	f = &fakeTarget{}
	if err := herdr.New(f, "/opt/it's/herdr").StartServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkArgv(t, f, "sh", "-c", `setsid '/opt/it'\''s/herdr' server </dev/null >/dev/null 2>&1 &`)

	f = &fakeTarget{replies: []reply{exitErr(nil, 1, []byte("sh: setsid: not found"))}}
	if err := herdr.New(f, "").StartServer(context.Background()); err == nil {
		t.Error("no error from failing starter")
	}
}

func TestVersion(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok([]byte("herdr 0.9.1\n"))}}
	v, err := herdr.New(f, "/opt/herdr").Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.9.1" {
		t.Errorf("Version = %q, want 0.9.1", v)
	}
	checkArgv(t, f, "/opt/herdr", "--version")
}

func TestVersionAfterLoginShellNoise(t *testing.T) {
	f := &fakeTarget{replies: []reply{ok([]byte("Welcome to vps\n\nherdr 0.9.0\n"))}}
	v, err := herdr.New(f, "").Version(context.Background())
	if err != nil || v != "0.9.0" {
		t.Errorf("Version = %q, %v; want 0.9.0", v, err)
	}
}

func TestVersionUnreadable(t *testing.T) {
	for _, r := range []reply{ok([]byte("\n")), ok([]byte("something else\n")), {err: errors.New("boom")}} {
		f := &fakeTarget{replies: []reply{r}}
		if v, err := herdr.New(f, "").Version(context.Background()); err == nil {
			t.Errorf("reply %+v: Version = %q, want an error", r, v)
		}
	}
}
