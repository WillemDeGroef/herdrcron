// Package config loads config.yaml and the task files under the herdrcron
// config directory, and validates tasks.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LocalTarget is the name of the target that runs herdr on the daemon's own
// host. It always exists and needs no entry in config.yaml.
const LocalTarget = "local"

// Config is the content of config.yaml.
type Config struct {
	Timezone string `yaml:"timezone"`
	// HistoryDays is how long run history is kept; default 90.
	HistoryDays int `yaml:"history_days"`
	// PollInterval is how often active runs are polled; default 15s.
	PollInterval Duration `yaml:"poll_interval"`
	// CatchUpWindow is how old a missed fire may be and still be caught
	// up (missed: run_once); default 24h.
	CatchUpWindow Duration          `yaml:"catch_up_window"`
	Notify        Notify            `yaml:"notify"`
	Targets       map[string]Target `yaml:"targets"`
}

// Notify is the notify section of config.yaml.
type Notify struct {
	// Command is the argv of the notify command; the message is appended
	// as one more argument. Empty: no notifications.
	Command []string `yaml:"command"`
	// On lists the events that notify; omitted means every event.
	On []string `yaml:"on"`
}

// Notify events: the run states that notify, and a target that stops
// answering.
const (
	EventBlocked           = "blocked"
	EventFailed            = "failed"
	EventTimeout           = "timeout"
	EventLost              = "lost"
	EventTargetUnreachable = "target_unreachable"
)

// NotifyEvents are the valid notify.on values, in the spec's order.
var NotifyEvents = []string{EventBlocked, EventFailed, EventTimeout, EventLost, EventTargetUnreachable}

// DefaultHistoryDays is the history_days default.
const DefaultHistoryDays = 90

// DefaultPollInterval is the poll_interval default.
const DefaultPollInterval = 15 * time.Second

// DefaultCatchUpWindow is the catch_up_window default.
const DefaultCatchUpWindow = 24 * time.Hour

// Location is the time zone of task t: its timezone field, or the config
// timezone when the task has none.
func (c Config) Location(t Task) (*time.Location, error) {
	name := t.Timezone
	if name == "" {
		name = c.Timezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", name, err)
	}
	return loc, nil
}

// Target is one machine herdrcron starts runs on.
type Target struct {
	Host        string `yaml:"host"`
	Shell       string `yaml:"shell"`
	HerdrPath   string `yaml:"herdr_path"`
	MaxParallel int    `yaml:"max_parallel"`
}

// Load reads config.yaml at path. A missing file gives the defaults. The
// "local" target is always present.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("read config: %w", err)
	default:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Local"
	}
	switch {
	case cfg.HistoryDays == 0:
		cfg.HistoryDays = DefaultHistoryDays
	case cfg.HistoryDays < 0:
		return Config{}, fmt.Errorf("history_days must be > 0, got %d", cfg.HistoryDays)
	}
	switch {
	case cfg.PollInterval.Duration == 0:
		cfg.PollInterval.Duration = DefaultPollInterval
	case cfg.PollInterval.Duration < 0:
		return Config{}, fmt.Errorf("poll_interval must be > 0, got %s", cfg.PollInterval.Duration)
	}
	switch {
	case cfg.CatchUpWindow.Duration == 0:
		cfg.CatchUpWindow.Duration = DefaultCatchUpWindow
	case cfg.CatchUpWindow.Duration < 0:
		return Config{}, fmt.Errorf("catch_up_window must be > 0, got %s", cfg.CatchUpWindow.Duration)
	}
	if cfg.Notify.On == nil {
		cfg.Notify.On = slices.Clone(NotifyEvents)
	}
	for _, ev := range cfg.Notify.On {
		if !slices.Contains(NotifyEvents, ev) {
			return Config{}, fmt.Errorf("notify.on: unknown event %q (want one of %s)", ev, strings.Join(NotifyEvents, ", "))
		}
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return Config{}, fmt.Errorf("config timezone %q: %w", cfg.Timezone, err)
	}
	targets := make(map[string]Target, len(cfg.Targets)+1)
	for name, t := range cfg.Targets {
		if t.Host == "" {
			t.Host = name
		}
		if t.Shell == "" {
			t.Shell = "bash -lc"
		}
		if t.MaxParallel <= 0 {
			t.MaxParallel = 4
		}
		targets[name] = t
	}
	local := targets[LocalTarget]
	if local.MaxParallel <= 0 {
		local.MaxParallel = 4
	}
	targets[LocalTarget] = local
	cfg.Targets = targets
	return cfg, nil
}
