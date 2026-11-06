package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
)

// fullTask is a valid task with every field set, including values YAML
// would read as another type if written unquoted.
func fullTask() config.Task {
	t := config.DefaultTask("peppol-sh-deps")
	t.Name = "peppol.sh dependency update"
	t.Target = "vps"
	t.Cwd = "~/src/peppol-sh"
	t.Schedule = "0 7 * * 1"
	t.Timezone = "Europe/Brussels"
	t.Enabled = false
	t.PausedUntil = "indefinite"
	t.Agent = config.Agent{Command: "claude", Args: []string{"--permission-mode", "acceptEdits", "yes", "1.5"}}
	t.Timeout = config.Duration{Duration: 90 * time.Minute}
	t.Overlap = "queue"
	t.Missed = "skip"
	t.OnDone = "close_tab"
	t.OnBlocked = "stop"
	t.Env = map[string]string{"FLAG": "true", "CODE": "007", "EMPTY": "", "NULLISH": "null", "Z": "a: b"}
	return t
}

func asValidationError(t *testing.T, err error) *config.ValidationError {
	t.Helper()
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError", err, err)
	}
	return ve
}

func TestDefaultTask(t *testing.T) {
	got := config.DefaultTask("nightly")
	want := config.Task{
		ID: "nightly", Enabled: true, Overlap: "skip", Missed: "run_once",
		OnDone: "close_tab", OnBlocked: "notify", PromptFile: filepath.Join("prompts", "nightly.md"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultTask = %+v, want %+v", got, want)
	}
}

func TestValidateReportsFieldErrors(t *testing.T) {
	f := newFixture(t)
	f.prompt("peppol-sh-deps.md")
	task := fullTask()
	task.Target = "nowhere"
	task.Schedule = "0 7 * *"
	task.Timezone = "Mars/Olympus"
	task.Agent.Command = ""
	task.Timeout = config.Duration{}
	task.PausedUntil = "tomorrow"
	task.Overlap = "sometimes"
	task.Missed = "x"
	task.OnDone = "x"
	task.OnBlocked = "x"
	ve := asValidationError(t, config.Validate(task, "other", f.dir, f.cfg))

	byField := ve.ByField()
	for _, field := range []string{"id", "target", "schedule", "timezone", "agent.command", "timeout",
		"paused_until", "overlap", "missed", "on_done", "on_blocked"} {
		if len(byField[field]) != 1 {
			t.Errorf("ByField()[%q] = %v, want one message", field, byField[field])
		}
	}
	if _, ok := byField["prompt_file"]; ok {
		t.Errorf("prompt_file reported, but it exists: %v", byField["prompt_file"])
	}
	if len(ve.Fields) != len(ve.Problems) {
		t.Fatalf("%d fields, %d problems", len(ve.Fields), len(ve.Problems))
	}
	for i, fe := range ve.Fields {
		if fe.Message != ve.Problems[i] {
			t.Errorf("Fields[%d].Message = %q, Problems[%d] = %q", i, fe.Message, i, ve.Problems[i])
		}
	}
	if !strings.Contains(ve.Error(), `unknown target "nowhere"; schedule "0 7 * *"`) {
		t.Errorf("Error() = %q", ve.Error())
	}
	if !strings.Contains(byField["id"][0], `does not match file name "other"`) {
		t.Errorf("id message = %q", byField["id"][0])
	}
}

func TestValidatePromptFileFieldErrors(t *testing.T) {
	f := newFixture(t)
	task := fullTask()
	ve := asValidationError(t, config.Validate(task, task.ID, f.dir, f.cfg))
	if got := ve.ByField()["prompt_file"]; len(got) != 1 || !strings.Contains(got[0], "does not exist") {
		t.Errorf("prompt_file = %v", got)
	}
	task.PromptFile = ""
	ve = asValidationError(t, config.Validate(task, task.ID, f.dir, f.cfg))
	if got := ve.ByField()["prompt_file"]; len(got) != 1 || !strings.Contains(got[0], "missing") {
		t.Errorf("empty prompt_file = %v", got)
	}
}

func TestValidateDraft(t *testing.T) {
	f := newFixture(t)
	task := fullTask()
	if err := config.ValidateDraft(task, f.dir, f.cfg); err != nil {
		t.Errorf("missing prompt file: %v", err)
	}
	f.prompt("peppol-sh-deps.md")
	if err := config.ValidateDraft(task, f.dir, f.cfg); err != nil {
		t.Errorf("existing prompt file: %v", err)
	}

	empty := task
	empty.PromptFile = ""
	ve := asValidationError(t, config.ValidateDraft(empty, f.dir, f.cfg))
	if got := ve.ByField()["prompt_file"]; len(got) != 1 {
		t.Errorf("empty prompt_file: %v", ve.Fields)
	}

	dir := task
	dir.PromptFile = "prompts"
	ve = asValidationError(t, config.ValidateDraft(dir, f.dir, f.cfg))
	if got := ve.ByField()["prompt_file"]; len(got) != 1 || !strings.Contains(got[0], "directory") {
		t.Errorf("directory prompt_file: %v", ve.Fields)
	}

	bad := task
	bad.ID = "Bad_ID"
	bad.Target = "nowhere"
	ve = asValidationError(t, config.ValidateDraft(bad, f.dir, f.cfg))
	byField := ve.ByField()
	if len(byField["id"]) != 1 || len(byField["target"]) != 1 || len(ve.Fields) != 2 {
		t.Errorf("fields = %v", ve.Fields)
	}
}

func TestWriteTaskCreateRoundTrips(t *testing.T) {
	f := newFixture(t)
	f.prompt("peppol-sh-deps.md")
	task := fullTask()
	path := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")

	if err := config.WriteTask(path, task, true); err != nil {
		t.Fatalf("WriteTask: %v", err)
	}
	e := config.LoadTask(f.dir, task.ID, f.cfg)
	if e.Err != nil {
		t.Fatalf("LoadTask: %v\n%s", e.Err, readFile(t, path))
	}
	if !reflect.DeepEqual(e.Task, task) {
		t.Errorf("read back %+v\nwant      %+v\nfile:\n%s", e.Task, task, readFile(t, path))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %v, want 0644", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("tasks dir mode = %v (err %v), want 0755", info.Mode().Perm(), err)
	}
	assertOnlyFile(t, filepath.Dir(path), "peppol-sh-deps.yaml")
}

func TestWriteTaskCreateFieldOrderAndOmissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks", "nightly.yaml")
	task := config.DefaultTask("nightly")
	task.Target = "local"
	task.Schedule = "@daily"
	task.Agent.Command = "claude"
	task.Timeout = config.Duration{Duration: 2 * time.Hour}

	if err := config.WriteTask(path, task, true); err != nil {
		t.Fatal(err)
	}
	want := `id: nightly
target: local
schedule: "@daily"
enabled: true
paused_until: null
agent:
  command: claude
prompt_file: prompts/nightly.md
timeout: 2h
overlap: skip
missed: run_once
on_done: close_tab
on_blocked: notify
`
	if got := readFile(t, path); got != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteTaskTimeoutFormat(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour:                "timeout: 2h\n",
		90 * time.Minute:             "timeout: 1h30m\n",
		30 * time.Second:             "timeout: 30s\n",
		5 * time.Minute:              "timeout: 5m\n",
		time.Hour + 30*time.Second:   "timeout: 1h0m30s\n",
		1500 * time.Millisecond:      "timeout: 1.5s\n",
		25*time.Hour + 1*time.Minute: "timeout: 25h1m\n",
	} {
		path := filepath.Join(t.TempDir(), "x.yaml")
		task := config.DefaultTask("x")
		task.Timeout = config.Duration{Duration: d}
		if err := config.WriteTask(path, task, true); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, path); !strings.Contains(got, "\n"+want) {
			t.Errorf("%v: file lacks %q:\n%s", d, want, got)
		}
	}
}

func TestWriteTaskCreateRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.yaml")
	writeFile(t, path, "id: x\n# keep me\n")
	err := config.WriteTask(path, config.DefaultTask("x"), true)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	if got := readFile(t, path); got != "id: x\n# keep me\n" {
		t.Errorf("file changed to %q", got)
	}
	assertOnlyFile(t, dir, "x.yaml")
}

func TestWriteTaskUpdateKeepsCommentsAndOrder(t *testing.T) {
	f := newFixture(t)
	f.prompt("peppol-sh-deps.md")
	path := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")
	f.task("peppol-sh-deps.yaml", commentedTask)

	e := config.LoadTask(f.dir, "peppol-sh-deps", f.cfg)
	if e.Err != nil {
		t.Fatal(e.Err)
	}
	task := e.Task
	task.Name = ""                // removed
	task.Schedule = "0 8 * * 1-5" // replaced, comment kept
	task.Agent.Args = []string{"--model", "opus"}
	task.Timeout = config.Duration{Duration: 30 * time.Minute}
	task.Cwd = "~/src/peppol-sh" // appended
	task.Env = map[string]string{"TOKEN": "true"}

	if err := config.WriteTask(path, task, false); err != nil {
		t.Fatalf("WriteTask: %v", err)
	}
	want := `# Weekly dependency update.
id: peppol-sh-deps
target: vps
schedule: "0 8 * * 1-5"
enabled: true
agent:
  command: claude
  # Accept edits without asking.
  args: ["--model", "opus"]
prompt_file: prompts/peppol-sh-deps.md
timeout: 30m
env:
  TOKEN: "true"
cwd: ~/src/peppol-sh
paused_until: null
overlap: skip
missed: run_once
on_done: close_tab
on_blocked: notify
`
	if got := readFile(t, path); got != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
	e = config.LoadTask(f.dir, "peppol-sh-deps", f.cfg)
	if e.Err != nil {
		t.Fatal(e.Err)
	}
	if !reflect.DeepEqual(e.Task, task) {
		t.Errorf("read back %+v\nwant      %+v", e.Task, task)
	}
}

func TestWriteTaskUpdateRemovesEmptyOptionalFields(t *testing.T) {
	f := newFixture(t)
	f.prompt("peppol-sh-deps.md")
	path := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")
	if err := config.WriteTask(path, fullTask(), true); err != nil {
		t.Fatal(err)
	}
	task := fullTask()
	task.Name, task.Cwd, task.Timezone, task.PausedUntil = "", "", "", ""
	task.Env, task.Agent.Args = nil, nil
	if err := config.WriteTask(path, task, false); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	for _, key := range []string{"name:", "cwd:", "timezone:", "env:", "args:"} {
		if strings.Contains(got, key) {
			t.Errorf("file still has %s:\n%s", key, got)
		}
	}
	if !strings.Contains(got, "paused_until: null\n") {
		t.Errorf("paused_until not null:\n%s", got)
	}
	e := config.LoadTask(f.dir, task.ID, f.cfg)
	if e.Err != nil {
		t.Fatal(e.Err)
	}
	if !reflect.DeepEqual(e.Task, task) {
		t.Errorf("read back %+v\nwant      %+v", e.Task, task)
	}
}

func TestWriteTaskUpdateKeepsModeAndRefusesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.yaml")
	if err := config.WriteTask(path, config.DefaultTask("x"), false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("update of a missing file: err = %v, want fs.ErrNotExist", err)
	}
	writeFile(t, path, "id: x\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteTask(path, config.DefaultTask("x"), false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
	assertOnlyFile(t, dir, "x.yaml")

	writeFile(t, path, "- not a mapping\n")
	if err := config.WriteTask(path, config.DefaultTask("x"), false); err == nil {
		t.Error("update of a sequence file: no error")
	}
}

func TestWritePrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts", "x.md")
	text := "Say \"hi\" and 'bye'.\n$HOME `date` \\n\n\n  indented\ttab"
	if err := config.WritePrompt(path, text); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != text {
		t.Errorf("prompt = %q, want %q", got, text)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v (err %v), want 0644", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("dir mode = %v (err %v), want 0755", info.Mode().Perm(), err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.WritePrompt(path, ""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("prompt = %q, want empty", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v (err %v), want 0600 kept", info.Mode().Perm(), err)
	}
	assertOnlyFile(t, filepath.Dir(path), "x.md")
}

func TestVersion(t *testing.T) {
	dir := t.TempDir()
	task := filepath.Join(dir, "x.yaml")
	prompt := filepath.Join(dir, "x.md")
	writeFile(t, task, "id: x\n")

	version := func() string {
		t.Helper()
		v, err := config.Version(task, prompt)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	noPrompt := version()
	if len(noPrompt) != 64 || strings.Trim(noPrompt, "0123456789abcdef") != "" {
		t.Errorf("version %q is not hex sha256", noPrompt)
	}
	if version() != noPrompt {
		t.Error("version not stable")
	}
	writeFile(t, prompt, "")
	if version() != noPrompt {
		t.Error("missing prompt and empty prompt differ")
	}
	writeFile(t, prompt, "do it\n")
	withPrompt := version()
	if withPrompt == noPrompt {
		t.Error("prompt change did not change the version")
	}
	writeFile(t, task, "id: y\n")
	if version() == withPrompt {
		t.Error("task change did not change the version")
	}

	// Moving bytes across the boundary must change the version.
	writeFile(t, task, "ab")
	writeFile(t, prompt, "c")
	v1 := version()
	writeFile(t, task, "a")
	writeFile(t, prompt, "bc")
	if version() == v1 {
		t.Error("versions of (ab, c) and (a, bc) are equal")
	}

	if _, err := config.Version(filepath.Join(dir, "missing.yaml"), prompt); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing task file: err = %v, want fs.ErrNotExist", err)
	}
}

func TestReadTaskLenient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nightly.yaml")
	writeFile(t, path, "id: Not_Valid\ntarget: nowhere\nenabeld: false\ntimeout: 0s\nagent:\n  command: claude\n")
	got, err := config.ReadTaskLenient(path)
	if err != nil {
		t.Fatalf("ReadTaskLenient: %v", err)
	}
	want := config.DefaultTask("nightly")
	want.ID = "Not_Valid"
	want.Target = "nowhere"
	want.Agent.Command = "claude"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	writeFile(t, path, "")
	want = config.DefaultTask("nightly")
	want.ID = "" // the file name is not the id
	if got, err := config.ReadTaskLenient(path); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("empty file: %+v, %v", got, err)
	}

	writeFile(t, path, "id: [unclosed\n")
	if _, err := config.ReadTaskLenient(path); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("malformed file: err = %v, want parse error", err)
	}

	writeFile(t, path, "target: vps\nenv: s3cr3t\ntimeout: soon\n")
	got, err = config.ReadTaskLenient(path)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("type error: err = %v, want a parse error without the value", err)
	}
	if got.Target != "vps" {
		t.Errorf("type error: other fields not decoded: %+v", got)
	}

	if _, err := config.ReadTaskLenient(filepath.Join(dir, "missing.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist", err)
	}
}

func assertOnlyFile(t *testing.T, dir, name string) {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	if len(names) != 1 || names[0] != name {
		t.Errorf("directory holds %v, want only %s", names, name)
	}
}

// Linux passes one argument of at most 131071 bytes, and the prompt reaches
// the agent as one argument, so a bigger prompt file is refused
// (docs/herdr-findings.md, "Prompts above 128 KiB").
func TestValidateRefusesAPromptFileAboveTheArgumentLimit(t *testing.T) {
	f := newFixture(t)
	task := fullTask()
	path := config.PromptPath(task.PromptFile, f.dir)
	writeFile(t, path, strings.Repeat("a", 131071))
	if err := config.Validate(task, task.ID, f.dir, f.cfg); err != nil {
		t.Fatalf("prompt of 131071 bytes refused: %v", err)
	}
	writeFile(t, path, strings.Repeat("a", 131072))
	ve := asValidationError(t, config.Validate(task, task.ID, f.dir, f.cfg))
	if got := ve.ByField()["prompt_file"]; len(got) != 1 || !strings.Contains(got[0], "131071") {
		t.Errorf("prompt_file = %v, want one error naming the 131071-byte limit", got)
	}
}
