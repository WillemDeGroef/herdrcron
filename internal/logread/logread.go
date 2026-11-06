// Package logread prints the daemon's JSON log in a human-readable form,
// optionally filtered by task and optionally following new lines.
//
// Each JSON record becomes one line:
//
//	<time> <LEVEL> <msg> key=value key=value ...
//
// The time is printed exactly as it appears in the record (slog writes
// RFC 3339 with milliseconds and the local offset). The remaining attributes
// follow in the order they appear in the JSON. String values that are empty
// or contain spaces, quotes, '=' or non-printable characters are quoted with
// strconv.Quote; objects and arrays are printed as compact JSON; numbers,
// booleans and null are printed as-is. Lines that are not JSON objects are
// printed verbatim.
package logread

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/WillemDeGroef/herdrcron/internal/logging"
)

// rotatedFiles is how many rotated files (<log>.1 newest … <log>.4 oldest)
// sit next to the live log, as written by logging.RotatingFile.
const rotatedFiles = logging.Keep - 1

// DefaultPollInterval is used when Options.PollInterval is zero.
const DefaultPollInterval = 250 * time.Millisecond

// Options controls Print.
type Options struct {
	// TaskID, when set, keeps only records whose top-level task_id equals
	// it. Non-JSON lines are dropped when filtering.
	TaskID string
	// Follow keeps tailing the live log after the existing content is
	// printed, until the context is done.
	Follow bool
	// PollInterval is how often the live log is checked when following.
	// Zero means DefaultPollInterval.
	PollInterval time.Duration
}

// Print writes the log at logPath and its rotated files (oldest first) to w.
// Without Follow it returns an error wrapping fs.ErrNotExist if none of the
// files exist. With Follow it then tails the live file, surviving rotation
// and truncation, and returns nil once ctx is done.
func Print(ctx context.Context, logPath string, w io.Writer, opts Options) error {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	emit := func(line []byte) error {
		out, ok := formatLine(line, opts.TaskID)
		if !ok {
			return nil
		}
		_, err := io.WriteString(w, out+"\n")
		return err
	}

	found := false
	for i := rotatedFiles; i >= 1; i-- {
		ok, err := printFile(fmt.Sprintf("%s.%d", logPath, i), emit)
		if err != nil {
			return err
		}
		found = found || ok
	}
	if !opts.Follow {
		ok, err := printFile(logPath, emit)
		if err != nil {
			return err
		}
		if !found && !ok {
			return fmt.Errorf("no log yet at %s: %w", logPath, fs.ErrNotExist)
		}
		return nil
	}
	t := &tailer{path: logPath, emit: emit}
	defer t.close()
	return t.run(ctx, opts.PollInterval)
}

// printFile emits every line of path, including a trailing line without a
// newline. It reports false if the file does not exist.
func printFile(path string, emit func([]byte) error) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if eerr := emit(bytes.TrimSuffix(line, []byte("\n"))); eerr != nil {
				return true, eerr
			}
		}
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return true, fmt.Errorf("read %s: %w", path, err)
		}
	}
}

// tailer follows the live log file, emitting only complete lines.
type tailer struct {
	path    string
	emit    func([]byte) error
	f       *os.File // nil while the file does not exist yet
	offset  int64
	partial []byte
}

func (t *tailer) run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := t.poll(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// poll reads whatever is new, switching to a fresh file after a rotation and
// rereading from the start after a truncation.
func (t *tailer) poll() error {
	if t.f == nil {
		if err := t.open(); err != nil || t.f == nil {
			return err
		}
	}
	if err := t.drain(); err != nil {
		return err
	}
	cur, err := t.f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", t.path, err)
	}
	onDisk, err := os.Stat(t.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Rotated away and the new file is not there yet: keep the old
		// handle, it may still get a last write.
		return nil
	case err != nil:
		return fmt.Errorf("stat %s: %w", t.path, err)
	case !os.SameFile(cur, onDisk):
		// Rotated: drain was just done; any partial line in the old file
		// will never be completed, so print it as-is.
		if len(t.partial) > 0 {
			if err := t.emit(t.partial); err != nil {
				return err
			}
		}
		t.close()
		if err := t.open(); err != nil || t.f == nil {
			return err
		}
		return t.drain()
	case cur.Size() < t.offset:
		// Truncated in place: start over.
		t.partial = nil
		t.offset = 0
		if _, err := t.f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek %s: %w", t.path, err)
		}
		return t.drain()
	}
	return nil
}

func (t *tailer) open() error {
	f, err := os.Open(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	t.f, t.offset, t.partial = f, 0, nil
	return nil
}

func (t *tailer) close() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

// drain reads from the current handle to EOF and emits complete lines,
// keeping a trailing partial line for the next read.
func (t *tailer) drain() error {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.f.Read(buf)
		if n > 0 {
			t.offset += int64(n)
			t.partial = append(t.partial, buf[:n]...)
			for {
				i := bytes.IndexByte(t.partial, '\n')
				if i < 0 {
					break
				}
				line := t.partial[:i]
				if eerr := t.emit(line); eerr != nil {
					return eerr
				}
				t.partial = t.partial[i+1:]
			}
		}
		if errors.Is(err, io.EOF) || (n == 0 && err == nil) {
			// Copy so the retained partial does not pin a large buffer.
			t.partial = append([]byte(nil), t.partial...)
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", t.path, err)
		}
	}
}

type attr struct {
	key string
	val json.RawMessage
}

// formatLine renders one log line. It reports false if the line is dropped
// by the task filter.
func formatLine(line []byte, taskID string) (string, bool) {
	attrs, ok := parseObject(line)
	if !ok {
		if taskID != "" {
			return "", false
		}
		return string(line), true
	}
	if taskID != "" {
		match := false
		for _, a := range attrs {
			if a.key == "task_id" {
				s, ok := jsonString(a.val)
				match = ok && s == taskID
				break
			}
		}
		if !match {
			return "", false
		}
	}

	var head []string
	for _, k := range []string{"time", "level", "msg"} {
		for _, a := range attrs {
			if a.key == k {
				head = append(head, plainValue(a.val))
				break
			}
		}
	}
	parts := head
	for _, a := range attrs {
		switch a.key {
		case "time", "level", "msg":
			continue
		}
		parts = append(parts, a.key+"="+attrValue(a.val))
	}
	return strings.Join(parts, " "), true
}

// parseObject decodes a JSON object into its members in document order.
func parseObject(line []byte) ([]attr, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var attrs []attr
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		attrs = append(attrs, attr{key: key, val: val})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false // trailing data after the object
	}
	return attrs, true
}

// plainValue renders time/level/msg: strings unquoted, anything else as
// attrValue would.
func plainValue(raw json.RawMessage) string {
	if s, ok := jsonString(raw); ok {
		return s
	}
	return attrValue(raw)
}

func attrValue(raw json.RawMessage) string {
	if s, ok := jsonString(raw); ok {
		if needsQuote(s) {
			return strconv.Quote(s)
		}
		return s
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) == nil {
		return b.String()
	}
	return string(raw)
}

// jsonString decodes raw if it is a JSON string (not null, which
// json.Unmarshal would silently accept).
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == '"' || r == '=' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}
