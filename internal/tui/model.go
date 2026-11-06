// Package tui is the herdrcron terminal UI: one list of all tasks on all
// targets, kept live from the daemon's "subscribe" stream, with every
// action one key away. It talks to the daemon only through a Backend.
// Quitting it leaves the daemon and every run as they are.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// Options configure a Model.
type Options struct {
	Backend Backend
	// Context bounds the subscription; nil means context.Background.
	Context context.Context
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Location is the zone times are shown in; nil means time.Local.
	Location *time.Location
	// Renderer renders the styles; nil means lipgloss's default renderer.
	Renderer *lipgloss.Renderer
	// Exec runs cmd in the foreground with the TUI suspended (attach, the
	// prompt editor) and returns done's message when it ends; nil means
	// tea.ExecProcess. Tests run cmd directly.
	Exec func(cmd *exec.Cmd, done func(error) tea.Msg) tea.Cmd
	// Editor is the editor argv for prompt editing (the file path is
	// appended); nil means $VISUAL, then $EDITOR, then vi, split on spaces.
	Editor []string
}

// screen is the screen the TUI shows. Each one beyond the main list keeps
// its state in its own field of Model and its code in its own file.
type screen int

const (
	screenMain   screen = iota
	screenDetail        // detail.go: task definition, next fires, runs; a run's output tail
	screenOutput        // output.go: live output of an active run
	screenForm          // form.go: new or edit task
)

// mode is what the keyboard drives: the list or one of the prompts.
type mode int

const (
	modeList      mode = iota
	modeFilter         // typing the filter text
	modePauseMenu      // choosing how long to pause the selected task
	modePauseDate      // typing the date to pause the selected task until
	modePauseAll       // choosing what to pause or resume all on
	modeConfirm        // y/n on a destructive action
	modeHelp           // the key help screen
)

// reconnectDelay is how long the TUI waits before subscribing again after
// it lost the daemon.
const reconnectDelay = time.Second

// refreshEvery re-reads the daemon status (uptime, global pause) and
// re-renders relative times. task.changed also re-reads status: pause_all
// and resume_all publish no status event, only task.changed.
const refreshEvery = 30 * time.Second

// runsListLimit is how many recent runs the TUI reads to find active runs.
const runsListLimit = 200

// Model is the Bubble Tea model of the main screen.
type Model struct {
	ctx            context.Context
	backend        Backend
	now            func() time.Time
	loc            *time.Location
	st             styles
	width          int
	height         int
	loaded         bool
	connErr        string // why the TUI has no live connection to the daemon; empty when it has
	gen            int    // generation of the current subscription
	events         <-chan api.Notification
	cancel         context.CancelFunc // ends the current subscription
	status         api.StatusResult
	statusAt       time.Time // when status was read, for the uptime
	statusInFlight bool      // a status read is running
	statusAgain    bool      // another status read should follow the one in flight
	tasks          map[string]api.TaskSummary
	active         map[string]map[string]api.RunSummary // task id → run id → active run

	selected string // id of the selected task
	offset   int    // first visible row
	grouped  bool
	filter   textinput.Model
	dateIn   textinput.Model
	mode     mode
	confirm  confirmation
	pauseAll []pauseAllChoice
	message  string
	msgErr   bool

	screen screen
	exec   func(cmd *exec.Cmd, done func(error) tea.Msg) tea.Cmd
	editor []string
	detail detailState // screenDetail
	output outputState // screenOutput
	form   formState   // screenForm
}

// confirmation is a pending y/n question.
type confirmation struct {
	question string
	yes      tea.Cmd
}

// New returns the main screen model.
func New(o Options) Model {
	m := Model{
		ctx:     o.Context,
		backend: o.Backend,
		now:     o.Now,
		loc:     o.Location,
		tasks:   map[string]api.TaskSummary{},
		active:  map[string]map[string]api.RunSummary{},
	}
	if m.ctx == nil {
		m.ctx = context.Background()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.loc == nil {
		m.loc = time.Local
	}
	r := o.Renderer
	if r == nil {
		r = lipgloss.DefaultRenderer()
	}
	m.st = newStyles(r)
	m.exec = o.Exec
	if m.exec == nil {
		m.exec = func(cmd *exec.Cmd, done func(error) tea.Msg) tea.Cmd {
			return tea.ExecProcess(cmd, done)
		}
	}
	m.editor = o.Editor
	m.filter = textinput.New()
	m.filter.Prompt = "/"
	m.dateIn = textinput.New()
	m.dateIn.Prompt = ""
	m.dateIn.Placeholder = "2026-11-10 08:00"
	return m
}

// Messages.
type (
	// The stream messages carry the generation of the subscription they
	// belong to; those of an abandoned subscription are dropped.
	subscribedMsg struct {
		gen    int
		events <-chan api.Notification
		cancel context.CancelFunc
	}
	streamErrMsg struct {
		gen int
		err error
	}
	eventMsg struct {
		gen int
		n   api.Notification
	}
	reconnectMsg struct{}
	refreshMsg   struct{}
	loadedMsg    struct {
		status api.StatusResult
		tasks  []api.TaskSummary
		runs   []api.RunSummary
	}
	statusMsg struct {
		status api.StatusResult
		err    error
	}
	// doneMsg reports a finished action: text for the message line and
	// whether to re-read the state. Changed tasks arrive on the stream.
	doneMsg struct {
		text   string
		err    error
		reload bool
	}
	// runActiveMsg is a run_now refused because the task already runs.
	runActiveMsg struct{ id string }
)

// Init subscribes to the daemon.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.subscribe(), tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshMsg{} }))
}

// subscribe opens subscription generation m.gen, on a context of its own
// so that abandoning it closes its connection.
func (m Model) subscribe() tea.Cmd {
	parent, b, gen := m.ctx, m.backend, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(parent)
		ch, err := b.Subscribe(ctx)
		if err != nil {
			cancel()
			return streamErrMsg{gen, err}
		}
		return subscribedMsg{gen, ch, cancel}
	}
}

func waitEvent(gen int, ch <-chan api.Notification) tea.Cmd {
	return func() tea.Msg {
		n, ok := <-ch
		if !ok {
			return streamErrMsg{gen, fmt.Errorf("lost the connection to the daemon")}
		}
		return eventMsg{gen, n}
	}
}

// load reads the whole state. It runs after subscribing, so no change
// falls between the two.
func (m Model) load() tea.Cmd {
	ctx, b, gen := m.ctx, m.backend, m.gen
	return func() tea.Msg {
		var msg loadedMsg
		if err := b.Call(ctx, api.MethodStatus, nil, &msg.status); err != nil {
			return streamErrMsg{gen, err}
		}
		var tl api.TasksListResult
		if err := b.Call(ctx, api.MethodTasksList, nil, &tl); err != nil {
			return streamErrMsg{gen, err}
		}
		var rl api.RunsListResult
		if err := b.Call(ctx, api.MethodRunsList, api.RunsListParams{Limit: runsListLimit}, &rl); err != nil {
			return streamErrMsg{gen, err}
		}
		msg.tasks, msg.runs = tl.Tasks, rl.Runs
		return msg
	}
}

func (m Model) loadStatus() tea.Cmd {
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var s api.StatusResult
		err := b.Call(ctx, api.MethodStatus, nil, &s)
		return statusMsg{status: s, err: err}
	}
}

// requestStatus reads the daemon status, coalescing a burst of asks into
// the read already running plus one follow-up. pause_all emits one
// task.changed per task; one read after the burst is enough for the banner.
func (m *Model) requestStatus() tea.Cmd {
	if m.statusInFlight {
		m.statusAgain = true
		return nil
	}
	m.statusInFlight = true
	return m.loadStatus()
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := m.update(msg)
	m.scroll()
	return m, cmd
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	// Each screen handles its own result messages, whichever screen shows.
	if m, cmd, ok := m.detailMsg(msg); ok {
		return m, cmd
	}
	if m, cmd, ok := m.outputMsg(msg); ok {
		return m, cmd
	}
	if m, cmd, ok := m.formMsg(msg); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case subscribedMsg:
		if msg.gen != m.gen {
			msg.cancel()
			return m, nil
		}
		m.events, m.cancel = msg.events, msg.cancel
		return m, tea.Batch(waitEvent(m.gen, msg.events), m.load())
	case streamErrMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		// Abandon this subscription; the next generation starts afresh.
		if m.cancel != nil {
			m.cancel()
		}
		m.connErr = msg.err.Error()
		m.events, m.cancel = nil, nil
		m.gen++
		return m, tea.Tick(reconnectDelay, func(time.Time) tea.Msg { return reconnectMsg{} })
	case reconnectMsg:
		return m, m.subscribe()
	case loadedMsg:
		m.applyLoad(msg)
		return m, nil
	case statusMsg:
		m.statusInFlight = false
		// A burst asked for another read: keep the status from that read,
		// not from the one that was already running when the burst began.
		if msg.err == nil && !m.statusAgain {
			m.status, m.statusAt = msg.status, m.now()
		}
		if m.statusAgain {
			m.statusAgain = false
			return m, m.requestStatus()
		}
		return m, nil
	case refreshMsg:
		next := tea.Tick(refreshEvery, func(time.Time) tea.Msg { return refreshMsg{} })
		if m.connErr != "" {
			return m, next
		}
		return m, tea.Batch(next, m.requestStatus())
	case eventMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		cmd := m.applyEvent(msg.n)
		return m, tea.Batch(waitEvent(m.gen, m.events), cmd)
	case doneMsg:
		m.setMessage(msg.text, msg.err)
		if msg.reload {
			return m, m.load()
		}
		return m, nil
	case runActiveMsg:
		m.ask(fmt.Sprintf("%s already has an active run. Start another in parallel?", msg.id), m.runNow(msg.id, true))
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) applyLoad(msg loadedMsg) {
	m.loaded, m.connErr = true, ""
	m.status, m.statusAt = msg.status, m.now()
	m.tasks = map[string]api.TaskSummary{}
	for _, t := range msg.tasks {
		m.tasks[t.ID] = t
	}
	m.active = map[string]map[string]api.RunSummary{}
	for _, r := range msg.runs {
		m.noteRun(r)
	}
}

func (m *Model) applyEvent(n api.Notification) tea.Cmd {
	switch n.Method {
	case api.EventTaskChanged:
		var ev api.TaskChangedEvent
		if json.Unmarshal(n.Params, &ev) != nil {
			return nil
		}
		if ev.Deleted || ev.Task == nil {
			delete(m.tasks, ev.ID)
			delete(m.active, ev.ID)
		} else {
			m.tasks[ev.ID] = *ev.Task
		}
		// pause_all changes the banner and only publishes task.changed.
		return m.requestStatus()
	case api.EventRunChanged:
		var ev api.RunChangedEvent
		if json.Unmarshal(n.Params, &ev) != nil {
			return nil
		}
		m.noteRun(ev.Run)
		if t, ok := m.tasks[ev.Run.TaskID]; ok && (t.LastRun == nil || t.LastRun.ID == ev.Run.ID || t.LastRun.StartedAt <= ev.Run.StartedAt) {
			r := ev.Run
			t.LastRun = &r
			m.tasks[t.ID] = t
		}
	case api.EventTargetChanged:
		var ev api.TargetChangedEvent
		if json.Unmarshal(n.Params, &ev) != nil {
			return nil
		}
		for i, t := range m.status.Targets {
			if t.Name == ev.Target.Name {
				m.status.Targets[i] = ev.Target
				return nil
			}
		}
		m.status.Targets = append(m.status.Targets, ev.Target)
	}
	return nil
}

// Run statuses the TUI tells apart (the "status" of api.RunSummary).
const (
	runStarting = "starting"
	runRunning  = "running"
	runBlocked  = "blocked"
	runDone     = "done"
	runFailed   = "failed"
	runTimeout  = "timeout"
)

// activeStatus reports whether a run in status s is still going.
func activeStatus(s string) bool {
	return s == runStarting || s == runRunning || s == runBlocked
}

// noteRun records r in the active runs, or drops it once it ended.
func (m *Model) noteRun(r api.RunSummary) {
	if !activeStatus(r.Status) {
		delete(m.active[r.TaskID], r.ID)
		return
	}
	if m.active[r.TaskID] == nil {
		m.active[r.TaskID] = map[string]api.RunSummary{}
	}
	m.active[r.TaskID][r.ID] = r
}

// activeRun returns the newest active run of task id: from the runs seen,
// or else the task's last run if that is still going.
func (m Model) activeRun(id string) (api.RunSummary, bool) {
	var best api.RunSummary
	found := false
	if t, ok := m.tasks[id]; ok && t.LastRun != nil && activeStatus(t.LastRun.Status) {
		best, found = *t.LastRun, true
	}
	for _, r := range m.active[id] {
		if !found || r.StartedAt > best.StartedAt || (r.StartedAt == best.StartedAt && r.ID > best.ID) {
			best, found = r, true
		}
	}
	return best, found
}

func (m *Model) setMessage(text string, err error) {
	if err != nil {
		m.message, m.msgErr = err.Error(), true
		return
	}
	m.message, m.msgErr = text, false
}

func (m *Model) ask(question string, yes tea.Cmd) {
	m.confirm = confirmation{question: question, yes: yes}
	m.mode = modeConfirm
}

// stateRank orders the list: blocked, then running, then the rest.
func stateRank(s string) int {
	switch s {
	case api.StateBlocked:
		return 0
	case api.StateRunning:
		return 1
	}
	return 2
}

// visible returns the tasks the list shows, in display order.
func (m Model) visible() []api.TaskSummary {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	out := make([]api.TaskSummary, 0, len(m.tasks))
	for _, t := range m.tasks {
		// Match the schedule as stored and as shown ("0 7 * * 1" and "Mon 07:00").
		shown := strings.Join([]string{t.ID, t.Name, t.Target, t.Schedule, scheduleText(t.Schedule), t.State}, " ")
		if q != "" && !strings.Contains(strings.ToLower(shown), q) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if m.grouped && a.Target != b.Target {
			return a.Target < b.Target
		}
		if ra, rb := stateRank(a.State), stateRank(b.State); ra != rb {
			return ra < rb
		}
		// Tasks without a next fire time go last.
		if (a.NextFireAt == "") != (b.NextFireAt == "") {
			return a.NextFireAt != ""
		}
		if na, nb := fireTime(a.NextFireAt), fireTime(b.NextFireAt); !na.Equal(nb) {
			return na.Before(nb)
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.ID < b.ID
	})
	return out
}

func fireTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// cursor returns the index of the selected task in tasks, or 0.
func (m Model) cursor(tasks []api.TaskSummary) int {
	for i, t := range tasks {
		if t.ID == m.selected {
			return i
		}
	}
	return 0
}

// current returns the selected task, if the list has any.
func (m Model) current() (api.TaskSummary, bool) {
	tasks := m.visible()
	if len(tasks) == 0 {
		return api.TaskSummary{}, false
	}
	return tasks[m.cursor(tasks)], true
}

// move moves the selection by delta rows, clamped to the list.
func (m *Model) move(delta int) {
	tasks := m.visible()
	if len(tasks) == 0 {
		return
	}
	i := min(max(m.cursor(tasks)+delta, 0), len(tasks)-1)
	m.selected = tasks[i].ID
}

// scroll keeps the selected row inside the visible rows.
func (m *Model) scroll() {
	tasks := m.visible()
	if len(tasks) > 0 {
		m.selected = tasks[m.cursor(tasks)].ID
	}
	n := m.listHeight()
	if n <= 0 {
		m.offset = 0
		return
	}
	line := m.lineOf(tasks, m.cursor(tasks))
	if line < m.offset {
		m.offset = line
	}
	if line >= m.offset+n {
		m.offset = line - n + 1
	}
	if total := len(m.rows(tasks)); m.offset > max(total-n, 0) {
		m.offset = max(total-n, 0)
	}
}
