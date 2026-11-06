package tui

import (
	"context"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// Backend is the daemon API as the TUI uses it. The TUI talks to the
// daemon only through it.
type Backend interface {
	// Call makes one API call and decodes its result into result.
	Call(ctx context.Context, method string, params, result any) error
	// Subscribe opens the "subscribe" event stream. The channel closes when
	// the stream ends (ctx done or the connection lost).
	Subscribe(ctx context.Context) (<-chan api.Notification, error)
}

// callTimeout bounds one API call.
const callTimeout = 10 * time.Second

// SocketBackend reaches the daemon on its Unix socket. Every call uses a
// connection of its own, so calls may run concurrently and a restarted
// daemon is picked up by the next call.
type SocketBackend struct {
	Path string
}

// Call dials the daemon and makes one call.
func (b SocketBackend) Call(ctx context.Context, method string, params, result any) error {
	c, err := api.Dial(b.Path)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.Call(ctx, method, params, result)
}

// Subscribe dials the daemon and subscribes on that connection; cancelling
// ctx closes it.
func (b SocketBackend) Subscribe(ctx context.Context) (<-chan api.Notification, error) {
	c, err := api.Dial(b.Path)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var res api.SubscribeResult
	// The call deadline bounds only the subscribe call; the stream lives
	// until ctx is done.
	ch, err := c.Subscribe(subscribeCtx{ctx, callCtx}, api.MethodSubscribe, nil, &res)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return ch, nil
}

// subscribeCtx is ctx (the stream's lifetime) with the deadline of call
// (the subscribe call's bound): api.Client.Subscribe applies a deadline
// only to the call and cancellation to the whole stream.
type subscribeCtx struct {
	context.Context
	call context.Context
}

func (s subscribeCtx) Deadline() (time.Time, bool) { return s.call.Deadline() }
