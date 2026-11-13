package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
	"github.com/WillemDeGroef/herdrcron/internal/config"
	"github.com/WillemDeGroef/herdrcron/internal/logging"
	"github.com/WillemDeGroef/herdrcron/internal/notify"
	"github.com/WillemDeGroef/herdrcron/internal/store"
	"github.com/WillemDeGroef/herdrcron/internal/watch"
)

// subscriberBuffer is how many events a subscriber may fall behind before
// it is disconnected. A TUI reads them at once; one that stalls this long
// re-subscribes and re-reads the state.
const subscriberBuffer = 256

// watchDebounce is how long a watched directory must be quiet before a
// hand edit is reloaded. Well under the spec's 2 s.
const watchDebounce = 150 * time.Millisecond

// bus fans events out to the "subscribe" connections. Publishing never
// blocks: a subscriber whose buffer is full is dropped (its channel
// closed, so its connection ends).
type bus struct {
	mu   sync.Mutex
	subs map[chan api.Notification]struct{}
	log  *slog.Logger
}

func newBus(log *slog.Logger) *bus {
	return &bus{subs: map[chan api.Notification]struct{}{}, log: log}
}

// subscribe adds a subscriber until ctx is done.
func (b *bus) subscribe(ctx context.Context) <-chan api.Notification {
	ch := make(chan api.Notification, subscriberBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	context.AfterFunc(ctx, func() { b.drop(ch) })
	return ch
}

// drop removes a subscriber and closes its channel, once.
func (b *bus) drop(ch chan api.Notification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}

// publish sends one event to every subscriber.
func (b *bus) publish(method string, params any) {
	data, err := json.Marshal(params)
	if err != nil {
		b.log.Warn("cannot encode event", "event", method, "err", err)
		return
	}
	n := api.Notification{Method: method, Params: data}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- n:
		default:
			b.log.Warn("subscriber too slow; disconnecting it", "event", method)
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// subscribe serves "subscribe": the result, then every event until the
// connection closes.
func (d *daemon) subscribe(ctx context.Context, _ json.RawMessage) (any, <-chan api.Notification, error) {
	return api.SubscribeResult{Subscribed: true}, d.bus.subscribe(ctx), nil
}

// runChanged is the runner's OnChange: it is the one place a stored run
// change becomes a "run.changed" event and optional notify.command calls
// (blocked, failed, timeout, lost). It also queues the task's display
// state to be republished; that needs the runner's lock, which OnChange
// may not take.
func (d *daemon) runChanged(run store.Run) {
	runLog := logging.ForRun(d.log, run.ID, run.TaskID, run.Target)
	switch run.Status {
	case store.StatusBlocked:
		d.notifier.Notify(config.EventBlocked, notify.RunMessage(run), runLog)
	case store.StatusFailed:
		d.notifier.Notify(config.EventFailed, notify.RunMessage(run), runLog)
	case store.StatusTimeout:
		d.notifier.Notify(config.EventTimeout, notify.RunMessage(run), runLog)
	case store.StatusLost:
		d.notifier.Notify(config.EventLost, notify.RunMessage(run), runLog)
	}
	d.runAmended(run)
}

// runAmended is the runner's OnAmend: an ended run's row changed (its stop
// was finished when the target came back). It publishes "run.changed" and
// queues the task's display state, without a second notification.
func (d *daemon) runAmended(run store.Run) {
	d.bus.publish(api.EventRunChanged, api.RunChangedEvent{Run: *runSummary(run)})
	d.dirtyMu.Lock()
	d.dirty[run.TaskID] = struct{}{}
	d.dirtyMu.Unlock()
	select {
	case d.dirtyWake <- struct{}{}:
	default:
	}
}

// publishDirty republishes "task.changed" for tasks whose runs changed,
// until ctx is done.
func (d *daemon) publishDirty(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.dirtyWake:
		}
		d.dirtyMu.Lock()
		ids := d.dirty
		d.dirty = map[string]struct{}{}
		d.dirtyMu.Unlock()
		d.mu.Lock()
		for id := range ids {
			if i := d.find(id); i >= 0 {
				d.publishTaskLocked(ctx, i)
			}
		}
		d.mu.Unlock()
	}
}

// publishTaskLocked publishes "task.changed" for d.tasks[i]. Call it with
// d.mu held.
func (d *daemon) publishTaskLocked(ctx context.Context, i int) {
	e := d.tasks[i]
	s, err := d.summary(ctx, e)
	if err != nil {
		d.log.Error("cannot describe task for subscribers", logging.KeyTaskID, e.ID, "err", err)
		return
	}
	d.bus.publish(api.EventTaskChanged, api.TaskChangedEvent{ID: e.ID, Task: &s, Version: d.versions[e.ID]})
}

// publishDeleted publishes "task.changed" for a task that is gone.
func (d *daemon) publishDeleted(id string) {
	d.bus.publish(api.EventTaskChanged, api.TaskChangedEvent{ID: id, Deleted: true})
}

// watchFiles reloads hand-edited task and prompt files until ctx is done.
func (d *daemon) watchFiles(ctx context.Context, w *watch.Watcher) {
	go w.Run(ctx)
	for ch := range w.Events() {
		switch ch.Dir {
		case filepath.Clean(d.tasksDir()):
			for _, name := range ch.Names {
				if id, ok := strings.CutSuffix(name, ".yaml"); ok {
					d.reloadOne(ctx, id)
				}
			}
		case filepath.Clean(d.promptsDir()):
			for _, name := range ch.Names {
				d.promptChanged(ctx, filepath.Join(d.promptsDir(), name))
			}
		}
	}
}

// promptChanged reloads every task whose prompt file is path.
func (d *daemon) promptChanged(ctx context.Context, path string) {
	d.mu.Lock()
	var ids []string
	for _, e := range d.tasks {
		if filepath.Clean(d.promptPathOf(e)) == filepath.Clean(path) {
			ids = append(ids, e.ID)
		}
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.reloadOne(ctx, id)
	}
}
