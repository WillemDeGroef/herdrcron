package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrReplaced means the task file was renamed into place, but the directory
// could not be synced afterwards. The new contents are already at the path;
// callers must reload the file and not treat the write as rolled back.
var ErrReplaced = errors.New("task file replaced")

// SetEnabled sets `enabled` in the task file at path.
func SetEnabled(path string, enabled bool) error {
	return setKey(path, "enabled", &yaml.Node{
		Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(enabled),
	})
}

// SetPausedUntil sets `paused_until` in the task file at path: "" writes null,
// otherwise "indefinite" or an RFC 3339 time (written as a string). Any other
// value is an error and leaves the file untouched.
func SetPausedUntil(path string, until string) error {
	if err := checkPausedUntil(until); err != nil {
		return err
	}
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: until}
	if until == "" {
		value = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	return setKey(path, "paused_until", value)
}

// LoadTask loads and validates one task file, tasks/<id>.yaml under configDir,
// exactly as LoadTasks does for each file.
func LoadTask(configDir, id string, cfg Config) TaskEntry {
	path := filepath.Join(configDir, "tasks", id+".yaml")
	e := TaskEntry{ID: id, File: path}
	e.Task, e.Err = loadTask(path, id, configDir, cfg)
	return e
}

// setKey sets key to value in the top-level mapping of the YAML file at
// path, adding it at the end if missing, and writes the file atomically.
// Every other key, value and comment is kept.
func setKey(path, key string, value *yaml.Node) error {
	return updateFile(path, []field{{key: key, value: value}})
}

// WriteTask writes t to the task file at path atomically. With create it
// creates the file (and its directory) and fails with an error wrapping
// fs.ErrExist if the file exists. Otherwise it updates the existing file in
// place: comments and key order are kept, missing keys are added at the end
// and empty optional fields (name, cwd, timezone, env, agent.args) are
// removed. ErrReplaced has the same meaning as for SetEnabled.
func WriteTask(path string, t Task, create bool) error {
	if !create {
		return updateFile(path, taskFields(t))
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mergeFields(m, taskFields(t))
	data, err := encodeDoc(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{m}})
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create tasks directory: %w", err)
	}
	return installFile(path, data, 0o644, true)
}

// WritePrompt writes text to the prompt file at path atomically, byte for
// byte. A missing file is created with mode 0644 (and its directory with
// 0755); an existing file keeps its mode.
func WritePrompt(path, text string) error {
	if _, err := os.Lstat(path); err == nil {
		return writeFileAtomic(path, []byte(text))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create prompts directory: %w", err)
	}
	return installFile(path, []byte(text), 0o644, false)
}

// Version is a hex sha256 over a task file and its prompt file, each
// prefixed with its length so bytes cannot move from one to the other
// unnoticed. A missing prompt file counts as empty; a missing task file is
// an error wrapping fs.ErrNotExist.
func Version(taskFile, promptFile string) (string, error) {
	task, err := os.ReadFile(taskFile)
	if err != nil {
		return "", fmt.Errorf("read task file: %w", err)
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read prompt file: %w", err)
	}
	return hashVersion(task, prompt), nil
}

// hashVersion is Version for bytes already read. A missing prompt is an
// empty slice, the same as a prompt file that is not there.
func hashVersion(task, prompt []byte) string {
	h := sha256.New()
	for _, part := range [][]byte{task, prompt} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// field is one key to write into a YAML mapping. A nil value and nil sub
// removes the key. sub, for a nested mapping like agent, is merged key by
// key into an existing mapping, so comments inside it are kept.
type field struct {
	key   string
	value *yaml.Node
	sub   []field
}

// node is the value to write, or nil to remove the key.
func (f field) node() *yaml.Node {
	if f.sub == nil {
		return f.value
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mergeFields(m, f.sub)
	return m
}

// taskFields lists t's keys in the order a new file has them.
func taskFields(t Task) []field {
	str := func(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }
	optional := func(s string) *yaml.Node {
		if s == "" {
			return nil
		}
		return str(s)
	}
	schedule := str(t.Schedule)
	schedule.Style = yaml.DoubleQuotedStyle // as in the spec: "0 7 * * 1"
	pausedUntil := str(t.PausedUntil)
	if t.PausedUntil == "" {
		pausedUntil = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	var args *yaml.Node
	if len(t.Agent.Args) > 0 {
		args = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, a := range t.Agent.Args {
			n := str(a)
			n.Style = yaml.DoubleQuotedStyle
			args.Content = append(args.Content, n)
		}
	}
	var env *yaml.Node
	if len(t.Env) > 0 {
		env = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range slices.Sorted(maps.Keys(t.Env)) {
			env.Content = append(env.Content, str(k), str(t.Env[k]))
		}
	}
	return []field{
		{key: "id", value: str(t.ID)},
		{key: "name", value: optional(t.Name)},
		{key: "target", value: str(t.Target)},
		{key: "cwd", value: optional(t.Cwd)},
		{key: "schedule", value: schedule},
		{key: "timezone", value: optional(t.Timezone)},
		{key: "enabled", value: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t.Enabled)}},
		{key: "paused_until", value: pausedUntil},
		{key: "agent", sub: []field{{key: "command", value: str(t.Agent.Command)}, {key: "args", value: args}}},
		{key: "prompt_file", value: str(t.PromptFile)},
		{key: "timeout", value: str(FormatDuration(t.Timeout.Duration))},
		{key: "overlap", value: str(t.Overlap)},
		{key: "missed", value: str(t.Missed)},
		{key: "on_done", value: str(t.OnDone)},
		{key: "on_blocked", value: str(t.OnBlocked)},
		{key: "env", value: env},
	}
}

// FormatDuration is d.String() without zero trailing units: 2h, 1h30m, 30s.
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// mergeFields writes fields into mapping m. An existing key keeps its place
// and its comments (and the quoting of a string value); a new key is added
// at the end.
func mergeFields(m *yaml.Node, fields []field) {
	for _, f := range fields {
		i := -1
		for k := 0; k+1 < len(m.Content); k += 2 {
			if m.Content[k].Value == f.key {
				i = k
				break
			}
		}
		value := f.node()
		switch {
		case value == nil && i >= 0:
			m.Content = slices.Delete(m.Content, i, i+2)
		case value == nil:
		case i < 0:
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.key}, value)
		case f.sub != nil && m.Content[i+1].Kind == yaml.MappingNode:
			mergeFields(m.Content[i+1], f.sub)
		default:
			old := m.Content[i+1]
			value.LineComment, value.HeadComment, value.FootComment = old.LineComment, old.HeadComment, old.FootComment
			if value.Kind == yaml.ScalarNode && value.Tag == "!!str" && old.Kind == yaml.ScalarNode &&
				old.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) != 0 {
				value.Style = old.Style
			}
			m.Content[i+1] = value
		}
	}
}

// updateFile merges fields into the top-level mapping of the YAML file at
// path and writes it back atomically.
func updateFile(path string, fields []field) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task file: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, parseError(err))
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: not a YAML mapping", path)
	}
	mergeFields(doc.Content[0], fields)
	out, err := encodeDoc(&doc)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeFileAtomic(path, out)
}

func encodeDoc(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic replaces the file at path with data: it writes a temp file
// in the same directory, syncs it, gives it the original file's mode and
// renames it over path. The temp file is named .<name>.tmp-*, so LoadTasks
// never reads it. On failure before the rename, the temp file is removed and
// path is untouched. A symlinked path is followed: the link's target is
// replaced and the link stays. The directory is synced after the rename, so
// the rename survives a crash. If that sync fails the new file is already
// in place: the error is ErrReplaced and nothing is removed.
func writeFileAtomic(path string, data []byte) error {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return installFile(path, data, info.Mode().Perm(), false)
}

// installFile writes data with mode to a synced temp file next to path and
// moves it into place, as writeFileAtomic describes. With create it hard
// links the temp file to path instead of renaming, so an existing path is
// never replaced: the error then wraps fs.ErrExist.
func installFile(path string, data []byte, mode fs.FileMode, create bool) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	replaced := false
	defer func() {
		if err != nil && !replaced {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if create {
		if err = os.Link(tmp.Name(), path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%s: %w", path, fs.ErrExist)
			}
			return err
		}
		_ = os.Remove(tmp.Name())
	} else if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	replaced = true
	if err = syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: sync directory: %w", ErrReplaced, err)
	}
	return nil
}

// syncDirectory fsyncs a directory. Tests replace it to simulate a failure
// after the rename.
var syncDirectory = syncDir

// syncDir fsyncs a directory, making a rename in it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
