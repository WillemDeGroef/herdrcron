// Command herdrcron schedules recurring coding-agent runs in herdr panes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/logread"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/service"
	"github.com/WillemDeGroef/herdrcron/internal/tui"
	untilparse "github.com/WillemDeGroef/herdrcron/internal/until"

	"github.com/charmbracelet/x/term"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: herdrcron [<command>]

Without a command herdrcron opens the TUI (over SSH: ssh -t host herdrcron).
Quitting it leaves the daemon and every run as they are.

commands:
  daemon            run the daemon in the foreground
  list              list tasks with target, schedule, state, last run and next fire
  run [--parallel] <id>
                    start a manual run of a task now, even if it is off or paused;
                    --parallel starts it next to an active run (overlap skip)
  stop <run-id>     stop an active run
  logs [-f] [--task <id>]
                    print the daemon log; -f follows it, --task filters by task
  install-service   install and start the daemon as a systemd user service
                    (Linux) or launchd agent (macOS)
  enable <id>       turn a task on (enabled: true)
  disable <id>      turn a task off (enabled: false)
  pause <id> [--until T]
                    pause a task until T (RFC 3339, or local time as
                    2026-11-10T08:00, "2026-11-10 08:00" or 2026-11-10),
                    or indefinitely without --until
  resume <id>       end a task's pause
  pause --all [--target NAME]
                    pause every task, or every task on one target
  resume --all [--target NAME]
                    end a global pause (of every target, or of one target)
  check <target>    check a target: herdr server, herdr version, and the
                    agent commands and cwds of its tasks; exits 1 if a check fails
`

// serviceRunner runs systemctl/launchctl for install-service; tests swap it.
var serviceRunner service.Runner = service.ExecRunner

// executable returns the resolved path of the running binary for
// install-service; tests swap it (the test binary lives in a temp dir).
var executable = func() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(bin)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	p, err := paths.FromEnv()
	if err != nil {
		fmt.Fprintln(stderr, "herdrcron:", err)
		return 1
	}
	if len(args) == 0 {
		return runTUI(ctx, p, stderr)
	}
	switch args[0] {
	case "daemon":
		return runDaemon(ctx, p, stderr)
	case "list":
		return runList(ctx, p, stdout, stderr)
	case "run":
		return runRun(ctx, p, args[1:], stdout, stderr)
	case "stop":
		return runStop(ctx, p, args[1:], stdout, stderr)
	case "logs":
		return runLogs(ctx, p, args[1:], stdout, stderr)
	case "install-service":
		return runInstallService(ctx, p, args[1:], stdout, stderr)
	case "enable", "disable":
		return runSetEnabled(ctx, p, args[0], args[1:], stdout, stderr)
	case "pause":
		return runPause(ctx, p, args[1:], time.Now(), stdout, stderr)
	case "resume":
		return runResume(ctx, p, args[1:], stdout, stderr)
	case "check":
		return runCheck(ctx, p, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "herdrcron: unknown command %q\n%s", args[0], usage)
	return 2
}

func runDaemon(ctx context.Context, p paths.Paths, stderr io.Writer) int {
	err := daemon.Run(ctx, p, daemon.Options{
		Version: version,
		OnListening: func(socket string) {
			fmt.Fprintf(stderr, "herdrcron daemon %s listening on %s (log: %s)\n", version, socket, p.Log())
		},
	})
	if err != nil {
		fmt.Fprintln(stderr, "herdrcron daemon:", err)
		return 1
	}
	return 0
}

// isTerminal reports whether stdin and stdout are a terminal, as the TUI
// needs; tests swap it.
var isTerminal = func() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// runTUI opens the TUI on the daemon. It needs a terminal and a running
// daemon; quitting it changes nothing in the daemon.
func runTUI(ctx context.Context, p paths.Paths, stderr io.Writer) int {
	if !isTerminal() {
		fmt.Fprint(stderr, "herdrcron: the TUI needs a terminal; over SSH use `ssh -t host herdrcron`, in scripts use a command\n\n"+usage)
		return 2
	}
	c := dial(p, stderr)
	if c == nil {
		return 1
	}
	_ = c.Close()
	if err := tui.Run(ctx, tui.SocketBackend{Path: p.Socket()}); err != nil {
		fmt.Fprintln(stderr, "herdrcron:", err)
		return 1
	}
	return 0
}

// dial connects to the daemon, or prints why not and returns nil.
func dial(p paths.Paths, stderr io.Writer) *api.Client {
	c, err := api.Dial(p.Socket())
	if err != nil {
		if errors.Is(err, api.ErrDaemonNotRunning) {
			fmt.Fprintf(stderr, "herdrcron: daemon is not running (no daemon on %s); start it with `herdrcron daemon`\n", p.Socket())
		} else {
			fmt.Fprintln(stderr, "herdrcron:", err)
		}
		return nil
	}
	return c
}

// callDaemon dials the daemon and makes one call. On failure it writes the
// error to stderr (with how to start the daemon if it is not running) and
// returns false.
func callDaemon(ctx context.Context, p paths.Paths, stderr io.Writer, method string, params, result any) bool {
	c := dial(p, stderr)
	if c == nil {
		return false
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Call(ctx, method, params, result); err != nil {
		fmt.Fprintf(stderr, "herdrcron: %s: %v\n", method, err)
		return false
	}
	return true
}

func runList(ctx context.Context, p paths.Paths, stdout, stderr io.Writer) int {
	var res api.TasksListResult
	if !callDaemon(ctx, p, stderr, api.MethodTasksList, nil, &res) {
		return 1
	}
	if len(res.Tasks) == 0 {
		fmt.Fprintf(stdout, "no tasks in %s\n", p.TasksDir())
		return 0
	}
	if err := printTasks(stdout, res.Tasks, time.Now(), time.Local); err != nil {
		fmt.Fprintln(stderr, "herdrcron:", err)
		return 1
	}
	return 0
}

// runRun starts a manual run and prints one line about it. It exits 1 if
// the run failed to start or the task has an active run (overlap skip)
// and --parallel is not given.
func runRun(ctx context.Context, p paths.Paths, args []string, stdout, stderr io.Writer) int {
	parallel := false
	if len(args) > 0 && args[0] == "--parallel" {
		parallel, args = true, args[1:]
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "usage: herdrcron run [--parallel] <id>\n")
		return 2
	}
	c := dial(p, stderr)
	if c == nil {
		return 1
	}
	defer func() { _ = c.Close() }()
	// Start waits for the launch in herdr, which includes preflight and
	// possibly starting the herdr server.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var r api.RunNowResult
	if err := c.Call(ctx, api.MethodTasksRunNow, api.RunNowParams{ID: args[0], ForceParallel: parallel}, &r); err != nil {
		var rpcErr *api.Error
		switch {
		case errors.As(err, &rpcErr) && rpcErr.Code == api.CodeRunActive:
			fmt.Fprintf(stderr, "herdrcron run: %s; use `herdrcron run --parallel %s` to start another one\n", rpcErr.Message, args[0])
		case errors.As(err, &rpcErr):
			fmt.Fprintln(stderr, "herdrcron run:", strings.TrimPrefix(rpcErr.Message, "invalid params: "))
		default:
			fmt.Fprintln(stderr, "herdrcron: tasks.run_now:", err)
		}
		return 1
	}
	if r.Queued {
		fmt.Fprintf(stdout, "run of %s queued: it starts when the active run ends or a slot on the target is free\n", r.TaskID)
		return 0
	}
	line := fmt.Sprintf("run %s started for %s: %s", r.ID, r.TaskID, r.Status)
	if r.HerdrTab != "" {
		line += fmt.Sprintf(", herdr tab %s pane %s", r.HerdrTab, r.HerdrPane)
	}
	if r.ExitReason != "" {
		line += ": " + r.ExitReason
	}
	fmt.Fprintln(stdout, line)
	if r.Status == "failed" {
		return 1
	}
	return 0
}

// runStop stops an active run and prints how it ended. The daemon answers
// once the stop sequence is done, which takes about 12 s.
func runStop(ctx context.Context, p paths.Paths, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "usage: herdrcron stop <run-id>\n")
		return 2
	}
	c := dial(p, stderr)
	if c == nil {
		return 1
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var r api.RunSummary
	if err := c.Call(ctx, api.MethodRunsStop, api.RunsStopParams{RunID: args[0]}, &r); err != nil {
		var rpcErr *api.Error
		if errors.As(err, &rpcErr) {
			fmt.Fprintln(stderr, "herdrcron stop:", strings.TrimPrefix(rpcErr.Message, "invalid params: "))
		} else {
			fmt.Fprintln(stderr, "herdrcron: runs.stop:", err)
		}
		return 1
	}
	line := fmt.Sprintf("run %s of %s: %s", r.ID, r.TaskID, r.Status)
	if r.ExitReason != "" {
		line += ": " + r.ExitReason
	}
	fmt.Fprintln(stdout, line)
	return 0
}

// runCheck prints the preflight report of a target. It exits 1 if a check
// failed; warnings keep it 0.
func runCheck(ctx context.Context, p paths.Paths, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "usage: herdrcron check <target>\n")
		return 2
	}
	c := dial(p, stderr)
	if c == nil {
		return 1
	}
	defer func() { _ = c.Close() }()
	// The daemon bounds a check at 60 s (ssh connect timeouts included).
	ctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	var r api.TargetCheck
	if err := c.Call(ctx, api.MethodTargetsCheck, api.TargetCheckParams{Name: args[0]}, &r); err != nil {
		var rpcErr *api.Error
		if errors.As(err, &rpcErr) {
			fmt.Fprintln(stderr, "herdrcron check:", strings.TrimPrefix(rpcErr.Message, "invalid params: "))
		} else {
			fmt.Fprintln(stderr, "herdrcron: targets.check:", err)
		}
		return 1
	}
	printCheck(stdout, r)
	if !r.OK {
		return 1
	}
	return 0
}

// printCheck writes a check report: a header, then one line per item with
// its result, name, detail and tasks.
func printCheck(out io.Writer, r api.TargetCheck) {
	verdict := "ok"
	if !r.OK {
		verdict = "FAILED"
	}
	fmt.Fprintf(out, "target %s: %s\n", r.Target, verdict)
	for _, it := range r.Items {
		line := fmt.Sprintf("  %-4s  %s", it.Result, it.Name)
		if it.Detail != "" {
			line += "  " + it.Detail
		}
		if len(it.Tasks) > 0 {
			line += "  (tasks: " + strings.Join(it.Tasks, ", ") + ")"
		}
		fmt.Fprintln(out, line)
	}
}

// printTasks writes the task table. LAST is the last run's status and age
// at now; NEXT is the next fire time in loc. An invalid task's error goes
// after the last column.
func printTasks(out io.Writer, tasks []api.TaskSummary, now time.Time, loc *time.Location) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTARGET\tSCHEDULE\tSTATE\tLAST\tNEXT")
	for _, t := range tasks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s", t.ID, orDash(t.Target), orDash(t.Schedule), t.State,
			lastRun(t.LastRun, now), nextFire(t.NextFireAt, loc))
		if t.Error != "" {
			fmt.Fprintf(w, "\t%s", t.Error)
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func lastRun(r *api.RunSummary, now time.Time) string {
	if r == nil {
		return "-"
	}
	started, err := time.Parse(time.RFC3339, r.StartedAt)
	if err != nil {
		return r.Status
	}
	return r.Status + " " + ago(now.Sub(started))
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

func nextFire(s string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "-"
	}
	return t.In(loc).Format("Mon 02 Jan 15:04")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parseInterleaved parses args with fl, allowing flags before, between and
// after positional arguments (the flag package stops at the first
// positional). Everything after "--" is positional. It returns the
// positional arguments.
func parseInterleaved(fl *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, err
		}
		rest := fl.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseTaskArgs parses the flags of a per-task command and returns its one
// task id. ok is false after a usage error (already written to stderr);
// code is then the exit code.
func parseTaskArgs(fl *flag.FlagSet, cmd string, args []string, stderr io.Writer) (id string, code int, ok bool) {
	pos, err := parseInterleaved(fl, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", 0, false
		}
		return "", 2, false
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "herdrcron %s: want exactly one task id, got %d arguments\n", cmd, len(pos))
		return "", 2, false
	}
	return pos[0], 0, true
}

func runSetEnabled(ctx context.Context, p paths.Paths, cmd string, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fl.SetOutput(stderr)
	id, code, ok := parseTaskArgs(fl, cmd, args, stderr)
	if !ok {
		return code
	}
	var t api.TaskSummary
	if !callDaemon(ctx, p, stderr, api.MethodTasksSetEnabled, api.SetEnabledParams{ID: id, Enabled: cmd == "enable"}, &t) {
		return 1
	}
	if t.State == api.StateOff {
		fmt.Fprintf(stdout, "%s: off\n", t.ID)
	} else {
		fmt.Fprintf(stdout, "%s: enabled%s\n", t.ID, taskOutlook(t, time.Local))
	}
	return 0
}

// taskOutlook describes when an enabled task fires next: ", next fire …",
// or why it does not.
func taskOutlook(t api.TaskSummary, loc *time.Location) string {
	switch {
	case t.State == api.StatePaused && t.PausedUntil != "":
		return ", " + pausedText(t.PausedUntil, loc)
	case t.State == api.StatePaused:
		return ", paused by a global pause"
	case t.NextFireAt != "":
		return ", next fire " + nextFire(t.NextFireAt, loc)
	case t.State != "" && t.State != api.StateIdle:
		return " (" + t.State + ")"
	}
	return ""
}

// pausedText describes a paused_until value.
func pausedText(until string, loc *time.Location) string {
	if until == api.PauseIndefinite {
		return "paused indefinitely"
	}
	return "paused until " + nextFire(until, loc)
}

func runPause(ctx context.Context, p paths.Paths, args []string, now time.Time, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("pause", flag.ContinueOnError)
	fl.SetOutput(stderr)
	all := fl.Bool("all", false, "pause every task (a global pause)")
	target := fl.String("target", "", "with --all: pause only the tasks on this target")
	until := fl.String("until", "", "pause until this time instead of indefinitely")
	pos, err := parseInterleaved(fl, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	set := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *all {
		if len(pos) > 0 {
			fmt.Fprintf(stderr, "herdrcron pause: --all takes no task id (got %q)\n", pos[0])
			return 2
		}
		if set["until"] {
			fmt.Fprintln(stderr, "herdrcron pause: --until is not supported with --all")
			return 2
		}
		return runGlobalPause(ctx, p, api.MethodPauseAll, *target, stdout, stderr)
	}
	if set["target"] {
		fmt.Fprintln(stderr, "herdrcron pause: --target needs --all")
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "herdrcron pause: want exactly one task id, got %d arguments\n", len(pos))
		return 2
	}
	params := api.PauseParams{ID: pos[0], Until: api.PauseIndefinite}
	if set["until"] {
		params.Until, err = untilparse.Parse(*until, now, time.Local)
		if err != nil {
			fmt.Fprintln(stderr, "herdrcron pause: --until:", err)
			return 2
		}
	}
	var t api.TaskSummary
	if !callDaemon(ctx, p, stderr, api.MethodTasksPause, params, &t) {
		return 1
	}
	if t.PausedUntil != "" {
		fmt.Fprintf(stdout, "%s: %s\n", t.ID, pausedText(t.PausedUntil, time.Local))
	} else {
		fmt.Fprintf(stdout, "%s: %s\n", t.ID, t.State)
	}
	return 0
}

func runResume(ctx context.Context, p paths.Paths, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("resume", flag.ContinueOnError)
	fl.SetOutput(stderr)
	all := fl.Bool("all", false, "end the global pause")
	target := fl.String("target", "", "with --all: end only the pause of this target")
	pos, err := parseInterleaved(fl, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	targetSet := false
	fl.Visit(func(f *flag.Flag) { targetSet = targetSet || f.Name == "target" })
	if *all {
		if len(pos) > 0 {
			fmt.Fprintf(stderr, "herdrcron resume: --all takes no task id (got %q)\n", pos[0])
			return 2
		}
		return runGlobalPause(ctx, p, api.MethodResumeAll, *target, stdout, stderr)
	}
	if targetSet {
		fmt.Fprintln(stderr, "herdrcron resume: --target needs --all")
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "herdrcron resume: want exactly one task id, got %d arguments\n", len(pos))
		return 2
	}
	var t api.TaskSummary
	if !callDaemon(ctx, p, stderr, api.MethodTasksResume, api.TaskIDParams{ID: pos[0]}, &t) {
		return 1
	}
	if t.State == api.StateOff {
		fmt.Fprintf(stdout, "%s: resumed, but off (enable it with `herdrcron enable %s`)\n", t.ID, t.ID)
	} else {
		fmt.Fprintf(stdout, "%s: resumed%s\n", t.ID, taskOutlook(t, time.Local))
	}
	return 0
}

func runGlobalPause(ctx context.Context, p paths.Paths, method, target string, stdout, stderr io.Writer) int {
	var gp api.GlobalPause
	if !callDaemon(ctx, p, stderr, method, api.TargetParams{Target: target}, &gp) {
		return 1
	}
	fmt.Fprintln(stdout, formatGlobalPause(gp))
	return 0
}

// formatGlobalPause describes the daemon's global pause in one line.
func formatGlobalPause(gp api.GlobalPause) string {
	switch {
	case gp.PausedAll:
		return "paused: all targets"
	case len(gp.PausedTargets) > 0:
		return "paused targets: " + strings.Join(gp.PausedTargets, ", ")
	}
	return "no global pause"
}

func runLogs(ctx context.Context, p paths.Paths, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("logs", flag.ContinueOnError)
	fl.SetOutput(stderr)
	follow := fl.Bool("f", false, "follow the log")
	task := fl.String("task", "", "only lines about this task id")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "herdrcron logs: unexpected argument %q\n", fl.Arg(0))
		return 2
	}
	err := logread.Print(ctx, p.Log(), stdout, logread.Options{TaskID: *task, Follow: *follow})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(stderr, "herdrcron:", err)
		} else {
			fmt.Fprintln(stderr, "herdrcron logs:", err)
		}
		return 1
	}
	return 0
}

// inTempDir reports whether path lies under os.TempDir (as given or with
// symlinks resolved, e.g. /var -> /private/var on macOS).
func inTempDir(path string) bool {
	dirs := []string{os.TempDir()}
	if d, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		dirs = append(dirs, d)
	}
	for _, d := range dirs {
		if rel, err := filepath.Rel(d, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func runInstallService(ctx context.Context, p paths.Paths, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprint(stdout, "usage: herdrcron install-service\n")
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "herdrcron install-service: unexpected argument %q\n", args[0])
		return 2
	}
	bin, err := executable()
	if err != nil {
		fmt.Fprintln(stderr, "herdrcron: cannot find own executable:", err)
		return 1
	}
	if inTempDir(bin) {
		fmt.Fprintf(stderr, "herdrcron install-service: %s is in a temporary directory (built by `go run`?); "+
			"install the binary first (e.g. `go install`) and run install-service from the installed copy\n", bin)
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "herdrcron install-service: cannot determine home directory:", err)
		return 1
	}
	username := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	_, err = service.Install(ctx, service.Options{
		GOOS:   runtime.GOOS,
		Binary: bin,
		Home:   home,
		Getenv: os.Getenv,
		Paths:  p,
		UID:    os.Getuid(),
		User:   username,
		Run:    serviceRunner,
		Out:    stdout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "herdrcron: %v\n", err)
		return 1
	}
	return 0
}
