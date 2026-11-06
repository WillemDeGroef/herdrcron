package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

// streamServer serves "echo" and the stream method "sub" handled by h.
// stop shuts the server down and waits for Serve to return.
func streamServer(t *testing.T, h api.StreamFunc) (sock string, stop func()) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "d.sock")
	srv := api.NewServer(nil)
	srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var p any
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, api.InvalidParams(err)
		}
		return p, nil
	})
	srv.HandleStream("sub", h)
	ln, err := api.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Serve did not return")
			}
		})
	}
	t.Cleanup(stop)
	return sock, stop
}

func dial(t *testing.T, sock string) *api.Client {
	t.Helper()
	c, err := api.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func note(i int) api.Notification {
	return api.Notification{Method: "task.changed", Params: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))}
}

func noteN(t *testing.T, n api.Notification) int {
	t.Helper()
	var p struct{ N int }
	if err := json.Unmarshal(n.Params, &p); err != nil {
		t.Fatalf("params %q: %v", n.Params, err)
	}
	return p.N
}

// waitClosed fails unless ch is closed (after draining) within 5 seconds.
func waitClosed(t *testing.T, ch <-chan api.Notification) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("notification channel was not closed")
		}
	}
}

func TestSubscribeResultThenNotificationsInOrder(t *testing.T) {
	const n = 100
	sock, _ := streamServer(t, func(context.Context, json.RawMessage) (any, <-chan api.Notification, error) {
		ch := make(chan api.Notification, n)
		for i := range n {
			ch <- note(i)
		}
		return api.SubscribeResult{Subscribed: true}, ch, nil
	})
	c := dial(t, sock)
	var res api.SubscribeResult
	ch, err := c.Subscribe(context.Background(), "sub", nil, &res)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Subscribed {
		t.Fatalf("result = %+v", res)
	}
	for i := range n {
		select {
		case got, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d notifications", i)
			}
			if got.Method != "task.changed" || noteN(t, got) != i {
				t.Fatalf("notification %d = %s %s", i, got.Method, got.Params)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for notification %d", i)
		}
	}
}

func TestSubscribeWireShape(t *testing.T) {
	sock, _ := streamServer(t, func(context.Context, json.RawMessage) (any, <-chan api.Notification, error) {
		ch := make(chan api.Notification, 1)
		ch <- api.Notification{Method: "run.changed", Params: json.RawMessage(`{"run":{"id":"r1"}}`)}
		return api.SubscribeResult{Subscribed: true}, ch, nil
	})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"sub"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(conn)
	var lines []string
	for len(lines) < 2 && sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("got lines %q, err %v", lines, sc.Err())
	}
	if lines[0] != `{"jsonrpc":"2.0","id":7,"result":{"subscribed":true}}` {
		t.Errorf("result line = %s", lines[0])
	}
	if lines[1] != `{"jsonrpc":"2.0","method":"run.changed","params":{"run":{"id":"r1"}}}` {
		t.Errorf("notification line = %s", lines[1])
	}
}

// Requests on a subscribed connection are answered with an error, written
// under the same lock as the notifications: no line is ever torn.
func TestSubscribedConnectionNeverInterleavesLines(t *testing.T) {
	const producers, perProducer, requests = 4, 50, 50
	big := strings.Repeat("x", 64<<10)
	sock, _ := streamServer(t, func(ctx context.Context, _ json.RawMessage) (any, <-chan api.Notification, error) {
		ch := make(chan api.Notification)
		var wg sync.WaitGroup
		for p := range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perProducer {
					params, _ := json.Marshal(map[string]any{"p": p, "i": i, "pad": big})
					select {
					case ch <- api.Notification{Method: "task.changed", Params: params}:
					case <-ctx.Done():
						return
					}
				}
			}()
		}
		go func() { wg.Wait(); close(ch) }()
		return api.SubscribeResult{Subscribed: true}, ch, nil
	})

	// Plain calls on another connection run concurrently.
	callsDone := make(chan error, 1)
	go func() {
		c, err := api.Dial(sock)
		if err != nil {
			callsDone <- err
			return
		}
		defer func() { _ = c.Close() }()
		for i := range requests {
			var got map[string]any
			if err := c.Call(context.Background(), "echo", map[string]any{"i": i, "pad": big}, &got); err != nil {
				callsDone <- err
				return
			}
		}
		callsDone <- nil
	}()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":0,"method":"sub"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := 1; i <= requests; i++ {
			_, _ = fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%d,"method":"echo","params":{"pad":%q}}`+"\n", i, big)
		}
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(nil, 1<<20)
	var notes, errs int
	last := map[float64]float64{}
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("torn line (%d bytes): %v", len(sc.Bytes()), err)
		}
		switch {
		case m["method"] == "task.changed":
			p := m["params"].(map[string]any)
			prod, i := p["p"].(float64), p["i"].(float64)
			if prev, ok := last[prod]; ok && i != prev+1 {
				t.Fatalf("producer %v: notification %v after %v", prod, i, prev)
			}
			last[prod] = i
			notes++
		case m["id"] == float64(0):
			if m["result"] == nil {
				t.Fatalf("subscribe response = %v", m)
			}
		default:
			if errCode(m) != api.CodeInvalidRequest {
				t.Fatalf("request on subscribed connection: %v", m)
			}
			errs++
		}
	}
	// The server closes the connection once the events channel is closed.
	if notes != producers*perProducer {
		t.Fatalf("got %d notifications, want %d (err %v)", notes, producers*perProducer, sc.Err())
	}
	if err := <-callsDone; err != nil {
		t.Fatal(err)
	}
	t.Logf("%d error responses interleaved with notifications", errs)
}

func TestClosingClientCancelsStreamContext(t *testing.T) {
	ctxDone := make(chan struct{})
	sock, _ := streamServer(t, func(ctx context.Context, _ json.RawMessage) (any, <-chan api.Notification, error) {
		go func() { <-ctx.Done(); close(ctxDone) }()
		return api.SubscribeResult{Subscribed: true}, make(chan api.Notification), nil
	})
	c := dial(t, sock)
	ch, err := c.Subscribe(context.Background(), "sub", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler ctx not cancelled after the client closed")
	}
	waitClosed(t, ch)
}

func TestSubscribeContextCancelEndsStream(t *testing.T) {
	ctxDone := make(chan struct{})
	sock, _ := streamServer(t, func(ctx context.Context, _ json.RawMessage) (any, <-chan api.Notification, error) {
		go func() { <-ctx.Done(); close(ctxDone) }()
		return api.SubscribeResult{Subscribed: true}, make(chan api.Notification), nil
	})
	c := dial(t, sock)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Subscribe(ctx, "sub", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitClosed(t, ch)
	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler ctx not cancelled after the subscribe ctx was cancelled")
	}
}

func TestServerClosingEventsClosesClientChannel(t *testing.T) {
	sock, _ := streamServer(t, func(context.Context, json.RawMessage) (any, <-chan api.Notification, error) {
		ch := make(chan api.Notification, 2)
		ch <- note(1)
		ch <- note(2)
		close(ch) // subscriber fell behind
		return api.SubscribeResult{Subscribed: true}, ch, nil
	})
	c := dial(t, sock)
	ch, err := c.Subscribe(context.Background(), "sub", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	timeout := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case n, ok := <-ch:
			if !ok {
				done = true
				break
			}
			got = append(got, noteN(t, n))
		case <-timeout:
			t.Fatal("client channel not closed after the server closed events")
		}
	}
	if fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("got %v, want the queued notifications before the close", got)
	}
}

func TestServerShutdownEndsSubscription(t *testing.T) {
	ctxDone := make(chan struct{})
	sock, stop := streamServer(t, func(ctx context.Context, _ json.RawMessage) (any, <-chan api.Notification, error) {
		go func() { <-ctx.Done(); close(ctxDone) }()
		return api.SubscribeResult{Subscribed: true}, make(chan api.Notification), nil // never closed by the handler
	})
	c := dial(t, sock)
	ch, err := c.Subscribe(context.Background(), "sub", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop() // waits for Serve, including the forwarding goroutine
	waitClosed(t, ch)
	select {
	case <-ctxDone:
	default:
		t.Fatal("handler ctx not cancelled by shutdown")
	}
}

func TestSubscribeHandlerErrorStreamsNothing(t *testing.T) {
	sock, _ := streamServer(t, func(context.Context, json.RawMessage) (any, <-chan api.Notification, error) {
		return nil, nil, &api.Error{Code: api.CodeInvalidParams, Message: "bad"}
	})
	c := dial(t, sock)
	ch, err := c.Subscribe(context.Background(), "sub", nil, nil)
	var rpcErr *api.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInvalidParams {
		t.Fatalf("err = %v, want *Error with CodeInvalidParams", err)
	}
	if ch != nil {
		t.Fatal("got a channel with an error")
	}
	// The connection is still a plain one.
	var got map[string]any
	if err := c.Call(context.Background(), "echo", map[string]any{"a": "b"}, &got); err != nil || got["a"] != "b" {
		t.Fatalf("Call after failed subscribe: %v %v", got, err)
	}
}

func TestStreamMethodAsNotificationIsIgnored(t *testing.T) {
	called := make(chan struct{}, 1)
	sock, _ := streamServer(t, func(context.Context, json.RawMessage) (any, <-chan api.Notification, error) {
		called <- struct{}{}
		return api.SubscribeResult{}, make(chan api.Notification), nil
	})
	resp := rawExchange(t, sock, `{"jsonrpc":"2.0","method":"sub"}`)
	if len(resp) != 0 {
		t.Fatalf("got responses %v", resp)
	}
	select {
	case <-called:
		t.Fatal("stream handler called for a notification")
	default:
	}
}

func TestPlainHandlerGetsConnectionContext(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	srv := api.NewServer(nil)
	ctxDone := make(chan struct{})
	started := make(chan struct{})
	srv.Handle("wait", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(ctxDone)
		return nil, ctx.Err()
	})
	ln, err := api.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	c := dial(t, sock)
	go func() { _ = c.Call(context.Background(), "wait", nil, nil) }()
	<-started
	cancel()
	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("plain handler ctx not cancelled by shutdown")
	}
	<-done
}
