package service_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/WillemDeGroef/herdrcron/internal/paths"
	"github.com/WillemDeGroef/herdrcron/internal/service"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []call
	fail  map[string]error // keyed by "name arg1 arg2..."
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, call{name, args})
	return f.fail[strings.Join(append([]string{name}, args...), " ")]
}

func (f *fakeRunner) commands() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(append([]string{c.name}, c.args...), " "))
	}
	return out
}

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func linuxOpts(t *testing.T, env map[string]string) (service.Options, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	if env == nil {
		env = map[string]string{}
	}
	env["HOME"] = home
	if _, ok := env["PATH"]; !ok {
		env["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	}
	p, err := paths.Resolve(envFunc(env))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{}
	out := &bytes.Buffer{}
	return service.Options{
		GOOS:   "linux",
		Binary: "/usr/local/bin/herdrcron",
		Home:   home,
		Getenv: envFunc(env),
		Paths:  p,
		UID:    1000,
		User:   "willem",
		Run:    fr.run,
		Out:    out,
	}, fr, out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLinuxWritesUnitUnderHomeConfig(t *testing.T) {
	o, _, _ := linuxOpts(t, nil)
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(o.Home, ".config", "systemd", "user", "herdrcron.service")
	if unit != want {
		t.Fatalf("unit path = %q, want %q", unit, want)
	}
	st, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("unit mode = %v, want 0644", st.Mode().Perm())
	}
}

func TestLinuxUnitContent(t *testing.T) {
	o, _, _ := linuxOpts(t, nil)
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, unit)
	for _, line := range []string{
		"[Unit]",
		"Description=herdrcron daemon",
		"After=network-online.target",
		"Wants=network-online.target",
		"[Service]",
		"Type=simple",
		"ExecStart=/usr/local/bin/herdrcron daemon",
		"Restart=on-failure",
		"RestartSec=5",
		`Environment="PATH=/usr/local/bin:/usr/bin:/bin"`,
		"[Install]",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("unit lacks line %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "XDG_") {
		t.Errorf("unit sets XDG vars that are not in the environment:\n%s", got)
	}
}

func TestLinuxRespectsXDGConfigHomeAndPassesXDGVars(t *testing.T) {
	cfg := t.TempDir()
	state := t.TempDir()
	o, _, _ := linuxOpts(t, map[string]string{"XDG_CONFIG_HOME": cfg, "XDG_STATE_HOME": state})
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "systemd", "user", "herdrcron.service"); unit != want {
		t.Fatalf("unit path = %q, want %q", unit, want)
	}
	got := readFile(t, unit)
	for _, line := range []string{
		`Environment="XDG_CONFIG_HOME=` + cfg + `"`,
		`Environment="XDG_STATE_HOME=` + state + `"`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("unit lacks line %q:\n%s", line, got)
		}
	}
}

func TestLinuxIgnoresRelativeXDGConfigHome(t *testing.T) {
	o, _, _ := linuxOpts(t, map[string]string{"XDG_CONFIG_HOME": "rel/cfg"})
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(o.Home, ".config", "systemd", "user", "herdrcron.service"); unit != want {
		t.Fatalf("unit path = %q, want %q", unit, want)
	}
}

func TestLinuxEscapesEnvironmentAndExecStart(t *testing.T) {
	o, _, _ := linuxOpts(t, map[string]string{"PATH": `/opt/a b/bin:/x"y\z:/100%`})
	o.Binary = "/opt/my apps/herdr$cron%"
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, unit)
	for _, line := range []string{
		`Environment="PATH=/opt/a b/bin:/x\"y\\z:/100%%"`,
		`ExecStart="/opt/my apps/herdr$$cron%%" daemon`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("unit lacks line %q:\n%s", line, got)
		}
	}
}

func TestLinuxOverwritesExistingUnit(t *testing.T) {
	o, _, _ := linuxOpts(t, nil)
	if _, err := service.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Binary = "/new/herdrcron"
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, unit)
	if !strings.Contains(got, "ExecStart=/new/herdrcron daemon\n") || strings.Contains(got, "/usr/local/bin/herdrcron") {
		t.Errorf("unit not rewritten:\n%s", got)
	}
}

func TestLinuxRunsSystemctlInOrder(t *testing.T) {
	o, fr, _ := linuxOpts(t, nil)
	if _, err := service.Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable herdrcron.service",
		"systemctl --user restart herdrcron.service",
	}
	if got := fr.commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestLinuxPrintsUnitPathAndLingerHint(t *testing.T) {
	o, _, out := linuxOpts(t, nil)
	unit, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{unit, "loginctl enable-linger willem"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
}

func TestLinuxRunnerFailureIsReturned(t *testing.T) {
	o, fr, _ := linuxOpts(t, nil)
	boom := errors.New("boom")
	fr.fail = map[string]error{"systemctl --user enable herdrcron.service": boom}
	_, err := service.Install(context.Background(), o)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping %v", err, boom)
	}
	if len(fr.calls) != 2 {
		t.Errorf("ran %d commands after failure, want stop after 2: %q", len(fr.calls), fr.commands())
	}
}

func TestUnsupportedGOOS(t *testing.T) {
	o, fr, _ := linuxOpts(t, nil)
	o.GOOS = "windows"
	_, err := service.Install(context.Background(), o)
	if err == nil || err.Error() != "install-service supports linux (systemd) and macOS (launchd)" {
		t.Fatalf("err = %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ran commands: %q", fr.commands())
	}
}

func darwinOpts(t *testing.T, env map[string]string) (service.Options, *fakeRunner, *bytes.Buffer) {
	o, fr, out := linuxOpts(t, env)
	o.GOOS = "darwin"
	o.UID = 501
	return o, fr, out
}

type plistDoc struct {
	Dict struct {
		Inner []byte `xml:",innerxml"`
	} `xml:"dict"`
}

func TestDarwinWritesPlist(t *testing.T) {
	o, _, out := darwinOpts(t, nil)
	plist, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(o.Home, "Library", "LaunchAgents", "io.github.willemdegroef.herdrcron.plist")
	if plist != want {
		t.Fatalf("plist path = %q, want %q", plist, want)
	}
	got := readFile(t, plist)
	var doc plistDoc
	if err := xml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, got)
	}
	compact := strings.Join(strings.Fields(got), "")
	for _, s := range []string{
		"<key>Label</key><string>io.github.willemdegroef.herdrcron</string>",
		"<key>ProgramArguments</key><array><string>/usr/local/bin/herdrcron</string><string>daemon</string></array>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
		"<key>EnvironmentVariables</key><dict><key>PATH</key><string>/usr/local/bin:/usr/bin:/bin</string></dict>",
		"<key>StandardOutPath</key><string>" + filepath.Join(o.Paths.StateDir, "launchd.out.log") + "</string>",
		"<key>StandardErrorPath</key><string>" + filepath.Join(o.Paths.StateDir, "launchd.err.log") + "</string>",
	} {
		if !strings.Contains(compact, s) {
			t.Errorf("plist lacks %q:\n%s", s, got)
		}
	}
	if _, err := os.Stat(o.Paths.StateDir); err != nil {
		t.Errorf("state dir not created: %v", err)
	}
	if !strings.Contains(out.String(), plist) {
		t.Errorf("output lacks plist path:\n%s", out.String())
	}
}

func TestDarwinPassesXDGVars(t *testing.T) {
	cfg, state := t.TempDir(), t.TempDir()
	o, _, _ := darwinOpts(t, map[string]string{"XDG_CONFIG_HOME": cfg, "XDG_STATE_HOME": state})
	plist, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	compact := strings.Join(strings.Fields(readFile(t, plist)), "")
	for _, s := range []string{
		"<key>XDG_CONFIG_HOME</key><string>" + cfg + "</string>",
		"<key>XDG_STATE_HOME</key><string>" + state + "</string>",
	} {
		if !strings.Contains(compact, s) {
			t.Errorf("plist lacks %q", s)
		}
	}
}

func TestDarwinEscapesXML(t *testing.T) {
	o, _, _ := darwinOpts(t, map[string]string{"PATH": "/a&b:/c<d"})
	o.Binary = "/Apps/R&D <x>/herdrcron"
	plist, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, plist)
	if err := xml.Unmarshal([]byte(got), new(plistDoc)); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, got)
	}
	for _, s := range []string{"/Apps/R&amp;D &lt;x&gt;/herdrcron", "/a&amp;b:/c&lt;d"} {
		if !strings.Contains(got, s) {
			t.Errorf("plist lacks escaped %q:\n%s", s, got)
		}
	}
}

func TestDarwinRunsLaunchctlAndIgnoresBootoutError(t *testing.T) {
	o, fr, _ := darwinOpts(t, nil)
	fr.fail = map[string]error{"launchctl bootout gui/501/io.github.willemdegroef.herdrcron": errors.New("not loaded")}
	plist, err := service.Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"launchctl bootout gui/501/io.github.willemdegroef.herdrcron",
		"launchctl bootstrap gui/501 " + plist,
	}
	if got := fr.commands(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestDarwinBootstrapFailureIsReturned(t *testing.T) {
	o, fr, _ := darwinOpts(t, nil)
	boom := errors.New("boom")
	plistPath := filepath.Join(o.Home, "Library", "LaunchAgents", "io.github.willemdegroef.herdrcron.plist")
	fr.fail = map[string]error{"launchctl bootstrap gui/501 " + plistPath: boom}
	if _, err := service.Install(context.Background(), o); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping %v", err, boom)
	}
}

func TestExecRunnerIncludesOutputInError(t *testing.T) {
	err := service.ExecRunner(context.Background(), "sh", "-c", "echo oops >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("err = %v, want it to contain the command output", err)
	}
	if err := service.ExecRunner(context.Background(), "sh", "-c", "exit 0"); err != nil {
		t.Fatalf("success case: %v", err)
	}
}
