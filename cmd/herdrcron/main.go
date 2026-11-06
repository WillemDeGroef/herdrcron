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
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: herdrcron <command>

commands:
  daemon            run the daemon in the foreground
  list              list tasks with target, schedule and state
  logs [-f] [--task <id>]
                    print the daemon log; -f follows it, --task filters by task
  install-service   install and start the daemon as a systemd user service
                    (Linux) or launchd agent (macOS)
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
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	p, err := paths.FromEnv()
	if err != nil {
		fmt.Fprintln(stderr, "herdrcron:", err)
		return 1
	}
	switch args[0] {
	case "daemon":
		return runDaemon(ctx, p, stderr)
	case "list":
		return runList(ctx, p, stdout, stderr)
	case "logs":
		return runLogs(ctx, p, args[1:], stdout, stderr)
	case "install-service":
		return runInstallService(ctx, p, args[1:], stdout, stderr)
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

func runList(ctx context.Context, p paths.Paths, stdout, stderr io.Writer) int {
	c, err := api.Dial(p.Socket())
	if err != nil {
		if errors.Is(err, api.ErrDaemonNotRunning) {
			fmt.Fprintf(stderr, "herdrcron: daemon is not running (no daemon on %s); start it with `herdrcron daemon`\n", p.Socket())
		} else {
			fmt.Fprintln(stderr, "herdrcron:", err)
		}
		return 1
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var res api.TasksListResult
	if err := c.Call(ctx, api.MethodTasksList, nil, &res); err != nil {
		fmt.Fprintln(stderr, "herdrcron: tasks.list:", err)
		return 1
	}
	if len(res.Tasks) == 0 {
		fmt.Fprintf(stdout, "no tasks in %s\n", p.TasksDir())
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTARGET\tSCHEDULE\tSTATE")
	for _, t := range res.Tasks {
		state := t.State
		if t.Error != "" {
			state += ": " + t.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, orDash(t.Target), orDash(t.Schedule), state)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(stderr, "herdrcron:", err)
		return 1
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
