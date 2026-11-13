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
// herdr (at most api.MaxOutputLines lines). A run still starting, with no
// pane yet, is live with its stored tail (usually empty) so the client
// keeps polling. A run that has ended returns the stored output tail.
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
	// No pane yet: the run is still starting. Say so, and stay live while
	// the runner tracks it, instead of looking like an ended run.
	if errors.Is(err, runner.ErrNoPane) {
		stored, gerr := d.store.GetRun(ctx, p.RunID)
		if errors.Is(gerr, store.ErrNotFound) {
			return nil, api.InvalidParams(fmt.Errorf("run %s not found", p.RunID))
		}
		if gerr != nil {
			return nil, gerr
		}
		return api.RunsOutputResult{RunID: stored.ID, Status: stored.Status, Live: d.runner.Live(p.RunID), Output: stored.OutputTail}, nil
	}
	// A run that ended between the lookup and the read has its tail stored;
	// a run still tracked (active or starting) gets the read error.
	if !errors.Is(err, runner.ErrNotActive) && d.runner.Live(p.RunID) {
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
	cfg := d.config()
	res := api.AttachCmdResult{Mode: cfg.AttachMode}
	if res.Mode == api.AttachPane && !hasAgent {
		res.Mode, res.Note = api.AttachFull, "the agent has exited; opening the full herdr UI"
	}
	res.Argv, err = d.attachArgv(cfg, run.Target, run.HerdrPane, res.Mode)
	if err != nil {
		return nil, api.InvalidParams(fmt.Errorf("run %s: %w", p.RunID, err))
	}
	return res, nil
}

// attachArgv builds the attach command for a pane on the named target, in
// mode api.AttachPane or api.AttachFull (docs/herdr-findings.md, Attach):
//
//   - local: herdr agent attach <pane>, or herdr;
//   - SSH: ssh -t <host> <shell> 'herdr agent attach <pane>', or
//     herdr --remote <host> with the local herdr.
//
// herdr is the target's herdr_path when set.
func (d *daemon) attachArgv(cfg config.Config, name, pane, mode string) ([]string, error) {
	t, ok := cfg.Targets[name]
	if !ok {
		return nil, fmt.Errorf("unknown target %q", name)
	}
	if name == config.LocalTarget {
		bin := herdrBin(t)
		if mode == api.AttachFull {
			return []string{bin}, nil
		}
		return []string{bin, "agent", "attach", pane}, nil
	}
	if mode == api.AttachFull {
		return []string{herdrBin(cfg.Targets[config.LocalTarget]), "--remote", t.Host}, nil
	}
	ssh := sshTarget(name, t, d.sshControlDir)
	return ssh.InteractiveArgv([]string{herdrBin(t), "agent", "attach", pane}), nil
}

// herdrBin is the herdr executable on target t.
func herdrBin(t config.Target) string {
	if t.HerdrPath != "" {
		return t.HerdrPath
	}
	return "herdr"
}
