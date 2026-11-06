// Package target is the boundary between the run logic and a machine that
// runs herdr. All herdr calls go through a Target, so local and SSH targets
// share the run logic and tests can use a fake.
package target

import (
	"context"
	"io"
	"slices"
	"sync"
)

// Target is one machine herdrcron starts runs on.
type Target interface {
	Name() string
	// Exec runs argv on the target and returns its stdout.
	Exec(ctx context.Context, argv []string, stdin io.Reader) (stdout []byte, err error)
	// PutFile writes data to remotePath on the target.
	PutFile(ctx context.Context, remotePath string, data []byte) error
}

// Fake is a Target that runs nothing: it records every call and answers
// with the error set by SetErr. Until the herdr adapter exists, the daemon
// fires runs through a Fake. It is safe for concurrent use.
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
