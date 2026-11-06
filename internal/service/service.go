// Package service installs herdrcron as a per-user background service: a
// systemd user unit on Linux, a launchd agent on macOS. All side effects
// outside the filesystem go through an injected Runner so the logic is
// testable without touching the real service manager.
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/WillemDeGroef/herdrcron/internal/paths"
)

const (
	unitName    = "herdrcron.service"
	launchLabel = "io.github.willemdegroef.herdrcron"
)

// Runner runs an external command (systemctl, launchctl) and returns an
// error if it fails.
type Runner func(ctx context.Context, name string, args ...string) error

// ExecRunner is the production Runner. On failure the command's combined
// output is included in the error.
func ExecRunner(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		cmd := strings.Join(append([]string{name}, args...), " ")
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}
		return fmt.Errorf("%s: %w", cmd, err)
	}
	return nil
}

// Options describes the environment Install works in.
type Options struct {
	GOOS   string              // "linux" or "darwin"
	Binary string              // absolute path to the herdrcron executable
	Home   string              // $HOME
	Getenv func(string) string // environment lookup (os.Getenv in production)
	Paths  paths.Paths
	UID    int    // for launchctl gui/<uid>
	User   string // for the loginctl enable-linger hint
	Run    Runner
	Out    io.Writer // messages for the user
}

// Install writes the service definition for o.GOOS, (re)loads it so it runs
// the given binary now and at every login, and returns the path written.
// It is idempotent: a reinstall overwrites the file and restarts the service.
func Install(ctx context.Context, o Options) (string, error) {
	switch o.GOOS {
	case "linux":
		return installSystemd(ctx, o)
	case "darwin":
		return installLaunchd(ctx, o)
	default:
		return "", errors.New("install-service supports linux (systemd) and macOS (launchd)")
	}
}

// passedEnv lists the variables copied into the service environment: PATH
// so the daemon finds herdr and ssh (service managers start with a minimal
// PATH), and the XDG dirs so it uses the same config and state as the CLI.
var passedEnv = []string{"PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME"}

type envVar struct{ key, value string }

func serviceEnv(getenv func(string) string) []envVar {
	var env []envVar
	for _, k := range passedEnv {
		if v := getenv(k); v != "" {
			env = append(env, envVar{k, v})
		}
	}
	return env
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return err
	}
	// WriteFile keeps the mode of an existing file; force it.
	return os.Chmod(path, 0o644)
}

// --- systemd ---

func systemdUnitPath(o Options) string {
	cfg := o.Getenv("XDG_CONFIG_HOME")
	if cfg == "" || !filepath.IsAbs(cfg) {
		cfg = filepath.Join(o.Home, ".config")
	}
	return filepath.Join(cfg, "systemd", "user", unitName)
}

func systemdUnit(o Options) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=herdrcron daemon\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s daemon\n", systemdExecArg(o.Binary))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	for _, e := range serviceEnv(o.Getenv) {
		fmt.Fprintf(&b, "Environment=\"%s\"\n", systemdQuoteBody(e.key+"="+e.value))
	}
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// systemdQuoteBody escapes s for use inside a double-quoted systemd value:
// backslash and double quote are backslash-escaped, % (specifier) is doubled.
func systemdQuoteBody(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s)
}

// systemdExecArg renders one ExecStart argument: $ (variable expansion) and
// % (specifiers) are doubled, and the word is quoted if it holds whitespace,
// quotes or backslashes.
func systemdExecArg(s string) string {
	s = strings.ReplaceAll(s, "$", "$$")
	if strings.ContainsAny(s, " \t\"'\\") {
		return `"` + systemdQuoteBody(s) + `"`
	}
	return strings.ReplaceAll(s, "%", "%%")
}

// installSystemd writes the user unit and runs daemon-reload, enable, and
// restart. restart (rather than enable --now) also starts a stopped service
// and makes a reinstall pick up a new binary or unit.
func installSystemd(ctx context.Context, o Options) (string, error) {
	unit := systemdUnitPath(o)
	if err := writeFile(unit, []byte(systemdUnit(o))); err != nil {
		return "", fmt.Errorf("writing systemd unit: %w", err)
	}
	fmt.Fprintf(o.Out, "Wrote %s\n", unit)
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", unitName},
		{"--user", "restart", unitName},
	} {
		if err := o.Run(ctx, "systemctl", args...); err != nil {
			return unit, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}
	fmt.Fprintf(o.Out, "Enabled and started %s.\n", unitName)
	fmt.Fprintf(o.Out, "The service stops when you log out unless lingering is enabled:\n  loginctl enable-linger %s\n", o.User)
	return unit, nil
}

// --- launchd ---

func launchdPlistPath(o Options) string {
	return filepath.Join(o.Home, "Library", "LaunchAgents", launchLabel+".plist")
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // writes to a bytes.Buffer cannot fail
	return b.String()
}

func launchdPlist(o Options) string {
	str := func(s string) string { return "<string>" + xmlEscape(s) + "</string>" }
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t%s\n", str(launchLabel))
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t%s\n\t\t%s\n\t</array>\n", str(o.Binary), str("daemon"))
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	if env := serviceEnv(o.Getenv); len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, e := range env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t%s\n", xmlEscape(e.key), str(e.value))
		}
		b.WriteString("\t</dict>\n")
	}
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t%s\n", str(filepath.Join(o.Paths.StateDir, "launchd.out.log")))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t%s\n", str(filepath.Join(o.Paths.StateDir, "launchd.err.log")))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// installLaunchd writes the agent plist and reloads it: bootout (whose error
// is ignored, as the agent may not be loaded yet) then bootstrap.
func installLaunchd(ctx context.Context, o Options) (string, error) {
	if err := os.MkdirAll(o.Paths.StateDir, 0o755); err != nil {
		return "", fmt.Errorf("creating state directory: %w", err)
	}
	plist := launchdPlistPath(o)
	if err := writeFile(plist, []byte(launchdPlist(o))); err != nil {
		return "", fmt.Errorf("writing launchd plist: %w", err)
	}
	fmt.Fprintf(o.Out, "Wrote %s\n", plist)
	domain := fmt.Sprintf("gui/%d", o.UID)
	_ = o.Run(ctx, "launchctl", "bootout", domain+"/"+launchLabel)
	if err := o.Run(ctx, "launchctl", "bootstrap", domain, plist); err != nil {
		return plist, fmt.Errorf("launchctl bootstrap: %w", err)
	}
	fmt.Fprintf(o.Out, "Loaded %s; launchd keeps it running and starts it at login.\n", launchLabel)
	return plist, nil
}
