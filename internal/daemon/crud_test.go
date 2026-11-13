package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/clock"
	"github.com/WillemDeGroef/herdrcron/internal/daemon"
	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

// newTaskDef is a valid definition of a new task "fresh" on local.
func newTaskDef() api.TaskDef {
	return api.TaskDef{
		ID: "fresh", Name: "Fresh task", Target: "local", Cwd: "/tmp/fresh", Schedule: "0 7 * * 1",
		Enabled: true, Agent: api.AgentDef{Command: "claude", Args: []string{"--permission-mode", "acceptEdits"}},
		Timeout: "2h", Overlap: "skip", Missed: "run_once", OnDone: "keep_tab", OnBlocked: "notify",
		Env: map[string]string{"FOO": "007"},
	}
}

const trickyPrompt = "Say \"hi\" and 'bye'.\n$HOME `date`\nend\n"

func rpcErr(t *testing.T, err error, code int) *api.Error {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want code %d", err, code)
	}
	return e
}

func fieldErrors(t *testing.T, e *api.Error) map[string][]string {
	t.Helper()
	data, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatal(err)
	}
	var v api.ValidationErrorData
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("error data %s: %v", data, err)
	}
	return v.Fields
}

func TestTasksGetReturnsDefinitionPromptVersionAndRuns(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	for range 3 {
		if _, err := runNow(c, "nightly"); err != nil {
			t.Fatal(err)
		}
	}
	got := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})
	if got.Task.Name != "Nightly deps" || got.Task.Timeout != "1h" || got.Task.Agent.Command != "claude" ||
		got.Task.Env["API_TOKEN"] != secretEnv || got.Task.PromptFile != "prompts/nightly.md" {
		t.Errorf("task = %+v", got.Task)
	}
	if got.Prompt != secretPrompt {
		t.Errorf("prompt = %q, want %q", got.Prompt, secretPrompt)
	}
	if got.Version == "" || got.Summary.ID != "nightly" {
		t.Errorf("version %q, summary %+v", got.Version, got.Summary)
	}
	if len(got.Runs) != 3 || got.Runs[0].TaskID != "nightly" {
		t.Errorf("runs = %+v, want 3 runs of nightly", got.Runs)
	}
}

func TestTasksGetLimitsRunsToTwenty(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	for range api.TaskRunsLimit + 2 {
		if _, err := runNow(c, "nightly"); err != nil {
			t.Fatal(err)
		}
	}
	got := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})
	if len(got.Runs) != api.TaskRunsLimit {
		t.Errorf("%d runs, want %d", len(got.Runs), api.TaskRunsLimit)
	}
}

func TestTasksGetShowsAnInvalidTaskWithItsError(t *testing.T) {
	c := start(t, setup(t))
	got := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "broken"})
	if got.Summary.State != api.StateInvalid || !strings.Contains(got.Summary.Error, "mars") || got.Task.Target != "mars" {
		t.Errorf("broken = %+v", got)
	}
	err := c.Call(context.Background(), api.MethodTasksGet, api.TaskIDParams{ID: "nope"}, nil)
	_ = rpcErr(t, err, api.CodeInvalidParams)
}

func TestTasksCreateWritesTaskAndPromptFiles(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	got := call[api.TaskDetail](t, c, api.MethodTasksCreate, api.TaskWriteParams{Task: newTaskDef(), Prompt: trickyPrompt})
	if got.Summary.State != api.StateIdle || got.Summary.NextFireAt != "2026-11-09T07:00:00Z" {
		t.Errorf("summary = %+v, want idle, next Monday 07:00", got.Summary)
	}
	if got.Task.PromptFile != "prompts/fresh.md" || got.Version == "" || got.Prompt != trickyPrompt {
		t.Errorf("created = %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(p.ConfigDir, "prompts", "fresh.md"))
	if err != nil || string(data) != trickyPrompt {
		t.Errorf("prompt file = %q, %v", data, err)
	}
	f := taskFile(t, p, "fresh")
	for _, want := range []string{"id: fresh", "0 7 * * 1", "timeout: 2h", "FOO:"} {
		if !strings.Contains(f, want) {
			t.Errorf("task file lacks %q:\n%s", want, f)
		}
	}
	if _, ok := listByID(t, c)["fresh"]; !ok {
		t.Error("tasks.list lacks the new task")
	}
	// The same id again is refused, per field.
	err = c.Call(context.Background(), api.MethodTasksCreate, api.TaskWriteParams{Task: newTaskDef(), Prompt: "x"}, nil)
	if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["id"]) == 0 {
		t.Errorf("duplicate id field errors = %v", f)
	}
}

func TestTasksCreateReturnsValidationErrorsPerField(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	def := newTaskDef()
	def.ID, def.Target, def.Schedule, def.Timeout, def.Overlap = "Bad Id", "mars", "every tuesday", "soon", "maybe"
	err := c.Call(context.Background(), api.MethodTasksCreate, api.TaskWriteParams{Task: def, Prompt: "x"}, nil)
	f := fieldErrors(t, rpcErr(t, err, api.CodeValidation))
	for _, field := range []string{"id", "target", "schedule", "timeout", "overlap"} {
		if len(f[field]) == 0 {
			t.Errorf("no error for %s in %v", field, f)
		}
	}
	if entries, _ := os.ReadDir(p.TasksDir()); len(entries) != 3 {
		t.Errorf("tasks dir has %d files after a refused create, want 3", len(entries))
	}
}

func TestTasksUpdateChangesTheFilesAndRefusesAStaleVersion(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	cur := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})
	def := cur.Task
	def.Schedule, def.Name = "0 3 * * *", "Renamed"
	got := call[api.TaskDetail](t, c, api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "nightly", Version: cur.Version, Task: def, Prompt: "new prompt"})
	if got.Summary.Name != "Renamed" || got.Summary.NextFireAt != "2026-11-07T03:00:00Z" || got.Version == cur.Version {
		t.Errorf("updated = %+v", got)
	}
	if data, _ := os.ReadFile(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); string(data) != "new prompt" {
		t.Errorf("prompt file = %q", data)
	}
	if f := taskFile(t, p, "nightly"); !strings.Contains(f, "0 3 * * *") || !strings.Contains(f, "API_TOKEN") {
		t.Errorf("task file:\n%s", f)
	}

	// The old version is stale now.
	err := c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "nightly", Version: cur.Version, Task: def, Prompt: "other"}, nil)
	_ = rpcErr(t, err, api.CodeVersionMismatch)

	// A hand edit makes the version stale too.
	write(t, filepath.Join(p.TasksDir(), "nightly.yaml"), taskFile(t, p, "nightly")+"# hand edit\n")
	err = c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "nightly", Version: got.Version, Task: def, Prompt: "other"}, nil)
	_ = rpcErr(t, err, api.CodeVersionMismatch)

	// An invalid update is refused per field and leaves the files alone.
	fresh := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})
	def.Schedule = "nonsense"
	err = c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "nightly", Version: fresh.Version, Task: def, Prompt: "other"}, nil)
	if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["schedule"]) == 0 {
		t.Errorf("field errors = %v", f)
	}
	if f := taskFile(t, p, "nightly"); !strings.Contains(f, "0 3 * * *") {
		t.Errorf("refused update changed the file:\n%s", f)
	}
}

// movePrompt updates a task so that it points at another prompt file.
func movePrompt(t *testing.T, c *api.Client, id, promptFile, prompt string) api.TaskDetail {
	t.Helper()
	cur := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: id})
	def := cur.Task
	def.PromptFile = promptFile
	return call[api.TaskDetail](t, c, api.MethodTasksUpdate,
		api.TaskWriteParams{ID: id, Version: cur.Version, Task: def, Prompt: prompt})
}

func TestTasksUpdateRemovesThePromptFileItStopsUsing(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	old := filepath.Join(p.ConfigDir, "prompts", "nightly.md")

	// A refused update leaves the old prompt file alone.
	cur := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})
	def := cur.Task
	def.PromptFile, def.Schedule = "prompts/moved.md", "nonsense"
	err := c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "nightly", Version: cur.Version, Task: def, Prompt: "moved prompt"}, nil)
	_ = rpcErr(t, err, api.CodeValidation)
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("refused update removed the prompt file: %v", err)
	}

	got := movePrompt(t, c, "nightly", "prompts/moved.md", "moved prompt")
	if got.Task.PromptFile != "prompts/moved.md" || got.Prompt != "moved prompt" {
		t.Errorf("updated = %+v", got)
	}
	if data := readFile(t, filepath.Join(p.ConfigDir, "prompts", "moved.md")); data != "moved prompt" {
		t.Errorf("new prompt file = %q", data)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old prompt file still exists: %v", err)
	}
}

func TestTasksUpdateKeepsAnOldPromptFileAnotherTaskUses(t *testing.T) {
	p := setupUTC(t)
	write(t, filepath.Join(p.TasksDir(), "twin.yaml"), `id: twin
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/nightly.md
timeout: 1h
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	before := readFile(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md"))
	movePrompt(t, c, "nightly", "prompts/moved.md", "moved prompt")
	if data, err := os.ReadFile(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); err != nil || string(data) != before {
		t.Errorf("shared prompt file = %q, %v; want it unchanged", data, err)
	}
	if twin := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "twin"}); twin.Summary.State != api.StateIdle {
		t.Errorf("twin = %+v", twin.Summary)
	}
}

func TestTasksUpdateKeepsThePromptFileWhenTheNewPathIsTheSameFile(t *testing.T) {
	p := setupUTC(t)
	if err := os.Symlink("prompts", filepath.Join(p.ConfigDir, "alias")); err != nil {
		t.Fatal(err)
	}
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	got := movePrompt(t, c, "nightly", "alias/nightly.md", "same file")
	if got.Prompt != "same file" || got.Summary.State != api.StateIdle {
		t.Errorf("updated = %+v", got)
	}
	if data := readFile(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md")); data != "same file" {
		t.Errorf("prompt file = %q", data)
	}
}

func TestTasksUpdateKeepsAnOldPromptFileOutsideTheConfigDirectory(t *testing.T) {
	p := setupUTC(t)
	outside := filepath.Join(t.TempDir(), "mine.md")
	write(t, outside, "hand written")
	write(t, filepath.Join(p.TasksDir(), "roamer.yaml"), `id: roamer
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: `+outside+`
timeout: 1h
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	movePrompt(t, c, "roamer", "prompts/roamer.md", "moved inside")
	if data := readFile(t, outside); data != "hand written" {
		t.Errorf("prompt file outside the config directory = %q", data)
	}
}

func TestTasksUpdateKeepsAnOldPromptFileBehindALinkOutOfTheConfigDirectory(t *testing.T) {
	p := setupUTC(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "roamer.md"), "hand written")
	if err := os.Symlink(outside, filepath.Join(p.ConfigDir, "alias")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.TasksDir(), "roamer.yaml"), `id: roamer
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: alias/roamer.md
timeout: 1h
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	movePrompt(t, c, "roamer", "prompts/roamer.md", "moved inside")
	if data := readFile(t, filepath.Join(outside, "roamer.md")); data != "hand written" {
		t.Errorf("prompt file outside the config directory = %q", data)
	}
}

func TestTasksUpdateKeepsTheOldPromptFileWhileATaskFileDoesNotParse(t *testing.T) {
	p := setupUTC(t)
	// Its prompt_file cannot be read, so it may be any file.
	write(t, filepath.Join(p.TasksDir(), "garbled.yaml"), "id: garbled\nprompt_file: prompts/nightly.md\nschedule: [\n")
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	movePrompt(t, c, "nightly", "prompts/moved.md", "moved prompt")
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); err != nil {
		t.Errorf("prompt file removed while garbled.yaml may use it: %v", err)
	}
}

func TestTasksUpdateRemovesTheOldPromptFileWhenAnotherTaskHasATypeError(t *testing.T) {
	p := setupUTC(t)
	// timeout does not parse, but prompt_file does: it names another file.
	write(t, filepath.Join(p.ConfigDir, "prompts", "other.md"), "other")
	write(t, filepath.Join(p.TasksDir(), "twin.yaml"), `id: twin
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/other.md
timeout: nope
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	movePrompt(t, c, "nightly", "prompts/moved.md", "moved prompt")
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old prompt file still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "other.md")); err != nil {
		t.Errorf("twin's prompt file = %v", err)
	}
}

func TestTasksUpdateKeepsThePromptFileATypeErrorStillNames(t *testing.T) {
	p := setupUTC(t)
	write(t, filepath.Join(p.TasksDir(), "twin.yaml"), `id: twin
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/nightly.md
timeout: nope
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	movePrompt(t, c, "nightly", "prompts/moved.md", "moved prompt")
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); err != nil {
		t.Errorf("prompt file removed while twin.yaml names it: %v", err)
	}
}

func TestTasksUpdateKeepsAPromptFileAHandEditStillNames(t *testing.T) {
	p := setupUTC(t)
	write(t, filepath.Join(p.ConfigDir, "prompts", "other.md"), "other")
	write(t, filepath.Join(p.TasksDir(), "twin.yaml"), `id: twin
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/other.md
timeout: 1h
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	// Loaded as prompts/other.md. Rewrite the file and update nightly
	// before the watcher reloads twin; do not tasks.get twin, that reloads it.
	write(t, filepath.Join(p.TasksDir(), "twin.yaml"), `id: twin
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/nightly.md
timeout: 1h
`)
	movePrompt(t, c, "nightly", "prompts/moved.md", "moved prompt")
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); err != nil {
		t.Errorf("prompt file removed while twin.yaml still names it: %v", err)
	}
}

func TestTasksUpdateRemovesTheOldPromptFileOfAnInvalidTask(t *testing.T) {
	p := setupUTC(t)
	write(t, filepath.Join(p.ConfigDir, "prompts", "stray.md"), "stray")
	write(t, filepath.Join(p.TasksDir(), "stray.yaml"), `id: stray
target: nowhere
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/stray.md
timeout: 1h
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	cur := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "stray"})
	def := cur.Task
	def.Target, def.PromptFile = "local", "prompts/moved.md"
	got := call[api.TaskDetail](t, c, api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "stray", Version: cur.Version, Task: def, Prompt: cur.Prompt})
	if got.Summary.State != api.StateIdle {
		t.Errorf("fixed task = %+v", got.Summary)
	}
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "stray.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old prompt file still exists: %v", err)
	}
}

func TestTasksUpdateCannotRenameATask(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	cur := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})
	def := cur.Task
	def.ID = "other"
	err := c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "nightly", Version: cur.Version, Task: def, Prompt: cur.Prompt}, nil)
	if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["id"]) == 0 {
		t.Errorf("field errors = %v", f)
	}
}

func TestTasksUpdateFixesAnInvalidTask(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	cur := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "broken"})
	def := cur.Task
	def.Target = "local"
	got := call[api.TaskDetail](t, c, api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "broken", Version: cur.Version, Task: def, Prompt: "now valid"})
	if got.Summary.State != api.StateIdle {
		t.Errorf("fixed task = %+v", got.Summary)
	}
}

func TestTasksDeleteRemovesTheFilesAndKeepsRunHistory(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	if _, err := runNow(c, "nightly"); err != nil {
		t.Fatal(err)
	}
	res := call[api.DeleteResult](t, c, api.MethodTasksDelete, api.DeleteParams{ID: "nightly"})
	if res.ID != "nightly" || len(res.StoppedRuns) != 0 {
		t.Errorf("delete = %+v", res)
	}
	for _, f := range []string{filepath.Join(p.TasksDir(), "nightly.yaml"), filepath.Join(p.ConfigDir, "prompts", "nightly.md")} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists: %v", f, err)
		}
	}
	if _, ok := listByID(t, c)["nightly"]; ok {
		t.Error("tasks.list still shows the deleted task")
	}
	runs := call[api.RunsListResult](t, c, api.MethodRunsList, api.RunsListParams{TaskID: "nightly"})
	if len(runs.Runs) != 1 {
		t.Errorf("run history after delete = %+v, want the one run", runs.Runs)
	}
	err := c.Call(context.Background(), api.MethodTasksDelete, api.DeleteParams{ID: "nightly"}, nil)
	_ = rpcErr(t, err, api.CodeInvalidParams)
}

func TestTasksDeleteKeepsAPromptFileAnotherTaskUses(t *testing.T) {
	p := setupUTC(t)
	write(t, filepath.Join(p.TasksDir(), "twin.yaml"), `id: twin
target: local
schedule: "@daily"
agent: {command: claude}
prompt_file: prompts/nightly.md
timeout: 1h
`)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	call[api.DeleteResult](t, c, api.MethodTasksDelete, api.DeleteParams{ID: "nightly"})
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "prompts", "nightly.md")); err != nil {
		t.Errorf("shared prompt file removed: %v", err)
	}
}

func TestTasksDeleteRefusesWhileARunIsActiveUnlessStopActive(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()
	if got := listByID(t, l.c)["job"]; got.State != api.StateRunning {
		t.Errorf("state with an active run = %q, want running", got.State)
	}
	err := l.c.Call(context.Background(), api.MethodTasksDelete, api.DeleteParams{ID: "job"}, nil)
	_ = rpcErr(t, err, api.CodeRunActiveDelete)
	if _, err := os.Stat(filepath.Join(l.p.TasksDir(), "job.yaml")); err != nil {
		t.Fatalf("refused delete removed the file: %v", err)
	}

	type result struct {
		res api.DeleteResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		var res api.DeleteResult
		err := l.c.Call(context.Background(), api.MethodTasksDelete, api.DeleteParams{ID: "job", StopActive: true}, &res)
		done <- result{res, err}
	}()
	var (
		r        result
		returned bool
	)
	l.advanceUntil("delete with stop_active returns", time.Second, func() bool {
		if !returned {
			select {
			case r = <-done:
				returned = true
			default:
			}
		}
		return returned
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.res.StoppedRuns) != 1 || r.res.StoppedRuns[0].ID != run.ID || r.res.StoppedRuns[0].Status != "stopped" {
		t.Errorf("stopped runs = %+v", r.res.StoppedRuns)
	}
	if _, err := os.Stat(filepath.Join(l.p.TasksDir(), "job.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("task file after delete: %v", err)
	}
}

func TestBlockedRunShowsTaskAsBlocked(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()
	l.herdr.set(run.HerdrPane, paneBlocked)
	l.advanceUntil("task blocked", pollStep, func() bool { return listByID(t, l.c)["job"].State == api.StateBlocked })
}

func TestEditsApplyToTheNextRunOnly(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	first := l.mustRunNow()
	cur := call[api.TaskDetail](t, l.c, api.MethodTasksGet, api.TaskIDParams{ID: "job"})
	def := cur.Task
	def.Timeout, def.Overlap = "10m", "parallel"
	call[api.TaskDetail](t, l.c, api.MethodTasksUpdate,
		api.TaskWriteParams{ID: "job", Version: cur.Version, Task: def, Prompt: "second prompt"})
	second := l.mustRunNow() // overlap parallel now applies
	// Past the new 10m timeout: only the second run times out; the first
	// keeps the 1h it started with.
	l.advanceUntil("second run timed out", pollStep, l.ended(second.ID))
	if got := l.run(second.ID); got.Status != "timeout" {
		t.Errorf("second run = %+v, want timeout", got)
	}
	if got := l.run(first.ID); got.EndedAt != "" {
		t.Errorf("first run ended with the edited timeout: %+v", got)
	}
	if got := l.herdr.putContents(); !slices.Equal(got, []string{"do the job", "second prompt"}) {
		t.Errorf("prompt files written = %q, want each run's own prompt", got)
	}
}

func TestReloadRereadsTaskFiles(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	write(t, filepath.Join(p.TasksDir(), "broken.yaml"), strings.Replace(taskFile(t, p, "broken"), "mars", "local", 1))
	write(t, filepath.Join(p.ConfigDir, "prompts", "broken.md"), "x")
	if err := os.Remove(filepath.Join(p.TasksDir(), "parked.yaml")); err != nil {
		t.Fatal(err)
	}
	res := call[api.ReloadResult](t, c, api.MethodReload, nil)
	ids := map[string]string{}
	for _, s := range res.Tasks {
		ids[s.ID] = s.State
	}
	if ids["broken"] != api.StateIdle || len(ids) != 2 || res.RestartRequired {
		t.Errorf("reload = %+v", res)
	}
	write(t, p.ConfigFile(), "timezone: UTC\ntargets:\n  vps:\n    host: vps2\n")
	if res := call[api.ReloadResult](t, c, api.MethodReload, nil); res.RestartRequired {
		t.Errorf("reload after a config change = %+v, want it applied without a restart", res)
	}
}

// subscribe opens a second connection and subscribes on it.
func subscribe(t *testing.T, p paths.Paths) <-chan api.Notification {
	t.Helper()
	c, err := api.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var res api.SubscribeResult
	ch, err := c.Subscribe(ctx, api.MethodSubscribe, nil, &res)
	if err != nil || !res.Subscribed {
		t.Fatalf("subscribe: %v, %+v", err, res)
	}
	return ch
}

// next waits up to within for a notification with method that matches.
func next[E any](t *testing.T, ch <-chan api.Notification, method string, within time.Duration, match func(E) bool) E {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case n, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %s", method)
			}
			if n.Method != method {
				continue
			}
			var e E
			if err := json.Unmarshal(n.Params, &e); err != nil {
				t.Fatalf("%s params %s: %v", method, n.Params, err)
			}
			if match(e) {
				return e
			}
		case <-timeout:
			var zero E
			t.Fatalf("no matching %s within %s", method, within)
			return zero
		}
	}
}

func TestHandEditReachesSubscribersWithinTwoSeconds(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	ch := subscribe(t, p)
	before := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"})

	write(t, filepath.Join(p.TasksDir(), "nightly.yaml"), strings.Replace(taskFile(t, p, "nightly"), "Nightly deps", "Edited by hand", 1))
	ev := next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool { return e.ID == "nightly" })
	if ev.Task == nil || ev.Task.Name != "Edited by hand" || ev.Version == before.Version {
		t.Errorf("event = %+v", ev)
	}
	if got := listByID(t, c)["nightly"]; got.Name != "Edited by hand" {
		t.Errorf("list after hand edit = %+v", got)
	}

	// A new file, an invalid edit and a removal are reported too.
	write(t, filepath.Join(p.ConfigDir, "prompts", "handmade.md"), "x")
	write(t, filepath.Join(p.TasksDir(), "handmade.yaml"), "id: handmade\ntarget: local\nschedule: \"@daily\"\nagent: {command: claude}\ntimeout: 1h\n")
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool {
		return e.ID == "handmade" && e.Task != nil && e.Task.State == api.StateIdle
	})
	write(t, filepath.Join(p.TasksDir(), "handmade.yaml"), "id: handmade\ntarget: mars\n")
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool {
		return e.ID == "handmade" && e.Task != nil && e.Task.State == api.StateInvalid
	})
	if err := os.Remove(filepath.Join(p.TasksDir(), "handmade.yaml")); err != nil {
		t.Fatal(err)
	}
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool { return e.ID == "handmade" && e.Deleted })
	if _, ok := listByID(t, c)["handmade"]; ok {
		t.Error("removed task still listed")
	}

	// A hand edit of the prompt changes the version.
	write(t, filepath.Join(p.ConfigDir, "prompts", "nightly.md"), "edited prompt")
	ev = next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool { return e.ID == "nightly" })
	if got := call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: "nightly"}); got.Version != ev.Version || got.Prompt != "edited prompt" {
		t.Errorf("after prompt edit: event version %q, get = %+v", ev.Version, got)
	}
}

func TestSubscribeStreamsAPIEditsAndRunChanges(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	ch := subscribe(t, p)

	call[api.TaskDetail](t, c, api.MethodTasksCreate, api.TaskWriteParams{Task: newTaskDef(), Prompt: "p"})
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool { return e.ID == "fresh" && e.Task != nil })

	run, err := runNow(c, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	// The fake target fails the run at preflight: starting, then failed.
	next(t, ch, api.EventRunChanged, 2*time.Second, func(e api.RunChangedEvent) bool {
		return e.Run.ID == run.ID && e.Run.Status == "failed"
	})

	call[api.DeleteResult](t, c, api.MethodTasksDelete, api.DeleteParams{ID: "fresh"})
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool { return e.ID == "fresh" && e.Deleted })
}

func TestRunChangesUpdateTheTaskStateForSubscribers(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	ch := subscribe(t, l.p)
	run := l.mustRunNow()
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool {
		return e.ID == "job" && e.Task != nil && e.Task.State == api.StateRunning
	})
	l.herdr.set(run.HerdrPane, paneBlocked)
	l.clk.Advance(pollStep)
	next(t, ch, api.EventRunChanged, 5*time.Second, func(e api.RunChangedEvent) bool { return e.Run.Status == "blocked" })
	next(t, ch, api.EventTaskChanged, 2*time.Second, func(e api.TaskChangedEvent) bool {
		return e.ID == "job" && e.Task != nil && e.Task.State == api.StateBlocked
	})
}

func TestTasksGetDoesNotLoadFilesOutsideTheTasksDirectory(t *testing.T) {
	p := setupUTC(t)
	write(t, filepath.Join(p.ConfigDir, "outside.yaml"), "id: outside\ntarget: local\nschedule: \"0 7 * * *\"\n")
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	err := c.Call(context.Background(), api.MethodTasksGet, api.TaskIDParams{ID: "../outside"}, nil)
	_ = rpcErr(t, err, api.CodeInvalidParams)
}

func TestTasksCreateRefusesAPromptFileOutsideTheConfigDirectory(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	outside := filepath.Join(t.TempDir(), "victim.md")
	for _, pf := range []string{"../escape.md", outside} {
		def := newTaskDef()
		def.PromptFile = pf
		err := c.Call(context.Background(), api.MethodTasksCreate, api.TaskWriteParams{Task: def, Prompt: "x"}, nil)
		if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["prompt_file"]) == 0 {
			t.Errorf("prompt_file %q: field errors = %v", pf, f)
		}
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file outside the config directory written: %v", err)
	}
}

func TestTasksCreateRefusesAPromptSymlinkThatLeavesTheConfigDirectory(t *testing.T) {
	p := setupUTC(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "victim.md")
	write(t, outsideFile, "keep")
	link := filepath.Join(p.ConfigDir, "prompts", "linked.md")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(p.ConfigDir, "prompts", "alias")
	if err := os.Symlink(outsideDir, alias); err != nil {
		t.Fatal(err)
	}
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	for _, pf := range []string{"prompts/linked.md", "prompts/alias/fresh.md"} {
		def := newTaskDef()
		def.PromptFile = pf
		err := c.Call(context.Background(), api.MethodTasksCreate, api.TaskWriteParams{Task: def, Prompt: "pwned"}, nil)
		if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["prompt_file"]) == 0 {
			t.Errorf("prompt_file %q: field errors = %v", pf, f)
		}
	}
	if got := readFile(t, outsideFile); got != "keep" {
		t.Errorf("symlink target = %q, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "fresh.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file written through a symlinked directory: %v", err)
	}
}

func TestTasksGetReportsAnUnreadablePrompt(t *testing.T) {
	p := setupUTC(t)
	path := filepath.Join(p.ConfigDir, "prompts", "nightly.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	err := c.Call(context.Background(), api.MethodTasksGet, api.TaskIDParams{ID: "nightly"}, nil)
	if err == nil || !strings.Contains(err.Error(), "read prompt") {
		t.Fatalf("err = %v, want a prompt read error", err)
	}
}

func TestInvalidTaskStaysInvalidWhileARunIsActive(t *testing.T) {
	l := newLifecycle(t, noon, "timeout: 1h\n")
	l.start()
	run := l.mustRunNow()
	write(t, filepath.Join(l.p.TasksDir(), "job.yaml"), "id: job\ntarget: mars\n")
	res := call[api.ReloadResult](t, l.c, api.MethodReload, nil)
	var got api.TaskSummary
	for _, s := range res.Tasks {
		if s.ID == "job" {
			got = s
		}
	}
	if got.State != api.StateInvalid || !strings.Contains(got.Error, "mars") {
		t.Errorf("reload = %+v, want invalid with the target error", got)
	}
	if listed := listByID(t, l.c)["job"]; listed.State != api.StateInvalid || listed.Error == "" {
		t.Errorf("list = %+v, want invalid with an error", listed)
	}
	if still := l.run(run.ID); still.Status != "running" || still.EndedAt != "" {
		t.Errorf("active run = %+v, want it still running", still)
	}
}

func TestReloadRereadsTasksWhenConfigYAMLDoesNotParse(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	write(t, filepath.Join(p.TasksDir(), "broken.yaml"), strings.Replace(taskFile(t, p, "broken"), "mars", "local", 1))
	write(t, filepath.Join(p.ConfigDir, "prompts", "broken.md"), "x")
	write(t, p.ConfigFile(), "timezone: [\n")
	err := c.Call(context.Background(), api.MethodReload, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "config.yaml") {
		t.Fatalf("err = %v, want a config.yaml error", err)
	}
	if got := listByID(t, c)["broken"]; got.State != api.StateIdle {
		t.Errorf("broken = %+v, want the task file applied despite the config error", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A prompt above Linux's 131071-byte argument limit is refused on create
// and update with a field error on the prompt, and no file is written.
func TestTasksCreateAndUpdateRefuseAPromptAboveTheArgumentLimit(t *testing.T) {
	p := setupUTC(t)
	c := startWith(t, p, daemon.Options{Version: "test", Clock: clock.NewFake(noon), Targets: fakeTargets()})
	big := strings.Repeat("a", 131072)
	def := newTaskDef()
	err := c.Call(context.Background(), api.MethodTasksCreate, api.TaskWriteParams{Task: def, Prompt: big}, nil)
	if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["prompt"]) != 1 || !strings.Contains(f["prompt"][0], "131071") {
		t.Errorf("create: field errors = %v, want one prompt error naming the limit", f)
	}
	if _, err := os.Stat(filepath.Join(p.TasksDir(), def.ID+".yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("task file written for a refused create: %v", err)
	}

	var created api.TaskDetail
	if err := c.Call(context.Background(), api.MethodTasksCreate, api.TaskWriteParams{Task: def, Prompt: strings.Repeat("a", 131071)}, &created); err != nil {
		t.Fatalf("create with a 131071-byte prompt: %v", err)
	}
	err = c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: def.ID, Version: created.Version, Task: def, Prompt: big}, nil)
	if f := fieldErrors(t, rpcErr(t, err, api.CodeValidation)); len(f["prompt"]) != 1 || !strings.Contains(f["prompt"][0], "131071") {
		t.Errorf("update: field errors = %v, want one prompt error naming the limit", f)
	}
	promptPath := filepath.Join(p.ConfigDir, "prompts", def.ID+".md")
	if data, _ := os.ReadFile(promptPath); len(data) != 131071 {
		t.Errorf("refused update changed the prompt file to %d bytes", len(data))
	}

	// A hand edit made the prompt too big: an update with a short prompt
	// fixes it, rather than failing on the file it replaces.
	write(t, promptPath, big)
	var detail api.TaskDetail
	for i := 0; detail.Summary.Error == ""; i++ {
		if i > 300 {
			t.Fatal("hand-edited oversized prompt never made the task invalid")
		}
		time.Sleep(10 * time.Millisecond)
		detail = call[api.TaskDetail](t, c, api.MethodTasksGet, api.TaskIDParams{ID: def.ID})
	}
	var updated api.TaskDetail
	if err := c.Call(context.Background(), api.MethodTasksUpdate,
		api.TaskWriteParams{ID: def.ID, Version: detail.Version, Task: def, Prompt: "short"}, &updated); err != nil {
		t.Fatalf("update with a short prompt over an oversized prompt file: %v", err)
	}
	if updated.Prompt != "short" || updated.Summary.Error != "" || updated.Summary.State == api.StateInvalid {
		t.Errorf("after replacing the oversized prompt: prompt %q state %s error %q",
			updated.Prompt, updated.Summary.State, updated.Summary.Error)
	}
	if got := readFile(t, promptPath); got != "short" {
		t.Errorf("prompt file = %q, want the short replacement", got)
	}
}
