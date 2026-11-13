package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// Check item results, the same strings as api.CheckOK, api.CheckWarn,
// api.CheckFail and api.CheckSkip.
const (
	CheckOK   = "ok"
	CheckWarn = "warn" // worth a look; runs still work
	CheckFail = "fail" // runs on the target fail
	CheckSkip = "skip" // not checked: the target is unreachable
)

// Stable ids for CheckItem.Kind (same values as api.CheckKind*).
const (
	CheckKindReachable    = "reachable"
	CheckKindHerdrServer  = "herdr_server"
	CheckKindHerdrVersion = "herdr_version"
	CheckKindAgentCommand = "agent_command"
	CheckKindCwd          = "cwd"
)

// Check is the preflight report of one target (CheckTarget).
type Check struct {
	Target    string
	CheckedAt time.Time
	// OK is true when no item failed; warnings are allowed.
	OK bool
	// HerdrVersion is what `herdr --version` printed; "" if unreadable.
	HerdrVersion     string
	WantHerdrVersion string
	Items            []CheckItem
}

// CheckItem is one line of a Check. Name is "reachable", "herdr server",
// "herdr version", "agent command <cmd>" or "cwd <dir>". Kind is a stable
// id (CheckKind*).
type CheckItem struct {
	Kind   string
	Name   string
	Result string // CheckOK, CheckWarn, CheckFail or CheckSkip
	Detail string
	// Tasks are the ids of the tasks an agent command or cwd item is for.
	Tasks []string
}

// Exit codes of the check scripts for a missing command or cwd.
const (
	exitNoCommand = 3
	exitNoCwd     = 4
)

// checkCommandScript checks that $1 is on PATH.
const checkCommandScript = `command -v "$1" >/dev/null 2>&1 || exit 3`

// checkCwdScript checks that the directory $1 exists, with ~ expanded.
const checkCwdScript = `case $1 in "~") d=$HOME ;; "~/"*) d=$HOME/${1#"~/"} ;; *) d=$1 ;; esac
cd "$d" 2>/dev/null || exit 4`

// CheckTarget runs the preflight report of target name for tasks (the
// tasks on that target): whether it answers, whether its herdr server
// runs, its herdr version against config's herdr_version, and every
// distinct agent command and cwd of tasks. Unlike a run's preflight it
// does not start the herdr server and neither reads nor fills the
// preflight cache. The error is only for an unknown target.
func (r *Runner) CheckTarget(ctx context.Context, name string, tasks []config.Task) (Check, error) {
	ts := r.target(name)
	if ts == nil {
		return Check{}, fmt.Errorf("unknown target %q", name)
	}
	want := r.config().HerdrVersion
	if want == "" {
		want = config.DefaultHerdrVersion
	}
	c := Check{Target: name, CheckedAt: r.clock.Now(), WantHerdrVersion: want}
	cmds, cwds := groupTasks(tasks)

	_, err := ts.herdr.PaneList(ctx)
	// A check discovers an outage, but it must not stretch a backoff the
	// poll worker is already counting down.
	r.observe(ts, name, err, false)
	if errors.Is(err, target.ErrUnreachable) {
		c.Items = append(c.Items, CheckItem{Kind: CheckKindReachable, Name: "reachable", Result: CheckFail, Detail: err.Error()})
		for _, item := range []struct {
			kind, name string
		}{{CheckKindHerdrServer, "herdr server"}, {CheckKindHerdrVersion, "herdr version"}} {
			c.Items = append(c.Items, CheckItem{Kind: item.kind, Name: item.name, Result: CheckSkip})
		}
		for _, g := range cmds {
			c.Items = append(c.Items, CheckItem{Kind: CheckKindAgentCommand, Name: "agent command " + g.key, Result: CheckSkip, Tasks: g.tasks})
		}
		for _, g := range cwds {
			c.Items = append(c.Items, CheckItem{Kind: CheckKindCwd, Name: "cwd " + g.key, Result: CheckSkip, Tasks: g.tasks})
		}
		return c, nil
	}
	c.Items = append(c.Items, CheckItem{Kind: CheckKindReachable, Name: "reachable", Result: CheckOK})

	server := CheckItem{Kind: CheckKindHerdrServer, Name: "herdr server", Result: CheckOK}
	switch {
	case herdr.IsCode(err, herdr.CodeServerNotRunning):
		server.Result, server.Detail = CheckWarn, "not running; a run starts it"
	case err != nil:
		server.Result, server.Detail = CheckFail, err.Error()
	}
	c.Items = append(c.Items, server)

	version := CheckItem{Kind: CheckKindHerdrVersion, Name: "herdr version", Result: CheckOK}
	got, err := ts.herdr.Version(ctx)
	switch {
	case err != nil:
		version.Result, version.Detail = CheckWarn, "cannot read: "+err.Error()
	case got != want:
		version.Result, version.Detail = CheckWarn, fmt.Sprintf("got %s, want %s", got, want)
	}
	c.HerdrVersion = got
	c.Items = append(c.Items, version)

	for _, g := range cmds {
		c.Items = append(c.Items, r.checkScript(ctx, ts, "agent command ", checkCommandScript, g, exitNoCommand, "not found on PATH"))
	}
	for _, g := range cwds {
		c.Items = append(c.Items, r.checkScript(ctx, ts, "cwd ", checkCwdScript, g, exitNoCwd, "does not exist"))
	}

	c.OK = !slices.ContainsFunc(c.Items, func(it CheckItem) bool { return it.Result == CheckFail })
	return c, nil
}

// taskGroup is an agent command or cwd and the ids of its tasks.
type taskGroup struct {
	key   string
	tasks []string
}

// groupTasks returns the distinct agent commands and cwds of tasks, each
// with its task ids, in task id order.
func groupTasks(tasks []config.Task) (cmds, cwds []taskGroup) {
	tasks = slices.SortedFunc(slices.Values(tasks), func(a, b config.Task) int { return cmp.Compare(a.ID, b.ID) })
	add := func(gs []taskGroup, key, id string) []taskGroup {
		if i := slices.IndexFunc(gs, func(g taskGroup) bool { return g.key == key }); i >= 0 {
			gs[i].tasks = append(gs[i].tasks, id)
			return gs
		}
		return append(gs, taskGroup{key: key, tasks: []string{id}})
	}
	for _, t := range tasks {
		cmds = add(cmds, t.Agent.Command, t.ID)
		cwds = add(cwds, t.Cwd, t.ID)
	}
	return cmds, cwds
}

// checkScript runs script with g.key as $1 through the target's shell. An
// exit with code missing is a fail with detail missingDetail.
func (r *Runner) checkScript(ctx context.Context, ts *targetState, prefix, script string, g taskGroup, missing int, missingDetail string) CheckItem {
	kind := CheckKindAgentCommand
	if prefix == "cwd " {
		kind = CheckKindCwd
	}
	it := CheckItem{Kind: kind, Name: prefix + g.key, Result: CheckOK, Tasks: g.tasks}
	argv := append(slices.Clone(ts.shell), script, "herdrcron", g.key)
	ctx, cancel := context.WithTimeout(ctx, checkCallTimeout)
	defer cancel()
	if _, err := ts.t.Exec(ctx, argv, nil); err != nil {
		it.Result = CheckFail
		var exit *target.ExitError
		switch {
		case errors.As(err, &exit) && exit.Code == missing:
			it.Detail = missingDetail
		case errors.As(err, &exit) && strings.TrimSpace(string(exit.Stderr)) != "":
			it.Detail = lastLine(string(exit.Stderr))
		default:
			it.Detail = err.Error()
		}
	}
	return it
}

// checkCallTimeout bounds each check script call.
const checkCallTimeout = 30 * time.Second
