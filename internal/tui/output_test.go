package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// fastPoll makes the live output poll quickly for the test.
func fastPoll(t *testing.T) {
	t.Helper()
	old := outputPollEvery
	outputPollEvery = 20 * time.Millisecond
	t.Cleanup(func() { outputPollEvery = old })
}

func liveOutput(text string) api.RunsOutputResult {
	return api.RunsOutputResult{RunID: "r3", Status: "running", Live: true, Output: text}
}

func openLiveOutput(t *testing.T, f *fakeBackend) *teatest.TestModel {
	t.Helper()
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("l"))
	return tm
}

func TestLiveOutputWithoutActiveRun(t *testing.T) {
	f := fixture()
	tm := start(t, newTestModel(f, plain()))
	selectTask(t, tm, order, "e2e-timeout")
	tm.Send(key("l"))
	waitText(t, tm, "e2e-timeout has no active run")
	finalView(t, tm)
	if got := f.callsOf(api.MethodRunsOutput); len(got) != 0 {
		t.Fatalf("runs.output called: %+v", got)
	}
}

func TestLiveOutputShowsThePaneWithANSIKept(t *testing.T) {
	f := fixture()
	f.set(api.MethodRunsOutput, liveOutput("plain line\n\x1b[31mred line\x1b[0m\n"))
	tm := openLiveOutput(t, f)
	got := waitCall(t, f, api.MethodRunsOutput, 1)
	if p := got[0].(api.RunsOutputParams); p.RunID != "r3" || p.Lines < 16 {
		t.Fatalf("runs.output params = %+v, want run r3 and at least a screenful", p)
	}
	waitText(t, tm, "red line")
	view := finalView(t, tm)
	for _, s := range []string{"factory-coordinator", "r3", "plain line", "\x1b[31mred line\x1b[0m", "esc back", "a attach"} {
		if !strings.Contains(view, s) {
			t.Errorf("live output lacks %q:\n%s", s, view)
		}
	}
}

func TestLiveOutputShowsTheLastScreenful(t *testing.T) {
	f := fixture()
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("pane line %03d", i))
	}
	f.set(api.MethodRunsOutput, liveOutput(strings.Join(lines, "\n")+"\n"))
	tm := openLiveOutput(t, f)
	waitText(t, tm, "pane line 100")
	view := finalView(t, tm)
	if strings.Contains(view, "pane line 050") {
		t.Errorf("live output shows more than the last screenful:\n%s", view)
	}
	if n := strings.Count(view, "\n") + 1; n > 16 {
		t.Errorf("live output is %d lines, taller than the terminal:\n%s", n, view)
	}
}

func TestLiveOutputPollsUntilTheRunEnds(t *testing.T) {
	fastPoll(t)
	f := fixture()
	f.set(api.MethodRunsOutput, liveOutput("working\n"))
	tm := openLiveOutput(t, f)
	waitCall(t, f, api.MethodRunsOutput, 3)
	f.set(api.MethodRunsOutput, api.RunsOutputResult{RunID: "r3", Status: "done", Output: "final words\n"})
	waitText(t, tm, "run ended: done")
	n := len(f.callsOf(api.MethodRunsOutput))
	time.Sleep(10 * outputPollEvery)
	if m := len(f.callsOf(api.MethodRunsOutput)); m != n {
		t.Errorf("still polling after the run ended: %d calls, then %d", n, m)
	}
	view := finalView(t, tm)
	if !strings.Contains(view, "final words") {
		t.Errorf("final output not shown:\n%s", view)
	}
}

func TestLiveOutputEscStopsPolling(t *testing.T) {
	fastPoll(t)
	f := fixture()
	f.set(api.MethodRunsOutput, liveOutput("working\n"))
	tm := openLiveOutput(t, f)
	waitCall(t, f, api.MethodRunsOutput, 2)
	tm.Send(key("esc"))
	waitText(t, tm, "space on/off")
	time.Sleep(3 * outputPollEvery)
	n := len(f.callsOf(api.MethodRunsOutput))
	time.Sleep(10 * outputPollEvery)
	if m := len(f.callsOf(api.MethodRunsOutput)); m != n {
		t.Errorf("still polling after esc: %d calls, then %d", n, m)
	}
	finalView(t, tm)
}

// TestLiveOutputReopenPollsOnce checks that the ticks of a closed screen
// die with it: reopening does not double the polling.
func TestLiveOutputReopenPollsOnce(t *testing.T) {
	old := outputPollEvery
	outputPollEvery = 200 * time.Millisecond
	t.Cleanup(func() { outputPollEvery = old })
	f := fixture()
	f.set(api.MethodRunsOutput, liveOutput("working\n"))
	tm := openLiveOutput(t, f)
	waitCall(t, f, api.MethodRunsOutput, 1)
	tm.Send(key("esc"))
	tm.Send(key("l"))
	waitCall(t, f, api.MethodRunsOutput, 2)
	time.Sleep(outputPollEvery * 5 / 2) // two ticks of the open screen
	if n := len(f.callsOf(api.MethodRunsOutput)); n > 4 {
		t.Errorf("%d runs.output calls in 2.5 intervals: the closed screen still polls", n)
	}
	finalView(t, tm)
}

func TestLiveOutputQGoesBack(t *testing.T) {
	f := fixture()
	f.set(api.MethodRunsOutput, liveOutput("working\n"))
	tm := openLiveOutput(t, f)
	waitText(t, tm, "working")
	tm.Send(key("q"))
	waitText(t, tm, "space on/off")
	finalView(t, tm)
}

func TestLiveOutputErrorShowsAsMessage(t *testing.T) {
	f := fixture()
	f.fail(api.MethodRunsOutput, errors.New("pane gone"))
	tm := openLiveOutput(t, f)
	waitText(t, tm, "pane gone")
	finalView(t, tm)
}

func TestLiveOutputFromDetail(t *testing.T) {
	f := detailFixture()
	f.set(api.MethodRunsOutput, liveOutput("from the detail\n"))
	tm := openCoordinatorDetail(t, f)
	tm.Send(key("l"))
	waitText(t, tm, "from the detail")
	got := waitCall(t, f, api.MethodRunsOutput, 1)
	if p := got[0].(api.RunsOutputParams); p.RunID != "r3" {
		t.Fatalf("runs.output params = %+v, want the active run r3", p)
	}
	tm.Send(key("esc"))
	waitText(t, tm, "Next fires")
	finalView(t, tm)
}

// execRecorder is an Options.Exec that records the commands it is given
// and ends them at once, without running anything.
type execRecorder struct {
	mu   sync.Mutex
	args [][]string
	err  error
}

func (e *execRecorder) exec(cmd *exec.Cmd, done func(error) tea.Msg) tea.Cmd {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.args = append(e.args, cmd.Args)
	err := e.err
	return func() tea.Msg { return done(err) }
}

func (e *execRecorder) calls() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.args...)
}

func newAttachModel(f *fakeBackend, e *execRecorder) Model {
	return New(Options{
		Backend: f, Now: func() time.Time { return now }, Location: time.UTC, Renderer: plain(), Exec: e.exec,
	})
}

func TestAttachRunsTheAttachCommand(t *testing.T) {
	f := fixture()
	f.set(api.MethodRunsAttachCmd, api.AttachCmdResult{Argv: []string{"ssh", "-t", "vps", "herdr", "agent", "attach", "p1"}, Mode: api.AttachPane})
	e := &execRecorder{}
	tm := start(t, newAttachModel(f, e))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("a"))
	got := waitCall(t, f, api.MethodRunsAttachCmd, 1)
	if want := (api.RunIDParams{RunID: "r3"}); got[0] != want {
		t.Fatalf("attach_cmd params = %+v, want %+v", got[0], want)
	}
	waitText(t, tm, "detached from r3")
	if c := e.calls(); len(c) != 1 || strings.Join(c[0], " ") != "ssh -t vps herdr agent attach p1" {
		t.Fatalf("exec got %q", c)
	}
	// Coming back re-reads the state.
	waitCall(t, f, api.MethodTasksList, 2)
	finalView(t, tm)
}

func TestAttachShowsTheNote(t *testing.T) {
	f := fixture()
	f.set(api.MethodRunsAttachCmd, api.AttachCmdResult{Argv: []string{"herdr"}, Mode: api.AttachFull, Note: "no agent left in the pane"})
	tm := start(t, newAttachModel(f, &execRecorder{}))
	selectTask(t, tm, order, "factory-coordinator")
	tm.Send(key("a"))
	waitText(t, tm, "no agent left in the pane")
	finalView(t, tm)
}

func TestAttachErrors(t *testing.T) {
	t.Run("attach_cmd fails", func(t *testing.T) {
		f := fixture()
		f.fail(api.MethodRunsAttachCmd, errors.New("target unreachable"))
		e := &execRecorder{}
		tm := start(t, newAttachModel(f, e))
		selectTask(t, tm, order, "factory-coordinator")
		tm.Send(key("a"))
		waitText(t, tm, "target unreachable")
		finalView(t, tm)
		if c := e.calls(); len(c) != 0 {
			t.Fatalf("exec ran %q", c)
		}
	})
	t.Run("command fails", func(t *testing.T) {
		f := fixture()
		f.set(api.MethodRunsAttachCmd, api.AttachCmdResult{Argv: []string{"herdr", "agent", "attach", "p1"}, Mode: api.AttachPane})
		tm := start(t, newAttachModel(f, &execRecorder{err: errors.New("exit status 255")}))
		selectTask(t, tm, order, "factory-coordinator")
		tm.Send(key("a"))
		waitText(t, tm, "exit status 255")
		finalView(t, tm)
	})
	t.Run("no active run", func(t *testing.T) {
		f := fixture()
		tm := start(t, newAttachModel(f, &execRecorder{}))
		selectTask(t, tm, order, "e2e-timeout")
		tm.Send(key("a"))
		waitText(t, tm, "e2e-timeout has no active run")
		finalView(t, tm)
	})
}

func TestAttachFromOutputAndDetail(t *testing.T) {
	for _, tc := range []struct {
		name string
		open []string
	}{
		{"output", []string{"l"}},
		{"detail", []string{"enter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := detailFixture()
			f.set(api.MethodRunsOutput, liveOutput("working\n"))
			f.set(api.MethodRunsAttachCmd, api.AttachCmdResult{Argv: []string{"herdr", "agent", "attach", "p1"}, Mode: api.AttachPane})
			e := &execRecorder{}
			tm := start(t, newAttachModel(f, e))
			selectTask(t, tm, order, "factory-coordinator")
			for _, k := range tc.open {
				tm.Send(key(k))
			}
			tm.Send(key("a"))
			waitText(t, tm, "detached from r3")
			if c := e.calls(); len(c) != 1 {
				t.Fatalf("exec got %q", c)
			}
			finalView(t, tm)
		})
	}
}
