package api

// Method names served by the daemon.
const (
	MethodStatus      = "status"
	MethodTasksList   = "tasks.list"
	MethodTasksRunNow = "tasks.run_now"
	MethodRunsList    = "runs.list"
	MethodRunsStop    = "runs.stop"
)

// CodeRunActive is the error code of "tasks.run_now" for a task with
// overlap skip and an active run, when force_parallel is not set. Clients
// ask the user to confirm a parallel run and call again with it set.
const CodeRunActive = -32001

// DefaultRunsLimit is the runs.list limit when the params give none.
const DefaultRunsLimit = 50

// Task display states. Later tickets add the run-driven states.
const (
	StateInvalid = "invalid" // the task file failed validation; see TaskSummary.Error
	StateOff     = "off"     // enabled: false
	StatePaused  = "paused"  // paused_until is indefinite or in the future
	StateIdle    = "idle"    // valid, enabled, not paused, no active run
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

// RunNowParams are the params of "tasks.run_now". The run follows the
// task's overlap: with skip and an active run the call fails with
// CodeRunActive unless ForceParallel is set.
type RunNowParams struct {
	ID string `json:"id"`
	// ForceParallel starts the run in parallel even if the task has overlap
	// skip and an active run.
	ForceParallel bool `json:"force_parallel,omitempty"`
}

// RunNowResult is the result of "tasks.run_now": the started run (running,
// or ended as failed with the reason when it could not start), or, with
// Queued set, only TaskID: the fire waits for the task's active run
// (overlap queue) or for a free slot on the target (max_parallel).
type RunNowResult struct {
	RunSummary
	Queued bool `json:"queued,omitempty"`
}

// RunsStopParams are the params of "runs.stop". The result is the
// RunSummary of the stopped run.
type RunsStopParams struct {
	RunID string `json:"run_id"`
}

// RunsListParams are the params of "runs.list".
type RunsListParams struct {
	TaskID string `json:"task_id,omitempty"` // empty: all tasks
	Limit  int    `json:"limit,omitempty"`   // zero: DefaultRunsLimit
}

// RunsListResult is the result of "runs.list": runs newest first.
type RunsListResult struct {
	Runs []RunSummary `json:"runs"`
}

// RunSummary is one run. Times are RFC 3339 in UTC; empty when not set.
type RunSummary struct {
	ID             string `json:"id"`
	TaskID         string `json:"task_id"`
	Target         string `json:"target"`
	Trigger        string `json:"trigger"`
	Status         string `json:"status"`
	ScheduledAt    string `json:"scheduled_at,omitempty"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at,omitempty"`
	ExitReason     string `json:"exit_reason,omitempty"`
	HerdrWorkspace string `json:"herdr_workspace,omitempty"`
	HerdrTab       string `json:"herdr_tab,omitempty"`
	HerdrPane      string `json:"herdr_pane,omitempty"`
}
