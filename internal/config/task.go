package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/schedule"
	"gopkg.in/yaml.v3"
)

// Task is the content of one tasks/<task-id>.yaml file.
type Task struct {
	ID          string            `yaml:"id"`
	Name        string            `yaml:"name"`
	Target      string            `yaml:"target"`
	Cwd         string            `yaml:"cwd"`
	Schedule    string            `yaml:"schedule"`
	Timezone    string            `yaml:"timezone"`
	Enabled     bool              `yaml:"enabled"`
	PausedUntil string            `yaml:"paused_until"`
	Agent       Agent             `yaml:"agent"`
	PromptFile  string            `yaml:"prompt_file"`
	Timeout     Duration          `yaml:"timeout"`
	Overlap     string            `yaml:"overlap"`
	Missed      string            `yaml:"missed"`
	OnDone      string            `yaml:"on_done"`
	OnBlocked   string            `yaml:"on_blocked"`
	Env         map[string]string `yaml:"env"`
}

// Agent is the coding-agent command a run starts.
type Agent struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// Duration is a time.Duration written as a Go duration string ("2h").
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		// A TypeError lets the decoder go on with the other fields.
		return &yaml.TypeError{Errors: []string{
			fmt.Sprintf("line %d: timeout: %q is not a Go duration (for example 2h or 30m)", n.Line, s),
		}}
	}
	d.Duration = v
	return nil
}

// TaskEntry is one task file as loaded: either a valid Task (Err nil) or the
// reason it is invalid. ID is always the file name without ".yaml", so an
// invalid file can still be listed.
type TaskEntry struct {
	ID   string
	File string
	Task Task
	Err  error
}

// ValidationError lists every problem found in one task file. Fields holds
// the same problems as Problems, in the same order, each with its key.
type ValidationError struct {
	Problems []string
	Fields   []FieldError
}

// FieldError is one problem with one field. Field is the YAML key, with
// nested keys joined by a dot ("agent.command").
type FieldError struct{ Field, Message string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// ByField groups the problems by field.
func (e *ValidationError) ByField() map[string][]string {
	m := make(map[string][]string, len(e.Fields))
	for _, f := range e.Fields {
		m[f.Field] = append(m[f.Field], f.Message)
	}
	return m
}

func (e *ValidationError) add(field, msg string) {
	e.Problems = append(e.Problems, msg)
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

// quotedValue matches the values yaml.v3 quotes in its errors.
var quotedValue = regexp.MustCompile("`[^`]*`")

// parseError turns a yaml error into one line without the quoted values,
// which can be secrets (a mistyped env block) or prompt text.
func parseError(err error) error {
	msg := quotedValue.ReplaceAllString(err.Error(), "`…`")
	msg = strings.Join(strings.Fields(msg), " ")
	return errors.New("parse task file: " + msg)
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var enums = map[string][]string{
	"overlap":    {"skip", "queue", "parallel"},
	"missed":     {"skip", "run_once"},
	"on_done":    {"keep_tab", "close_tab"},
	"on_blocked": {"notify", "stop"},
}

// EnumValues returns the values a task's enum field accepts (overlap,
// missed, on_done or on_blocked), or nil for any other field.
func EnumValues(field string) []string { return slices.Clone(enums[field]) }

// LoadTasks reads every tasks/*.yaml file under configDir and validates it
// against cfg. A missing tasks directory means no tasks. An invalid file is
// returned as an entry with Err set; it never fails the whole load. Entries
// are sorted by ID.
func LoadTasks(configDir string, cfg Config) ([]TaskEntry, error) {
	dir := filepath.Join(configDir, "tasks")
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tasks directory: %w", err)
	}
	var entries []TaskEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
			continue
		}
		entries = append(entries, LoadTask(configDir, strings.TrimSuffix(f.Name(), ".yaml"), cfg))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

// DefaultTask is a task with the spec's defaults for every optional field
// and the default prompt file, prompts/<id>.md.
func DefaultTask(id string) Task {
	return Task{
		ID: id, Enabled: true, Overlap: "skip", Missed: "run_once", OnDone: "keep_tab", OnBlocked: "notify",
		PromptFile: defaultPromptFile(id),
	}
}

func defaultPromptFile(fileID string) string { return filepath.Join("prompts", fileID+".md") }

func loadTask(path, fileID, configDir string, cfg Config) (Task, error) {
	t, err := readTask(path, fileID, true)
	if err != nil {
		return Task{}, err
	}
	return t, Validate(t, fileID, configDir, cfg)
}

// ReadTaskLenient reads the task file at path without validating it and
// without rejecting unknown keys, applying the same defaults as LoadTask
// (the file name gives the default prompt file). It lets an invalid task's
// fields be shown. On a type error (a field of the wrong type) the other
// fields are still returned, with the error.
func ReadTaskLenient(path string) (Task, error) {
	return readTask(path, strings.TrimSuffix(filepath.Base(path), ".yaml"), false)
}

func readTask(path, fileID string, strict bool) (Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Task{}, fmt.Errorf("read task file: %w", err)
	}
	t := DefaultTask(fileID)
	t.ID = "" // the id must come from the file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(strict) // a typo like "enabeld" must not be silently ignored
	if err := dec.Decode(&t); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if strict || !errors.As(err, &te) {
			return Task{}, parseError(err)
		}
		return fillDefaults(t, fileID), parseError(err)
	}
	return fillDefaults(t, fileID), nil
}

// fillDefaults restores the default prompt file when the file sets it empty.
func fillDefaults(t Task, fileID string) Task {
	if t.PromptFile == "" {
		t.PromptFile = defaultPromptFile(fileID)
	}
	return t
}

// Validate checks t, loaded from tasks/<fileID>.yaml, against the rules in
// the spec. It returns a *ValidationError listing every problem, or nil.
func Validate(t Task, fileID, configDir string, cfg Config) error {
	return validate(t, fileID, configDir, cfg, false)
}

// ValidateDraft checks a task about to be written to tasks/<t.ID>.yaml: the
// same rules as Validate, except that the prompt file need not exist yet.
func ValidateDraft(t Task, configDir string, cfg Config) error {
	return validate(t, t.ID, configDir, cfg, true)
}

func validate(t Task, fileID, configDir string, cfg Config, draft bool) error {
	e := &ValidationError{}
	switch {
	case t.ID == "":
		e.add("id", "id is missing")
	case !idPattern.MatchString(t.ID):
		e.add("id", fmt.Sprintf("id %q must match %s", t.ID, idPattern))
	case t.ID != fileID:
		e.add("id", fmt.Sprintf("id %q does not match file name %q", t.ID, fileID))
	}
	if t.Target == "" {
		e.add("target", "target is missing")
	} else if _, ok := cfg.Targets[t.Target]; !ok {
		e.add("target", fmt.Sprintf("unknown target %q", t.Target))
	}
	if t.Schedule == "" {
		e.add("schedule", "schedule is missing")
	} else if err := checkSchedule(t.Schedule); err != nil {
		e.add("schedule", fmt.Sprintf("schedule %q: %v", t.Schedule, err))
	}
	if t.Timezone != "" {
		if _, err := time.LoadLocation(t.Timezone); err != nil {
			e.add("timezone", fmt.Sprintf("timezone %q is unknown", t.Timezone))
		}
	}
	if t.Agent.Command == "" {
		e.add("agent.command", "agent.command is missing")
	}
	if problem := checkPromptFile(t.PromptFile, configDir, draft); problem != "" {
		e.add("prompt_file", problem)
	}
	if t.Timeout.Duration <= 0 {
		e.add("timeout", "timeout must be > 0")
	}
	if err := checkPausedUntil(t.PausedUntil); err != nil {
		e.add("paused_until", err.Error())
	}
	for _, f := range []struct{ key, val string }{
		{"overlap", t.Overlap}, {"missed", t.Missed}, {"on_done", t.OnDone}, {"on_blocked", t.OnBlocked},
	} {
		if !slices.Contains(enums[f.key], f.val) {
			e.add(f.key, fmt.Sprintf("%s %q must be one of %s", f.key, f.val, strings.Join(enums[f.key], ", ")))
		}
	}
	if len(e.Problems) == 0 {
		return nil
	}
	return e
}

// checkSchedule accepts a 5-field cron expression or a descriptor (@every
// <duration>, @daily, @hourly, ...); see package schedule.
func checkSchedule(s string) error {
	_, err := schedule.Parse(s, time.UTC)
	return err
}

// checkPausedUntil accepts the spec's values: empty (YAML null), "indefinite",
// or an RFC 3339 time.
func checkPausedUntil(s string) error {
	_, _, err := parsePausedUntil(s)
	return err
}

// parsePausedUntil interprets paused_until. indefinite is true for the
// permanent pause. An empty value is not paused.
func parsePausedUntil(s string) (until time.Time, indefinite bool, err error) {
	switch s {
	case "":
		return time.Time{}, false, nil
	case "indefinite":
		return time.Time{}, true, nil
	}
	until, err = time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("paused_until %q must be null, an RFC 3339 time, or \"indefinite\"", s)
	}
	return until, false, nil
}

// PausedAt reports whether the task is paused at at. "indefinite" is always
// paused. A timestamp is paused while at is still before it. An unparseable
// value is paused, so a bad file does not fire.
func (t Task) PausedAt(at time.Time) bool {
	until, indefinite, err := parsePausedUntil(t.PausedUntil)
	if err != nil || indefinite {
		return true
	}
	return !until.IsZero() && until.After(at)
}

// PromptPath resolves a task's prompt_file: relative paths are relative to
// the config directory.
func PromptPath(promptFile, configDir string) string {
	if filepath.IsAbs(promptFile) {
		return promptFile
	}
	return filepath.Join(configDir, promptFile)
}

// checkPromptFile reports a problem with prompt_file. A draft's prompt file
// may be missing; it is written with the task.
func checkPromptFile(promptFile, configDir string, draft bool) string {
	if promptFile == "" {
		return "prompt_file is missing"
	}
	info, err := os.Stat(PromptPath(promptFile, configDir))
	switch {
	case errors.Is(err, fs.ErrNotExist) && draft:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("prompt file %s does not exist", promptFile)
	case err != nil:
		return fmt.Sprintf("prompt file %s: %v", promptFile, err)
	case info.IsDir():
		return fmt.Sprintf("prompt file %s is a directory", promptFile)
	}
	return ""
}
