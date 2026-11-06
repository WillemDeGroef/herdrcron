package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/watch"
)

const debounce = 20 * time.Millisecond

// start creates a watcher on dirs and runs it until the test ends.
func start(t *testing.T, dirs ...string) *watch.Watcher {
	t.Helper()
	w, err := watch.New(dirs, debounce, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return w
}

func next(t *testing.T, w *watch.Watcher) watch.Change {
	t.Helper()
	select {
	case c, ok := <-w.Events():
		if !ok {
			t.Fatal("Events closed")
		}
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a Change")
	}
	return watch.Change{}
}

// quiet asserts no Change arrives for a while.
func quiet(t *testing.T, w *watch.Watcher) {
	t.Helper()
	select {
	case c := <-w.Events():
		t.Fatalf("unexpected Change %+v", c)
	case <-time.After(10 * debounce):
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func want(t *testing.T, got watch.Change, dir string, names ...string) {
	t.Helper()
	if got.Dir != dir || !reflect.DeepEqual(got.Names, names) {
		t.Fatalf("got %+v, want {Dir:%s Names:%v}", got, dir, names)
	}
}

func TestWriteReportsName(t *testing.T) {
	dir := t.TempDir()
	w := start(t, dir)
	write(t, filepath.Join(dir, "a.yaml"), "x")
	want(t, next(t, w), dir, "a.yaml")
	quiet(t, w)
}

func TestAtomicRenameReportsOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yaml"), "old")
	w := start(t, dir)
	tmp := filepath.Join(dir, ".a.yaml.tmp-123")
	write(t, tmp, "new")
	if err := os.Rename(tmp, filepath.Join(dir, "a.yaml")); err != nil {
		t.Fatal(err)
	}
	want(t, next(t, w), dir, "a.yaml")
	quiet(t, w)
}

func TestRapidWritesCoalesce(t *testing.T) {
	dir := t.TempDir()
	w := start(t, dir)
	p := filepath.Join(dir, "a.yaml")
	for i := 0; i < 10; i++ {
		write(t, p, string(rune('a'+i)))
	}
	write(t, filepath.Join(dir, "b.md"), "b")
	want(t, next(t, w), dir, "a.yaml", "b.md")
	quiet(t, w)
}

func TestRemoveReportsName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.yaml")
	write(t, p, "x")
	w := start(t, dir)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	want(t, next(t, w), dir, "gone.yaml")
}

func TestRenameReportsOldAndNew(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "old.yaml"), "x")
	w := start(t, dir)
	if err := os.Rename(filepath.Join(dir, "old.yaml"), filepath.Join(dir, "new.yaml")); err != nil {
		t.Fatal(err)
	}
	want(t, next(t, w), dir, "new.yaml", "old.yaml")
}

func TestIgnoredNames(t *testing.T) {
	dir := t.TempDir()
	w := start(t, dir)
	for _, n := range []string{".a.yaml.swp", ".hidden", "a.yaml~", "4913"} {
		write(t, filepath.Join(dir, n), "x")
	}
	quiet(t, w)
}

func TestSeparateDirsSeparateChanges(t *testing.T) {
	root := t.TempDir()
	tasks, prompts := filepath.Join(root, "tasks"), filepath.Join(root, "prompts")
	w := start(t, tasks, prompts)
	write(t, filepath.Join(tasks, "t.yaml"), "x")
	write(t, filepath.Join(prompts, "p.md"), "x")
	got := map[string][]string{}
	for i := 0; i < 2; i++ {
		c := next(t, w)
		got[c.Dir] = c.Names
	}
	if !reflect.DeepEqual(got, map[string][]string{tasks: {"t.yaml"}, prompts: {"p.md"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestMissingDirIsCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config", "tasks")
	w := start(t, dir)
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	write(t, filepath.Join(dir, "a.yaml"), "x")
	want(t, next(t, w), dir, "a.yaml")
}

func TestSlowConsumerKeepsNames(t *testing.T) {
	dir := t.TempDir()
	w := start(t, dir)
	write(t, filepath.Join(dir, "a.yaml"), "x")
	time.Sleep(10 * debounce) // first batch ready but not read
	write(t, filepath.Join(dir, "b.yaml"), "x")
	time.Sleep(10 * debounce)
	seen := map[string]bool{}
	for len(seen) < 2 {
		for _, n := range next(t, w).Names {
			seen[n] = true
		}
	}
	if !seen["a.yaml"] || !seen["b.yaml"] {
		t.Fatalf("seen %v", seen)
	}
}

func TestSteadyStreamIsCapped(t *testing.T) {
	dir := t.TempDir()
	w, err := watch.New([]string{dir}, 200*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("x"), 0o644)
			}
		}
	}()
	select {
	case c := <-w.Events():
		want(t, c, dir, "a.yaml")
	case <-time.After(2 * time.Second):
		t.Fatal("steady stream starved the debounce")
	}
}

func TestRunClosesEventsOnCancel(t *testing.T) {
	dir := t.TempDir()
	w, err := watch.New([]string{dir}, debounce, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	select {
	case _, ok := <-w.Events():
		if ok {
			t.Fatal("Events not closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events not closed")
	}
}
