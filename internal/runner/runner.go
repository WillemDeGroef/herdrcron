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
	// stopKeyGap is the wait between the two Ctrl-C pairs of a stop, and
	// stopGrace the wait after them before the tab is closed.
	stopKeyGap = 2 * time.Second
	stopGrace  = 10 * time.Second
)

// ErrNotActive is returned by Stop, ReadOutput and PaneHasAgent for a run
// the runner does not track.
var ErrNotActive = errors.New("run is not active")

// ErrNoPane is returned by ReadOutput and PaneHasAgent for a run that is
// still starting and has no herdr pane yet.
var ErrNoPane = errors.New("run has no pane yet")

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
	// OnEnd, if set, is called after a run's end is stored, on every end
	// path (done, lost, failed, stopped, timeout, failed to start), with no
	// runner lock held.
	OnEnd func(ctx context.Context, run store.Run)
	// OnChange, if set, is called after every successful write of a run
	// row (the run reserved, its herdr ids stored, a status change, its
	// end), with the run as stored and no runner lock held. It must not
	// block: it may run inside a poll or a stop.
	OnChange func(run store.Run)
	// OnTargetChange, if set, is called when a target's reachability
	// changes (unknown, reachable, unreachable), with no runner lock held.
	// It must not block: it runs inside a poll or a run start.
	OnTargetChange func(TargetState)
}

// Runner starts and tracks runs. Its methods are safe for concurrent use.
type Runner struct {
	clock     clock.Clock
	store     *store.Store
	configDir string
	log       *slog.Logger
	waitStart time.Duration
	onEnd     func(context.Context, store.Run)
	onChange  func(store.Run)

	onTargetChange func(TargetState)

	// stopCtx is the context of stop sequences: Run cancels it when it
	// returns, and waits for the stops to give up (the runs stay open for
	// Recover).
	stopCtx    context.Context
	cancelStop context.CancelFunc
	stops      sync.WaitGroup

	// targetsMu guards cfg, targets, nextGen and the poll workers. Lock
	// order: mu, then a target's changeMu, then targetsMu. observe and a
	// drop in Reconfigure both take changeMu before targetsMu. Read the
	// map with config() and target().
	targetsMu sync.RWMutex
	cfg       config.Config
	targets   map[string]*targetState
	nextGen   uint64 // last generation stamped onto a targetState
	// runCtx is Run's context while Run is polling, else nil. workers
	// holds the cancel func of each target's poll worker.
	runCtx  context.Context
	workers map[string]context.CancelFunc
	polls   sync.WaitGroup

	mu     sync.Mutex
	active map[string]*activeRun // by run id
	// starting holds the runs between their insert and the end of their
	// launch, by run id; they are not in active yet.
	starting map[string]*activeRun
	closed   bool // Run has returned; no new stops
}

// targetState is a target with its herdr client and preflight cache.
type targetState struct {
	t     target.Target
	conf  config.Target // the settings it was built from
	herdr *herdr.Client
	shell []string

	mu          sync.Mutex
	serverUntil time.Time            // herdr server answered; valid until
	checks      map[string]checkedAt // agent command and cwd, by key

	// openMu serialises workspace lookup and creation, so two runs that
	// start at once for the same repo share one hc- workspace.
	openMu sync.Mutex

	// gen identifies this targetState. It changes when Reconfigure
	// replaces the target, so a reachability result from the old one can
	// be dropped. Immutable after stamp.
	gen uint64

	changeMu sync.Mutex // serializes reachability changes and their callbacks
	reachMu  sync.Mutex
	reach    reach
}

type checkedAt struct {
	cwd   string // cwd as resolved on the target
	until time.Time
}

// activeRun is the tracking state of a run that has not ended.
//
// mu is held by whoever acts on the run (a poll tracking it, or its stop
// sequence), so a poll and a stop never end it both. Fields of run change
// with Runner.mu held as well, so ActiveRuns can copy run under Runner.mu
// alone.
type activeRun struct {
	mu   sync.Mutex
	run  store.Run
	task config.Task
	log  *slog.Logger

	// Under Runner.mu.
	stopping bool          // a stop sequence was started; polls skip the run
	ended    bool          // the run's end was stored
	stopDone chan struct{} // closed when the stop sequence returns
	stopRun  store.Run     // the result of the stop, valid after stopDone
	stopErr  error

	idlePolls     int  // polls in a row with idle or done
	sawAgent      bool // herdr detected an agent in the pane at some point
	promptSamples int  // process-info samples taken
}

// New returns a runner with no active runs.
func New(o Options) *Runner {
	r := &Runner{
		clock: o.Clock, store: o.Store, cfg: o.Config, configDir: o.ConfigDir, log: o.Log,
		waitStart: o.ServerStartWait, onEnd: o.OnEnd, onChange: o.OnChange, onTargetChange: o.OnTargetChange,
		targets:  make(map[string]*targetState, len(o.Targets)),
		workers:  map[string]context.CancelFunc{},
		active:   map[string]*activeRun{},
		starting: map[string]*activeRun{},
	}
	r.stopCtx, r.cancelStop = context.WithCancel(context.Background())
	if r.waitStart <= 0 {
		r.waitStart = defaultServerStartWait
	}
	for name, t := range o.Targets {
		r.targets[name] = r.stamp(newTargetState(t, o.Config.Targets[name]))
	}
	return r
}

// stamp gives ts a new generation. The caller holds targetsMu, or is New.
func (r *Runner) stamp(ts *targetState) *targetState {
	r.nextGen++
	ts.gen = r.nextGen
	return ts
}

func newTargetState(t target.Target, tc config.Target) *targetState {
	shell := strings.Fields(tc.Shell)
	if len(shell) == 0 {
		shell = strings.Fields(defaultShell)
	}
	return &targetState{
		t: t, conf: tc, herdr: herdr.New(t, tc.HerdrPath), shell: shell, checks: map[string]checkedAt{},
	}
}

// target returns the named target, or nil if the runner has none.
func (r *Runner) target(name string) *targetState {
	r.targetsMu.RLock()
	defer r.targetsMu.RUnlock()
	return r.targets[name]
}

func (r *Runner) config() config.Config {
	r.targetsMu.RLock()
	defer r.targetsMu.RUnlock()
	return r.cfg
}

// Reconfigure applies cfg while the runner runs: the time zone and
// herdr_version at once, poll_interval from each poll worker's next wait,
// and the targets as far as that is safe. A target new in cfg is added (newTarget builds it; nil leaves
// it out) and polled from now on. A target that is gone from cfg, or
// whose host, shell or herdr_path changed, is removed or replaced only if
// it has no active run: replacing it forgets its reachability and its
// preflight cache, and a run must end on the target it started on. The
// targets that keep their old settings for that reason are returned,
// sorted; a later call applies them once their runs have ended.
// max_parallel is not the runner's and never defers a target.
func (r *Runner) Reconfigure(cfg config.Config, newTarget func(name string, tc config.Target) target.Target) (deferred []string) {
	// Under mu, so no run can start on a target between the check for
	// active runs and its replacement: Start registers the run before it
	// looks its target up.
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reconfigureLocked(cfg, newTarget)
}

// ReconfigureAfter is Reconfigure, but prepare runs once the deferred set
// is known and before the runner changes. The daemon uses it to load task
// files against the config it is about to publish: a non-nil error from
// prepare leaves the runner unchanged. prepare may run again when a run
// ends while it reads (the deferred set shrank). The caller must hold off
// new runs for the duration — a run that starts and ends in between would
// make prepare repeat without end. Passing a nil prepare is Reconfigure.
func (r *Runner) ReconfigureAfter(cfg config.Config, newTarget func(name string, tc config.Target) target.Target, prepare func(deferred []string) error) (deferred []string, err error) {
	if prepare == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.reconfigureLocked(cfg, newTarget), nil
	}
	for {
		r.mu.Lock()
		deferred = r.wouldDeferLocked(cfg)
		r.mu.Unlock()
		if err = prepare(deferred); err != nil {
			return nil, err
		}
		r.mu.Lock()
		// A run can have ended during prepare. Starting one cannot: the
		// caller is holding that off. The set only shrinks, so this ends.
		if now := r.wouldDeferLocked(cfg); !slices.Equal(deferred, now) {
			r.mu.Unlock()
			continue
		}
		deferred = r.reconfigureLocked(cfg, newTarget)
		r.mu.Unlock()
		return deferred, nil
	}
}

// wouldDeferLocked is the deferred set Reconfigure would return for cfg.
// The caller holds r.mu.
func (r *Runner) wouldDeferLocked(cfg config.Config) []string {
	busy := r.busyLocked()
	r.targetsMu.RLock()
	defer r.targetsMu.RUnlock()
	var deferred []string
	for name, ts := range r.targets {
		tc, wanted := cfg.Targets[name]
		if wanted && sameTarget(ts.conf, tc) {
			continue
		}
		if busy[name] {
			deferred = append(deferred, name)
		}
	}
	slices.Sort(deferred)
	return deferred
}

// busyLocked is the set of targets with a run active or still starting.
// The caller holds r.mu.
func (r *Runner) busyLocked() map[string]bool {
	busy := make(map[string]bool, len(r.active)+len(r.starting))
	for _, a := range r.active {
		busy[a.run.Target] = true
	}
	for _, a := range r.starting {
		busy[a.run.Target] = true
	}
	return busy
}

// reconfigureLocked is Reconfigure with r.mu held.
//
// A target that is dropped has its changeMu taken before targetsMu, the
// same order as observe, so an in-flight reachability callback finishes
// before the target leaves the map.
func (r *Runner) reconfigureLocked(cfg config.Config, newTarget func(name string, tc config.Target) target.Target) (deferred []string) {
	busy := r.busyLocked()
	type retire struct {
		name   string
		ts     *targetState
		wanted bool
	}
	r.targetsMu.RLock()
	var retirees []retire
	for name, ts := range r.targets {
		tc, wanted := cfg.Targets[name]
		if wanted && sameTarget(ts.conf, tc) {
			continue
		}
		if busy[name] {
			deferred = append(deferred, name)
			continue
		}
		retirees = append(retirees, retire{name: name, ts: ts, wanted: wanted})
	}
	r.targetsMu.RUnlock()

	for _, rt := range retirees {
		rt.ts.changeMu.Lock()
	}
	defer func() {
		for _, rt := range retirees {
			rt.ts.changeMu.Unlock()
		}
	}()

	r.targetsMu.Lock()
	defer r.targetsMu.Unlock()
	r.cfg = cfg
	for _, rt := range retirees {
		if r.targets[rt.name] != rt.ts {
			continue
		}
		delete(r.targets, rt.name)
		// Its worker may be sleeping out the old target's backoff; a
		// replacement gets a fresh one below.
		if stop := r.workers[rt.name]; stop != nil {
			stop()
			delete(r.workers, rt.name)
		}
		if !rt.wanted {
			r.log.Info("target removed", "target", rt.name)
		}
	}
	for name, tc := range cfg.Targets {
		if r.targets[name] != nil {
			continue
		}
		t := newTarget(name, tc)
		if t == nil {
			continue
		}
		r.targets[name] = r.stamp(newTargetState(t, tc))
		r.startWorkerLocked(name)
		r.log.Info("target configured", "target", name)
	}
	for name, stop := range r.workers {
		if r.targets[name] == nil {
			stop()
			delete(r.workers, name)
		}
	}
	slices.Sort(deferred)
	return deferred
}

// sameTarget reports whether a and b are the same target to the runner:
// max_parallel is the dispatcher's.
func sameTarget(a, b config.Target) bool {
	a.MaxParallel, b.MaxParallel = 0, 0
	return a == b
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
	r.mu.Lock()
	r.starting[run.ID] = a
	r.mu.Unlock()
	r.changed(run)
	a.log.Info("run starting", "trigger", trigger, "scheduled_at", scheduledAt)

	err := r.launch(ctx, a)
	if obs, ok := launchObservation(err); ok {
		r.observe(r.target(t.Target), t.Target, obs, true)
	}
	if err == nil {
		r.mu.Lock()
		delete(r.starting, run.ID)
		r.active[run.ID] = a
		r.mu.Unlock()
		return a.run, nil
	}
	// A run that failed to launch stays in starting until end takes it
	// out, so Reconfigure cannot replace its target before the cleanup.
	if errors.Is(err, target.ErrUnreachable) {
		// Recorded so the missed policy can catch the fire up when the
		// target answers again.
		a.log.Warn("target unreachable at fire time", "err", err)
		return r.end(context.WithoutCancel(ctx), a, store.StatusHostUnreachable, err.Error())
	}
	a.log.Warn("run failed to start", "err", err)
	return r.end(context.WithoutCancel(ctx), a, store.StatusFailed, err.Error())
}

// launch runs steps 2 to 6. On error, a.run holds whatever herdr ids were
// already created, so end can clean up.
func (r *Runner) launch(ctx context.Context, a *activeRun) error {
	t := a.task
	ts := r.target(t.Target)
	ok := ts != nil
	if !ok {
		return fmt.Errorf("unknown target %q", t.Target)
	}
	if err := r.knownUnreachable(ts, t.Target); err != nil {
		return err
	}
	cwd, err := r.preflight(ctx, ts, t, a.log)
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

	// If openTab succeeds but a later step fails, close the hc- tab so it is
	// not left behind (step 9 only runs on end for successful launches).
	launchOK := false
	defer func() {
		if launchOK {
			return
		}
		r.cleanupFailedLaunch(context.WithoutCancel(ctx), ts, a)
	}()

	if err := r.openTab(ctx, ts, a, cwd, label, env); err != nil {
		return err
	}
	if err := r.store.UpdateRun(ctx, a.run); err != nil {
		return err
	}
	r.changed(a.run)

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
	r.mu.Lock()
	a.run = running
	r.mu.Unlock()
	r.changed(running)
	launchOK = true
	a.log.Info("run launched", "herdr_workspace", a.run.HerdrWorkspace, "herdr_tab", a.run.HerdrTab,
		"herdr_pane", a.run.HerdrPane, "agent", t.Agent.Command, "env_keys", envKeys(t.Env))
	return nil
}

// cleanupFailedLaunch closes the tab opened for a run whose launch did not
// finish. Prompt removal and a second tab close in end() are harmless.
func (r *Runner) cleanupFailedLaunch(ctx context.Context, ts *targetState, a *activeRun) {
	if a.run.HerdrTab == "" {
		return
	}
	if cerr := ts.herdr.TabClose(ctx, a.run.HerdrTab); cerr != nil && !herdr.IsCode(cerr, herdr.CodeTabNotFound) {
		a.log.Warn("cannot close tab after failed launch", "herdr_tab", a.run.HerdrTab, "err", cerr)
	}
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
		r.setHerdrIDs(a, res.Tab.WorkspaceID, res.Tab.TabID, res.RootPane.PaneID)
		return nil
	}
	res, err := ts.herdr.WorkspaceCreate(ctx, cwd, wsLabel, env)
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	r.setHerdrIDs(a, res.Workspace.WorkspaceID, res.Tab.TabID, res.RootPane.PaneID)
	if err := ts.herdr.TabRename(ctx, res.Tab.TabID, label); err != nil {
		return fmt.Errorf("rename tab: %w", err)
	}
	return nil
}

// setHerdrIDs stores a starting run's herdr ids under r.mu, which LiveRuns
// reads it with.
func (r *Runner) setHerdrIDs(a *activeRun, workspace, tab, pane string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a.run.HerdrWorkspace, a.run.HerdrTab, a.run.HerdrPane = workspace, tab, pane
}

// tabLabel is hc-<task-id>-<HHMM>, in the task's time zone.
func (r *Runner) tabLabel(t config.Task) (string, error) {
	loc, err := r.config().Location(t)
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
func (r *Runner) preflight(ctx context.Context, ts *targetState, t config.Task, log *slog.Logger) (string, error) {
	now := r.clock.Now()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if now.After(ts.serverUntil) {
		if err := r.checkServer(ctx, ts, log); err != nil {
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
func (r *Runner) checkServer(ctx context.Context, ts *targetState, log *slog.Logger) error {
	_, err := ts.herdr.PaneList(ctx)
	if !herdr.IsCode(err, herdr.CodeServerNotRunning) {
		if err != nil {
			return fmt.Errorf("preflight: herdr server: %w", err)
		}
		return nil
	}
	log.Info("herdr server not running; starting it")
	// notRunning stays in the chain: the host answered, so a start failure
	// is still a reachable target (launchObservation looks for this code).
	notRunning := err
	if err := ts.herdr.StartServer(ctx); err != nil {
		return fmt.Errorf("preflight: start herdr server: %w", errors.Join(err, notRunning))
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

// Recover takes over the runs a previous daemon left open (no ended_at),
// as the spec's recovery says: a run whose pane still exists is tracked
// again from step 7, a run whose pane is gone is lost, and a run on a
// target that does not answer stays open for the next poll. A run on a
// target that is no longer configured is lost; a run still starting (the
// daemon stopped during its launch, before the agent ran) failed. Only reading the open runs
// can fail Recover. tasks gives each run's task by
// id; a run of a task that is gone is tracked with no timeout.
func (r *Runner) Recover(ctx context.Context, tasks []config.Task) error {
	runs, err := r.store.ListOpenRuns(ctx)
	if err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	byID := make(map[string]config.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	var targets []string
	for _, run := range runs {
		if r.Active(run.ID) {
			continue
		}
		t, ok := byID[run.TaskID]
		if !ok {
			t = config.Task{ID: run.TaskID, Target: run.Target}
		}
		a := &activeRun{run: run, task: t, log: logging.ForRun(r.log, run.ID, run.TaskID, run.Target)}
		a.log.Info("recovering run", "status", run.Status, "started_at", run.StartedAt, "herdr_pane", run.HerdrPane)
		var err error
		switch {
		case r.target(run.Target) == nil:
			_, err = r.end(ctx, a, store.StatusLost, "target "+run.Target+" not configured")
		case run.Status == store.StatusStarting:
			// The agent was never launched; end closes the tab if there is one.
			_, err = r.end(ctx, a, store.StatusFailed, "daemon stopped during launch")
		default:
			r.mu.Lock()
			r.active[run.ID] = a
			r.mu.Unlock()
			if !slices.Contains(targets, run.Target) {
				targets = append(targets, run.Target)
			}
		}
		if err != nil {
			a.log.Error("cannot store run end", "err", err)
		}
	}
	for _, name := range targets {
		r.PollTarget(ctx, name)
	}
	return nil
}

// Active reports whether the run is being tracked.
func (r *Runner) Active(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[runID]
	return ok
}

// Live reports whether the run is tracked as active or still starting:
// the runs ReadOutput and PaneHasAgent can answer for.
func (r *Runner) Live(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[runID]
	if !ok {
		_, ok = r.starting[runID]
	}
	return ok
}

// ReadOutput returns a snapshot of an active run and the last lines of
// its pane with ANSI escapes kept, for a live view. A run the runner does
// not track gives ErrNotActive; one without a pane yet gives ErrNoPane.
func (r *Runner) ReadOutput(ctx context.Context, runID string, lines int) (store.Run, string, error) {
	run, ts, err := r.livePane(runID)
	if err != nil {
		return store.Run{}, "", err
	}
	out, err := ts.herdr.PaneReadANSI(ctx, run.HerdrPane, lines)
	return run, out, err
}

// PaneHasAgent returns a snapshot of an active run and whether herdr
// still detects an agent in its pane: false once the agent has exited to
// the shell, or the pane is gone. Errors are those of ReadOutput, or the
// pane list's.
func (r *Runner) PaneHasAgent(ctx context.Context, runID string) (store.Run, bool, error) {
	run, ts, err := r.livePane(runID)
	if err != nil {
		return store.Run{}, false, err
	}
	panes, err := ts.herdr.PaneList(ctx)
	if err != nil {
		return store.Run{}, false, err
	}
	for _, p := range panes {
		if p.PaneID == run.HerdrPane {
			return run, p.Agent != nil, nil
		}
	}
	return run, false, nil
}

// livePane returns a snapshot of a tracked run (active, or starting with
// its pane created) and its target.
func (r *Runner) livePane(runID string) (store.Run, *targetState, error) {
	r.mu.Lock()
	a, ok := r.active[runID]
	if !ok {
		a, ok = r.starting[runID]
	}
	var run store.Run
	if ok {
		run = a.run
	}
	r.mu.Unlock()
	switch {
	case !ok:
		return store.Run{}, nil, fmt.Errorf("run %s: %w", runID, ErrNotActive)
	case run.HerdrPane == "":
		return store.Run{}, nil, fmt.Errorf("run %s: %w", runID, ErrNoPane)
	}
	ts := r.target(run.Target)
	if ts == nil {
		return store.Run{}, nil, fmt.Errorf("run %s: unknown target %q", runID, run.Target)
	}
	return run, ts, nil
}

// PollTarget runs step 7 once for every active run on the target: one
// `herdr pane list` covers them all. Panes without an agent get one
// `pane process-info` each, to see whether the command has ended. Runs
// being stopped are skipped: the stop decides their end.
func (r *Runner) PollTarget(ctx context.Context, name string) {
	r.mu.Lock()
	var runs []*activeRun
	for _, a := range r.active {
		if a.run.Target == name && !a.stopping {
			runs = append(runs, a)
		}
	}
	r.mu.Unlock()
	ts := r.target(name)
	if ts == nil {
		return
	}
	if len(runs) == 0 {
		r.probe(ctx, ts, name)
		return
	}
	slices.SortFunc(runs, func(a, b *activeRun) int { return strings.Compare(a.run.ID, b.run.ID) })

	// Step 8, before the pane list: the timeout counts while the target
	// does not answer.
	now := r.clock.Now()
	runs = slices.DeleteFunc(runs, func(a *activeRun) bool {
		d := a.task.Timeout.Duration
		if d <= 0 || now.Before(a.run.StartedAt.Add(d)) {
			return false
		}
		r.beginStop(a, store.StatusTimeout, "timeout after "+formatDuration(d))
		return true
	})
	if len(runs) == 0 {
		return
	}

	panes, err := ts.herdr.PaneList(ctx)
	r.observe(ts, name, err, true)
	if herdr.IsCode(err, herdr.CodeServerNotRunning) {
		// The server and every pane in it are gone (a host reboot, say).
		// The next launch must start the server again.
		ts.mu.Lock()
		ts.serverUntil = time.Time{}
		ts.mu.Unlock()
		ctx = context.WithoutCancel(ctx)
		for _, a := range runs {
			r.pollRun(a, func() { r.finish(ctx, ts, a, store.StatusLost, "herdr server not running") })
		}
		return
	}
	if err != nil {
		// The agents keep running on the target: the runs stay open, and
		// the next poll (after the backoff, for an unreachable target)
		// tries again. Only the timeout above ends them meanwhile. One
		// line per run so each carries that run's id: `herdrcron logs
		// --task` keeps only records with task_id.
		for _, a := range runs {
			a.log.Warn("poll failed", "err", err, "active_runs", len(runs))
		}
		return
	}
	// A run that ends must be stored even when the daemon stops mid-poll.
	ctx = context.WithoutCancel(ctx)
	byID := make(map[string]herdr.PaneInfo, len(panes))
	for _, p := range panes {
		byID[p.PaneID] = p
	}
	for _, a := range runs {
		r.pollRun(a, func() {
			p, ok := byID[a.run.HerdrPane]
			if !ok {
				r.finish(ctx, ts, a, store.StatusLost, "pane gone")
				return
			}
			r.track(ctx, ts, a, p)
		})
	}
}

// pollRun applies the poll's result to one run with a.mu held, unless a
// stop started since the poll began or the run has ended.
func (r *Runner) pollRun(a *activeRun, apply func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.mu.Lock()
	skip := a.stopping || a.ended
	r.mu.Unlock()
	if skip {
		return
	}
	apply()
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
	if status != a.run.Status {
		a.log.Info("run status changed", "from", a.run.Status, "to", status, "agent_status", p.AgentStatus)
		r.mu.Lock()
		a.run.Status = status
		r.mu.Unlock()
		if err := r.store.UpdateRun(ctx, a.run); err != nil {
			a.log.Error("cannot store run status", "err", err)
		} else {
			r.changed(a.run)
		}
	}
	// On every blocked sample, not only on the change: a run recovered as
	// blocked has no change. Polls skip a run being stopped, and beginStop
	// starts at most one stop.
	if status == store.StatusBlocked && a.task.OnBlocked == "stop" {
		r.beginStop(a, store.StatusStopped, "blocked (on_blocked: stop)")
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
		r.mu.Lock()
		a.run.OutputTail = tail
		r.mu.Unlock()
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
	// The target is taken while the run still counts as active, so the
	// cleanup below goes to the target the run started on even if
	// Reconfigure replaces or removes it right after.
	ts := r.target(a.run.Target)
	delete(r.active, a.run.ID)
	delete(r.starting, a.run.ID)
	a.ended = true
	r.mu.Unlock()
	// Out of the active map, a.run is no longer read by ActiveRuns.
	failedToStart := a.run.Status == store.StatusStarting
	a.run.Status, a.run.ExitReason, a.run.EndedAt = status, reason, r.clock.Now()
	err := r.store.UpdateRun(ctx, a.run)
	if err == nil {
		r.changed(a.run)
	}
	a.log.Info("run ended", "status", status, "exit_reason", reason)
	if r.onEnd != nil {
		defer r.onEnd(ctx, a.run)
	}

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

// Stop stops an active run at the user's request (step 10): status
// stopped, reason "stopped by user". It blocks until the stop sequence has
// finished and returns the ended run. A run already being stopped is not
// stopped twice: Stop waits for that stop and returns its result. A run the
// runner does not track gives ErrNotActive.
func (r *Runner) Stop(ctx context.Context, runID string) (store.Run, error) {
	r.mu.Lock()
	a, ok := r.active[runID]
	if !ok {
		r.mu.Unlock()
		return store.Run{}, fmt.Errorf("stop %s: %w", runID, ErrNotActive)
	}
	started := r.beginStopLocked(a, store.StatusStopped, "stopped by user")
	r.mu.Unlock()
	if !started {
		return store.Run{}, fmt.Errorf("stop %s: runner is shutting down", runID)
	}
	select {
	case <-a.stopDone:
		return a.stopRun, a.stopErr
	case <-ctx.Done():
		return store.Run{}, ctx.Err()
	}
}

// beginStop starts the stop sequence of a in its own goroutine, unless one
// is already running.
func (r *Runner) beginStop(a *activeRun, status, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beginStopLocked(a, status, reason)
}

// beginStopLocked is beginStop with r.mu held. It reports whether a stop
// is running for a: false once Run has returned.
func (r *Runner) beginStopLocked(a *activeRun, status, reason string) bool {
	if a.stopping {
		return true
	}
	if r.closed {
		return false
	}
	a.stopping = true
	a.stopDone = make(chan struct{})
	r.stops.Add(1)
	go func() {
		defer r.stops.Done()
		run, err := r.stop(r.stopCtx, a, status, reason)
		r.mu.Lock()
		a.stopRun, a.stopErr = run, err
		r.mu.Unlock()
		close(a.stopDone)
	}()
	return true
}

// stop runs step 10 as adapted in docs/herdr-findings.md: esc (dismisses a
// question dialog), Ctrl-C twice in one call (exits Claude Code), again
// after 2 s, then after 10 s the output tail is read and the tab closed,
// whatever on_done says. Key, read and close errors are logged and do not
// keep the run from ending. If ctx ends during a wait (the daemon stops),
// the run stays open for Recover.
func (r *Runner) stop(ctx context.Context, a *activeRun, status, reason string) (store.Run, error) {
	// Wait for a poll that is tracking the run; it may have ended it.
	a.mu.Lock()
	defer a.mu.Unlock()
	r.mu.Lock()
	ended := a.ended
	r.mu.Unlock()
	if ended {
		return a.run, nil
	}
	ts := r.target(a.run.Target)
	if ts == nil {
		return r.end(context.WithoutCancel(ctx), a, status, reason)
	}
	a.log.Info("stopping run", "status", status, "exit_reason", reason)
	pane := a.run.HerdrPane
	keys := func(k ...string) {
		if err := ts.herdr.PaneSendKeys(ctx, pane, k...); err != nil {
			a.log.Warn("cannot send keys", "keys", strings.Join(k, " "), "err", err)
		}
	}
	keys("esc")
	keys("ctrl+c", "ctrl+c")
	if err := r.sleep(ctx, stopKeyGap); err != nil {
		a.log.Warn("stop interrupted; run left open", "err", err)
		return a.run, err
	}
	keys("ctrl+c", "ctrl+c")
	if err := r.sleep(ctx, stopGrace); err != nil {
		a.log.Warn("stop interrupted; run left open", "err", err)
		return a.run, err
	}
	// From here the stop is quick and must be stored.
	ctx = context.WithoutCancel(ctx)
	tail, err := ts.herdr.PaneRead(ctx, pane, tailLines)
	if err != nil {
		a.log.Warn("cannot read pane output", "err", err)
	}
	r.mu.Lock()
	a.run.OutputTail = tail
	r.mu.Unlock()
	if a.run.HerdrTab != "" {
		if err := ts.herdr.TabClose(ctx, a.run.HerdrTab); err != nil && !herdr.IsCode(err, herdr.CodeTabNotFound) {
			a.log.Warn("cannot close tab", "herdr_tab", a.run.HerdrTab, "err", err)
		}
	}
	return r.end(ctx, a, status, reason)
}

// sleep waits d on the runner's clock, or returns ctx's error.
func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.clock.After(d):
		return nil
	}
}

// changed calls OnChange for a stored run.
func (r *Runner) changed(run store.Run) {
	if r.onChange != nil {
		r.onChange(run)
	}
}

// LiveRuns is ActiveRuns plus the runs still starting (reserved, launch
// not finished), oldest first. It is the view for display; dispatch counts
// starting runs itself and uses ActiveRuns.
func (r *Runner) LiveRuns() []store.Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Run, 0, len(r.active)+len(r.starting))
	for _, a := range r.active {
		out = append(out, a.run)
	}
	for _, a := range r.starting {
		out = append(out, a.run)
	}
	sortRuns(out)
	return out
}

// ActiveRuns returns a snapshot of every tracked run, on any target,
// including runs being stopped, oldest first.
func (r *Runner) ActiveRuns() []store.Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Run, 0, len(r.active))
	for _, a := range r.active {
		out = append(out, a.run)
	}
	sortRuns(out)
	return out
}

// sortRuns sorts runs oldest first, by start time then id.
func sortRuns(runs []store.Run) {
	slices.SortFunc(runs, func(a, b store.Run) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// Run polls every target with active runs each poll_interval until ctx is
// done: one worker per target. Before it returns it cancels the stop
// sequences in flight and waits for them; their runs stay open, and no new
// stop starts. A Runner runs once.
func (r *Runner) Run(ctx context.Context) {
	defer func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancelStop()
		r.stops.Wait()
	}()
	r.targetsMu.Lock()
	r.runCtx = ctx
	for name := range r.targets {
		r.startWorkerLocked(name)
	}
	r.targetsMu.Unlock()
	<-ctx.Done()
	// No worker starts once runCtx is nil, so Wait cannot race an Add.
	r.targetsMu.Lock()
	r.runCtx = nil
	r.targetsMu.Unlock()
	r.polls.Wait()
}

// startWorkerLocked starts the poll worker of the named target, if Run is
// polling and the target has none; targetsMu must be held. The worker
// stops when Reconfigure removes or replaces the target (a replacement
// gets a new worker), or when Run's context is done.
func (r *Runner) startWorkerLocked(name string) {
	if r.runCtx == nil || r.workers[name] != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.runCtx)
	r.workers[name] = cancel
	r.polls.Add(1)
	go func() {
		defer r.polls.Done()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.clock.After(r.pollWait(name)):
				r.PollTarget(ctx, name)
			}
		}
	}()
}

// formatDuration is d without zero trailing units: 2h, 1h30m, 45s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
