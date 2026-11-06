package logging

import "log/slog"

// Attribute keys shared by every log line about a task or run.
const (
	KeyRunID  = "run_id"
	KeyTaskID = "task_id"
	KeyTarget = "target"
)

// ForRun returns log tagged with the run's run_id, task_id and target. Every
// line about a run must be logged through it, so `herdrcron logs` can filter
// and correlate run lines.
func ForRun(log *slog.Logger, runID, taskID, target string) *slog.Logger {
	return log.With(KeyRunID, runID, KeyTaskID, taskID, KeyTarget, target)
}
