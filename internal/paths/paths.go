// Package paths resolves herdrcron's config and state directories from the
// XDG base directory variables.
package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// Paths holds the herdrcron config and state directories.
type Paths struct {
	ConfigDir string // $XDG_CONFIG_HOME/herdrcron, default ~/.config/herdrcron
	StateDir  string // $XDG_STATE_HOME/herdrcron, default ~/.local/state/herdrcron
}

// Resolve computes the directories from the environment lookup getenv
// (os.Getenv in production). Relative XDG values are ignored, as the XDG
// spec requires.
func Resolve(getenv func(string) string) (Paths, error) {
	cfg, err := xdgDir(getenv, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Paths{}, err
	}
	state, err := xdgDir(getenv, "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		ConfigDir: filepath.Join(cfg, "herdrcron"),
		StateDir:  filepath.Join(state, "herdrcron"),
	}, nil
}

// FromEnv resolves the directories from the process environment.
func FromEnv() (Paths, error) { return Resolve(os.Getenv) }

func xdgDir(getenv func(string) string, key, homeRel string) (string, error) {
	if v := getenv(key); v != "" && filepath.IsAbs(v) {
		return v, nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("cannot find config and state directories: neither $" + key + " nor $HOME is set")
	}
	return filepath.Join(home, homeRel), nil
}

// ConfigFile is config.yaml in the config directory.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.yaml") }

// TasksDir holds one <task-id>.yaml file per task.
func (p Paths) TasksDir() string { return filepath.Join(p.ConfigDir, "tasks") }

// Socket is the daemon's JSON-RPC Unix socket.
func (p Paths) Socket() string { return filepath.Join(p.StateDir, "daemon.sock") }

// Log is the daemon's JSON log file.
func (p Paths) Log() string { return filepath.Join(p.StateDir, "daemon.log") }

// SSHControlDir holds the ssh ControlMaster sockets of the SSH targets.
func (p Paths) SSHControlDir() string { return filepath.Join(p.StateDir, "ssh") }

// DB is the daemon's SQLite state database.
func (p Paths) DB() string { return filepath.Join(p.StateDir, "herdrcron.db") }
