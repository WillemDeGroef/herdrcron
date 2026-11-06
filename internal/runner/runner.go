// Package runner starts task runs in herdr tabs and tracks them to the end.
// A run goes through the spec's run steps: reserve the run row, preflight
// the target, find or create the hc- workspace, open an hc- tab, write the
// prompt file, launch the agent, track it by polling, and finish by storing
// the output tail. Every herdr call goes through internal/herdr on a
// target.Target, so tests drive the runner with a scripted fake herdr.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/shellquote"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

const (
	// preflightTTL is how long a passed preflight check stays valid.
	preflightTTL = 10 * time.Minute
	// tailLines is how many lines of pane output a run keeps.
	tailLines = 200
	// donePolls is how many polls in a row must show idle or done.
	donePolls = 2
	// defaultShell runs the preflight check on a target without a shell.
	defaultShell = "bash -lc"
	// defaultServerStartWait is how long a run waits for a herdr server it
	// started to answer.
	defaultServerStartWait = 5 * time.Second
)

// promptDir holds the prompt files on the target, one per run.
const promptDir = "~/.cache/herdrcron/runs/"

func promptPath(runID string) string { return promptDir + runID + ".md" }

// Options configures New.
type Options struct {
	Clock     clock.Clock
	Store     *store.Store
	Targets   map[string]target.Target // by target name
	Config    config.Config            // target settings, time zone, poll_interval
	ConfigDir string                   // prompt_file paths are relative to it
	Log       *slog.Logger
	// ServerStartWait is how long to wait for a herdr server the runner
	// started; zero means 5 s.
	ServerStartWait time.Duration
}

// Runner starts and tracks runs. Its methods are safe for concurrent use.
type Runner struct {
	clock     clock.Clock
	store     *store.Store
	cfg       config.Config
	configDir string
	log       *slog.Logger
	waitStart time.Duration
	targets   map[string]*targetState

	mu     sync.Mutex
	active map[string]*activeRun // by run id
}

// targetState is a target with its herdr client and preflight cache.
type targetState struct {
	t     target.Target
	herdr *herdr.Client
	shell []string

	mu          sync.Mutex
	serverUntil time.Time            // herdr server answered; valid until
	checks      map[string]checkedAt // agent command and cwd, by key

	// openMu serialises workspace lookup and creation, so two runs that
	// start at once for the same repo share one hc- workspace.
	openMu sync.Mutex
}

type checkedAt struct {
	cwd   string // cwd as resolved on the target
	until time.Time
}

// activeRun is the tracking state of a run that has not ended.
type activeRun struct {
	run  store.Run
	task config.Task
	log  *slog.Logger

	idlePolls     int  // polls in a row with idle or done
	sawAgent      bool // herdr detected an agent in the pane at some point
	promptSamples int  // process-info samples taken
}

// New returns a runner with no active runs.
func New(o Options) *Runner {
	r := &Runner{
		clock: o.Clock, store: o.Store, cfg: o.Config, configDir: o.ConfigDir, log: o.Log,
		waitStart: o.ServerStartWait,
		targets:   make(map[string]*targetState, len(o.Targets)),
		active:    map[string]*activeRun{},
	}
	if r.waitStart <= 0 {
		r.waitStart = defaultServerStartWait
	}
	for name, t := range o.Targets {
		tc := o.Config.Targets[name]
		shell := strings.Fields(tc.Shell)
		if len(shell) == 0 {
			shell = strings.Fields(defaultShell)
		}
		r.targets[name] = &targetState{
			t: t, herdr: herdr.New(t, tc.HerdrPath), shell: shell, checks: map[string]checkedAt{},
		}
	}
	return r
}

// Start runs steps 1 to 6 for task t: it reserves the run row, launches
// the agent in a new hc- tab, and returns the run as running. A run that
// cannot start is stored as failed with the reason, and returned without
// an error; the error is for a run that could not be stored at all.
func (r *Runner) Start(ctx context.Context, t config.Task, trigger string, scheduledAt time.Time) (store.Run, error) {
	now := r.clock.Now()
	run := store.Run{
		ID: store.NewRunID(now), TaskID: t.ID, Target: t.Target, Trigger: trigger,
		ScheduledAt: scheduledAt, StartedAt: now, Status: store.StatusStarting,
	}
	if err := r.store.InsertRun(ctx, run); err != nil {
		return store.Run{}, err
	}
	a := &activeRun{run: run, task: t, log: logging.ForRun(r.log, run.ID, t.ID, t.Target)}
	a.log.Info("run starting", "trigger", trigger, "scheduled_at", scheduledAt)

	if err := r.launch(ctx, a); err != nil {
		a.log.Warn("run failed to start", "err", err)
		return r.end(context.WithoutCancel(ctx), a, store.StatusFailed, err.Error())
	}
	r.mu.Lock()
	r.active[run.ID] = a
	r.mu.Unlock()
	return a.run, nil
}

// launch runs steps 2 to 6. On error, a.run holds whatever herdr ids were
// already created, so end can clean up.
func (r *Runner) launch(ctx context.Context, a *activeRun) error {
	t := a.task
	ts, ok := r.targets[t.Target]
	if !ok {
		return fmt.Errorf("unknown target %q", t.Target)
	}
	cwd, err := r.preflight(ctx, ts, t)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(config.PromptPath(t.PromptFile, r.configDir))
	if err != nil {
		return fmt.Errorf("read prompt file %s: %w", t.PromptFile, err)
	}

	label, err := r.tabLabel(t)
	if err != nil {
		return err
	}
	env := runEnv(a.run.ID, t.Env)
	if err := r.openTab(ctx, ts, a, cwd, label, env); err != nil {
		return err
	}
	if err := r.store.UpdateRun(ctx, a.run); err != nil {
		return err
	}

	promptPath := promptPath(a.run.ID)
	if err := ts.t.PutFile(ctx, promptPath, data); err != nil {
		return fmt.Errorf("write prompt file: %w", err)
	}
	a.log.Info("prompt file written", "prompt_file", t.PromptFile, "path", promptPath, "bytes", len(data))

	if err := ts.herdr.PaneRun(ctx, a.run.HerdrPane, launchCommand(t.Agent, a.run.ID)); err != nil {
		return fmt.Errorf("launch agent: %w", err)
	}
	// Until the running status is stored the run counts as starting, so a
	// failed write still closes the tab with the agent in it.
	running := a.run
	running.Status = store.StatusRunning
	if err := r.store.UpdateRun(ctx, running); err != nil {
		return err
	}
	a.run = running
	a.log.Info("run launched", "herdr_workspace", a.run.HerdrWorkspace, "herdr_tab", a.run.HerdrTab,
		"herdr_pane", a.run.HerdrPane, "agent", t.Agent.Command, "env_keys", envKeys(t.Env))
	return nil
}

// openTab runs steps 3 and 4: it finds the workspace labelled
// hc-<repo-basename> or creates it, and opens the run's tab there. A new
// workspace comes with a first tab, which the run uses. The workspace is
// looked up on every run: closing its last tab closes it.
func (r *Runner) openTab(ctx context.Context, ts *targetState, a *activeRun, cwd, label string, env []string) error {
	wsLabel := "hc-" + path.Base(cwd)
	ts.openMu.Lock()
	defer ts.openMu.Unlock()
	workspaces, err := ts.herdr.WorkspaceList(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces: %w", err)
	}
	i := slices.IndexFunc(workspaces, func(w herdr.WorkspaceInfo) bool { return w.Label == wsLabel })
	if i >= 0 {
		res, err := ts.herdr.TabCreate(ctx, workspaces[i].WorkspaceID, cwd, label, env)
		if err != nil {
			return fmt.Errorf("create tab: %w", err)
		}
		a.run.HerdrWorkspace, a.run.HerdrTab, a.run.HerdrPane = res.Tab.WorkspaceID, res.Tab.TabID, res.RootPane.PaneID
		return nil
	}
	res, err := ts.herdr.WorkspaceCreate(ctx, cwd, wsLabel, env)
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	a.run.HerdrWorkspace, a.run.HerdrTab, a.run.HerdrPane = res.Workspace.WorkspaceID, res.Tab.TabID, res.RootPane.PaneID
	if err := ts.herdr.TabRename(ctx, res.Tab.TabID, label); err != nil {
		return fmt.Errorf("rename tab: %w", err)
	}
	return nil
}

// tabLabel is hc-<task-id>-<HHMM>, in the task's time zone.
func (r *Runner) tabLabel(t config.Task) (string, error) {
	loc, err := r.cfg.Location(t)
	if err != nil {
		return "", err
	}
	return "hc-" + t.ID + "-" + r.clock.Now().In(loc).Format("1504"), nil
}

// runEnv is the agent's environment: HERDRCRON_RUN_ID, then the task's env
// in key order.
func runEnv(runID string, env map[string]string) []string {
	out := []string{"HERDRCRON_RUN_ID=" + runID}
	for _, k := range envKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}

func envKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// launchCommand is the line typed into the pane's shell: the agent and its
// arguments, quoted, and the prompt read from the run's prompt file as the
// last argument. The prompt never appears in the command line itself. As
// with any command substitution, trailing newlines of the prompt are
// dropped.
func launchCommand(agent config.Agent, runID string) string {
	// The run id is a ULID, so the path needs no quoting, and $HOME stays
	// outside quotes the shell would not expand.
	file := `"$HOME"/` + strings.TrimPrefix(promptDir, "~/") + runID + ".md"
	return shellquote.Join(append([]string{agent.Command}, agent.Args...)...) + ` "$(cat ` + file + `)"`
}

// preflightScript checks the agent command and the cwd on the target and
// prints the cwd as an absolute path. Arguments: $1 command, $2 cwd.
const preflightScript = `case $2 in "~") d=$HOME ;; "~/"*) d=$HOME/${2#"~/"} ;; *) d=$2 ;; esac
command -v "$1" >/dev/null 2>&1 || { echo "agent command $1 not found on PATH" >&2; exit 3; }
cd "$d" 2>/dev/null || { echo "cwd $2 does not exist" >&2; exit 4; }
pwd`

// preflight runs step 2 and returns the cwd as resolved on the target. A
// pass is cached for 10 min: the herdr server per target, the agent
// command and cwd per target, command and cwd. Failures are not cached.
func (r *Runner) preflight(ctx context.Context, ts *targetState, t config.Task) (string, error) {
	now := r.clock.Now()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if now.After(ts.serverUntil) {
		if err := r.checkServer(ctx, ts); err != nil {
			return "", err
		}
		ts.serverUntil = now.Add(preflightTTL)
	}
	key := t.Agent.Command + "\x00" + t.Cwd
	if c, ok := ts.checks[key]; ok && !now.After(c.until) {
		return c.cwd, nil
	}
	argv := append(slices.Clone(ts.shell), preflightScript, "herdrcron", t.Agent.Command, t.Cwd)
	out, err := ts.t.Exec(ctx, argv, nil)
	if err != nil {
		var exit *target.ExitError
		if errors.As(err, &exit) && len(strings.TrimSpace(string(exit.Stderr))) > 0 {
			return "", fmt.Errorf("preflight: %s", lastLine(string(exit.Stderr)))
		}
		return "", fmt.Errorf("preflight: %w", err)
	}
	// A login shell can print to stdout before the script runs; pwd is the
	// last line.
	cwd := lastLine(string(out))
	if !strings.HasPrefix(cwd, "/") {
		return "", fmt.Errorf("preflight: cannot resolve cwd %s", t.Cwd)
	}
	ts.checks[key] = checkedAt{cwd: cwd, until: now.Add(preflightTTL)}
	return cwd, nil
}

// checkServer checks that the herdr server answers. If it is not running,
// it starts it once and waits up to waitStart for it to answer.
func (r *Runner) checkServer(ctx context.Context, ts *targetState) error {
	_, err := ts.herdr.PaneList(ctx)
	if !herdr.IsCode(err, herdr.CodeServerNotRunning) {
		if err != nil {
			return fmt.Errorf("preflight: herdr server: %w", err)
		}
		return nil
	}
	r.log.Info("herdr server not running; starting it", logging.KeyTarget, ts.t.Name())
	if err := ts.herdr.StartServer(ctx); err != nil {
		return fmt.Errorf("preflight: start herdr server: %w", err)
	}
	// Wall time, not r.clock: this waits for a real process to come up.
	deadline := time.Now().Add(r.waitStart)
	step := min(250*time.Millisecond, r.waitStart/5)
	for {
		if _, err = ts.herdr.PaneList(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("preflight: herdr server does not answer after start: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(step):
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Active reports whether the run is being tracked.
func (r *Runner) Active(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[runID]
	return ok
}

// PollTarget runs step 7 once for every active run on the target: one
// `herdr pane list` covers them all. Panes without an agent get one
// `pane process-info` each, to see whether the command has ended.
func (r *Runner) PollTarget(ctx context.Context, name string) {
	r.mu.Lock()
	var runs []*activeRun
	for _, a := range r.active {
		if a.run.Target == name {
			runs = append(runs, a)
		}
	}
	r.mu.Unlock()
	ts := r.targets[name]
	if len(runs) == 0 || ts == nil {
		return
	}
	slices.SortFunc(runs, func(a, b *activeRun) int { return strings.Compare(a.run.ID, b.run.ID) })

	panes, err := ts.herdr.PaneList(ctx)
	if err != nil {
		// Unreachable targets and backoff are a later ticket; the runs stay
		// open and the next poll tries again.
		r.log.Warn("poll failed", logging.KeyTarget, name, "err", err, "active_runs", len(runs))
		return
	}
	// A run that ends must be stored even when the daemon stops mid-poll.
	ctx = context.WithoutCancel(ctx)
	byID := make(map[string]herdr.PaneInfo, len(panes))
	for _, p := range panes {
		byID[p.PaneID] = p
	}
	for _, a := range runs {
		p, ok := byID[a.run.HerdrPane]
		if !ok {
			r.finish(ctx, ts, a, store.StatusLost, "pane gone")
			continue
		}
		r.track(ctx, ts, a, p)
	}
}

// track maps one pane sample to the run's state.
func (r *Runner) track(ctx context.Context, ts *targetState, a *activeRun, p herdr.PaneInfo) {
	hasAgent := p.Agent != nil
	if hasAgent {
		a.sawAgent = true
	}
	status := a.run.Status
	switch {
	case hasAgent && (p.AgentStatus == herdr.AgentStatusIdle || p.AgentStatus == herdr.AgentStatusDone):
		a.idlePolls++
		if a.idlePolls >= donePolls {
			r.finish(ctx, ts, a, store.StatusDone, "")
			return
		}
	case p.AgentStatus == herdr.AgentStatusWorking:
		a.idlePolls, status = 0, store.StatusRunning
	case p.AgentStatus == herdr.AgentStatusBlocked:
		a.idlePolls, status = 0, store.StatusBlocked
	case !hasAgent:
		a.idlePolls = 0
		if r.shellAtPrompt(ctx, ts, a) {
			if a.sawAgent {
				r.finish(ctx, ts, a, store.StatusFailed, "agent exited")
			} else {
				r.finish(ctx, ts, a, store.StatusDone, "")
			}
			return
		}
	default:
		// Unknown status with an agent: running, as the spec says.
		a.idlePolls, status = 0, store.StatusRunning
	}
	if status == a.run.Status {
		return
	}
	a.log.Info("run status changed", "from", a.run.Status, "to", status, "agent_status", p.AgentStatus)
	a.run.Status = status
	if err := r.store.UpdateRun(ctx, a.run); err != nil {
		a.log.Error("cannot store run status", "err", err)
	}
}

// shellAtPrompt reports whether the pane's shell is back in the
// foreground, so the launched command has ended. The first sample after
// the launch does not count: the shell's startup files run in its own
// process group too. An agent that was seen and is gone counts at once.
func (r *Runner) shellAtPrompt(ctx context.Context, ts *targetState, a *activeRun) bool {
	info, err := ts.herdr.PaneProcessInfo(ctx, a.run.HerdrPane)
	if err != nil {
		a.log.Warn("process-info failed", "err", err)
		return false
	}
	a.promptSamples++
	atPrompt := info.ShellPID != nil && info.ForegroundProcessGroupID != nil &&
		*info.ShellPID == *info.ForegroundProcessGroupID
	return atPrompt && (a.promptSamples > 1 || a.sawAgent)
}

// finish runs step 9: it stores the output tail and the end of the run,
// closes the tab for on_done: close_tab, and deletes the prompt file.
func (r *Runner) finish(ctx context.Context, ts *targetState, a *activeRun, status, reason string) {
	if status != store.StatusLost {
		tail, err := ts.herdr.PaneRead(ctx, a.run.HerdrPane, tailLines)
		if err != nil {
			a.log.Warn("cannot read pane output", "err", err)
		}
		a.run.OutputTail = tail
	}
	if _, err := r.end(ctx, a, status, reason); err != nil {
		a.log.Error("cannot store run end", "err", err)
	}
}

// end stores the run as ended with status and reason, removes it from the
// active runs and cleans up on the target: the prompt file always, the tab
// for a run that failed to start or a done run with on_done: close_tab.
func (r *Runner) end(ctx context.Context, a *activeRun, status, reason string) (store.Run, error) {
	r.mu.Lock()
	delete(r.active, a.run.ID)
	r.mu.Unlock()
	failedToStart := a.run.Status == store.StatusStarting
	a.run.Status, a.run.ExitReason, a.run.EndedAt = status, reason, r.clock.Now()
	err := r.store.UpdateRun(ctx, a.run)
	a.log.Info("run ended", "status", status, "exit_reason", reason)

	ts := r.targets[a.task.Target]
	if ts == nil {
		return a.run, err
	}
	closeTab := failedToStart || (status == store.StatusDone && a.task.OnDone == "close_tab")
	if closeTab && a.run.HerdrTab != "" {
		if cerr := ts.herdr.TabClose(ctx, a.run.HerdrTab); cerr != nil && !herdr.IsCode(cerr, herdr.CodeTabNotFound) {
			a.log.Warn("cannot close tab", "herdr_tab", a.run.HerdrTab, "err", cerr)
		}
	}
	// The prompt file is written only after the tab and pane exist.
	if a.run.HerdrPane != "" {
		if rerr := ts.t.RemoveFile(ctx, promptPath(a.run.ID)); rerr != nil {
			a.log.Warn("cannot delete prompt file", "err", rerr)
		}
	}
	return a.run, err
}

// Run polls every target with active runs each poll_interval until ctx is
// done: one worker per target.
func (r *Runner) Run(ctx context.Context) {
	interval := r.cfg.PollInterval.Duration
	if interval <= 0 {
		interval = config.DefaultPollInterval
	}
	var wg sync.WaitGroup
	for name := range r.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-r.clock.After(interval):
					r.PollTarget(ctx, name)
				}
			}
		}()
	}
	wg.Wait()
}
