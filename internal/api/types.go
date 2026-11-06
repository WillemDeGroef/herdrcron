package api

// Method names served by the daemon.
const (
	MethodStatus      = "status"
	MethodTasksList   = "tasks.list"
	MethodTasksRunNow = "tasks.run_now"
	MethodRunsList    = "runs.list"
	MethodRunsStop    = "runs.stop"

	MethodTasksSetEnabled = "tasks.set_enabled" // SetEnabledParams → TaskSummary
	MethodTasksPause      = "tasks.pause"       // PauseParams → TaskSummary
	MethodTasksResume     = "tasks.resume"      // TaskIDParams → TaskSummary
	MethodPauseAll        = "pause_all"         // TargetParams → GlobalPause
	MethodResumeAll       = "resume_all"        // TargetParams → GlobalPause

	MethodTasksGet    = "tasks.get"    // TaskIDParams → TaskDetail
	MethodTasksCreate = "tasks.create" // TaskWriteParams → TaskDetail
	MethodTasksUpdate = "tasks.update" // TaskWriteParams (ID, Version set) → TaskDetail
	MethodTasksDelete = "tasks.delete" // DeleteParams → DeleteResult
	MethodReload      = "reload"       // no params → ReloadResult
	MethodSubscribe   = "subscribe"    // no params → SubscribeResult, then Event notifications
)

// Event names: the "method" of the notifications a subscribed connection
// receives after the "subscribe" result.
const (
	EventTaskChanged   = "task.changed"   // params TaskChangedEvent
	EventRunChanged    = "run.changed"    // params RunChangedEvent
	EventTargetChanged = "target.changed" // params TargetChangedEvent
)

// CodeRunActive is the error code of "tasks.run_now" for a task with
// overlap skip and an active run, when force_parallel is not set. Clients
// ask the user to confirm a parallel run and call again with it set.
const CodeRunActive = -32001

// CodeValidation is the error code of "tasks.create" and "tasks.update" for
// a task that does not validate. Its data is a ValidationErrorData.
const CodeValidation = -32002

// CodeVersionMismatch is the error code of "tasks.update" when the version
// in the params is not the task's current version: the task or prompt file
// changed since the client read it. The client reads the task again.
const CodeVersionMismatch = -32003

// CodeRunActiveDelete is the error code of "tasks.delete" for a task with an
// active run, when stop_active is not set.
const CodeRunActiveDelete = -32004

// TaskRunsLimit is the number of runs "tasks.get" returns.
const TaskRunsLimit = 20

// DefaultRunsLimit is the runs.list limit when the params give none.
const DefaultRunsLimit = 50

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
	StateRunning = "running" // a run of the task is active (starting or running); wins over off, paused and idle. Invalid stays invalid.
	StateBlocked = "blocked" // an active run waits for the user; wins over running
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
	// Enabled is the task file's enabled; false for an invalid task whose
	// file could not be read. State shows running over off, so clients
	// read the on/off flag here.
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	// PausedUntil is the task file's paused_until: an RFC 3339 time or
	// "indefinite"; empty when the task itself is not paused. A task paused
	// only by a global pause has State paused and no PausedUntil.
	PausedUntil string `json:"paused_until,omitempty"`
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

// TaskDef is a task definition as the API carries it: the fields of a task
// file. Timeout is a Go duration string; PausedUntil is empty, an RFC 3339
// time or "indefinite". In "tasks.create" an empty PromptFile means
// prompts/<id>.md, and Enabled and the policy fields take their defaults
// only when the client sends them (the TUI form always sends every field).
type TaskDef struct {
	ID          string            `json:"id"`
	Name        string            `json:"name,omitempty"`
	Target      string            `json:"target"`
	Cwd         string            `json:"cwd,omitempty"`
	Schedule    string            `json:"schedule"`
	Timezone    string            `json:"timezone,omitempty"`
	Enabled     bool              `json:"enabled"`
	PausedUntil string            `json:"paused_until,omitempty"`
	Agent       AgentDef          `json:"agent"`
	PromptFile  string            `json:"prompt_file,omitempty"`
	Timeout     string            `json:"timeout"`
	Overlap     string            `json:"overlap"`
	Missed      string            `json:"missed"`
	OnDone      string            `json:"on_done"`
	OnBlocked   string            `json:"on_blocked"`
	Env         map[string]string `json:"env,omitempty"`
}

// AgentDef is the agent command of a TaskDef.
type AgentDef struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// TaskDetail is the result of "tasks.get", "tasks.create" and
// "tasks.update". For an invalid task Task holds what could be read (empty
// for a file that does not parse) and Summary.Error says why.
type TaskDetail struct {
	Summary TaskSummary `json:"summary"`
	Task    TaskDef     `json:"task"`
	Prompt  string      `json:"prompt"`
	// Version identifies the task file and prompt file contents; pass it
	// back in "tasks.update".
	Version string `json:"version"`
	// Runs are the task's last TaskRunsLimit runs, newest first.
	Runs []RunSummary `json:"runs"`
}

// TaskWriteParams are the params of "tasks.create" and "tasks.update". In
// "tasks.update" ID names the task to change and must equal Task.ID (a
// task cannot be renamed), and Version is the version the client read.
type TaskWriteParams struct {
	ID      string  `json:"id,omitempty"`
	Version string  `json:"version,omitempty"`
	Task    TaskDef `json:"task"`
	Prompt  string  `json:"prompt"`
}

// ValidationErrorData is the data of a CodeValidation error: the problems
// by task field, keyed by the JSON field name ("schedule", "agent.command",
// "prompt", ...). A field can have more than one problem.
type ValidationErrorData struct {
	Fields map[string][]string `json:"fields"`
}

// DeleteParams are the params of "tasks.delete". With StopActive set the
// task's active runs are stopped first; without it a task with an active
// run is refused with CodeRunActiveDelete.
type DeleteParams struct {
	ID         string `json:"id"`
	StopActive bool   `json:"stop_active,omitempty"`
}

// DeleteResult is the result of "tasks.delete".
type DeleteResult struct {
	ID string `json:"id"`
	// StoppedRuns are the runs stopped because of stop_active.
	StoppedRuns []RunSummary `json:"stopped_runs"`
}

// ReloadResult is the result of "reload".
type ReloadResult struct {
	Tasks []TaskSummary `json:"tasks"`
	// RestartRequired is set when config.yaml differs from the config the
	// daemon runs with. Tasks are always validated against the running
	// config; a changed config.yaml takes effect after a daemon restart.
	// A config.yaml that does not parse fails the call after the task
	// files have been re-read.
	RestartRequired bool `json:"restart_required,omitempty"`
}

// SubscribeResult is the result of "subscribe". After it the connection
// carries only Event notifications, until the client closes it. A client
// that falls too far behind is disconnected and should subscribe again
// and re-read the state.
type SubscribeResult struct {
	Subscribed bool `json:"subscribed"`
}

// TaskChangedEvent is the params of "task.changed": a task was created,
// edited (through the API or by hand), deleted, or its display state
// changed with a run. Task is nil when Deleted is set.
type TaskChangedEvent struct {
	ID      string       `json:"id"`
	Deleted bool         `json:"deleted,omitempty"`
	Task    *TaskSummary `json:"task,omitempty"`
	// Version is the task's new version; empty when Deleted.
	Version string `json:"version,omitempty"`
}

// RunChangedEvent is the params of "run.changed": a run was stored with a
// new status (started, status change, ended).
type RunChangedEvent struct {
	Run RunSummary `json:"run"`
}

// TargetChangedEvent is the params of "target.changed".
type TargetChangedEvent struct {
	Target TargetStatus `json:"target"`
}
