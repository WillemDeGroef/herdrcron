// Package notify runs the user's notify command (notify.command in
// config.yaml) for the events listed in notify.on.
package notify

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// DefaultTimeout is how long a notify command may run before it is killed.
const DefaultTimeout = 30 * time.Second

// maxOutput is how much of a failing command's output goes into the log.
const maxOutput = 500

// Notifier runs the notify command. Its methods are safe for concurrent
// use. A nil *Notifier notifies nothing.
type Notifier struct {
	command []string
	on      []string
	log     *slog.Logger
	// Timeout bounds each command; DefaultTimeout unless changed before
	// the first Notify.
	Timeout time.Duration

	wg sync.WaitGroup
}

// New returns a notifier for cfg. log is used when Notify gets no logger.
func New(cfg config.Notify, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{command: slices.Clone(cfg.Command), on: slices.Clone(cfg.On), log: log, Timeout: DefaultTimeout}
}

// Notify runs the command with message appended as its last argument, if
// event is listed in notify.on. It does not wait for the command: a slow
// or failing command never holds up a run. A failure is logged on log
// (the run's logger, so the line carries the run's ids), or on the
// notifier's logger when log is nil.
func (n *Notifier) Notify(event, message string, log *slog.Logger) {
	if n == nil || len(n.command) == 0 || !slices.Contains(n.on, event) {
		return
	}
	if log == nil {
		log = n.log
	}
	argv := append(slices.Clone(n.command), message)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.run(argv, event, log)
	}()
}

func (n *Notifier) run(argv []string, event string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), n.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// A command that leaves a child holding the output open must not keep
	// Wait from returning after the kill.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (killed after %s)", err, n.Timeout)
		}
		log.Warn("notify command failed", "event", event, "err", err.Error(), "output", truncate(strings.TrimSpace(out.String())))
		return
	}
	log.Info("notification sent", "event", event)
}

func truncate(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "…"
}

// Wait waits for the notify commands started so far.
func (n *Notifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

// RunMessage is the notification text for run in its current status.
func RunMessage(r store.Run) string {
	what := r.Status
	if r.ExitReason != "" {
		what += ": " + r.ExitReason
	}
	return fmt.Sprintf("herdrcron: %s %s (target %s, run %s)", r.TaskID, what, r.Target, r.ID)
}
