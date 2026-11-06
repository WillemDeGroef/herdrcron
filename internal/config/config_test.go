package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingConfigGivesDefaultsWithLocalTarget(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Targets["local"]; !ok {
		t.Fatalf("local target missing: %+v", cfg.Targets)
	}
	if cfg.Timezone != "Local" {
		t.Errorf("Timezone = %q, want Local", cfg.Timezone)
	}
}

func TestLoadReadsTargetsAndKeepsLocal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
timezone: Europe/Brussels
targets:
  vps:
    host: vps
    shell: bash -lc
    max_parallel: 2
  buildbox:
    host: willem@10.0.0.12
    herdr_path: /usr/local/bin/herdr
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timezone != "Europe/Brussels" {
		t.Errorf("Timezone = %q", cfg.Timezone)
	}
	for _, name := range []string{"local", "vps", "buildbox"} {
		if _, ok := cfg.Targets[name]; !ok {
			t.Errorf("target %q missing", name)
		}
	}
	if got := cfg.Targets["vps"]; got.Host != "vps" || got.MaxParallel != 2 {
		t.Errorf("vps = %+v", got)
	}
	if got := cfg.Targets["buildbox"]; got.HerdrPath != "/usr/local/bin/herdr" || got.Shell != "bash -lc" || got.MaxParallel != 4 {
		t.Errorf("buildbox defaults not applied: %+v", got)
	}
}

func TestLoadRejectsBadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "targets: [unterminated\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("want error for malformed config.yaml")
	}
}

func TestLoadRejectsUnknownTimezone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "timezone: Mars/Olympus\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("want error for unknown timezone")
	}
}

func TestLoadHistoryDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HistoryDays != 90 {
		t.Errorf("default HistoryDays = %d, want 90", cfg.HistoryDays)
	}
	writeFile(t, path, "history_days: 7\n")
	if cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.HistoryDays != 7 {
		t.Errorf("HistoryDays = %d, want 7", cfg.HistoryDays)
	}
	writeFile(t, path, "history_days: -1\n")
	if _, err := config.Load(path); err == nil {
		t.Error("history_days -1: want error")
	}
}

func TestLoadCatchUpWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatchUpWindow.Duration != 24*time.Hour {
		t.Errorf("default CatchUpWindow = %s, want 24h", cfg.CatchUpWindow.Duration)
	}
	writeFile(t, path, "catch_up_window: 6h\n")
	if cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.CatchUpWindow.Duration != 6*time.Hour {
		t.Errorf("CatchUpWindow = %s, want 6h", cfg.CatchUpWindow.Duration)
	}
	writeFile(t, path, "catch_up_window: -1h\n")
	if _, err := config.Load(path); err == nil {
		t.Error("catch_up_window -1h: want error")
	}
}

func TestLocationFallsBackToConfigTimezone(t *testing.T) {
	cfg := config.Config{Timezone: "Europe/Brussels"}
	loc, err := cfg.Location(config.Task{})
	if err != nil || loc.String() != "Europe/Brussels" {
		t.Errorf("no task timezone: %v, %v; want Europe/Brussels", loc, err)
	}
	loc, err = cfg.Location(config.Task{Timezone: "America/New_York"})
	if err != nil || loc.String() != "America/New_York" {
		t.Errorf("task timezone: %v, %v; want America/New_York", loc, err)
	}
	if _, err := cfg.Location(config.Task{Timezone: "Mars/Olympus"}); err == nil {
		t.Error("unknown task timezone: want error")
	}
}

func TestLoadNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, `
notify:
  command: ["ntfy", "publish", "herdrcron"]
  on: [blocked, lost]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Notify.Command; len(got) != 3 || got[0] != "ntfy" || got[2] != "herdrcron" {
		t.Errorf("Notify.Command = %q, want the argv as written", got)
	}
	if got := cfg.Notify.On; len(got) != 2 || got[0] != "blocked" || got[1] != "lost" {
		t.Errorf("Notify.On = %q, want [blocked lost]", got)
	}
}

func TestLoadNotifyOnDefaultsToEveryEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "notify:\n  command: [notify-send]\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"blocked", "failed", "timeout", "lost", "target_unreachable"}
	if got := cfg.Notify.On; len(got) != len(want) {
		t.Fatalf("Notify.On = %q, want %q", got, want)
	}
	for i := range want {
		if cfg.Notify.On[i] != want[i] {
			t.Errorf("Notify.On = %q, want %q", cfg.Notify.On, want)
		}
	}
}

func TestLoadNotifyEmptyOnNotifiesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "notify:\n  command: [notify-send]\n  on: []\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notify.On) != 0 {
		t.Errorf("Notify.On = %q, want none", cfg.Notify.On)
	}
}

func TestLoadNotifyRejectsUnknownEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "notify:\n  command: [notify-send]\n  on: [blocked, done]\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load accepted notify.on: done")
	}
}

func TestLoadNotifyRejectsEmptyProgram(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "notify:\n  command: [\"\", x]\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal(`Load accepted notify.command: ["", x]`)
	}
}

func TestLoadAttachMode(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AttachMode != "pane" {
		t.Errorf("default AttachMode = %q, want pane", cfg.AttachMode)
	}
	for _, mode := range []string{"pane", "full"} {
		path := filepath.Join(dir, mode+".yaml")
		writeFile(t, path, "attach_mode: "+mode+"\n")
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("attach_mode %s: %v", mode, err)
		}
		if cfg.AttachMode != mode {
			t.Errorf("AttachMode = %q, want %q", cfg.AttachMode, mode)
		}
	}
}

func TestLoadRejectsUnknownAttachMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "attach_mode: window\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("want error for attach_mode: window")
	}
}
