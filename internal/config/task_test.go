package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
)

const validTask = `id: peppol-sh-deps
name: peppol.sh dependency update
target: vps
cwd: ~/src/peppol-sh
schedule: "0 7 * * 1"
timezone: Europe/Brussels
enabled: true
agent:
  command: claude
  args: ["--permission-mode", "acceptEdits"]
prompt_file: prompts/peppol-sh-deps.md
timeout: 2h
env:
  FOO: bar
`

type fixture struct {
	t   *testing.T
	dir string
	cfg config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, dir: t.TempDir(), cfg: config.Config{
		Timezone: "Local",
		Targets:  map[string]config.Target{"local": {}, "vps": {Host: "vps"}},
	}}
}

func (f *fixture) task(file, content string) {
	writeFile(f.t, filepath.Join(f.dir, "tasks", file), content)
}

func (f *fixture) prompt(file string) {
	writeFile(f.t, filepath.Join(f.dir, "prompts", file), "do the thing\n")
}

func (f *fixture) load() []config.TaskEntry {
	f.t.Helper()
	entries, err := config.LoadTasks(f.dir, f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return entries
}

func (f *fixture) only() config.TaskEntry {
	f.t.Helper()
	entries := f.load()
	if len(entries) != 1 {
		f.t.Fatalf("got %d entries, want 1: %+v", len(entries), entries)
	}
	return entries[0]
}

func TestValidTaskLoads(t *testing.T) {
	f := newFixture(t)
	f.task("peppol-sh-deps.yaml", validTask)
	f.prompt("peppol-sh-deps.md")
	e := f.only()
	if e.Err != nil {
		t.Fatalf("unexpected error: %v", e.Err)
	}
	if e.ID != "peppol-sh-deps" || e.Task.Target != "vps" || e.Task.Schedule != "0 7 * * 1" {
		t.Errorf("entry = %+v", e)
	}
	if e.Task.Timeout.Duration != 2*time.Hour {
		t.Errorf("timeout = %v", e.Task.Timeout)
	}
	if !e.Task.Enabled || e.Task.Overlap != "skip" || e.Task.Missed != "run_once" || e.Task.OnDone != "keep_tab" || e.Task.OnBlocked != "notify" {
		t.Errorf("defaults not applied: %+v", e.Task)
	}
}

func TestPromptFileDefaultsToPromptsDirByID(t *testing.T) {
	f := newFixture(t)
	f.task("peppol-sh-deps.yaml", strings.Replace(validTask, "prompt_file: prompts/peppol-sh-deps.md\n", "", 1))
	f.prompt("peppol-sh-deps.md")
	if e := f.only(); e.Err != nil {
		t.Fatalf("unexpected error: %v", e.Err)
	}
}

func TestScheduleDescriptorsParse(t *testing.T) {
	for _, s := range []string{"@every 2h", "@daily", "@hourly", "*/2 * * * *"} {
		f := newFixture(t)
		f.task("peppol-sh-deps.yaml", strings.Replace(validTask, `"0 7 * * 1"`, `"`+s+`"`, 1))
		f.prompt("peppol-sh-deps.md")
		if e := f.only(); e.Err != nil {
			t.Errorf("schedule %q: %v", s, e.Err)
		}
	}
}

func TestValidationRules(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		edit     func(string) string
		noPrompt bool
		wantErr  string
	}{
		{
			name:    "id pattern: uppercase",
			file:    "Bad_ID.yaml",
			edit:    func(s string) string { return strings.Replace(s, "id: peppol-sh-deps", "id: Bad_ID", 1) },
			wantErr: "id",
		},
		{
			name:    "id pattern: leading dash",
			file:    "-x.yaml",
			edit:    func(s string) string { return strings.Replace(s, "id: peppol-sh-deps", "id: -x", 1) },
			wantErr: "id",
		},
		{
			name: "id pattern: too long",
			file: strings.Repeat("a", 64) + ".yaml",
			edit: func(s string) string {
				return strings.Replace(s, "id: peppol-sh-deps", "id: "+strings.Repeat("a", 64), 1)
			},
			wantErr: "id",
		},
		{
			name:    "id must equal file name",
			file:    "other-name.yaml",
			edit:    func(s string) string { return s },
			wantErr: `does not match file name "other-name"`,
		},
		{
			name:    "id missing",
			edit:    func(s string) string { return strings.Replace(s, "id: peppol-sh-deps\n", "", 1) },
			wantErr: "id",
		},
		{
			name:    "target must exist",
			edit:    func(s string) string { return strings.Replace(s, "target: vps", "target: nowhere", 1) },
			wantErr: `unknown target "nowhere"`,
		},
		{
			name:    "target missing",
			edit:    func(s string) string { return strings.Replace(s, "target: vps\n", "", 1) },
			wantErr: "target",
		},
		{
			name:    "schedule must parse",
			edit:    func(s string) string { return strings.Replace(s, `"0 7 * * 1"`, `"0 7 * *"`, 1) },
			wantErr: "schedule",
		},
		{
			name:    "schedule rejects seconds field",
			edit:    func(s string) string { return strings.Replace(s, `"0 7 * * 1"`, `"0 0 7 * * 1"`, 1) },
			wantErr: "schedule",
		},
		{
			name:    "schedule rejects zero @every",
			edit:    func(s string) string { return strings.Replace(s, `"0 7 * * 1"`, `"@every 0s"`, 1) },
			wantErr: "schedule",
		},
		{
			name:    "schedule rejects negative @every",
			edit:    func(s string) string { return strings.Replace(s, `"0 7 * * 1"`, `"@every -2h"`, 1) },
			wantErr: "schedule",
		},
		{
			name:    "schedule rejects sub-minute @every",
			edit:    func(s string) string { return strings.Replace(s, `"0 7 * * 1"`, `"@every 30s"`, 1) },
			wantErr: "schedule",
		},
		{
			name:    "schedule rejects CRON_TZ prefix (use timezone)",
			edit:    func(s string) string { return strings.Replace(s, `"0 7 * * 1"`, `"CRON_TZ=Asia/Tokyo 0 7 * * 1"`, 1) },
			wantErr: "timezone",
		},
		{
			name:    "unknown key is an error, not silently ignored",
			edit:    func(s string) string { return strings.Replace(s, "enabled: true", "enabeld: false", 1) },
			wantErr: "enabeld",
		},
		{
			name:     "prompt file must exist",
			noPrompt: true,
			edit:     func(s string) string { return s },
			wantErr:  "prompt file",
		},
		{
			name:    "timeout must be > 0",
			edit:    func(s string) string { return strings.Replace(s, "timeout: 2h", "timeout: 0s", 1) },
			wantErr: "timeout",
		},
		{
			name:    "timeout negative",
			edit:    func(s string) string { return strings.Replace(s, "timeout: 2h", "timeout: -5m", 1) },
			wantErr: "timeout",
		},
		{
			name:    "timeout missing",
			edit:    func(s string) string { return strings.Replace(s, "timeout: 2h\n", "", 1) },
			wantErr: "timeout",
		},
		{
			name:    "timeout not a duration",
			edit:    func(s string) string { return strings.Replace(s, "timeout: 2h", "timeout: soon", 1) },
			wantErr: "timeout",
		},
		{
			name:    "timezone must be known",
			edit:    func(s string) string { return strings.Replace(s, "Europe/Brussels", "Mars/Olympus", 1) },
			wantErr: "timezone",
		},
		{
			name:    "agent command required",
			edit:    func(s string) string { return strings.Replace(s, "  command: claude\n", "", 1) },
			wantErr: "agent.command",
		},
		{
			name:    "overlap must be a known mode",
			edit:    func(s string) string { return s + "overlap: sometimes\n" },
			wantErr: "overlap",
		},
		{
			name:    "malformed yaml",
			edit:    func(s string) string { return s + "agent: [\n" },
			wantErr: "parse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			file := tc.file
			if file == "" {
				file = "peppol-sh-deps.yaml"
			}
			f.task(file, tc.edit(validTask))
			if !tc.noPrompt {
				f.prompt("peppol-sh-deps.md")
			}
			e := f.only()
			if e.Err == nil {
				t.Fatalf("want error containing %q, got none", tc.wantErr)
			}
			if !strings.Contains(e.Err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", e.Err, tc.wantErr)
			}
			if e.ID != strings.TrimSuffix(file, ".yaml") {
				t.Errorf("entry ID = %q, want file name", e.ID)
			}
		})
	}
}

func TestValidationReportsAllProblems(t *testing.T) {
	f := newFixture(t)
	bad := strings.Replace(validTask, "target: vps", "target: nowhere", 1)
	bad = strings.Replace(bad, "timeout: 2h", "timeout: 0s", 1)
	f.task("peppol-sh-deps.yaml", bad)
	f.prompt("peppol-sh-deps.md")
	msg := f.only().Err.Error()
	if !strings.Contains(msg, "target") || !strings.Contains(msg, "timeout") {
		t.Fatalf("want both target and timeout problems in %q", msg)
	}
}

func TestLoadTasksMixesValidAndInvalidSortedByID(t *testing.T) {
	f := newFixture(t)
	f.task("peppol-sh-deps.yaml", validTask)
	f.prompt("peppol-sh-deps.md")
	f.task("aaa.yaml", "id: aaa\n")
	f.task("notes.txt", "ignored")
	entries := f.load()
	if len(entries) != 2 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if entries[0].ID != "aaa" || entries[0].Err == nil {
		t.Errorf("entries[0] = %+v, want invalid aaa", entries[0])
	}
	if entries[1].ID != "peppol-sh-deps" || entries[1].Err != nil {
		t.Errorf("entries[1] = %+v, want valid peppol-sh-deps", entries[1])
	}
}

func TestLoadTasksWithoutTasksDir(t *testing.T) {
	f := newFixture(t)
	if entries := f.load(); len(entries) != 0 {
		t.Fatalf("got %+v", entries)
	}
}

func TestLoadTasksUnreadableDirIsError(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.dir, "tasks"), "not a dir")
	if _, err := config.LoadTasks(f.dir, f.cfg); err == nil {
		t.Fatal("want error when tasks is not a directory")
	}
	_ = os.Remove(filepath.Join(f.dir, "tasks"))
}

func TestParseErrorsDoNotEchoValues(t *testing.T) {
	// yaml type errors quote the offending value; for a mistyped env block
	// that value is a secret and must not reach the log or the TUI.
	f := newFixture(t)
	f.task("peppol-sh-deps.yaml", strings.Replace(validTask, "env:\n  FOO: bar\n", "env: s3cr3t\n", 1))
	f.prompt("peppol-sh-deps.md")
	e := f.only()
	if e.Err == nil {
		t.Fatal("want parse error")
	}
	if strings.Contains(e.Err.Error(), "s3cr3t") {
		t.Fatalf("error echoes the value: %v", e.Err)
	}
	if !strings.Contains(e.Err.Error(), "line") {
		t.Errorf("want the line number kept: %v", e.Err)
	}
}
