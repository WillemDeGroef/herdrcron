package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// targetsState is the state of the targets screen. The targets themselves
// are m.status.Targets, kept live by target.changed.
type targetsState struct {
	selected string          // name of the selected target
	checking map[string]bool // targets with a targets.check on its way
	offset   int             // first visible body line
}

// targetCheckMsg is the answer of targets.check.
type targetCheckMsg struct {
	name string
	res  api.TargetCheck
	err  error
}

// openTargets opens the targets screen.
func (m Model) openTargets() (Model, tea.Cmd) {
	m.screen = screenTargets
	m.targets.offset = 0
	if m.targets.checking == nil {
		m.targets.checking = map[string]bool{}
	}
	m.setMessage("", nil)
	return m, nil
}

// targetCursor returns the index of the selected target, or 0.
func (m Model) targetCursor() int {
	for i, t := range m.status.Targets {
		if t.Name == m.targets.selected {
			return i
		}
	}
	return 0
}

// currentTarget returns the selected target, if there is any.
func (m Model) currentTarget() (api.TargetStatus, bool) {
	if len(m.status.Targets) == 0 {
		return api.TargetStatus{}, false
	}
	return m.status.Targets[m.targetCursor()], true
}

// targetsKey handles a key on the targets screen.
func (m Model) targetsKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	ts := m.status.Targets
	switch msg.String() {
	case "esc", "q", "t":
		m.screen = screenMain
		m.setMessage("", nil)
	case "up", "k":
		if len(ts) > 0 {
			m.targets.selected = ts[max(m.targetCursor()-1, 0)].Name
			m.showTargetCursor()
		}
	case "down", "j":
		if len(ts) > 0 {
			m.targets.selected = ts[min(m.targetCursor()+1, len(ts)-1)].Name
			m.showTargetCursor()
		}
	case "pgup":
		m.scrollTargets(-max(m.bodyHeight(), 1))
	case "pgdown":
		m.scrollTargets(max(m.bodyHeight(), 1))
	case "c":
		t, ok := m.currentTarget()
		if !ok {
			return m, nil
		}
		return m, m.checkTarget(t.Name)
	}
	return m, nil
}

// checkTarget runs targets.check on target name.
func (m *Model) checkTarget(name string) tea.Cmd {
	if m.targets.checking == nil {
		m.targets.checking = map[string]bool{}
	}
	if m.targets.checking[name] {
		return nil
	}
	m.targets.checking[name] = true
	m.setMessage("checking "+name+"…", nil)
	ctx, b := m.ctx, m.backend
	return func() tea.Msg {
		var res api.TargetCheck
		err := b.Call(ctx, api.MethodTargetsCheck, api.TargetCheckParams{Name: name}, &res)
		if err != nil {
			err = fmt.Errorf("targets.check %s: %w", name, err)
		}
		return targetCheckMsg{name: name, res: res, err: err}
	}
}

// targetsMsg handles the targets screen's result messages.
func (m Model) targetsMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	cm, ok := msg.(targetCheckMsg)
	if !ok {
		return m, nil, false
	}
	delete(m.targets.checking, cm.name)
	// The message line belongs to whichever screen shows; only the targets
	// screen reports the check there.
	show := m.screen == screenTargets
	if cm.err != nil {
		if show {
			m.setMessage("", cm.err)
		}
		return m, nil, true
	}
	// The daemon also publishes target.changed with this check; applying
	// it here shows it even if that event is still on its way. A newer
	// check (from the CLI, say) that already arrived wins.
	for i, t := range m.status.Targets {
		if t.Name == cm.name {
			res := cm.res
			m.status.Targets[i].LastCheck = newerCheck(t.LastCheck, &res)
		}
	}
	if show {
		m.setMessage(cm.name+": check "+checkVerdict(&cm.res), nil)
	}
	return m, nil, true
}

// checkTime is when c ran; zero if unreadable.
func checkTime(c *api.TargetCheck) time.Time {
	t, _ := time.Parse(time.RFC3339, c.CheckedAt)
	return t
}

// newerCheck is whichever of current and incoming ran later. A missing
// check loses to one that ran: a status read from before c has none, or
// an older one. Equal times keep incoming.
func newerCheck(current, incoming *api.TargetCheck) *api.TargetCheck {
	switch {
	case incoming == nil:
		return current
	case current == nil:
		return incoming
	case checkTime(current).After(checkTime(incoming)):
		return current
	default:
		return incoming
	}
}

// targetObservedAt is the later of when the target last answered and when
// it was last seen to be down. Zero when the status never said.
func targetObservedAt(t api.TargetStatus) time.Time {
	seen, since := fireTime(t.LastSeenAt), fireTime(t.UnreachableSince)
	if since.After(seen) {
		return since
	}
	return seen
}

// mergeStatusTargets keeps a local target observation that is newer than
// the one in a status snapshot. The snapshot is taken when the read
// starts, so it can land after target.changed or after c and carry the
// older reachability or the older check. gen is the rev at which each
// target last changed; readRev is the rev when this read started.
func mergeStatusTargets(local, incoming []api.TargetStatus, gen map[string]int, readRev int) []api.TargetStatus {
	if len(incoming) == 0 || len(local) == 0 {
		return incoming
	}
	prev := make(map[string]api.TargetStatus, len(local))
	for _, t := range local {
		prev[t.Name] = t
	}
	out := slices.Clone(incoming)
	for i, t := range out {
		old, ok := prev[t.Name]
		if !ok {
			continue
		}
		if gen[t.Name] > readRev || targetObservedAt(old).After(targetObservedAt(t)) {
			out[i].Reachability = old.Reachability
			out[i].UnreachableSince = old.UnreachableSince
			out[i].NextRetryAt = old.NextRetryAt
			out[i].LastError = old.LastError
			out[i].LastSeenAt = old.LastSeenAt
		}
		out[i].LastCheck = newerCheck(old.LastCheck, t.LastCheck)
	}
	return out
}

// checkVerdict sums up a check: "ok", "ok with warnings" or "failed".
func checkVerdict(c *api.TargetCheck) string {
	switch {
	case !c.OK:
		return "failed"
	case hasResult(c, api.CheckWarn):
		return "ok with warnings"
	}
	return "ok"
}

func hasResult(c *api.TargetCheck, result string) bool {
	for _, it := range c.Items {
		if it.Result == result {
			return true
		}
	}
	return false
}

// versionWarning reports whether the check's herdr version item (matched
// on its stable kind) warned or failed: a version other than config.yaml's
// herdr_version, or one it could not read.
func versionWarning(c *api.TargetCheck) bool {
	for _, it := range c.Items {
		if it.Kind == api.CheckKindHerdrVersion && (it.Result == api.CheckWarn || it.Result == api.CheckFail) {
			return true
		}
	}
	return false
}

// targetsBody is the targets screen between the title and the footer.
func (m Model) targetsBody() []string {
	ts := m.status.Targets
	if len(ts) == 0 {
		return []string{m.st.dim.Render("  no targets")}
	}
	body := m.targetsTable()
	body = append(body, "")
	return append(body, m.targetDetail(ts[m.targetCursor()])...)
}

// scrollTargets moves the body by delta lines, clamped to it.
func (m *Model) scrollTargets(delta int) {
	n := m.bodyHeight()
	if n <= 0 {
		m.targets.offset = 0
		return
	}
	m.targets.offset = min(max(m.targets.offset+delta, 0), max(len(m.targetsBody())-n, 0))
}

// showTargetCursor scrolls the body just enough to show the selected target.
func (m *Model) showTargetCursor() {
	n := m.bodyHeight()
	if n <= 0 || len(m.status.Targets) == 0 {
		return
	}
	line := 1 + m.targetCursor() // line 0 is the column header
	if line < m.targets.offset {
		m.targets.offset = line
	}
	if line >= m.targets.offset+n {
		m.targets.offset = line - n + 1
	}
	m.targets.offset = min(max(m.targets.offset, 0), max(len(m.targetsBody())-n, 0))
}

// targetsView renders the targets screen.
func (m Model) targetsView() string {
	body := m.targetsBody()
	// Title, message and footer stay; the body scrolls between them.
	if n := m.bodyHeight(); n > 0 && len(body) > n {
		from := min(max(m.targets.offset, 0), len(body)-n)
		body = body[from : from+n]
	}
	var b strings.Builder
	b.WriteString(m.st.title.Render(" herdrcron · targets"))
	b.WriteByte('\n')
	for _, l := range body {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString(m.messageLine())
	b.WriteByte('\n')
	if m.mode == modeConfirm {
		b.WriteString(m.footer())
	} else {
		b.WriteString(m.st.dim.Render(" ↑/↓ select  c check  pgup/pgdn scroll  esc back"))
	}
	return b.String()
}

// targetsTable is one line per target under a column header.
func (m Model) targetsTable() []string {
	type cell struct {
		text  string
		style lipgloss.Style
	}
	head := []string{"TARGET", "REACHABILITY", "HERDR", "LAST CHECK"}
	rows := make([][]cell, 0, len(m.status.Targets))
	for _, t := range m.status.Targets {
		reach, reachStyle := m.reachText(t)
		herdr, herdrStyle := m.herdrText(t.LastCheck)
		last, lastStyle := m.lastCheckText(t)
		rows = append(rows, []cell{{t.Name, m.st.plain}, {reach, reachStyle}, {herdr, herdrStyle}, {last, lastStyle}})
	}
	width := make([]int, len(head))
	for i, h := range head {
		width[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, c := range r {
			width[i] = max(width[i], lipgloss.Width(c.text))
		}
	}
	parts := make([]string, len(head))
	for i, h := range head {
		parts[i] = pad(h, width[i])
	}
	out := []string{m.st.header.Render("  " + strings.TrimRight(strings.Join(parts, gap), " "))}
	cur := m.targetCursor()
	for i, r := range rows {
		parts := make([]string, len(r))
		for c, cl := range r {
			s := cl.text
			if c < len(r)-1 {
				s = pad(s, width[c])
			}
			parts[c] = cl.style.Render(s)
		}
		mark := "  "
		if i == cur {
			mark = "› "
		}
		out = append(out, mark+strings.Join(parts, gap))
	}
	return out
}

// reachText is the reachability column of t.
func (m Model) reachText(t api.TargetStatus) (string, lipgloss.Style) {
	switch t.Reachability {
	case api.ReachReachable:
		return "● reachable", m.st.plain
	case api.ReachUnreachable:
		s := "○ unreachable"
		if since, err := time.Parse(time.RFC3339, t.UnreachableSince); err == nil {
			s += " " + short(max(m.now().Sub(since), 0))
		}
		return s, m.st.critical
	}
	return "? unknown", m.st.dim
}

// herdrText is the herdr version column: the version the last check read,
// a warning when it is not the version config.yaml expects.
func (m Model) herdrText(c *api.TargetCheck) (string, lipgloss.Style) {
	if c == nil {
		return "—", m.st.dim
	}
	if versionWarning(c) {
		if c.HerdrVersion == "" {
			return "! unreadable", m.st.warning
		}
		s := "! " + c.HerdrVersion
		if c.WantHerdrVersion != "" {
			s += " (want " + c.WantHerdrVersion + ")"
		}
		return s, m.st.warning
	}
	if c.HerdrVersion == "" {
		return "—", m.st.dim
	}
	return c.HerdrVersion, m.st.plain
}

// lastCheckText is the last preflight column of t.
func (m Model) lastCheckText(t api.TargetStatus) (string, lipgloss.Style) {
	if m.targets.checking[t.Name] {
		return "… checking", m.st.accent
	}
	c := t.LastCheck
	if c == nil {
		return "— never", m.st.dim
	}
	when := ""
	if at, err := time.Parse(time.RFC3339, c.CheckedAt); err == nil {
		when = " " + ago(max(m.now().Sub(at), 0))
	}
	switch checkVerdict(c) {
	case "failed":
		return "✗ failed" + when, m.st.critical
	case "ok with warnings":
		return "! warnings" + when, m.st.warning
	}
	return "✓ ok" + when, m.st.plain
}

// targetDetail is the block under the table for the selected target: its
// outage and the items of its last check.
func (m Model) targetDetail(t api.TargetStatus) []string {
	head := " " + t.Name
	if t.Reachability == api.ReachUnreachable {
		if t.UnreachableSince != "" {
			head += " · unreachable since " + m.clock(t.UnreachableSince)
		}
		if t.NextRetryAt != "" {
			head += " · retry " + m.clock(t.NextRetryAt)
		}
	} else if t.LastSeenAt != "" {
		head += " · last seen " + m.clock(t.LastSeenAt)
	}
	out := []string{m.st.header.Render(head)}
	if t.LastError != "" && t.Reachability == api.ReachUnreachable {
		out = append(out, m.st.critical.Render("  "+t.LastError))
	}
	c := t.LastCheck
	if c == nil {
		return append(out, m.st.dim.Render("  not checked since the daemon started; c checks"))
	}
	out = append(out, m.st.dim.Render("  last check "+m.clock(c.CheckedAt)))
	width := 0
	for _, it := range c.Items {
		width = max(width, lipgloss.Width(it.Name))
	}
	for _, it := range c.Items {
		mark, style := "✓", m.st.plain
		switch it.Result {
		case api.CheckWarn:
			mark, style = "!", m.st.warning
		case api.CheckFail:
			mark, style = "✗", m.st.critical
		case api.CheckSkip:
			mark, style = "-", m.st.dim
		}
		line := mark + " " + pad(it.Name, width)
		if it.Detail != "" {
			line += gap + it.Detail
		}
		if len(it.Tasks) > 0 {
			line += gap + "(" + strings.Join(it.Tasks, ", ") + ")"
		}
		out = append(out, "  "+style.Render(strings.TrimRight(line, " ")))
	}
	return out
}
