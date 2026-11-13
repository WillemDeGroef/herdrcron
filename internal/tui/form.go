package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/shellquote"
)

// fieldKind is how a form field is edited.
type fieldKind int

const (
	kindText   fieldKind = iota // a line of text
	kindPick                    // one of a few values, cycled with ←/→ or space
	kindToggle                  // on or off
	kindPrompt                  // the prompt text, edited in the editor
)

// formField is one field of the form. Key is the task field's JSON name,
// as the daemon keys its validation errors.
type formField struct {
	key     string
	label   string
	kind    fieldKind
	input   textinput.Model // kindText
	options []string        // kindPick
	choice  int             // kindPick
	on      bool            // kindToggle
	fixed   bool            // shown, not editable (the id of an edited task)
}

func (f formField) value() string {
	switch f.kind {
	case kindPick:
		if f.choice < len(f.options) {
			return f.options[f.choice]
		}
		return ""
	case kindToggle:
		if f.on {
			return "yes"
		}
		return "no"
	}
	return f.input.Value()
}

// formState is the state of the new/edit task form.
type formState struct {
	seq      int  // generation of the form, so results of a closed form are dropped
	editing  bool // tasks.update of id; else tasks.create
	id       string
	version  string
	prompt   string
	fields   []formField
	focus    int
	initial  string              // snapshot of the values as opened, to tell edits apart
	errs     map[string][]string // problems by field key
	general  []string            // problems with no field of their own
	conflict bool                // the task changed on disk since the form opened
	saving   bool
}

// The values the policy pickers offer: those config accepts.
var (
	overlapValues   = config.EnumValues("overlap")
	missedValues    = config.EnumValues("missed")
	onDoneValues    = config.EnumValues("on_done")
	onBlockedValues = config.EnumValues("on_blocked")
)

// Form messages. Each carries the form's seq.
type (
	formSavedMsg struct {
		seq int
		id  string
	}
	formErrMsg struct {
		seq      int
		fields   map[string][]string
		conflict bool
		err      error
	}
	formLoadedMsg struct {
		seq int
		det api.TaskDetail
		err error
	}
	formCloseMsg struct{ seq int }
	// formReloadMsg reloads the form's task from disk, edits lost.
	formReloadMsg struct{ seq int }
)

// newTask is the definition a new task starts from: config.NewTask (the
// daemon's defaults, an agent command and a timeout) and the first target.
func (m Model) newTask() api.TaskDef {
	d := config.NewTask("")
	def := api.TaskDef{Enabled: d.Enabled, Overlap: d.Overlap, Missed: d.Missed,
		OnDone: d.OnDone, OnBlocked: d.OnBlocked, Target: config.LocalTarget,
		Agent:   api.AgentDef{Command: d.Agent.Command, Args: d.Agent.Args},
		Timeout: config.FormatDuration(d.Timeout.Duration)}
	if len(m.status.Targets) > 0 {
		def.Target = m.status.Targets[0].Name
	}
	return def
}

// openNewForm opens the form for a new task.
func (m Model) openNewForm() (Model, tea.Cmd) {
	m.form = m.buildForm(m.newTask(), "", "", false)
	m.screen = screenForm
	m.setMessage("", nil)
	return m, nil
}

// openEditForm opens the form on task id. The form shows at once, while
// tasks.get is in flight: a load that finished later must not cover a
// screen the user opened in the meantime, and a failed load must not
// send them back to the list from detail or live output.
func (m Model) openEditForm(id string) (Model, tea.Cmd) {
	m.form = formState{seq: m.form.seq + 1}
	m.screen = screenForm
	m.setMessage("", nil)
	return m, m.loadForm(id)
}

func (m Model) loadForm(id string) tea.Cmd {
	ctx, b, seq := m.ctx, m.backend, m.form.seq
	return func() tea.Msg {
		var det api.TaskDetail
		err := b.Call(ctx, api.MethodTasksGet, api.TaskIDParams{ID: id}, &det)
		if err != nil {
			err = fmt.Errorf("read %s: %w", id, err)
		}
		return formLoadedMsg{seq: seq, det: det, err: err}
	}
}

// buildForm makes the fields for def.
func (m Model) buildForm(def api.TaskDef, prompt, version string, editing bool) formState {
	text := func(key, label, v string) formField {
		in := textinput.New()
		in.Prompt = ""
		in.SetValue(v)
		return formField{key: key, label: label, kind: kindText, input: in}
	}
	pick := func(key, label string, options []string, v string) formField {
		f := formField{key: key, label: label, kind: kindPick, options: options}
		f.choice = indexOf(options, v)
		if f.choice < 0 {
			f.options = append([]string{v}, options...)
			f.choice = 0
		}
		return f
	}
	var targets []string
	for _, t := range m.status.Targets {
		targets = append(targets, t.Name)
	}
	if len(targets) == 0 {
		targets = []string{config.LocalTarget}
	}
	id := text("id", "id", def.ID)
	id.fixed = editing
	fields := []formField{
		id,
		text("name", "name", def.Name),
		pick("target", "target", targets, def.Target),
		text("cwd", "cwd", def.Cwd),
		text("schedule", "schedule", def.Schedule),
		text("timezone", "timezone", def.Timezone),
		{key: "enabled", label: "enabled", kind: kindToggle, on: def.Enabled},
		text("paused_until", "paused until", def.PausedUntil),
		text("agent.command", "agent command", def.Agent.Command),
		text("agent.args", "agent args", shellquote.Join(def.Agent.Args...)),
		text("prompt_file", "prompt file", def.PromptFile),
		text("timeout", "timeout", def.Timeout),
		pick("overlap", "overlap", overlapValues, def.Overlap),
		pick("missed", "missed", missedValues, def.Missed),
		pick("on_done", "on done", onDoneValues, def.OnDone),
		pick("on_blocked", "on blocked", onBlockedValues, def.OnBlocked),
		text("env", "env", envText(def.Env)),
		{key: "prompt", label: "prompt", kind: kindPrompt},
	}
	s := formState{seq: m.form.seq + 1, editing: editing, id: def.ID, version: version, prompt: prompt, fields: fields}
	tz := "empty: the daemon's zone"
	if m.status.Timezone != "" {
		tz = "empty: " + m.status.Timezone
	}
	for key, p := range map[string]string{
		"timezone": tz, "paused_until": "empty, RFC 3339 time or indefinite", "agent.args": "e.g. --flag 'two words'",
		"prompt_file": "empty: prompts/<id>.md", "timeout": "e.g. 2h", "env": "e.g. KEY=VALUE OTHER='two words'",
	} {
		s.field(key).input.Placeholder = p
	}
	if editing {
		s.focus = 1
	}
	s.focusField()
	s.initial = s.snapshot()
	return s
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func envText(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	words := make([]string, len(keys))
	for i, k := range keys {
		words[i] = shellquote.Quote(k + "=" + env[k])
	}
	return strings.Join(words, " ")
}

// snapshot is every value of the form, to tell whether it changed.
func (s formState) snapshot() string {
	var b strings.Builder
	for _, f := range s.fields {
		b.WriteString(f.value())
		b.WriteByte(0)
	}
	b.WriteString(s.prompt)
	return b.String()
}

func (s formState) changed() bool { return s.snapshot() != s.initial }

// focusField focuses the input of the field under the cursor.
func (s *formState) focusField() {
	for i := range s.fields {
		if s.fields[i].kind != kindText {
			continue
		}
		if i == s.focus && !s.fields[i].fixed {
			s.fields[i].input.Focus()
			s.fields[i].input.CursorEnd()
		} else {
			s.fields[i].input.Blur()
		}
	}
}

func (s formState) field(key string) *formField {
	for i := range s.fields {
		if s.fields[i].key == key {
			return &s.fields[i]
		}
	}
	return nil
}

func (s formState) get(key string) string { return s.field(key).value() }

// def builds the task definition from the form. Problems found here
// (args and env that do not parse) are keyed by field.
func (s formState) def() (api.TaskDef, map[string][]string) {
	problems := map[string][]string{}
	def := api.TaskDef{
		ID: strings.TrimSpace(s.get("id")), Name: s.get("name"), Target: s.get("target"),
		Cwd: s.get("cwd"), Schedule: strings.TrimSpace(s.get("schedule")), Timezone: strings.TrimSpace(s.get("timezone")),
		Enabled: s.field("enabled").on, PausedUntil: strings.TrimSpace(s.get("paused_until")),
		Agent:      api.AgentDef{Command: strings.TrimSpace(s.get("agent.command"))},
		PromptFile: strings.TrimSpace(s.get("prompt_file")), Timeout: strings.TrimSpace(s.get("timeout")),
		Overlap: s.get("overlap"), Missed: s.get("missed"), OnDone: s.get("on_done"), OnBlocked: s.get("on_blocked"),
	}
	args, err := splitWords(s.get("agent.args"))
	if err != nil {
		problems["agent.args"] = []string{err.Error()}
	}
	if len(args) > 0 {
		def.Agent.Args = args
	}
	env, err := parseEnv(s.get("env"))
	if err != nil {
		problems["env"] = []string{err.Error()}
	}
	def.Env = env
	return def, problems
}

// splitWords splits s into words as a POSIX shell would, without any
// expansion: single quotes keep everything, double quotes keep all but
// \" and \\, a backslash outside quotes escapes the next character.
func splitWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case ' ', '\t', '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\'':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j == len(rs) {
				return nil, errors.New("unterminated ' quote")
			}
			cur.WriteString(string(rs[i+1 : j]))
			i = j
		case '"':
			inWord = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == '"' || rs[j+1] == '\\') {
					j++
				}
				cur.WriteRune(rs[j])
			}
			if j == len(rs) {
				return nil, errors.New(`unterminated " quote`)
			}
			i = j
		case '\\':
			inWord = true
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
		default:
			inWord = true
			cur.WriteRune(c)
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// parseEnv reads KEY=VALUE words.
func parseEnv(s string) (map[string]string, error) {
	words, err := splitWords(s)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not KEY=VALUE", w)
		}
		env[k] = v
	}
	return env, nil
}

// save sends the form to the daemon.
func (m Model) save() (Model, tea.Cmd) {
	def, problems := m.form.def()
	if len(problems) > 0 {
		m.form.errs, m.form.general = problems, nil
		return m, nil
	}
	p := api.TaskWriteParams{Task: def, Prompt: m.form.prompt}
	method := api.MethodTasksCreate
	if m.form.editing {
		method = api.MethodTasksUpdate
		p.ID, p.Version = m.form.id, m.form.version
	}
	m.form.saving = true
	ctx, b, seq := m.ctx, m.backend, m.form.seq
	return m, func() tea.Msg {
		var det api.TaskDetail
		err := b.Call(ctx, method, p, &det)
		if err == nil {
			return formSavedMsg{seq: seq, id: def.ID}
		}
		var aerr *api.Error
		if errors.As(err, &aerr) {
			switch aerr.Code {
			case api.CodeValidation:
				return formErrMsg{seq: seq, fields: validationFields(aerr), err: err}
			case api.CodeVersionMismatch:
				return formErrMsg{seq: seq, conflict: true, err: err}
			}
		}
		return formErrMsg{seq: seq, err: fmt.Errorf("%s: %w", method, err)}
	}
}

// validationFields reads a CodeValidation error's data: a typed
// ValidationErrorData from in-process callers, a map[string]any off the
// socket. Re-marshalling covers both.
func validationFields(e *api.Error) map[string][]string {
	var d api.ValidationErrorData
	if data, err := json.Marshal(e.Data); err == nil {
		_ = json.Unmarshal(data, &d)
	}
	return d.Fields
}

// closeForm goes back to the main list.
func (m Model) closeForm() Model {
	m.screen = screenMain
	m.form = formState{seq: m.form.seq + 1}
	return m
}

// formMsg handles the form's, the editor's and delete's result messages.
func (m Model) formMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	if m, cmd, ok := m.editorMsg(msg); ok {
		return m, cmd, true
	}
	if m, cmd, ok := m.deleteMsg(msg); ok {
		return m, cmd, true
	}
	switch msg := msg.(type) {
	case formLoadedMsg:
		if msg.seq != m.form.seq || m.screen != screenForm {
			return m, nil, true
		}
		if msg.err != nil {
			m.setMessage("", msg.err)
			return m.closeForm(), nil, true
		}
		d := msg.det
		m.form = m.buildForm(d.Task, d.Prompt, d.Version, true)
		m.form.seq = msg.seq
		m.form.id = d.Task.ID
		if m.form.id == "" {
			m.form.id = d.Summary.ID
			m.form.field("id").input.SetValue(d.Summary.ID)
			m.form.initial = m.form.snapshot()
		}
		m.screen = screenForm
		m.setMessage("", nil)
		return m, nil, true
	case formSavedMsg:
		if msg.seq != m.form.seq {
			return m, nil, true
		}
		m = m.closeForm()
		m.setMessage("saved "+msg.id, nil)
		return m, m.load(), true
	case formErrMsg:
		if msg.seq != m.form.seq {
			return m, nil, true
		}
		m.form.saving = false
		m.form.errs, m.form.general, m.form.conflict = map[string][]string{}, nil, msg.conflict
		switch {
		case msg.conflict:
		case msg.fields != nil:
			for k, v := range msg.fields {
				if m.form.field(k) != nil {
					m.form.errs[k] = v
				} else {
					for _, p := range v {
						m.form.general = append(m.form.general, k+": "+p)
					}
				}
			}
			sort.Strings(m.form.general)
		default:
			m.form.general = []string{msg.err.Error()}
		}
		return m, nil, true
	case formReloadMsg:
		if msg.seq == m.form.seq && m.screen == screenForm {
			return m, m.loadForm(m.form.id), true
		}
		return m, nil, true
	case formCloseMsg:
		if msg.seq == m.form.seq && m.screen == screenForm {
			m = m.closeForm()
			m.setMessage("discarded the changes", nil)
		}
		return m, nil, true
	}
	return m, nil, false
}

// formKey handles a key on the form.
func (m Model) formKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	s := &m.form
	if len(s.fields) == 0 {
		if msg.String() == "esc" {
			return m.closeForm(), nil
		}
		return m, nil
	}
	f := &s.fields[s.focus]
	switch msg.String() {
	case "ctrl+s":
		if s.saving {
			return m, nil
		}
		return m.save()
	case "esc":
		if s.changed() {
			seq := s.seq
			m.ask("Discard the changes to "+m.formName()+"?", func() tea.Msg { return formCloseMsg{seq} })
			return m, nil
		}
		return m.closeForm(), nil
	case "ctrl+r":
		if !s.editing || !s.conflict {
			return m, nil
		}
		if s.changed() {
			seq := s.seq
			m.ask("Reload "+s.id+" from disk and lose your edits?", func() tea.Msg { return formReloadMsg{seq} })
			return m, nil
		}
		return m, m.loadForm(s.id)
	case "tab", "down":
		s.focus = (s.focus + 1) % len(s.fields)
		s.focusField()
		return m, nil
	case "shift+tab", "up":
		s.focus = (s.focus + len(s.fields) - 1) % len(s.fields)
		s.focusField()
		return m, nil
	}
	switch f.kind {
	case kindPick:
		switch msg.String() {
		case "right", " ", "l":
			f.choice = (f.choice + 1) % len(f.options)
			s.edited(f.key)
		case "left", "h":
			f.choice = (f.choice + len(f.options) - 1) % len(f.options)
			s.edited(f.key)
		case "enter":
			return m.nextField(), nil
		}
	case kindToggle:
		switch msg.String() {
		case "right", "left", " ", "h", "l":
			f.on = !f.on
			s.edited(f.key)
		case "enter":
			return m.nextField(), nil
		}
	case kindPrompt:
		if msg.String() == "enter" {
			return m, m.editFormPrompt()
		}
	case kindText:
		if msg.String() == "enter" {
			return m.nextField(), nil
		}
		if f.fixed {
			return m, nil
		}
		before := f.input.Value()
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		if f.input.Value() != before {
			s.edited(f.key)
		}
		return m, cmd
	}
	return m, nil
}

// edited drops the problems shown for field key: they were about the
// value the user just changed.
func (s *formState) edited(key string) {
	delete(s.errs, key)
}

func (m Model) nextField() Model {
	m.form.focus = (m.form.focus + 1) % len(m.form.fields)
	m.form.focusField()
	return m
}

func (m Model) formName() string {
	if m.form.editing {
		return m.form.id
	}
	return "the new task"
}

// labelWidth is the width of the label column.
const labelWidth = 14

// formView renders the form.
func (m Model) formView() string {
	s := m.form
	var b strings.Builder
	if len(s.fields) == 0 {
		b.WriteString(m.st.title.Render(" herdrcron") + " · loading the task…\n")
		b.WriteString(m.st.dim.Render(" esc back"))
		return b.String()
	}
	title := " herdrcron · new task"
	if s.editing {
		title = " herdrcron · edit task " + s.id
	}
	b.WriteString(m.st.title.Render(title))
	b.WriteString("\n\n")
	for i, f := range s.fields {
		marker := "  "
		if i == s.focus {
			marker = "› "
		}
		b.WriteString(marker)
		b.WriteString(m.st.header.Render(pad(f.label, labelWidth)))
		b.WriteString(m.fieldText(f, i == s.focus))
		if note := m.fieldNote(f); note != "" {
			b.WriteString("  " + note)
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if s.conflict {
		b.WriteString(m.st.critical.Render(" ✗ " + s.id + " changed on disk since this form was opened: saving would overwrite that change."))
		b.WriteByte('\n')
		b.WriteString(m.st.warning.Render("   ctrl+r reloads the task from disk (your edits here are lost) · esc leaves without saving"))
		b.WriteByte('\n')
	}
	for _, g := range s.general {
		b.WriteString(m.st.critical.Render(" ✗ " + g))
		b.WriteByte('\n')
	}
	switch {
	case m.mode == modeConfirm:
		b.WriteString(m.st.warning.Render(" " + m.confirm.question + " y/n"))
	case s.saving:
		b.WriteString(" saving…")
	default:
		if line := m.messageLine(); line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString(m.st.dim.Render(" tab/↓ next  shift+tab/↑ previous  ←/→ space change  enter on prompt edits it  ctrl+s save  esc cancel"))
	}
	return b.String()
}

// fieldText shows a field's value.
func (m Model) fieldText(f formField, focused bool) string {
	switch f.kind {
	case kindPick:
		if focused {
			return "‹ " + f.value() + " ›"
		}
		return f.value()
	case kindToggle:
		box := "[ ]"
		if f.on {
			box = "[x]"
		}
		return box
	case kindPrompt:
		return promptSummary(m.form.prompt)
	}
	if f.fixed {
		return f.value() + m.st.dim.Render("  (fixed)")
	}
	if focused {
		return f.input.View()
	}
	if f.value() == "" && f.input.Placeholder != "" {
		return m.st.dim.Render(f.input.Placeholder)
	}
	return f.value()
}

// promptSummary is the first line of the prompt and its line count.
func promptSummary(p string) string {
	if strings.TrimSpace(p) == "" {
		return "(empty)  enter edits it in the editor"
	}
	lines := strings.Split(strings.TrimRight(p, "\n"), "\n")
	first := lines[0]
	if r := []rune(first); len(r) > 40 {
		first = string(r[:39]) + "…"
	}
	n := "1 line"
	if len(lines) != 1 {
		n = fmt.Sprintf("%d lines", len(lines))
	}
	return fmt.Sprintf("%q (%s)  enter edits it in the editor", first, n)
}

// fieldNote is what shows after a field: its errors, or for the schedule
// the next fire times.
func (m Model) fieldNote(f formField) string {
	if errs := m.form.errs[f.key]; len(errs) > 0 {
		return m.st.critical.Render("✗ " + strings.Join(errs, "; "))
	}
	if f.key == "schedule" {
		return m.schedulePreview()
	}
	return ""
}

// schedulePreview is the next 3 fire times of the schedule as typed, or
// why it does not parse.
func (m Model) schedulePreview() string {
	expr := strings.TrimSpace(m.form.get("schedule"))
	if expr == "" {
		return ""
	}
	fires, err := nextFires(expr, strings.TrimSpace(m.form.get("timezone")), m.status.Timezone, m.loc, m.now(), 3)
	if err != nil {
		return m.st.critical.Render("✗ " + err.Error())
	}
	if len(fires) == 0 {
		return m.st.dim.Render("never fires again")
	}
	var ts []string
	for _, t := range fires {
		ts = append(ts, t.In(m.loc).Format(fireLayout))
	}
	return m.st.dim.Render("next: " + strings.Join(ts, " · "))
}
