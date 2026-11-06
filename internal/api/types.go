package api

// Method names served by the daemon.
const (
	MethodStatus    = "status"
	MethodTasksList = "tasks.list"

	MethodTasksSetEnabled = "tasks.set_enabled" // SetEnabledParams → TaskSummary
	MethodTasksPause      = "tasks.pause"       // PauseParams → TaskSummary
	MethodTasksResume     = "tasks.resume"      // TaskIDParams → TaskSummary
	MethodPauseAll        = "pause_all"         // TargetParams → GlobalPause
	MethodResumeAll       = "resume_all"        // TargetParams → GlobalPause
)

// PauseIndefinite is the paused_until value of a pause without an end.
const PauseIndefinite = "indefinite"

// TaskIDParams names one task.
type TaskIDParams struct {
	ID string `json:"id"`
}

// SetEnabledParams are the params of "tasks.set_enabled".
type SetEnabledParams struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// PauseParams are the params of "tasks.pause". Until is an RFC 3339 time in
// the future or "indefinite".
type PauseParams struct {
	ID    string `json:"id"`
	Until string `json:"until"`
}

// TargetParams are the params of "pause_all" and "resume_all". An empty
// Target means every target.
type TargetParams struct {
	Target string `json:"target,omitempty"`
}

// GlobalPause is the daemon's global pause state: the result of "pause_all"
// and "resume_all", and part of "status". It is kept in the daemon state,
// never in task files.
type GlobalPause struct {
	// PausedAll pauses every task on every target.
	PausedAll bool `json:"paused_all"`
	// PausedTargets pauses every task on these targets, sorted by name.
	PausedTargets []string `json:"paused_targets"`
}

// Task display states. Later tickets add the run-driven states.
const (
	StateInvalid = "invalid" // the task file failed validation; see TaskSummary.Error
	StateOff     = "off"     // enabled: false
	StatePaused  = "paused"  // paused_until is indefinite or in the future, or a global pause covers the task
	StateIdle    = "idle"    // valid, enabled, not paused, no active run
)

// StatusResult is the result of "status".
type StatusResult struct {
	Version       string         `json:"version"`
	StartedAt     string         `json:"started_at"` // RFC 3339
	UptimeSeconds int64          `json:"uptime_seconds"`
	Targets       []TargetStatus `json:"targets"`
	GlobalPause
}

// TargetStatus is one target in StatusResult.
type TargetStatus struct {
	Name string `json:"name"`
	// Reachability is "unknown" until target checks exist.
	Reachability string `json:"reachability"`
}

// TasksListResult is the result of "tasks.list".
type TasksListResult struct {
	Tasks []TaskSummary `json:"tasks"`
}

// TaskSummary is one task in TasksListResult. For an invalid task the
// fields that could not be read are empty.
type TaskSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Target   string `json:"target,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	// PausedUntil is the task file's paused_until: an RFC 3339 time or
	// "indefinite"; empty when the task itself is not paused. A task paused
	// only by a global pause has State paused and no PausedUntil.
	PausedUntil string `json:"paused_until,omitempty"`
	// NextFireAt is the next fire time (RFC 3339, UTC); empty for off and
	// invalid tasks.
	NextFireAt string      `json:"next_fire_at,omitempty"`
	LastRun    *RunSummary `json:"last_run,omitempty"`
}

// RunSummary is one run. Times are RFC 3339 in UTC; empty when not set.
type RunSummary struct {
	ID          string `json:"id"`
	Trigger     string `json:"trigger"`
	Status      string `json:"status"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at,omitempty"`
	ExitReason  string `json:"exit_reason,omitempty"`
}
