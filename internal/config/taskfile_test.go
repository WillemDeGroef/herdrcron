package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/config"
)

const commentedTask = `# Weekly dependency update.
id: peppol-sh-deps
name: peppol.sh dependency update # shown in list
target: vps
schedule: "0 7 * * 1"
enabled: true
agent:
  command: claude
  # Accept edits without asking.
  args: ["--permission-mode", "acceptEdits"]
prompt_file: prompts/peppol-sh-deps.md
timeout: 2h
env:
  FOO: bar
  TOKEN: "s3cret"
`

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetEnabledKeepsCommentsAndOtherKeys(t *testing.T) {
	f := newFixture(t)
	f.task("peppol-sh-deps.yaml", commentedTask)
	f.prompt("peppol-sh-deps.md")
	path := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")

	if err := config.SetEnabled(path, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	got := readFile(t, path)
	want := strings.Replace(commentedTask, "enabled: true", "enabled: false", 1)
	if got != want {
		t.Errorf("file after SetEnabled:\n%s\nwant:\n%s", got, want)
	}
	e := config.LoadTask(f.dir, "peppol-sh-deps", f.cfg)
	if e.Err != nil {
		t.Fatalf("LoadTask: %v", e.Err)
	}
	if e.Task.Enabled {
		t.Error("Enabled = true, want false")
	}
	if e.Task.Env["TOKEN"] != "s3cret" || len(e.Task.Agent.Args) != 2 {
		t.Errorf("other keys changed: %+v", e.Task)
	}
}

func TestSetPausedUntil(t *testing.T) {
	f := newFixture(t)
	f.task("peppol-sh-deps.yaml", commentedTask) // has no paused_until key
	f.prompt("peppol-sh-deps.md")
	path := filepath.Join(f.dir, "tasks", "peppol-sh-deps.yaml")

	for _, until := range []string{"indefinite", "2026-11-07T08:00:00+01:00", "2026-11-07T07:00:00.5Z", ""} {
		if err := config.SetPausedUntil(path, until); err != nil {
			t.Fatalf("SetPausedUntil(%q): %v", until, err)
		}
		e := config.LoadTask(f.dir, "peppol-sh-deps", f.cfg)
		if e.Err != nil {
			t.Fatalf("LoadTask after SetPausedUntil(%q): %v", until, e.Err)
		}
		if e.Task.PausedUntil != until {
			t.Errorf("PausedUntil = %q, want %q", e.Task.PausedUntil, until)
		}
	}
	got := readFile(t, path)
	if want := commentedTask + "paused_until: null\n"; got != want {
		t.Errorf("file after SetPausedUntil(\"\"):\n%s\nwant:\n%s", got, want)
	}
}

func TestSetKeyRefusesNonMapping(t *testing.T) {
	for name, content := range map[string]string{
		"sequence": "- id: x\n",
		"scalar":   "just text\n",
		"empty":    "",
		"invalid":  "id: [unclosed\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.yaml")
			writeFile(t, path, content)
			if err := config.SetEnabled(path, false); err == nil {
				t.Error("SetEnabled: no error")
			}
			if err := config.SetPausedUntil(path, "indefinite"); err == nil {
				t.Error("SetPausedUntil: no error")
			}
			if got := readFile(t, path); got != content {
				t.Errorf("file changed to %q", got)
			}
		})
	}
}

func TestSetPausedUntilRefusesBadValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.yaml")
	writeFile(t, path, "id: x\n")
	if err := config.SetPausedUntil(path, "tomorrow"); err == nil {
		t.Error("no error")
	}
	if got := readFile(t, path); got != "id: x\n" {
		t.Errorf("file changed to %q", got)
	}
}

func TestSetKeepsModeAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.yaml")
	writeFile(t, path, "id: x\n")
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := config.SetEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPausedUntil(path, ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Errorf("directory holds %v, want only x.yaml", names)
	}
	if got, want := readFile(t, path), "id: x\nenabled: false\npaused_until: null\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}
