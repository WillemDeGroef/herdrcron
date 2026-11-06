package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WillemDeGroef/herdrcron/internal/api"
)

func startServer(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	srv := api.NewServer(nil)
	srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var p map[string]string
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, api.InvalidParams(err)
		}
		return p, nil
	})
	srv.Handle("boom", func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("it broke")
	})
	ln, err := api.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return sock
}

func TestSocketHasMode0600(t *testing.T) {
	sock := startServer(t)
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
}

func TestClientCallRoundTrip(t *testing.T) {
	c, err := api.Dial(startServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, v := range []string{"one", "two"} { // two calls on one connection
		var got map[string]string
		if err := c.Call(context.Background(), "echo", map[string]string{"v": v}, &got); err != nil {
			t.Fatal(err)
		}
		if got["v"] != v {
			t.Fatalf("got %v", got)
		}
	}
}

func TestClientSeesRPCErrors(t *testing.T) {
	c, err := api.Dial(startServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var rpcErr *api.Error
	err = c.Call(context.Background(), "nope", nil, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeMethodNotFound {
		t.Fatalf("unknown method: err = %v", err)
	}
	err = c.Call(context.Background(), "boom", nil, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInternalError || !strings.Contains(rpcErr.Message, "it broke") {
		t.Fatalf("handler error: err = %v", err)
	}
	err = c.Call(context.Background(), "echo", []int{1}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeInvalidParams {
		t.Fatalf("invalid params: err = %v", err)
	}
}

// rawExchange speaks the wire protocol directly, one line per message.
func rawExchange(t *testing.T, sock string, lines ...string) []map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for _, l := range lines {
		if _, err := conn.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	// A final request with a known id marks the end of the responses.
	if _, err := conn.Write([]byte(`{"jsonrpc":"2.0","id":"end","method":"echo","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("response is not one JSON object per line: %q", sc.Text())
		}
		if m["id"] == "end" {
			return out
		}
		out = append(out, m)
	}
	t.Fatalf("connection ended early: %v", sc.Err())
	return nil
}

func errCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

func TestWireProtocol(t *testing.T) {
	sock := startServer(t)
	resp := rawExchange(t, sock,
		`{"jsonrpc":"2.0","id":1,"method":"echo","params":{"a":"b"}}`,
		`not json`,
		`{"jsonrpc":"1.0","id":2,"method":"echo"}`,
		`{"jsonrpc":"2.0","method":"echo","params":{}}`, // notification: no response
		`{"jsonrpc":"2.0","method":"nope"}`,             // notification to unknown method: no response
		`{"jsonrpc":"2.0","id":{"x":1},"method":"echo"}`,
		`[{"jsonrpc":"2.0","id":3,"method":"echo"}]`, // batches are not supported
	)
	if len(resp) != 5 {
		t.Fatalf("got %d responses, want 5: %v", len(resp), resp)
	}
	if resp[0]["jsonrpc"] != "2.0" || resp[0]["id"] != float64(1) {
		t.Errorf("resp[0] = %v", resp[0])
	}
	if r, _ := resp[0]["result"].(map[string]any); r["a"] != "b" {
		t.Errorf("resp[0] result = %v", resp[0]["result"])
	}
	if errCode(resp[1]) != api.CodeParseError || resp[1]["id"] != nil {
		t.Errorf("parse error resp = %v", resp[1])
	}
	if errCode(resp[2]) != api.CodeInvalidRequest || resp[2]["id"] != float64(2) {
		t.Errorf("invalid request resp = %v", resp[2])
	}
	if errCode(resp[3]) != api.CodeInvalidRequest || resp[3]["id"] != nil {
		t.Errorf("object id resp = %v, want invalid request with null id", resp[3])
	}
	if errCode(resp[4]) != api.CodeInvalidRequest {
		t.Errorf("batch resp = %v", resp[4])
	}
}

func TestDialWithoutDaemon(t *testing.T) {
	_, err := api.Dial(filepath.Join(t.TempDir(), "daemon.sock"))
	if !errors.Is(err, api.ErrDaemonNotRunning) {
		t.Fatalf("err = %v, want ErrDaemonNotRunning", err)
	}
}

func TestListenDoesNotDeleteNonSocketFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.sock")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Listen(path); err == nil {
		t.Fatal("want error: path exists and is not a socket")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep me" {
		t.Fatal("regular file was removed")
	}
}

func TestListenReplacesStaleSocketFromCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // simulate a crash: socket file stays
	_ = ln.Close()
	ln2, err := api.Listen(path)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	_ = ln2.Close()
}

func TestListenTightensExistingStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := api.Listen(filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	info, _ := os.Stat(dir)
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("state dir mode = %o, want 700", got)
	}
}

func TestServeShutsDownWithIdleClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := api.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = api.NewServer(nil).Serve(ctx, ln) }()
	c, err := api.Dial(sock) // idle client that never sends anything
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return with an idle client connected")
	}
}

func TestListenRefusesWhenDaemonRunning(t *testing.T) {
	sock := startServer(t)
	if _, err := api.Listen(sock); !errors.Is(err, api.ErrDaemonRunning) {
		t.Fatalf("err = %v, want ErrDaemonRunning", err)
	}
}

func TestSocketPathTooLongIsClear(t *testing.T) {
	sock := filepath.Join(t.TempDir(), strings.Repeat("d", 120), "daemon.sock")
	for name, err := range map[string]error{
		"Listen": func() error { _, err := api.Listen(sock); return err }(),
		"Dial":   func() error { _, err := api.Dial(sock); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), "too long") || !strings.Contains(err.Error(), "XDG_STATE_HOME") {
			t.Errorf("%s: err = %v, want a clear 'too long' error naming XDG_STATE_HOME", name, err)
		}
	}
}
