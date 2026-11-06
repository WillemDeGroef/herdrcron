// Package store keeps the herdrcron daemon state in SQLite: one row per run
// in table runs, per task scheduling state in table task_state, and the
// global pauses in table global_pause.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // Registers the "sqlite" database/sql driver.
)

// Trigger values: why a run started.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerCatchUp  = "catch_up"
)

// Run status values.
const (
	StatusStarting        = "starting"
	StatusRunning         = "running"
	StatusBlocked         = "blocked"
	StatusDone            = "done"
	StatusFailed          = "failed"
	StatusTimeout         = "timeout"
	StatusStopped         = "stopped"
	StatusLost            = "lost" // The pane no longer exists.
	StatusHostUnreachable = "host_unreachable"
)

// ErrNotFound is returned, wrapped, when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// schemaVersion is stored in PRAGMA user_version so later versions can
// migrate the schema.
const schemaVersion = 1

// schema creates the tables if they are missing.
const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id              TEXT PRIMARY KEY,
	task_id         TEXT NOT NULL,
	target          TEXT NOT NULL,
	trigger         TEXT NOT NULL,
	scheduled_at    TIMESTAMP,
	started_at      TIMESTAMP NOT NULL,
	ended_at        TIMESTAMP,
	status          TEXT NOT NULL,
	herdr_workspace TEXT,
	herdr_tab       TEXT,
	herdr_pane      TEXT,
	exit_reason     TEXT,
	output_tail     TEXT
);
CREATE INDEX IF NOT EXISTS runs_task_started ON runs (task_id, started_at);
CREATE TABLE IF NOT EXISTS task_state (
	task_id      TEXT PRIMARY KEY,
	last_run_id  TEXT,
	next_fire_at TIMESTAMP,
	last_fire_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS global_pause (
	target TEXT PRIMARY KEY -- "" means all targets.
);
`

// Store is the daemon's SQLite database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Run is one execution of a task.
type Run struct {
	ID             string // ULID, see NewRunID.
	TaskID         string
	Target         string    // Target name at start time.
	Trigger        string    // One of the Trigger values.
	ScheduledAt    time.Time // Planned fire time; zero for manual runs.
	StartedAt      time.Time
	EndedAt        time.Time // Zero while the run is active.
	Status         string    // One of the Status values.
	HerdrWorkspace string
	HerdrTab       string
	HerdrPane      string
	ExitReason     string // Short human text, for example "timeout after 2h".
	OutputTail     string // Last lines of pane output at the end of the run.
}

// Open opens the database at path, creating the file and tables if missing.
// The parent directory must exist.
func Open(path string) (*Store, error) {
	// Pragmas go in the DSN so every connection database/sql opens gets them.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: SQLite has one writer anyway, and this avoids
	// SQLITE_BUSY between connections of the same process.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// migrate creates the tables, and refuses a database written by a newer
// schema.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this herdrcron (%d)", version, schemaVersion)
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	_, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	return err
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// NewRunID returns a new ULID with timestamp t. It is safe for concurrent use.
func NewRunID(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

const runColumns = `id, task_id, target, trigger, scheduled_at, started_at,
	ended_at, status, herdr_workspace, herdr_tab, herdr_pane, exit_reason,
	output_tail`

// InsertRun adds a new run. r.ID must be set.
func (s *Store) InsertRun(ctx context.Context, r Run) error {
	if r.ID == "" {
		return errors.New("insert run: empty id")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (`+runColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.TaskID, r.Target, r.Trigger, timeValue(r.ScheduledAt),
		timeValue(r.StartedAt), timeValue(r.EndedAt), r.Status,
		r.HerdrWorkspace, r.HerdrTab, r.HerdrPane, r.ExitReason, r.OutputTail)
	if err != nil {
		return fmt.Errorf("insert run %s: %w", r.ID, err)
	}
	return nil
}

// GetRun returns the run with the given id. The error wraps ErrNotFound if
// there is none.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run %s: %w", id, err)
	}
	return r, nil
}

// scanner is a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanRun reads one row selected with runColumns.
func scanRun(sc scanner) (Run, error) {
	var (
		r                         Run
		scheduled, started, ended sql.NullString
		workspace, tab, pane      sql.NullString
		exitReason, outputTail    sql.NullString
	)
	err := sc.Scan(&r.ID, &r.TaskID, &r.Target, &r.Trigger, &scheduled,
		&started, &ended, &r.Status, &workspace, &tab, &pane, &exitReason,
		&outputTail)
	if err != nil {
		return Run{}, err
	}
	r.HerdrWorkspace, r.HerdrTab, r.HerdrPane = workspace.String, tab.String, pane.String
	r.ExitReason, r.OutputTail = exitReason.String, outputTail.String
	if r.ScheduledAt, err = parseTime(scheduled); err != nil {
		return Run{}, err
	}
	if r.StartedAt, err = parseTime(started); err != nil {
		return Run{}, err
	}
	if r.EndedAt, err = parseTime(ended); err != nil {
		return Run{}, err
	}
	return r, nil
}

// timeValue converts t to its stored form: UTC RFC 3339 text with
// nanoseconds, or NULL for the zero time. The text sorts in time order.
func timeValue(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

// formatTime renders t as stored. The fixed nine digit fraction keeps
// lexical order equal to time order.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// timeLayout is RFC 3339 with a fixed width fraction. time.RFC3339Nano
// drops trailing zeros, which would break lexical ordering.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// parseTime reads a stored time; NULL gives the zero time.
func parseTime(v sql.NullString) (time.Time, error) {
	if !v.Valid {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", v.String, err)
	}
	return t.UTC(), nil
}

// UpdateRun stores every field of r except ID and TaskID, for the run with
// id r.ID. The error wraps ErrNotFound if there is no such run.
func (s *Store) UpdateRun(ctx context.Context, r Run) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET
		target = ?, trigger = ?, scheduled_at = ?, started_at = ?,
		ended_at = ?, status = ?, herdr_workspace = ?, herdr_tab = ?,
		herdr_pane = ?, exit_reason = ?, output_tail = ?
		WHERE id = ?`,
		r.Target, r.Trigger, timeValue(r.ScheduledAt), timeValue(r.StartedAt),
		timeValue(r.EndedAt), r.Status, r.HerdrWorkspace, r.HerdrTab,
		r.HerdrPane, r.ExitReason, r.OutputTail, r.ID)
	if err != nil {
		return fmt.Errorf("update run %s: %w", r.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update run %s: %w", r.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("update run %s: %w", r.ID, ErrNotFound)
	}
	return nil
}

// ListRuns returns runs newest first. An empty taskID means all tasks. A
// limit of zero or less means no limit.
func (s *Store) ListRuns(ctx context.Context, taskID string, limit int) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM runs`
	var args []any
	if taskID != "" {
		query += ` WHERE task_id = ?`
		args = append(args, taskID)
	}
	query += ` ORDER BY started_at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	runs, err := s.queryRuns(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}

// ListOpenRuns returns every run that has not ended (no ended_at), oldest
// first. The daemon recovers them at start.
func (s *Store) ListOpenRuns(ctx context.Context) ([]Run, error) {
	runs, err := s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE ended_at IS NULL ORDER BY started_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list open runs: %w", err)
	}
	return runs, nil
}

// queryRuns runs a query that selects runColumns.
func (s *Store) queryRuns(ctx context.Context, query string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// LastRun returns the newest run of the task by start time. ok is false if
// the task has no runs.
func (s *Store) LastRun(ctx context.Context, taskID string) (r Run, ok bool, err error) {
	if taskID == "" {
		return Run{}, false, errors.New("last run: empty task id")
	}
	runs, err := s.ListRuns(ctx, taskID, 1)
	if err != nil {
		return Run{}, false, fmt.Errorf("last run of %s: %w", taskID, err)
	}
	if len(runs) == 0 {
		return Run{}, false, nil
	}
	return runs[0], true, nil
}

// TaskState is the scheduling state the daemon keeps per task.
type TaskState struct {
	TaskID     string
	LastRunID  string    // Empty if the task never ran.
	NextFireAt time.Time // Zero if no fire is planned.
	LastFireAt time.Time // Zero if the task never fired.
}

// PutTaskState inserts or replaces the state of st.TaskID.
func (s *Store) PutTaskState(ctx context.Context, st TaskState) error {
	if st.TaskID == "" {
		return errors.New("put task state: empty task id")
	}
	lastRun := sql.NullString{String: st.LastRunID, Valid: st.LastRunID != ""}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_state
		(task_id, last_run_id, next_fire_at, last_fire_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET
		last_run_id = excluded.last_run_id,
		next_fire_at = excluded.next_fire_at,
		last_fire_at = excluded.last_fire_at`,
		st.TaskID, lastRun, timeValue(st.NextFireAt), timeValue(st.LastFireAt))
	if err != nil {
		return fmt.Errorf("put task state %s: %w", st.TaskID, err)
	}
	return nil
}

// SetNextFire sets the task's next_fire_at (zero: none) and keeps the rest
// of its state.
func (s *Store) SetNextFire(ctx context.Context, taskID string, next time.Time) error {
	if taskID == "" {
		return errors.New("set next fire: empty task id")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_state (task_id, next_fire_at) VALUES (?, ?)
		ON CONFLICT (task_id) DO UPDATE SET next_fire_at = excluded.next_fire_at`,
		taskID, timeValue(next))
	if err != nil {
		return fmt.Errorf("set next fire %s: %w", taskID, err)
	}
	return nil
}

// RecordFire sets the task's last_run_id and last_fire_at and keeps its
// next_fire_at.
func (s *Store) RecordFire(ctx context.Context, taskID, runID string, firedAt time.Time) error {
	if taskID == "" {
		return errors.New("record fire: empty task id")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_state (task_id, last_run_id, last_fire_at) VALUES (?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET
		last_run_id = excluded.last_run_id, last_fire_at = excluded.last_fire_at`,
		taskID, runID, timeValue(firedAt))
	if err != nil {
		return fmt.Errorf("record fire %s: %w", taskID, err)
	}
	return nil
}

// GetTaskState returns the state of the task. ok is false if none is stored.
func (s *Store) GetTaskState(ctx context.Context, taskID string) (st TaskState, ok bool, err error) {
	var lastRun, next, last sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT task_id, last_run_id, next_fire_at,
		last_fire_at FROM task_state WHERE task_id = ?`, taskID).
		Scan(&st.TaskID, &lastRun, &next, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskState{}, false, nil
	}
	if err != nil {
		return TaskState{}, false, fmt.Errorf("get task state %s: %w", taskID, err)
	}
	st.LastRunID = lastRun.String
	if st.NextFireAt, err = parseTime(next); err != nil {
		return TaskState{}, false, fmt.Errorf("get task state %s: %w", taskID, err)
	}
	if st.LastFireAt, err = parseTime(last); err != nil {
		return TaskState{}, false, fmt.Errorf("get task state %s: %w", taskID, err)
	}
	return st, true, nil
}

// DeleteRunsBefore deletes ended runs that started before cutoff and
// returns how many it deleted. Active runs (no end time) are always kept.
// Task states that pointed at a deleted run lose their last run id.
func (s *Store) DeleteRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("delete runs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM runs
		WHERE ended_at IS NOT NULL AND started_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("delete runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete runs: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE task_state SET last_run_id = NULL
		WHERE last_run_id IS NOT NULL
		AND last_run_id NOT IN (SELECT id FROM runs)`)
	if err != nil {
		return 0, fmt.Errorf("delete runs: clear last run ids: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("delete runs: %w", err)
	}
	return n, nil
}

// SetGlobalPause pauses (paused true) or resumes every task on target; ""
// means all targets. Resuming "" clears every global pause, the per-target
// ones included.
func (s *Store) SetGlobalPause(ctx context.Context, target string, paused bool) error {
	var err error
	if paused {
		_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO global_pause (target) VALUES (?)`, target)
	} else {
		// Resuming "" deletes every row, the per-target pauses included.
		_, err = s.db.ExecContext(ctx, `DELETE FROM global_pause WHERE ? = '' OR target = ?`, target, target)
	}
	if err != nil {
		return fmt.Errorf("set global pause %q: %w", target, err)
	}
	return nil
}

// GlobalPause returns whether all targets are paused and the paused target
// names, sorted.
func (s *Store) GlobalPause(ctx context.Context) (all bool, targets []string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target FROM global_pause ORDER BY target`)
	if err != nil {
		return false, nil, fmt.Errorf("global pause: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return false, nil, fmt.Errorf("global pause: %w", err)
		}
		if target == "" {
			all = true
		} else {
			targets = append(targets, target)
		}
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("global pause: %w", err)
	}
	return all, targets, nil
}
