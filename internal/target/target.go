// Package target is the boundary between the run logic and a machine that
// runs herdr. All herdr calls go through a Target, so local and SSH targets
// share the run logic and tests can use a fake.
package target

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// Target is one machine herdrcron starts runs on.
type Target interface {
	Name() string
	// Exec runs argv on the target and returns its stdout.
	Exec(ctx context.Context, argv []string, stdin io.Reader) (stdout []byte, err error)
	// PutFile writes data to remotePath on the target.
	PutFile(ctx context.Context, remotePath string, data []byte) error
	// RemoveFile removes the file; a missing file is not an error.
	RemoveFile(ctx context.Context, path string) error
}

// Spawner starts long-lived processes on a target.
type Spawner interface {
	// Spawn starts argv detached in a new session, with stdio to /dev/null,
	// and does not wait for it to exit. Used to start `herdr server`.
	Spawn(argv []string) error
}

// ExitError is returned by Exec when the command ran and exited non-zero.
type ExitError struct {
	Argv   []string
	Code   int
	Stderr []byte
}

// maxStderrInError caps how much stderr ExitError.Error includes.
const maxStderrInError = 200

// Error names at most the first three argv elements (later ones can hold
// env values or prompt text) and the first line of stderr.
func (e *ExitError) Error() string {
	name := strings.Join(e.Argv[:min(len(e.Argv), 3)], " ")
	return fmt.Sprintf("%s: exit %d: %s", name, e.Code, firstLine(e.Stderr))
}

// Fake is a Target that runs nothing: it records every call and answers
// with the error set by SetErr. Tests use it where no real herdr is needed.
// It is safe for concurrent use.
type Fake struct {
	name  string
	mu    sync.Mutex
	calls [][]string
	files map[string][]byte
	err   error
}

// NewFake returns a fake target called name.
func NewFake(name string) *Fake { return &Fake{name: name, files: map[string][]byte{}} }

// Name returns the target name.
func (f *Fake) Name() string { return f.name }

// Exec records argv and returns the error set by SetErr.
func (f *Fake) Exec(_ context.Context, argv []string, _ io.Reader) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(argv))
	return nil, f.err
}

// PutFile records the file and returns the error set by SetErr.
func (f *Fake) PutFile(_ context.Context, remotePath string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.files[remotePath] = slices.Clone(data)
	}
	return f.err
}

// RemoveFile forgets the file and returns the error set by SetErr.
func (f *Fake) RemoveFile(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		delete(f.files, path)
	}
	return f.err
}

// SetErr makes every later call fail with err (nil: succeed).
func (f *Fake) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Calls returns the argv of every Exec call so far.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = slices.Clone(c)
	}
	return out
}

// Files returns the files written with PutFile so far.
func (f *Fake) Files() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.files))
	for k, v := range f.files {
		out[k] = slices.Clone(v)
	}
	return out
}
