package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// sample is one scripted answer for a pane in `herdr pane list`.
type sample struct {
	status herdr.AgentStatus
	agent  string // "" = no agent detected
	gone   bool   // the pane no longer exists
	// atPrompt is what `pane process-info` says while this sample is
	// current: the shell's own process group is in the foreground.
	atPrompt bool
}

var (
	starting = sample{status: herdr.AgentStatusUnknown}
	working  = sample{status: herdr.AgentStatusWorking, agent: "claude"}
	blocked  = sample{status: herdr.AgentStatusBlocked, agent: "claude"}
	idle     = sample{status: herdr.AgentStatusIdle, agent: "claude"}
	doneS    = sample{status: herdr.AgentStatusDone, agent: "claude"}
	vanished = sample{gone: true}
	shell    = sample{status: herdr.AgentStatusUnknown, atPrompt: true}
)

type fakePane struct {
	id, tab, ws string
	terminal    string // changes when a restarted server restores the pane
	script      []sample
	polls       int // pane lists since pane run
	command     string
	closed      bool
	// interrupted: ctrl+c reached the agent, which exited to the shell.
	interrupted bool
}

// current is the sample of the latest pane list; the last sample repeats.
func (p *fakePane) current() sample {
	switch {
	case p.closed:
		return vanished
	case p.interrupted:
		return shell
	case p.polls == 0 || len(p.script) == 0:
		return starting
	}
	return p.script[min(p.polls, len(p.script))-1]
}

// fakeHerdr is a target.Target that plays herdr 0.9.1: it answers the
// argv the herdr client sends with JSON built from the generated types.
// Every `pane list` advances each pane's script by one sample; the first
// list after `pane run` shows the first sample.
type fakeHerdr struct {
	name string
	mu   sync.Mutex

	calls      [][]string
	files      map[string][]byte
	workspaces map[string]string // label -> id
	tabs       map[string]string // tab id -> workspace id
	tabLabels  map[string]string // tab id -> label
	panes      map[string]*fakePane
	order      []string // pane ids in creation order
	scripts    [][]sample
	nextID     int
	terminals  int

	serverDown  bool // herdr calls fail with server_not_running
	unreachable bool // herdr calls fail as an ssh target that does not answer
	// unreachableFor makes only this herdr command ("tab close") fail so.
	unreachableFor string
	serverStarts   bool   // starting the server brings it up
	preflight      string // stderr line of a failing preflight; "" = ok
	resolvedCwd    string // what the preflight prints; "" = the cwd as given
	output         string // pane read text
	failRun        bool   // pane run fails
	// loseRunReply: pane run types the command, then the target drops
	// before the reply arrives and stays unreachable.
	loseRunReply bool
	// dropBeforeRun: the target drops before pane run reaches it and stays
	// unreachable; nothing is typed into the pane.
	dropBeforeRun bool
	failPut       bool   // PutFile fails
	version       string // `herdr --version` prints "herdr <version>"; "" = 0.9.1
	// hostEnv, when set, makes the shell-script calls (preflight and
	// target check) run for real in a local sh with this environment
	// (a temp HOME and PATH), instead of the canned answers above.
	hostEnv []string
}

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{
		name: "local", files: map[string][]byte{}, workspaces: map[string]string{},
		tabs: map[string]string{}, tabLabels: map[string]string{}, panes: map[string]*fakePane{},
	}
}

// script queues the status sequence for the next pane that is created.
func (f *fakeHerdr) script(s ...sample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, s)
}

func (f *fakeHerdr) Name() string { return f.name }

func (f *fakeHerdr) PutFile(_ context.Context, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut {
		return fmt.Errorf("put failed")
	}
	f.files[path] = slices.Clone(data)
	return nil
}

func (f *fakeHerdr) RemoveFile(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return &target.UnreachableError{Target: f.name, Stderr: []byte("ssh: connect to host: Connection refused\n")}
	}
	delete(f.files, path)
	return nil
}

func (f *fakeHerdr) Files() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.files)
}

func (f *fakeHerdr) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// count returns how many calls started with prefix.
func (f *fakeHerdr) count(prefix ...string) int {
	n := 0
	for _, c := range f.Calls() {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			n++
		}
	}
	return n
}

func (f *fakeHerdr) pane(i int) *fakePane {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.panes[f.order[i]]
}

func (f *fakeHerdr) tabOpen(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tabs[id]
	return ok
}

func herdrErr(code string) error {
	b, _ := json.Marshal(herdr.ErrorResponse{Error: herdr.ErrorBody{Code: code, Message: code}, ID: "cli"})
	return &target.ExitError{Code: 1, Stderr: b}
}

func ok[T any](v T) ([]byte, error) {
	return json.Marshal(herdr.Response[T]{ID: "cli", Result: v})
}

func (f *fakeHerdr) Exec(_ context.Context, argv []string, _ io.Reader) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(argv))
	if argv[0] != "herdr" {
		return f.preflightCheck(argv)
	}
	cmd := strings.Join(argv[1:min(3, len(argv))], " ")
	if f.unreachable || (f.unreachableFor != "" && f.unreachableFor == cmd) {
		return nil, &target.UnreachableError{Target: f.name, Stderr: []byte("ssh: connect to host: Connection refused\n")}
	}
	if cmd == "--version" {
		v := f.version
		if v == "" {
			v = "0.9.1"
		}
		return []byte("herdr " + v + "\n"), nil
	}
	if f.serverDown {
		return nil, herdrErr(herdr.CodeServerNotRunning)
	}
	switch cmd {
	case "pane list":
		var list []herdr.PaneInfo
		for _, id := range f.order {
			p := f.panes[id]
			if p.command != "" {
				p.polls++
			}
			s := p.current()
			if s.gone {
				continue
			}
			info := herdr.PaneInfo{PaneID: p.id, TabID: p.tab, WorkspaceID: p.ws, TerminalID: p.terminal, AgentStatus: s.status}
			if s.agent != "" {
				info.Agent = &s.agent
			}
			list = append(list, info)
		}
		return ok(herdr.PaneListResult{Type: "pane_list", Panes: list})
	case "workspace list":
		var list []herdr.WorkspaceInfo
		for label, id := range f.workspaces {
			list = append(list, herdr.WorkspaceInfo{WorkspaceID: id, Label: label})
		}
		return ok(herdr.WorkspaceListResult{Type: "workspace_list", Workspaces: list})
	case "workspace create":
		label := flag(argv, "--label")
		f.nextID++
		ws := fmt.Sprintf("w%d", f.nextID)
		f.workspaces[label] = ws
		tab, pane := f.newTab(ws, "1")
		return ok(herdr.WorkspaceCreatedResult{
			Type: "workspace_created", Workspace: herdr.WorkspaceInfo{WorkspaceID: ws, Label: label},
			Tab: herdr.TabInfo{TabID: tab, WorkspaceID: ws, Label: "1"}, RootPane: pane,
		})
	case "tab create":
		ws := flag(argv, "--workspace")
		if !slices.Contains(slices.Collect(maps.Values(f.workspaces)), ws) {
			return nil, herdrErr(herdr.CodeWorkspaceNotFound)
		}
		label := flag(argv, "--label")
		tab, pane := f.newTab(ws, label)
		return ok(herdr.TabCreatedResult{
			Type: "tab_created", Tab: herdr.TabInfo{TabID: tab, WorkspaceID: ws, Label: label}, RootPane: pane,
		})
	case "tab list":
		ws := flag(argv, "--workspace")
		list := []herdr.TabInfo{}
		for id, w := range f.tabs {
			if w == ws {
				list = append(list, herdr.TabInfo{TabID: id, WorkspaceID: w, Label: f.tabLabels[id]})
			}
		}
		return ok(herdr.TabListResult{Type: "tab_list", Tabs: list})
	case "tab rename":
		f.tabLabels[argv[3]] = argv[4]
		return ok(herdr.TabInfoResult{Type: "tab_info", Tab: herdr.TabInfo{TabID: argv[3], Label: argv[4]}})
	case "tab close":
		id := argv[3]
		if _, found := f.tabs[id]; !found {
			return nil, herdrErr(herdr.CodeTabNotFound)
		}
		delete(f.tabs, id)
		for _, p := range f.panes {
			if p.tab == id {
				p.closed = true
			}
		}
		return ok(herdr.OkResult{Type: "ok"})
	case "pane run":
		p, found := f.panes[argv[3]]
		if !found {
			return nil, herdrErr(herdr.CodePaneNotFound)
		}
		if f.failRun {
			return nil, &target.ExitError{Code: 1, Stderr: []byte("boom")}
		}
		if f.dropBeforeRun {
			f.dropBeforeRun, f.unreachable = false, true
			return nil, &target.UnreachableError{Target: f.name, Stderr: []byte("ssh: connect to host: timed out\n")}
		}
		p.command = argv[4]
		if f.loseRunReply {
			f.loseRunReply, f.unreachable = false, true
			return nil, &target.UnreachableError{Target: f.name, Stderr: []byte("ssh: connection reset\n")}
		}
		return nil, nil
	case "pane send-keys":
		p, found := f.panes[argv[3]]
		if !found {
			return nil, herdrErr(herdr.CodePaneNotFound)
		}
		if slices.Contains(argv[4:], "ctrl+c") {
			p.interrupted = true
		}
		return nil, nil
	case "pane read":
		if p, found := f.panes[argv[3]]; !found || p.closed {
			return nil, herdrErr(herdr.CodePaneNotFound)
		}
		return []byte(f.output), nil
	case "pane process-info":
		p, found := f.panes[flag(argv, "--pane")]
		if !found {
			return nil, herdrErr(herdr.CodePaneNotFound)
		}
		pid, fg := uint32(100), uint32(200)
		// A pane nothing was typed into is a shell at its prompt.
		if p.command == "" || p.current().atPrompt {
			fg = pid
		}
		return ok(herdr.PaneProcessInfoResult{Type: "pane_process_info", ProcessInfo: herdr.PaneProcessInfo{
			PaneID: p.id, ShellPID: &pid, ForegroundProcessGroupID: &fg,
		}})
	}
	return nil, fmt.Errorf("fake herdr: unexpected argv %q", argv)
}

// newTab creates a tab with a root pane that plays the next queued script.
func (f *fakeHerdr) newTab(ws, label string) (string, herdr.PaneInfo) {
	f.nextID++
	tab := fmt.Sprintf("%s:t%d", ws, f.nextID)
	pane := fmt.Sprintf("%s:p%d", ws, f.nextID)
	f.tabs[tab] = ws
	f.tabLabels[tab] = label
	p := &fakePane{id: pane, tab: tab, ws: ws, terminal: f.newTerminal()}
	if len(f.scripts) > 0 {
		p.script, f.scripts = f.scripts[0], f.scripts[1:]
	}
	f.panes[pane] = p
	f.order = append(f.order, pane)
	return tab, herdr.PaneInfo{PaneID: pane, TabID: tab, WorkspaceID: ws, TerminalID: p.terminal, AgentStatus: herdr.AgentStatusUnknown}
}

func (f *fakeHerdr) newTerminal() string {
	f.terminals++
	return fmt.Sprintf("term_%d", f.terminals)
}

// restartServer plays a herdr server that crashed and was started again,
// as herdr 0.9.1 does it: the session is restored, so every workspace, tab
// and pane keeps its id, but each pane is a new shell with a new terminal
// id and the processes that ran in it are gone.
func (f *fakeHerdr) restartServer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.panes {
		if p.closed {
			continue
		}
		p.terminal = f.newTerminal()
		p.script, p.polls, p.command, p.interrupted = nil, 0, "", false
	}
}

// preflightCheck answers the shell script that checks the agent command and
// cwd: argv is <shell...> <script> <$0> <command> <cwd>.
func (f *fakeHerdr) preflightCheck(argv []string) ([]byte, error) {
	if isServerStart(argv) {
		if f.serverStarts {
			f.serverDown = false
		}
		return nil, nil
	}
	if f.hostEnv != nil {
		return f.runScript(argv)
	}
	if f.preflight != "" {
		return nil, &target.ExitError{Argv: argv, Code: 3, Stderr: []byte(f.preflight + "\n")}
	}
	cwd := f.resolvedCwd
	if cwd == "" {
		cwd = argv[len(argv)-1]
	}
	return []byte(cwd + "\n"), nil
}

// runScript runs a shell-script call, argv <shell...> <script> herdrcron
// <args...>, in a local sh with hostEnv; a non-zero exit is an ExitError
// with the script's stderr, as a target gives it.
func (f *fakeHerdr) runScript(argv []string) ([]byte, error) {
	i := slices.Index(argv, "herdrcron")
	if i < 1 {
		return nil, fmt.Errorf("fake herdr: unexpected argv %q", argv)
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", argv[i-1]}, argv[i:]...)...)
	cmd.Env = f.hostEnv
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, &target.ExitError{Argv: argv, Code: ee.ExitCode(), Stderr: stderr.Bytes()}
	}
	return out, err
}

func flag(argv []string, name string) string {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// flags returns every value of a repeated flag.
func flags(argv []string, name string) []string {
	var out []string
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func isServerStart(argv []string) bool {
	return argv[0] == "sh" && strings.Contains(argv[len(argv)-1], "herdr' server") ||
		argv[0] == "sh" && strings.Contains(argv[len(argv)-1], "herdr server")
}

func (f *fakeHerdr) serverStartCalls() int {
	n := 0
	for _, c := range f.Calls() {
		if isServerStart(c) {
			n++
		}
	}
	return n
}
