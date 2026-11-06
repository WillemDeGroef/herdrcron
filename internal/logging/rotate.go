// Package logging holds the daemon's log plumbing: the size-rotated
// daemon.log writer and the helpers that keep every run line tagged.
package logging

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

const (
	// MaxBytes is the size at which daemon.log is rotated.
	MaxBytes = 10 << 20
	// Keep is the number of log files kept, the live one included.
	Keep = 5
)

// RotatingFile is an append-only log file that rotates by size: path is the
// live file, path.1 the newest rotated one, path.(keep-1) the oldest. It is
// safe for concurrent use, and a single Write never spans two files.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// Open opens path for appending with the default MaxBytes and Keep.
func Open(path string) (*RotatingFile, error) {
	return OpenSize(path, MaxBytes, Keep)
}

// OpenSize opens path for appending, rotating once it would exceed maxBytes
// and keeping at most keep files (at least 1). An existing file's size counts
// toward the limit.
func OpenSize(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{path: path, maxBytes: maxBytes, keep: max(keep, 1)}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat log: %w", err)
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Write appends p, rotating first if p would push a non-empty file past the
// size limit. A write larger than the limit goes whole into a fresh file.
//
// If rotation fails, the live file is reopened and p is appended to it anyway
// (so it may grow past the limit); the rotation error is dropped so logging
// keeps working, and rotation is retried on the next write. Write fails only
// if the live file cannot be reopened.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil && r.f == nil {
			if oerr := r.open(); oerr != nil {
				return 0, errors.Join(err, oerr)
			}
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate shifts the files and opens a fresh live file. On error r.f may be
// nil; Write then reopens the live path.
func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("close log: %w", err)
	}
	r.f = nil
	if r.keep == 1 {
		if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate log: %w", err)
		}
		return r.open()
	}
	if err := os.Remove(numbered(r.path, r.keep-1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate log: %w", err)
	}
	for n := r.keep - 2; n >= 1; n-- {
		if err := os.Rename(numbered(r.path, n), numbered(r.path, n+1)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate log: %w", err)
		}
	}
	if err := os.Rename(r.path, numbered(r.path, 1)); err != nil {
		return fmt.Errorf("rotate log: %w", err)
	}
	return r.open()
}

// Close closes the live file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Files returns the existing log files for path under the default Keep,
// oldest first: path.4, …, path.1, path. Read them in this order to get the
// log in chronological order.
func Files(path string) []string {
	return FilesKeep(path, Keep)
}

// FilesKeep is Files for a log rotated with keep files.
func FilesKeep(path string, keep int) []string {
	var out []string
	for n := keep - 1; n >= 0; n-- {
		p := numbered(path, n)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func numbered(path string, n int) string {
	if n == 0 {
		return path
	}
	return fmt.Sprintf("%s.%d", path, n)
}
