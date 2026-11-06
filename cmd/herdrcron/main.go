// Command herdrcron schedules recurring coding-agent runs in herdr panes.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: herdrcron <command>

commands:
  daemon   run the daemon in the foreground
  list     list tasks with target, schedule and state
`

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
