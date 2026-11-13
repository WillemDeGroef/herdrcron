package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/WillemDeGroef/herdrcron/internal/herdr"
	"github.com/WillemDeGroef/herdrcron/internal/target"
)

// paneState is what `herdr pane list` reports for a pane.
type paneState int

const (
	paneWorking paneState = iota // agent claude, working (the default)
	paneIdle
	paneDone
	paneBlocked
	paneNoAgent // the agent exited: no agent, status unknown, shell busy
	paneGone    // the pane no longer exists
)

type fakePane struct {
	id, tab, ws string
	state       paneState
}

// fakeHerdr is a stateful target.Target that plays the subset of herdr
// 0.9.1 the runner uses (a trimmed copy of the runner package's fake).
// Panes report agent claude, working, until the test changes them.
type fakeHerdr struct {
	mu sync.Mutex

	calls      [][]string
	files      map[string][]byte
	puts       []string          // contents of every PutFile, in order
	workspaces map[string]string // label -> id
	tabs       map[string]string // tab id -> workspace id
	tabLabels  map[string]string // tab id -> label
	panes      map[string]*fakePane
	order      []string // pane ids in creation order
	nextID     int

	name        string // target name; "" means "local"
	unreachable bool   // every call fails as ssh exit 255 would
	// loseRunReply: the next pane run is accepted, then the target drops
	// before the reply arrives and stays unreachable.
	loseRunReply bool
	output       string        // pane read text
	ansiOutput   string        // pane read --format ansi text
	bin          string        // the herdr binary it answers to; "" means "herdr"
	readErr      error         // pane read --format ansi fails with it
	putGate      chan struct{} // PutFile waits until it is closed
	execGate     chan struct{} // Exec waits until it is closed
	execHeld     chan struct{} // closed when an Exec reaches execGate
	execHeldOnce *sync.Once
}

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{
		files: map[string][]byte{}, workspaces: map[string]string{},
		tabs: map[string]string{}, tabLabels: map[string]string{}, panes: map[string]*fakePane{},
	}
}

func (f *fakeHerdr) Name() string {
	if f.name == "" {
		return "local"
	}
	return f.name
}

// unreachableErr is what ssh reports for a host that does not answer;
// f.mu must be held.
func (f *fakeHerdr) unreachableErr() error {
	return &target.UnreachableError{Target: f.Name(), Stderr: []byte("ssh: connect to host: Connection refused\n")}
}

func (f *fakeHerdr) PutFile(_ context.Context, path string, data []byte) error {
	f.mu.Lock()
	gate := f.putGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return f.unreachableErr()
	}
	f.files[path] = slices.Clone(data)
	f.puts = append(f.puts, string(data))
	return nil
}

// putContents returns the contents of every file written, in order.
func (f *fakeHerdr) putContents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.puts)
}

func (f *fakeHerdr) RemoveFile(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, path)
	return nil
}

// set changes what pane list reports for the pane.
func (f *fakeHerdr) set(paneID string, s paneState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[paneID].state = s
}

func (f *fakeHerdr) setLoseRunReply() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseRunReply = true
}

func (f *fakeHerdr) setUnreachable(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreachable = v
}

func (f *fakeHerdr) setOutput(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.output = s
}

// setReadErr makes pane read --format ansi fail with err.
func (f *fakeHerdr) setReadErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
}

// holdExec makes every Exec wait until the returned function is called.
// A run blocked here has been stored as starting and has no pane yet.
func (f *fakeHerdr) holdExec() (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.execGate = gate
	f.execHeld = make(chan struct{})
	f.execHeldOnce = &sync.Once{}
	return sync.OnceFunc(func() {
		f.mu.Lock()
		f.execGate = nil
		f.mu.Unlock()
		close(gate)
	})
}

// execHeldCh is closed when an Exec reaches the holdExec gate.
func (f *fakeHerdr) execHeldCh() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execHeld
}

// holdPutFile makes PutFile (the prompt write, after the tab is opened)
// wait until the returned function is called.
func (f *fakeHerdr) holdPutFile() (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := make(chan struct{})
	f.putGate = gate
	return sync.OnceFunc(func() { close(gate) })
}

func (f *fakeHerdr) setANSIOutput(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ansiOutput = s
}

// setBin makes the fake answer to bin (a target's herdr_path) as herdr.
func (f *fakeHerdr) setBin(bin string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bin = bin
}

// lastCall returns the last call that started with prefix, or nil.
func (f *fakeHerdr) lastCall(prefix ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		c := f.calls[i]
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return slices.Clone(c)
		}
	}
	return nil
}

func (f *fakeHerdr) tabOpen(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tabs[id]
	return ok
}

// count returns how many calls started with prefix.
func (f *fakeHerdr) count(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			n++
		}
	}
	return n
}

// keysSent returns the key lists of `pane send-keys` calls for the pane.
func (f *fakeHerdr) keysSent(paneID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if len(c) > 4 && c[1] == "pane" && c[2] == "send-keys" && c[3] == paneID {
			out = append(out, strings.Join(c[4:], " "))
		}
	}
	return out
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
	gate := f.execGate
	held, once := f.execHeld, f.execHeldOnce
	f.mu.Unlock()
	if once != nil {
		once.Do(func() { close(held) })
	}
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(argv))
	if f.unreachable {
		return nil, f.unreachableErr()
	}
	bin := f.bin
	if bin == "" {
		bin = "herdr"
	}
	if argv[0] != bin {
		// The preflight script: argv ends with <command> <cwd>; print the cwd.
		return []byte(argv[len(argv)-1] + "\n"), nil
	}
	switch strings.Join(argv[1:min(3, len(argv))], " ") {
	case "--version":
		return []byte("herdr 0.9.1\n"), nil
	case "pane list":
		list := []herdr.PaneInfo{}
		for _, id := range f.order {
			p := f.panes[id]
			if p.state == paneGone {
				continue
			}
			info := herdr.PaneInfo{PaneID: p.id, TabID: p.tab, WorkspaceID: p.ws}
			agent := "claude"
			info.Agent = &agent
			switch p.state {
			case paneNoAgent:
				info.Agent = nil
				info.AgentStatus = herdr.AgentStatusUnknown
			case paneWorking:
				info.AgentStatus = herdr.AgentStatusWorking
			case paneIdle:
				info.AgentStatus = herdr.AgentStatusIdle
			case paneDone:
				info.AgentStatus = herdr.AgentStatusDone
			case paneBlocked:
				info.AgentStatus = herdr.AgentStatusBlocked
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
		tab, pane := f.newTab(ws)
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
		tab, pane := f.newTab(ws)
		f.tabLabels[tab] = label
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
				p.state = paneGone
			}
		}
		return ok(herdr.OkResult{Type: "ok"})
	case "pane run", "pane send-keys", "pane read":
		if p, found := f.panes[argv[3]]; !found || p.state == paneGone {
			return nil, herdrErr(herdr.CodePaneNotFound)
		}
		if argv[2] == "read" {
			if flag(argv, "--format") == "ansi" {
				if f.readErr != nil {
					return nil, f.readErr
				}
				return []byte(f.ansiOutput), nil
			}
			return []byte(f.output), nil
		}
		if argv[2] == "run" && f.loseRunReply {
			f.loseRunReply, f.unreachable = false, true
			return nil, f.unreachableErr()
		}
		return nil, nil
	case "pane process-info":
		p, found := f.panes[flag(argv, "--pane")]
		if !found || p.state == paneGone {
			return nil, herdrErr(herdr.CodePaneNotFound)
		}
		pid, fg := uint32(100), uint32(200)
		return ok(herdr.PaneProcessInfoResult{Type: "pane_process_info", ProcessInfo: herdr.PaneProcessInfo{
			PaneID: p.id, ShellPID: &pid, ForegroundProcessGroupID: &fg,
		}})
	}
	return nil, fmt.Errorf("fake herdr: unexpected argv %q", argv)
}

// newTab creates a tab with a root pane whose agent is working.
func (f *fakeHerdr) newTab(ws string) (string, herdr.PaneInfo) {
	f.nextID++
	tab := fmt.Sprintf("%s:t%d", ws, f.nextID)
	pane := fmt.Sprintf("%s:p%d", ws, f.nextID)
	f.tabs[tab] = ws
	f.panes[pane] = &fakePane{id: pane, tab: tab, ws: ws}
	f.order = append(f.order, pane)
	return tab, herdr.PaneInfo{PaneID: pane, TabID: tab, WorkspaceID: ws, AgentStatus: herdr.AgentStatusUnknown}
}

func flag(argv []string, name string) string {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}
