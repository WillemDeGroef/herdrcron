package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// lostCreatesSchema is created on every open, like queued_fires: it is not
// part of a schemaVersion bump.
const lostCreatesSchema = `
CREATE TABLE IF NOT EXISTS lost_creates (
	run_id    TEXT PRIMARY KEY,
	task_id   TEXT NOT NULL,
	target    TEXT NOT NULL,
	host      TEXT NOT NULL,
	workspace TEXT NOT NULL, -- "": the create was a workspace create.
	label     TEXT NOT NULL,
	known     TEXT NOT NULL  -- JSON array of the ids that existed before.
);
`

// LostCreate is a `herdr tab create` or `herdr workspace create` of a run
// that got no answer: herdr may have created the tab, and the run does not
// know its id. It holds what is needed to find that tab once the target
// answers again.
type LostCreate struct {
	RunID  string
	TaskID string
	Target string
	// Host is the target's host setting at the time, so a target name that
	// points at another machine after a restart is not asked.
	Host string
	// Workspace is the id of the workspace the tab was created in. Empty
	// when the create was a workspace create: herdr makes the tab with it.
	Workspace string
	// Label is the tab's label, or the workspace's for a workspace create.
	Label string
	// Known holds the ids that had the label's kind before the create: the
	// tabs of Workspace, or all workspaces. The created one is not in it.
	Known []string
}

// AddLostCreate stores c; a second one for the same run replaces the first.
func (s *Store) AddLostCreate(ctx context.Context, c LostCreate) error {
	known, err := json.Marshal(append([]string{}, c.Known...))
	if err != nil {
		return fmt.Errorf("add lost create of run %s: %w", c.RunID, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO lost_creates
		(run_id, task_id, target, host, workspace, label, known) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.RunID, c.TaskID, c.Target, c.Host, c.Workspace, c.Label, string(known))
	if err != nil {
		return fmt.Errorf("add lost create of run %s: %w", c.RunID, err)
	}
	return nil
}

// DeleteLostCreate removes the lost create of the run, if there is one.
func (s *Store) DeleteLostCreate(ctx context.Context, runID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM lost_creates WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete lost create of run %s: %w", runID, err)
	}
	return nil
}

// ListLostCreates returns the lost creates, oldest run first.
func (s *Store) ListLostCreates(ctx context.Context) ([]LostCreate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, task_id, target, host, workspace, label, known
		FROM lost_creates ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("list lost creates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LostCreate
	for rows.Next() {
		var c LostCreate
		var known string
		if err := rows.Scan(&c.RunID, &c.TaskID, &c.Target, &c.Host, &c.Workspace, &c.Label, &known); err != nil {
			return nil, fmt.Errorf("list lost creates: %w", err)
		}
		if err := json.Unmarshal([]byte(known), &c.Known); err != nil {
			return nil, fmt.Errorf("list lost creates: run %s: %w", c.RunID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list lost creates: %w", err)
	}
	return out, nil
}
