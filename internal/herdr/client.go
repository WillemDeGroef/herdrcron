package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/shellquote"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// callTimeout bounds every herdr call.
const callTimeout = 30 * time.Second

// Error codes herdr puts in its JSON error envelope (docs/herdr-findings.md).
const (
	CodeServerNotRunning  = "server_not_running"
	CodePaneNotFound      = "pane_not_found"
	CodeTabNotFound       = "tab_not_found"
	CodeWorkspaceNotFound = "workspace_not_found"
)

// Error is a herdr error from the JSON envelope on stderr.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is (wraps) a *Error with that code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client runs the herdr CLI on a target and decodes its output. Its
// methods follow docs/herdr-findings.md, one herdr command each.
type Client struct {
	t   target.Target
	bin string
}

// New returns a client that runs bin (empty: "herdr") on t.
func New(t target.Target, bin string) *Client {
	if bin == "" {
		bin = "herdr"
	}
	return &Client{t: t, bin: bin}
}

// PaneList runs `herdr pane list`.
func (c *Client) PaneList(ctx context.Context) ([]PaneInfo, error) {
	r, err := call[PaneListResult](ctx, c, "pane_list", "pane", "list")
	return r.Panes, err
}

// WorkspaceList runs `herdr workspace list`.
func (c *Client) WorkspaceList(ctx context.Context) ([]WorkspaceInfo, error) {
	r, err := call[WorkspaceListResult](ctx, c, "workspace_list", "workspace", "list")
	return r.Workspaces, err
}

// WorkspaceCreate runs `herdr workspace create` without taking focus. env
// elements are "K=V", passed as --env in the given order.
func (c *Client) WorkspaceCreate(ctx context.Context, cwd, label string, env []string) (WorkspaceCreatedResult, error) {
	args := append([]string{"--cwd", cwd, "--label", label, "--no-focus"}, envArgs(env)...)
	return call[WorkspaceCreatedResult](ctx, c, "workspace_created", "workspace", "create", args...)
}

// TabCreate runs `herdr tab create` in workspaceID without taking focus.
// env elements are "K=V", passed as --env in the given order.
func (c *Client) TabCreate(ctx context.Context, workspaceID, cwd, label string, env []string) (TabCreatedResult, error) {
	args := append([]string{"--workspace", workspaceID, "--cwd", cwd, "--label", label, "--no-focus"}, envArgs(env)...)
	return call[TabCreatedResult](ctx, c, "tab_created", "tab", "create", args...)
}

// TabRename runs `herdr tab rename`.
func (c *Client) TabRename(ctx context.Context, tabID, label string) error {
	_, err := call[TabInfoResult](ctx, c, "tab_info", "tab", "rename", tabID, label)
	return err
}

// TabClose runs `herdr tab close`.
func (c *Client) TabClose(ctx context.Context, tabID string) error {
	_, err := call[OkResult](ctx, c, "ok", "tab", "close", tabID)
	return err
}

// PaneRun types command into the pane's shell and presses Enter. command
// is one argv element; herdr prints nothing on success.
func (c *Client) PaneRun(ctx context.Context, paneID, command string) error {
	_, err := c.exec(ctx, "pane", "run", paneID, command)
	return err
}

// PaneRead returns the last lines of the pane's text, soft wraps joined.
func (c *Client) PaneRead(ctx context.Context, paneID string, lines int) (string, error) {
	out, err := c.exec(ctx, "pane", "read", paneID, "--source", "recent-unwrapped", "--lines", strconv.Itoa(lines))
	return string(out), err
}

// PaneProcessInfo runs `herdr pane process-info`.
func (c *Client) PaneProcessInfo(ctx context.Context, paneID string) (PaneProcessInfo, error) {
	r, err := call[PaneProcessInfoResult](ctx, c, "pane_process_info", "pane", "process-info", "--pane", paneID)
	return r.ProcessInfo, err
}

// StartServer starts a headless `herdr server` in its own session and does
// not wait for it. Its error is only the starter's; it says nothing about
// the server's health, so callers retry a call afterwards.
func (c *Client) StartServer(ctx context.Context) error {
	if s, ok := c.t.(target.Spawner); ok {
		if err := s.Spawn([]string{c.bin, "server"}); err != nil {
			return fmt.Errorf("herdr server: %w", err)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	script := "setsid " + shellquote.Quote(c.bin) + " server </dev/null >/dev/null 2>&1 &"
	if _, err := c.t.Exec(ctx, []string{"sh", "-c", script}, nil); err != nil {
		return fmt.Errorf("herdr server: %w", err)
	}
	return nil
}

func envArgs(env []string) []string {
	var out []string
	for _, kv := range env {
		out = append(out, "--env", kv)
	}
	return out
}

// resultType is the part of every result that names its variant.
type resultType struct {
	Result struct {
		Type string `json:"type"`
	} `json:"result"`
}

// call runs a JSON command and decodes its result, which must be of type
// want.
func call[T any](ctx context.Context, c *Client, want, group, cmd string, args ...string) (T, error) {
	var zero T
	out, err := c.exec(ctx, group, cmd, args...)
	if err != nil {
		return zero, err
	}
	name := "herdr " + group + " " + cmd
	var rt resultType
	if err := json.Unmarshal(out, &rt); err != nil {
		return zero, fmt.Errorf("%s: decode output: %w", name, err)
	}
	if rt.Result.Type != want {
		return zero, fmt.Errorf("%s: result type %q, want %q", name, rt.Result.Type, want)
	}
	var resp Response[T]
	if err := json.Unmarshal(out, &resp); err != nil {
		return zero, fmt.Errorf("%s: decode output: %w", name, err)
	}
	return resp.Result, nil
}

// exec runs `bin group cmd args...` with the call timeout. Errors name
// only the group and command: the other args can hold secrets.
func (c *Client) exec(ctx context.Context, group, cmd string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	argv := append([]string{c.bin, group, cmd}, args...)
	out, err := c.t.Exec(ctx, argv, nil)
	if err == nil {
		return out, nil
	}
	name := "herdr " + group + " " + cmd
	var ee *target.ExitError
	if errors.As(err, &ee) {
		var er ErrorResponse
		if json.Unmarshal(bytes.TrimSpace(ee.Stderr), &er) == nil && er.Error.Code != "" {
			return nil, fmt.Errorf("%s: %w", name, &Error{Code: er.Error.Code, Message: er.Error.Message})
		}
	}
	return nil, fmt.Errorf("%s: %w", name, err)
}
