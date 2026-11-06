// Package watch reports changed files in a set of directories, debounced, so
// hand edits of task and prompt files reach the daemon promptly while the
// bursts of events editors and atomic writes produce collapse into one batch.
package watch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// maxDelay caps how long a steady stream of events can postpone a batch.
const maxDelay = time.Second

// Change is one debounced batch of changed files in a watched directory.
type Change struct {
	Dir   string   // the watched directory, as passed to New (cleaned)
	Names []string // base names, sorted and unique
}

// Watcher reports changed files in a set of directories, debounced.
type Watcher struct {
	fs       *fsnotify.Watcher
	dirs     map[string]bool // cleaned watched dirs
	debounce time.Duration
	maxWait  time.Duration
	log      *slog.Logger
	events   chan Change
}

// batch is the pending set of names for one directory.
type batch struct {
	names map[string]bool
	first time.Time // first event of the batch
	last  time.Time // most recent event
}

func (b *batch) deadline(debounce, maxWait time.Duration) time.Time {
	d := b.last.Add(debounce)
	if c := b.first.Add(maxWait); c.Before(d) {
		return c
	}
	return d
}

// New watches dirs (each is created with 0755 if missing, so a fresh config
// dir works). Changes are reported on Events as batches of base file names
// per directory, after the directory has been quiet for debounce, and at the
// latest one second (or debounce, if longer) after the batch's first event.
// A nil log discards fsnotify errors.
func New(dirs []string, debounce time.Duration, log *slog.Logger) (*Watcher, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	w := &Watcher{
		fs:       fw,
		dirs:     map[string]bool{},
		debounce: debounce,
		maxWait:  max(maxDelay, debounce),
		log:      log,
		events:   make(chan Change),
	}
	for _, d := range dirs {
		d = filepath.Clean(d)
		if err := os.MkdirAll(d, 0o755); err != nil {
			_ = fw.Close()
			return nil, fmt.Errorf("watch: create %s: %w", d, err)
		}
		if err := fw.Add(d); err != nil {
			_ = fw.Close()
			return nil, fmt.Errorf("watch: %s: %w", d, err)
		}
		w.dirs[d] = true
	}
	return w, nil
}

// Events delivers debounced batches. It is closed when Run returns.
func (w *Watcher) Events() <-chan Change { return w.events }

// Run processes file system events until ctx is done, then closes the
// fsnotify watcher and the Events channel. A slow consumer never blocks event
// intake: new names merge into the pending batch while a send is waiting.
func (w *Watcher) Run(ctx context.Context) {
	defer close(w.events)
	defer func() { _ = w.fs.Close() }()

	pending := map[string]*batch{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		// Pick the due batch with the earliest deadline to offer, and arm the
		// timer for the earliest deadline still in the future.
		now := time.Now()
		var out chan Change
		var offer Change
		var dueAt, wakeAt time.Time
		for dir, b := range pending {
			dl := b.deadline(w.debounce, w.maxWait)
			if !dl.After(now) {
				if out == nil || dl.Before(dueAt) {
					out, dueAt = w.events, dl
					offer = Change{Dir: dir, Names: sortedNames(b.names)}
				}
			} else if wakeAt.IsZero() || dl.Before(wakeAt) {
				wakeAt = dl
			}
		}
		timer.Stop()
		if !wakeAt.IsZero() {
			timer.Reset(wakeAt.Sub(now))
		}

		select {
		case <-ctx.Done():
			return
		case out <- offer:
			delete(pending, offer.Dir)
		case <-timer.C:
		case ev, ok := <-w.fs.Events:
			if !ok {
				<-ctx.Done()
				return
			}
			w.add(pending, ev)
		case err, ok := <-w.fs.Errors:
			if !ok {
				<-ctx.Done()
				return
			}
			w.log.Warn("file watch error", "err", err)
		}
	}
}

func (w *Watcher) add(pending map[string]*batch, ev fsnotify.Event) {
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) &&
		!ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
		return // chmod only
	}
	dir, name := filepath.Split(ev.Name)
	dir = filepath.Clean(dir)
	if !w.dirs[dir] || ignored(name) {
		return
	}
	now := time.Now()
	b := pending[dir]
	if b == nil {
		b = &batch{names: map[string]bool{}, first: now}
		pending[dir] = b
	}
	b.names[name] = true
	b.last = now
}

// ignored reports names that are never task or prompt files: dot files (the
// daemon's own temp files, vim swap files), backups ending in "~", and names
// without an extension (vim's "4913" probe file).
func ignored(name string) bool {
	return name == "" || strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, "~") || filepath.Ext(name) == ""
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
