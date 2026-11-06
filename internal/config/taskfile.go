package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

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
	m := doc.Content[0]
	found := false
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			value.LineComment, value.HeadComment, value.FootComment = old.LineComment, old.HeadComment, old.FootComment
			m.Content[i+1] = value
			found = true
			break
		}
	}
	if !found {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeFileAtomic(path, buf.Bytes())
}

// writeFileAtomic replaces the file at path with data: it writes a temp file
// in the same directory, syncs it, gives it the original file's mode and
// renames it over path. The temp file is named .<name>.tmp-*, so LoadTasks
// never reads it. On failure the temp file is removed and path is untouched.
func writeFileAtomic(path string, data []byte) (err error) {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
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
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
