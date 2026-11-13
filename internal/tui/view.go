package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// styles are the TUI's colours: warning, critical, accent and dim, in the
// terminal's own ANSI palette.
type styles struct {
	title, header, warning, critical, accent, dim, plain lipgloss.Style
}

func newStyles(r *lipgloss.Renderer) styles {
	return styles{
		title:    r.NewStyle().Bold(true),
		header:   r.NewStyle().Bold(true),
		warning:  r.NewStyle().Foreground(lipgloss.Color("3")),
		critical: r.NewStyle().Foreground(lipgloss.Color("1")),
		accent:   r.NewStyle().Foreground(lipgloss.Color("6")),
		dim:      r.NewStyle().Faint(true),
		plain:    r.NewStyle(),
	}
}

// keyHelp is the footer of the list, as in the spec. On a narrow terminal
// footerKeys drops keys from the end, but always keeps help and quit.
var keyHelp = []string{"space on/off", "p pause", "r run now", "n new", "e edit", "d delete",
	"enter details", "a attach", "/ filter", "? help", "q quit"}

// footerKeys is the key help that fits in width columns, with its leading
// space.
func footerKeys(width int) string {
	keys, tail := keyHelp[:len(keyHelp)-2], keyHelp[len(keyHelp)-2:]
	for n := len(keys); n >= 0; n-- {
		s := strings.Join(append(slices.Clone(keys[:n]), tail...), "  ")
		if width <= 0 || 1+len([]rune(s)) <= width {
			return s
		}
	}
	return strings.Join(tail, "  ")
}

var columns = []string{"TARGET", "TASK", "SCHEDULE", "STATE", "LAST", "NEXT"}

// lastColumn is the index of LAST in columns.
const lastColumn = 4

const gap = "  "

// row is one line of the list: a task, or a target heading when grouped.
type row struct {
	task    int    // index into the visible tasks; -1 for a heading
	heading string // target name of a heading
}

func (m Model) rows(tasks []api.TaskSummary) []row {
	var out []row
	for i, t := range tasks {
		if m.grouped && (i == 0 || tasks[i-1].Target != t.Target) {
			out = append(out, row{task: -1, heading: orDash(t.Target)})
		}
		out = append(out, row{task: i})
	}
	return out
}

// lineOf returns the row index of task i.
func (m Model) lineOf(tasks []api.TaskSummary, i int) int {
	for n, r := range m.rows(tasks) {
		if r.task == i {
			return n
		}
	}
	return 0
}

// chrome is the number of lines around the list: title, banner, column
// header, message and footer.
func (m Model) chrome() int {
	n := 4
	if m.banner() != "" {
		n++
	}
	return n
}

// listHeight is how many rows fit; 0 means no limit (size unknown).
func (m Model) listHeight() int {
	if m.height == 0 {
		return 0
	}
	return max(m.height-m.chrome(), 1)
}

// View renders the screen.
func (m Model) View() string {
	switch m.screen {
	case screenDetail:
		return m.fit(m.detailView())
	case screenOutput:
		return m.fit(m.outputView())
	case screenForm:
		return m.fit(m.formView())
	case screenTargets:
		return m.fit(m.targetsView())
	}
	if m.mode == modeHelp {
		return m.fit(m.helpView())
	}
	var b strings.Builder
	b.WriteString(m.titleLine())
	b.WriteByte('\n')
	if banner := m.banner(); banner != "" {
		b.WriteString(banner)
		b.WriteByte('\n')
	}
	b.WriteString(m.table())
	b.WriteString(m.messageLine())
	b.WriteByte('\n')
	b.WriteString(m.footer())
	return m.fit(b.String())
}

// fit cuts every line to the terminal width.
func (m Model) fit(s string) string {
	if m.width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "…")
	}
	return strings.Join(lines, "\n")
}

func (m Model) titleLine() string {
	parts := []string{m.st.title.Render("herdrcron")}
	if m.loaded {
		up := time.Duration(m.status.UptimeSeconds)*time.Second + m.now().Sub(m.statusAt)
		parts = append(parts, "daemon up "+short(up))
		var ts []string
		for _, t := range m.status.Targets {
			ts = append(ts, m.targetBadge(t))
		}
		if len(ts) > 0 {
			parts = append(parts, "targets: "+strings.Join(ts, "  "))
		}
	}
	return " " + strings.Join(parts, " · ")
}

func (m Model) targetBadge(t api.TargetStatus) string {
	switch t.Reachability {
	case api.ReachReachable:
		return t.Name + " ●"
	case api.ReachUnreachable:
		return t.Name + " " + m.st.critical.Render("○ unreachable")
	}
	return t.Name + " " + m.st.dim.Render("?")
}

// banner is the line under the title: a lost daemon, or a global pause.
func (m Model) banner() string {
	switch {
	case m.connErr != "" && !m.loaded:
		return m.st.critical.Render(" ✗ cannot reach the daemon: " + m.connErr + " — retrying")
	case m.connErr != "":
		return m.st.critical.Render(" ✗ " + m.connErr + " — reconnecting; the list may be stale")
	case m.status.PausedAll:
		return m.st.warning.Render(" ⏸ ALL TARGETS PAUSED — no task fires until resumed (P)")
	case len(m.status.PausedTargets) > 0:
		return m.st.warning.Render(" ⏸ PAUSED: " + strings.Join(m.status.PausedTargets, ", ") + " — no task there fires until resumed (P)")
	}
	return ""
}

// cells returns the column texts of t.
func (m Model) cells(t api.TaskSummary) []string {
	return []string{orDash(t.Target), t.ID, orDash(scheduleText(t.Schedule)), stateText(t.State), m.lastText(t.LastRun), m.nextText(t)}
}

// scheduleText shows the common cron forms as the spec mockup does:
// "0 2 * * *" as "02:00 daily", "0 7 * * 1" as "Mon 07:00". Anything else
// is shown as written.
func scheduleText(s string) string {
	f := strings.Fields(s)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" {
		return s
	}
	minute, errM := strconv.Atoi(f[0])
	hour, errH := strconv.Atoi(f[1])
	if errM != nil || errH != nil || minute < 0 || minute > 59 || hour < 0 || hour > 23 {
		return s
	}
	clock := fmt.Sprintf("%02d:%02d", hour, minute)
	if f[4] == "*" {
		return clock + " daily"
	}
	day, err := strconv.Atoi(f[4])
	if err != nil || day < 0 || day > 7 {
		return s
	}
	return time.Weekday(day % 7).String()[:3] + " " + clock
}

func stateText(s string) string {
	glyph := map[string]string{
		api.StateIdle: "●", api.StateRunning: "◐", api.StatePaused: "⏸", api.StateOff: "○",
		api.StateBlocked: "!", api.StateInvalid: "✗",
	}[s]
	if glyph == "" {
		return s
	}
	return glyph + " " + s
}

func (m Model) lastText(r *api.RunSummary) string {
	if r == nil {
		return "—"
	}
	if activeStatus(r.Status) {
		return "…"
	}
	at := r.EndedAt
	if at == "" {
		at = r.StartedAt
	}
	mark := "✗"
	if r.Status == runDone {
		mark = "✓"
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return mark + " " + r.Status
	}
	return mark + " " + ago(m.now().Sub(t))
}

// nextText is the NEXT column: when the task fires next, or the pause that
// holds it. The daemon's next fire time ignores pauses, so a paused task
// shows its pause instead, whatever its state (a manual run shows as
// running over paused).
func (m Model) nextText(t api.TaskSummary) string {
	if t.State == api.StateOff || t.State == api.StateInvalid {
		return "—"
	}
	h := m.holdOn(t.Target, t.PausedUntil)
	switch {
	case h.endless():
		return "paused"
	case !h.none():
		return "until " + m.when(t.PausedUntil)
	case t.State == api.StatePaused && !pauseEnded(t.PausedUntil, m.now()):
		// Paused with no time of its own: a global pause the status read
		// has not shown yet. A timestamp that has passed does not land
		// here; that pause has ended and the next fire stands.
		return "paused"
	case t.NextFireAt == "":
		return "—"
	}
	return m.when(t.NextFireAt)
}

// when formats an RFC 3339 time: weekday and clock within a week, else
// the date.
func (m Model) when(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	t = t.In(m.loc)
	if d := t.Sub(m.now()); d >= 0 && d < 6*24*time.Hour {
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2")
}

// critical reports whether the last run ended badly enough to show red.
func critical(r *api.RunSummary) bool {
	return r != nil && (r.Status == runFailed || r.Status == runTimeout)
}

// rowStyle is the colour of a task's row.
func (m Model) rowStyle(t api.TaskSummary) lipgloss.Style {
	switch t.State {
	case api.StateBlocked:
		return m.st.warning
	case api.StateRunning:
		return m.st.accent
	case api.StateInvalid:
		return m.st.critical
	case api.StateOff, api.StatePaused:
		return m.st.dim
	}
	return m.st.plain
}

func (m Model) table() string {
	tasks := m.visible()
	width := make([]int, len(columns))
	for i, c := range columns {
		width[i] = lipgloss.Width(c)
	}
	all := make([][]string, len(tasks))
	for i, t := range tasks {
		all[i] = m.cells(t)
		for c, s := range all[i] {
			width[c] = max(width[c], lipgloss.Width(s))
		}
	}
	var b strings.Builder
	head := make([]string, len(columns))
	for i, c := range columns {
		head[i] = pad(c, width[i])
	}
	b.WriteString(m.st.header.Render("  " + strings.TrimRight(strings.Join(head, gap), " ")))
	b.WriteByte('\n')

	rows := m.rows(tasks)
	from, to := 0, len(rows)
	if n := m.listHeight(); n > 0 {
		from = min(m.offset, len(rows))
		to = min(from+n, len(rows))
	}
	cur := m.cursor(tasks)
	for _, r := range rows[from:to] {
		if r.task < 0 {
			b.WriteString(m.st.header.Render(" ▾ " + r.heading))
			b.WriteByte('\n')
			continue
		}
		b.WriteString(m.taskLine(tasks[r.task], all[r.task], width, r.task == cur))
		b.WriteByte('\n')
	}
	if len(tasks) == 0 {
		switch {
		case !m.loaded:
			b.WriteString(m.st.dim.Render("  loading…"))
		case m.filter.Value() != "":
			b.WriteString(m.st.dim.Render("  no task matches the filter"))
		default:
			b.WriteString(m.st.dim.Render("  no tasks yet"))
		}
		b.WriteByte('\n')
	}
	// Pad the list to its height so the footer stays at the bottom.
	if n := m.listHeight(); n > 0 {
		shown := to - from
		if len(tasks) == 0 {
			shown = 1
		}
		b.WriteString(strings.Repeat("\n", max(n-shown, 0)))
	}
	return b.String()
}

func (m Model) taskLine(t api.TaskSummary, cells []string, width []int, selected bool) string {
	base := m.rowStyle(t)
	marker := " "
	if selected {
		base = base.Reverse(true)
		marker = "›"
	}
	parts := make([]string, len(cells))
	for i, c := range cells {
		s := base
		if i == lastColumn && critical(t.LastRun) {
			s = m.st.critical.Inherit(base)
		}
		text := pad(c, width[i])
		if i == len(cells)-1 && !selected {
			text = c
		}
		parts[i] = s.Render(text)
	}
	return marker + " " + strings.Join(parts, base.Render(gap))
}

func pad(s string, w int) string {
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

func (m Model) messageLine() string {
	if m.message == "" {
		return ""
	}
	if m.msgErr {
		return m.st.critical.Render(" " + m.message)
	}
	return " " + m.message
}

func (m Model) footer() string {
	name := ""
	if t, ok := m.current(); ok {
		name = t.ID
	}
	switch m.mode {
	case modeFilter:
		return " " + m.filter.View() + m.st.dim.Render("   enter keep  esc clear")
	case modePauseMenu:
		return " Pause " + name + ": 1 for 1 h · 2 until tomorrow 08:00 · 3 until a date · 4 indefinitely · esc"
	case modePauseDate:
		return " Pause " + name + " until (YYYY-MM-DD [HH:MM]): " + m.dateIn.View()
	case modePauseAll:
		var opts []string
		for i, c := range m.pauseAll {
			opts = append(opts, fmt.Sprintf("%d %s", i+1, c.label))
		}
		return " " + strings.Join(opts, "  ") + "  esc cancel"
	case modeConfirm:
		return m.st.warning.Render(" " + m.confirm.question + " y/n")
	}
	help := footerKeys(m.width)
	if f := m.filter.Value(); f != "" {
		help = "filter: " + f + " (esc clears)  " + help
	}
	return m.st.dim.Render(" " + help)
}

func (m Model) helpView() string {
	keys := [][2]string{
		{"↑/k ↓/j", "move"},
		{"space", "enable or disable the selected task"},
		{"p", "pause: 1 h, until tomorrow 08:00, until a date, indefinitely; resumes a paused task"},
		{"P", "pause or resume all on the selected task's target, or on all targets"},
		{"r", "run now (asks before a parallel run)"},
		{"s", "stop the active run (asks first)"},
		{"enter", "task detail: definition, next fires, last runs; enter on a run shows its output"},
		{"l", "live output of the active run"},
		{"a", "attach to the active run's agent (detach with ctrl+b q)"},
		{"n", "new task (form)"},
		{"e", "edit the task (form)"},
		{"E", "edit the prompt in $EDITOR"},
		{"d", "delete the task (asks first; offers stop and delete with an active run)"},
		{"t", "targets: reachability, herdr version, last preflight; c checks"},
		{"/", "filter by text; esc clears"},
		{"g", "group by target"},
		{"?", "this help"},
		{"q", "quit (the daemon and its runs keep going)"},
	}
	var b strings.Builder
	b.WriteString(m.st.title.Render(" herdrcron keys"))
	b.WriteString("\n\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-9s %s\n", k[0], k[1])
	}
	b.WriteString("\n")
	b.WriteString(m.st.dim.Render(" any key closes this help"))
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ago is the age of an event d ago: 28m ago, 5h ago, 3d ago.
func ago(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return short(d) + " ago"
}

// short is a coarse duration: 45s, 28m, 5h, 3d.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
