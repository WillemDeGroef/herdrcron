package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/runner"
	"github.com/WillemDeGroef/herdrcron/internal/store"
)

// runsOutput returns an active run's pane text, ANSI kept, read live from
// herdr (at most api.MaxOutputLines lines); for a run that has ended (or has no pane yet) it returns the
// stored output tail.
func (d *daemon) runsOutput(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.RunsOutputParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.RunID == "" {
		return nil, api.InvalidParams(errors.New("run_id is required"))
	}
	if p.Lines < 0 {
		return nil, api.InvalidParams(errors.New("lines must not be negative"))
	}
	if p.Lines == 0 {
		p.Lines = api.DefaultOutputLines
	}
	p.Lines = min(p.Lines, api.MaxOutputLines)
	run, out, err := d.runner.ReadOutput(ctx, p.RunID, p.Lines)
	if err == nil {
		return api.RunsOutputResult{RunID: run.ID, Status: run.Status, Live: true, Output: out}, nil
	}
	// A run that ended between the lookup and the read has its tail stored;
	// a run still tracked (active or starting) gets the read error.
	if !errors.Is(err, runner.ErrNotActive) && !errors.Is(err, runner.ErrNoPane) && d.runner.Live(p.RunID) {
		return nil, fmt.Errorf("read output of run %s: %w", p.RunID, err)
	}
	stored, err := d.store.GetRun(ctx, p.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, api.InvalidParams(fmt.Errorf("run %s not found", p.RunID))
	}
	if err != nil {
		return nil, err
	}
	return api.RunsOutputResult{RunID: stored.ID, Status: stored.Status, Output: stored.OutputTail}, nil
}

// runsAttachCmd returns the argv the TUI runs to attach to an active run:
// its agent pane, or the full herdr UI for attach_mode full or when the
// pane has no agent left (herdr agent attach would fail).
func (d *daemon) runsAttachCmd(ctx context.Context, params json.RawMessage) (any, error) {
	var p api.RunIDParams
	if err := decodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.RunID == "" {
		return nil, api.InvalidParams(errors.New("run_id is required"))
	}
	run, hasAgent, err := d.runner.PaneHasAgent(ctx, p.RunID)
	switch {
	case errors.Is(err, runner.ErrNotActive):
		return nil, api.InvalidParams(fmt.Errorf("run %s is not active", p.RunID))
	case errors.Is(err, runner.ErrNoPane):
		return nil, api.InvalidParams(fmt.Errorf("run %s has no pane yet", p.RunID))
	case err != nil:
		return nil, fmt.Errorf("attach to run %s: %w", p.RunID, err)
	}
	res := api.AttachCmdResult{Mode: d.cfg.AttachMode}
	if res.Mode == api.AttachPane && !hasAgent {
		res.Mode, res.Note = api.AttachFull, "the agent has exited; opening the full herdr UI"
	}
	res.Argv, err = attachArgv(run.Target, d.cfg.Targets[run.Target], run.HerdrPane, res.Mode)
	if err != nil {
		return nil, api.InvalidParams(fmt.Errorf("run %s: %w", p.RunID, err))
	}
	return res, nil
}

// attachArgv builds the attach command for a pane on the named target, in
// mode api.AttachPane or api.AttachFull (docs/herdr-findings.md, Attach).
// Only the local target exists so far; SSH targets add the `ssh -t` and
// `herdr --remote` forms here.
func attachArgv(name string, t config.Target, pane, mode string) ([]string, error) {
	if name != config.LocalTarget {
		return nil, fmt.Errorf("attach to remote target %q comes with SSH targets", name)
	}
	bin := t.HerdrPath
	if bin == "" {
		bin = "herdr"
	}
	if mode == api.AttachFull {
		return []string{bin}, nil
	}
	return []string{bin, "agent", "attach", pane}, nil
}
