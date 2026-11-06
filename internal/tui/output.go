package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// outputPollEvery is how often the live output reads the pane again.
var outputPollEvery = 2 * time.Second

// outputState is the state of the live output screen.
type outputState struct {
	gen     int // generation of the open screen; results and ticks of others are dropped
	task    string
	run     string
	back    screen // the screen esc goes back to
	loading bool
	res     api.RunsOutputResult
	ended   bool   // the run ended: res is its final output and polling stopped
	readErr string // the message a failed read set, cleared by the next good read
}

// Messages of the live output screen and of attach.
type (
	outputMsg struct {
		gen int
		res api.RunsOutputResult
		err error
	}
	outputTickMsg struct{ gen int }
	attachCmdMsg  struct {
		run string
		res api.AttachCmdResult
		err error
	}
	attachedMsg struct {
		run, note string
		err       error
	}
)

// requireActiveRun returns task id's active run, or says it has none.
func (m *Model) requireActiveRun(id string) (api.RunSummary, bool) {
	run, ok := m.activeRun(id)
	if !ok {
		m.setMessage(id+" has no active run", nil)
	}
	return run, ok
}

// openOutput opens the live output of task id's active run.
func (m Model) openOutput(id string) (Model, tea.Cmd) {
	run, ok := m.requireActiveRun(id)
	if !ok {
		return m, nil
	}
	back := m.screen
	m.output = outputState{gen: m.output.gen + 1, task: id, run: run.ID, back: back, loading: true}
	m.screen = screenOutput
	m.setMessage("", nil)
	return m, m.pollOutput()
}

// readOutput calls runs.output for run (lines 0: the daemon's default)
// and hands the answer to done.
func (m Model) readOutput(run string, lines int, done func(api.RunsOutputResult, error) tea.Msg) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var res api.RunsOutputResult
		err := b.Call(ctx, api.MethodRunsOutput, api.RunsOutputParams{RunID: run, Lines: lines}, &res)
		if err != nil {
			err = fmt.Errorf("runs.output %s: %w", run, err)
		}
		return done(res, err)
	}
}

// pollOutput reads the pane of the open screen's run once.
func (m Model) pollOutput() tea.Cmd {
	gen := m.output.gen
	lines := min(max(api.DefaultOutputLines, m.height), api.MaxOutputLines)
	return m.readOutput(m.output.run, lines, func(res api.RunsOutputResult, err error) tea.Msg {
		return outputMsg{gen: gen, res: res, err: err}
	})
}

// closeOutput goes back to the screen the output was opened from.
func (m Model) closeOutput() (Model, tea.Cmd) {
	m.output.gen++ // drop the pending tick and read
	m.screen = m.output.back
	m.setMessage("", nil)
	if m.screen == screenDetail {
		// The detail heard no events while hidden.
		return m, m.fetchDetail()
	}
	return m, nil
}

// attach suspends the TUI and attaches to task id's active run.
func (m Model) attach(id string) (Model, tea.Cmd) {
	run, ok := m.requireActiveRun(id)
	if !ok {
		return m, nil
	}
	return m, m.attachRun(run.ID)
}

// attachRun asks the daemon how to attach to run id.
func (m Model) attachRun(id string) tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var res api.AttachCmdResult
		err := b.Call(ctx, api.MethodRunsAttachCmd, api.RunIDParams{RunID: id}, &res)
		if err == nil && len(res.Argv) == 0 {
			err = fmt.Errorf("the daemon gave no attach command")
		}
		if err != nil {
			err = fmt.Errorf("attach %s: %w", id, err)
		}
		return attachCmdMsg{run: id, res: res, err: err}
	}
}

// outputMsg handles the live output screen's and attach's result messages.
func (m Model) outputMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case outputMsg:
		if msg.gen != m.output.gen || m.screen != screenOutput {
			return m, nil, true
		}
		m.output.loading = false
		if msg.err != nil {
			// Keep trying: the pane may be back at the next read.
			m.setMessage("", msg.err)
			m.output.readErr = m.message
			return m, m.outputTick(), true
		}
		// Clear the error a read set, and only that one.
		if m.output.readErr != "" && m.msgErr && m.message == m.output.readErr {
			m.setMessage("", nil)
		}
		m.output.readErr = ""
		m.output.res = msg.res
		// Live is the pane read. A run that is still starting has no pane
		// yet: its status is active and Live is false, and polling goes on.
		if !msg.res.Live && !activeStatus(msg.res.Status) {
			m.output.ended = true
			return m, nil, true
		}
		return m, m.outputTick(), true
	case outputTickMsg:
		if msg.gen != m.output.gen || m.screen != screenOutput {
			return m, nil, true
		}
		return m, m.pollOutput(), true
	case attachCmdMsg:
		if msg.err != nil {
			m.setMessage("", msg.err)
			return m, nil, true
		}
		run, note := msg.run, msg.res.Note
		cmd := exec.Command(msg.res.Argv[0], msg.res.Argv[1:]...)
		return m, m.exec(cmd, func(err error) tea.Msg { return attachedMsg{run: run, note: note, err: err} }), true
	case attachedMsg:
		if msg.err != nil {
			m.setMessage("", fmt.Errorf("attach %s: %w", msg.run, msg.err))
		} else {
			text := "detached from " + msg.run
			if msg.note != "" {
				text += " (" + msg.note + ")"
			}
			m.setMessage(text, nil)
		}
		return m, m.load(), true
	}
	return m, nil, false
}

func (m Model) outputTick() tea.Cmd {
	gen := m.output.gen
	return tea.Tick(outputPollEvery, func(time.Time) tea.Msg { return outputTickMsg{gen} })
}

// outputKey handles a key on the live output screen.
func (m Model) outputKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		return m.closeOutput()
	case "a":
		return m, m.attachRun(m.output.run)
	}
	return m, nil
}

// outputView renders the live output screen.
func (m Model) outputView() string {
	o := m.output
	var b strings.Builder
	b.WriteString(m.st.title.Render(fmt.Sprintf(" %s · run %s · live output", o.task, o.run)))
	b.WriteByte('\n')
	var body []string
	switch {
	case o.loading:
		body = []string{m.st.dim.Render("  loading…")}
	case o.res.Output != "":
		body = strings.Split(strings.TrimRight(o.res.Output, "\n"), "\n")
	case !o.ended && activeStatus(o.res.Status):
		body = []string{m.st.dim.Render("  waiting for output…")}
	}
	if o.ended {
		body = append(body, m.st.warning.Render("run ended: "+o.res.Status))
	}
	// Reflow to the terminal width before keeping the last screenful, so
	// the tail of a long line stays on screen. fit would otherwise cut it.
	body = wrapLines(body, m.width)
	// The last screenful: title, message and footer take three lines.
	if n := m.height - 3; m.height > 0 && len(body) > n {
		body = body[len(body)-max(n, 0):]
	}
	for _, l := range body {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	// Close any colour the pane left open, and keep the footer at the bottom.
	b.WriteString("\x1b[0m")
	if n := m.height - 3; m.height > 0 {
		b.WriteString(strings.Repeat("\n", max(n-len(body), 0)))
	}
	b.WriteString(m.messageLine())
	b.WriteByte('\n')
	if m.mode == modeConfirm {
		b.WriteString(m.footer())
	} else {
		b.WriteString(m.st.dim.Render(" esc back  a attach"))
	}
	return b.String()
}

// wrapLines reflows each line to width columns, keeping ANSI sequences.
// width <= 0 leaves the lines alone (the terminal size is not known yet).
func wrapLines(lines []string, width int) []string {
	if width <= 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		wrapped := strings.TrimRight(ansi.Wrap(l, width, ""), "\n")
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	return out
}
