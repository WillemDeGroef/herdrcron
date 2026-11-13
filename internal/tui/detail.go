package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// detailState is the state of the task detail screen.
type detailState struct {
	id      string
	seq     int // number of the latest tasks.get; older answers are dropped
	loading bool
	loaded  bool
	d       api.TaskDetail
	run     string // id of the selected run
	offset  int    // first body line shown

	// The output of a run, shown over the detail when out is set.
	out        bool
	outSeq     int // number of the latest runs.output; older answers are dropped
	outRun     string
	outLoading bool
	outRes     api.RunsOutputResult
	vp         viewport.Model
}

// runOutputMsg is the answer of runs.output for a run of the detail.
type runOutputMsg struct {
	seq int
	res api.RunsOutputResult
	err error
}

// detailLoadedMsg is the answer of tasks.get for the detail screen.
type detailLoadedMsg struct {
	seq int
	d   api.TaskDetail
	err error
}

// openDetail opens the detail screen of task id.
func (m Model) openDetail(id string) (Model, tea.Cmd) {
	m.screen = screenDetail
	m.detail = detailState{id: id, seq: m.detail.seq}
	m.setMessage("", nil)
	return m, m.fetchDetail()
}

// fetchDetail reads the open detail's task again.
func (m *Model) fetchDetail() tea.Cmd {
	m.detail.seq++
	m.detail.loading = true
	ctx, b, id, seq := m.ctx, m.backend, m.detail.id, m.detail.seq
	return func() tea.Msg {
		var d api.TaskDetail
		err := b.Call(ctx, api.MethodTasksGet, api.TaskIDParams{ID: id}, &d)
		if err != nil {
			err = fmt.Errorf("tasks.get %s: %w", id, err)
		}
		return detailLoadedMsg{seq: seq, d: d, err: err}
	}
}

// detailMsg handles the detail screen's result messages.
func (m Model) detailMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case detailLoadedMsg:
		if msg.seq != m.detail.seq || m.screen != screenDetail {
			return m, nil, true
		}
		m.detail.loading = false
		if msg.err != nil {
			m.setMessage("", msg.err)
			return m, nil, true
		}
		m.detail.d, m.detail.loaded = msg.d, true
		return m, nil, true
	case runOutputMsg:
		if msg.seq != m.detail.outSeq || m.screen != screenDetail || !m.detail.out {
			return m, nil, true
		}
		m.detail.outLoading = false
		if msg.err != nil {
			m.setMessage("", msg.err)
			return m, nil, true
		}
		m.detail.outRes = msg.res
		m.sizeRunOutput()
		m.detail.vp.SetContent(strings.TrimRight(msg.res.Output, "\n"))
		m.detail.vp.GotoBottom()
		return m, nil, true
	}
	return m, nil, false
}

// detailKey handles a key on the detail screen.
func (m Model) detailKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.detail.out {
		return m.runOutputKey(msg)
	}
	runs := m.detail.runs()
	switch msg.String() {
	case "esc", "q":
		m.closeDetail()
	case "up", "k":
		if len(runs) > 0 {
			m.detail.run = runs[max(m.runCursor()-1, 0)].ID
			m.showRunCursor()
		}
	case "down", "j":
		if len(runs) > 0 {
			m.detail.run = runs[min(m.runCursor()+1, len(runs)-1)].ID
			m.showRunCursor()
		}
	case "pgup":
		m.scrollDetail(-max(m.bodyHeight(), 1))
	case "pgdown":
		m.scrollDetail(max(m.bodyHeight(), 1))
	case "enter":
		if len(runs) > 0 {
			return m, m.openRunOutput(runs[m.runCursor()].ID)
		}
	case "l":
		return m.openOutput(m.detail.id)
	case "a":
		return m.attach(m.detail.id)
	}
	return m, nil
}

// bodyHeight is how many body lines fit; 0 means no limit (size unknown).
func (m Model) bodyHeight() int {
	if m.height == 0 {
		return 0
	}
	return max(m.height-3, 1)
}

// scrollDetail moves the body by delta lines, clamped to it.
func (m *Model) scrollDetail(delta int) {
	body, _ := m.detailBody()
	m.detail.offset = min(max(m.detail.offset+delta, 0), max(len(body)-m.bodyHeight(), 0))
}

// showRunCursor scrolls the body just enough to show the selected run.
func (m *Model) showRunCursor() {
	n := m.bodyHeight()
	if n == 0 {
		return
	}
	_, cur := m.detailBody()
	if cur < m.detail.offset {
		m.detail.offset = cur
	}
	if cur >= m.detail.offset+n {
		m.detail.offset = cur - n + 1
	}
}

// openRunOutput shows the output of run id over the detail.
func (m *Model) openRunOutput(id string) tea.Cmd {
	m.detail.out, m.detail.outRun, m.detail.outLoading = true, id, true
	m.detail.outRes = api.RunsOutputResult{}
	m.detail.vp = viewport.New(0, 0)
	m.detail.outSeq++
	m.setMessage("", nil)
	seq := m.detail.outSeq
	return m.readOutput(id, 0, func(res api.RunsOutputResult, err error) tea.Msg {
		return runOutputMsg{seq: seq, res: res, err: err}
	})
}

func (m Model) runOutputKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.detail.out = false
		m.detail.outSeq++
		m.setMessage("", nil)
		return m, nil
	case "l":
		// The title says l follows a live run. The viewport would otherwise
		// take l as "scroll right".
		if m.detail.outRes.Live {
			return m.openOutput(m.detail.id)
		}
	}
	m.sizeRunOutput()
	var cmd tea.Cmd
	m.detail.vp, cmd = m.detail.vp.Update(msg)
	return m, cmd
}

// sizeRunOutput fits the output viewport between its header and footer.
func (m *Model) sizeRunOutput() {
	m.detail.vp.Width = m.width
	m.detail.vp.Height = max(m.height-3, 1)
}

func (m Model) runOutputView() string {
	ds := m.detail
	var b strings.Builder
	head := fmt.Sprintf(" %s · run %s", ds.id, ds.outRun)
	if ds.outRes.Status != "" {
		head += " · " + ds.outRes.Status
	}
	if ds.outRes.Live {
		head += " · live (l follows it)"
	}
	b.WriteString(m.st.title.Render(head))
	b.WriteByte('\n')
	switch {
	case ds.outLoading:
		b.WriteString(m.st.dim.Render("  loading…"))
	case ds.outRes.Output == "" && m.message == "":
		b.WriteString(m.st.dim.Render("  no output"))
	default:
		m.sizeRunOutput()
		b.WriteString(m.detail.vp.View())
		// Close any colour an active run's pane left open.
		b.WriteString("\x1b[0m")
	}
	b.WriteByte('\n')
	b.WriteString(m.messageLine())
	b.WriteByte('\n')
	if m.mode == modeConfirm {
		b.WriteString(m.footer())
	} else {
		help := " ↑/↓ pgup/pgdn scroll  esc back"
		if ds.outRes.Live {
			help = " ↑/↓ pgup/pgdn scroll  l follow  esc back"
		}
		b.WriteString(m.st.dim.Render(help))
	}
	return b.String()
}

// closeDetail goes back to the main list; answers still on their way are dropped.
func (m *Model) closeDetail() {
	m.screen = screenMain
	m.detail.seq++
	m.setMessage("", nil)
}

// detailView renders the detail screen.
func (m Model) detailView() string {
	if m.detail.out {
		return m.runOutputView()
	}
	ds := m.detail
	var b strings.Builder
	b.WriteString(m.st.title.Render(" " + ds.id))
	b.WriteByte('\n')
	if !ds.loaded {
		if ds.loading {
			b.WriteString(m.st.dim.Render("  loading…"))
		}
		b.WriteByte('\n')
		b.WriteString(m.messageLine())
		b.WriteByte('\n')
		b.WriteString(m.detailFooter())
		return b.String()
	}
	body, _ := m.detailBody()
	// Title, message and footer stay; the body scrolls between them.
	if n := m.bodyHeight(); n > 0 && len(body) > n {
		from := min(max(m.detail.offset, 0), len(body)-n)
		body = body[from : from+n]
	}
	for _, l := range body {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString(m.messageLine())
	b.WriteByte('\n')
	b.WriteString(m.detailFooter())
	return b.String()
}

// detailBody returns the lines of the detail under its title and the
// index of the selected run's line.
func (m Model) detailBody() ([]string, int) {
	lines := m.definitionLines()
	lines = append(lines, "", m.st.header.Render(" Next fires"))
	lines = append(lines, m.fireLines()...)
	lines = append(lines, "", m.st.header.Render(" Runs"))
	runs := m.detail.runs()
	if len(runs) == 0 {
		lines = append(lines, m.st.dim.Render("  no runs yet"))
		return lines, len(lines) - 1
	}
	cells := [][]string{{"STATUS", "TRIGGER", "START", "DURATION", "REASON"}}
	for _, r := range runs {
		cells = append(cells, []string{r.Status, orDash(r.Trigger), m.clock(r.StartedAt), m.runDuration(r), r.ExitReason})
	}
	width := make([]int, len(cells[0]))
	for _, row := range cells {
		for i, c := range row {
			width[i] = max(width[i], lipgloss.Width(c))
		}
	}
	cur := m.runCursor()
	first := len(lines)
	for i, row := range cells {
		parts := make([]string, len(row))
		for c, s := range row {
			parts[c] = pad(s, width[c])
		}
		text := strings.TrimRight(strings.Join(parts, gap), " ")
		switch {
		case i == 0:
			lines = append(lines, m.st.header.Render("  "+text))
		case i-1 == cur:
			lines = append(lines, "› "+m.st.plain.Reverse(true).Render(text))
		default:
			lines = append(lines, "  "+m.runStyle(runs[i-1]).Render(text))
		}
	}
	return lines, first + 1 + cur
}

// fireLines preview the next five fire times of the task's schedule. A
// pause without an end shows that instead; a timed pause shows the fires
// after it.
func (m Model) fireLines() []string {
	def, sum := m.detail.d.Task, m.detail.d.Summary
	h := hold{}
	if sum.State != api.StateOff && sum.State != api.StateInvalid {
		h = m.holdOn(sum.Target, sum.PausedUntil)
	}
	after := h.from(def.Schedule, m.now())
	if phase, ok := everyPhaseAfter(def.Schedule, sum.NextFireAt, h); ok {
		after = phase
	}
	fires, err := nextFires(def.Schedule, def.Timezone, m.status.Timezone, m.loc, after, 5)
	if err != nil {
		return []string{m.st.critical.Render("  " + err.Error())}
	}
	var out []string
	switch {
	case h.endless():
		return []string{m.st.dim.Render("  " + h.text(m.loc))}
	case !h.none():
		out = append(out, m.st.dim.Render("  "+h.text(m.loc)))
	case sum.State == api.StatePaused && !pauseEnded(sum.PausedUntil, m.now()):
		return []string{m.st.dim.Render("  paused: no fires until resumed")}
	case sum.State == api.StateOff, sum.State == api.StateInvalid:
		out = append(out, m.st.dim.Render("  (task is "+sum.State+": the schedule would fire at)"))
	}
	if len(fires) == 0 {
		out = append(out, m.st.dim.Render("  never fires again"))
	}
	for _, t := range fires {
		out = append(out, "  "+t.In(m.loc).Format(fireLayout))
	}
	return out
}

// runs are the runs the detail shows: at most api.TaskRunsLimit.
func (ds detailState) runs() []api.RunSummary {
	return ds.d.Runs[:min(len(ds.d.Runs), api.TaskRunsLimit)]
}

// runCursor is the index of the selected run, clamped to the runs shown.
func (m Model) runCursor() int {
	for i, r := range m.detail.runs() {
		if r.ID == m.detail.run {
			return i
		}
	}
	return 0
}

func (m Model) runStyle(r api.RunSummary) lipgloss.Style {
	switch {
	case r.Status == runBlocked:
		return m.st.warning
	case activeStatus(r.Status):
		return m.st.accent
	case critical(&r):
		return m.st.critical
	}
	return m.st.plain
}

// clock shows an RFC 3339 time as "Nov 5 09:58" in the TUI's zone.
func (m Model) clock(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return orDash(s)
	}
	return t.In(m.loc).Format("Jan 2 15:04")
}

// runDuration is how long r ran, or has run so far ("…" while it goes).
func (m Model) runDuration(r api.RunSummary) string {
	start, err := time.Parse(time.RFC3339, r.StartedAt)
	if err != nil {
		return "—"
	}
	if r.EndedAt == "" {
		if !activeStatus(r.Status) {
			return "—"
		}
		return exact(m.now().Sub(start)) + "…"
	}
	end, err := time.Parse(time.RFC3339, r.EndedAt)
	if err != nil {
		return "—"
	}
	return exact(end.Sub(start))
}

// exact is a duration to the second: 45s, 1m30s, 2h05m.
func exact(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// definitionLines are the task definition and state, one field a line.
func (m Model) definitionLines() []string {
	def, sum := m.detail.d.Task, m.detail.d.Summary
	agent := strings.TrimSpace(strings.Join(append([]string{def.Agent.Command}, def.Agent.Args...), " "))
	keys := make([]string, 0, len(def.Env))
	for k := range def.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	enabled := "no"
	if def.Enabled {
		enabled = "yes"
	}
	fields := [][2]string{
		{"name", def.Name},
		{"target", def.Target},
		{"cwd", def.Cwd},
		{"schedule", def.Schedule},
		{"timezone", def.Timezone},
		{"enabled", enabled},
		{"paused until", def.PausedUntil},
		{"agent", agent},
		{"prompt file", def.PromptFile},
		{"timeout", def.Timeout},
		{"overlap", def.Overlap},
		{"missed", def.Missed},
		{"on done", def.OnDone},
		{"on blocked", def.OnBlocked},
		{"env", strings.Join(keys, " ")},
	}
	out := []string{"  state         " + stateText(sum.State)}
	if sum.Error != "" {
		out = append(out, m.st.critical.Render("  error         "+sum.Error))
	}
	for _, f := range fields {
		out = append(out, fmt.Sprintf("  %-13s %s", f[0], orDash(f[1])))
	}
	return out
}

func (m Model) detailFooter() string {
	if m.mode == modeConfirm {
		return m.footer()
	}
	return m.st.dim.Render(" ↑/↓ run  enter output  l live output  a attach  esc back")
}

// detailEvent re-reads the open detail when an event concerns its task.
func (m *Model) detailEvent(n api.Notification) tea.Cmd {
	if m.screen != screenDetail {
		return nil
	}
	var id string
	switch n.Method {
	case api.EventTaskChanged:
		var ev api.TaskChangedEvent
		if json.Unmarshal(n.Params, &ev) != nil {
			return nil
		}
		id = ev.ID
	case api.EventRunChanged:
		var ev api.RunChangedEvent
		if json.Unmarshal(n.Params, &ev) != nil {
			return nil
		}
		id = ev.Run.TaskID
	}
	if id != m.detail.id {
		return nil
	}
	return m.fetchDetail()
}
