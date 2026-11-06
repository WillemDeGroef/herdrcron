// Package config loads config.yaml and the task files under the herdrcron
// config directory, and validates tasks.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// LocalTarget is the name of the target that runs herdr on the daemon's own
// host. It always exists and needs no entry in config.yaml.
const LocalTarget = "local"

// Config is the content of config.yaml. Later tickets add the remaining
// global settings (poll_interval, notify, ...).
type Config struct {
	Timezone string            `yaml:"timezone"`
	Targets  map[string]Target `yaml:"targets"`
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
