package api

// Method names served by the daemon.
const (
	MethodStatus    = "status"
	MethodTasksList = "tasks.list"
)

// Task display states. Later tickets add the run-driven states.
const (
	StateInvalid = "invalid" // the task file failed validation; see TaskSummary.Error
	StateOff     = "off"     // enabled: false
	StateIdle    = "idle"    // valid, enabled, no active run
)

// StatusResult is the result of "status".
type StatusResult struct {
	Version       string         `json:"version"`
	StartedAt     string         `json:"started_at"` // RFC 3339
	UptimeSeconds int64          `json:"uptime_seconds"`
	Targets       []TargetStatus `json:"targets"`
	PausedAll     bool           `json:"paused_all"`
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
