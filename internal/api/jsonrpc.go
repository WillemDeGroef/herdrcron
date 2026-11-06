// Package api is the daemon API transport: JSON-RPC 2.0 over a Unix socket,
// one JSON message per line. Batch requests are not supported: a batch gets a
// single "invalid request" error. The daemon registers handlers on a Server; the
// TUI and CLI subcommands use a Client.
//
// A stream method (Server.HandleStream, Client.Subscribe) turns its
// connection into a one-way event stream: after the method's result the
// server pushes JSON-RPC notifications on the connection until either side
// closes it.
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// maxMessage bounds one request or response line. Later methods carry prompt
// text, so leave plenty of room.
const maxMessage = 16 << 20

var (
	// ErrDaemonNotRunning means nothing listens on the socket.
	ErrDaemonNotRunning = errors.New("daemon is not running")
	// ErrDaemonRunning means another daemon already serves the socket.
	ErrDaemonRunning = errors.New("another daemon is already running")
)

// Error is a JSON-RPC error object. Handlers return one to choose the code;
// any other error becomes CodeInternalError.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// InvalidParams wraps err as a CodeInvalidParams error.
func InvalidParams(err error) *Error {
	return &Error{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
}

// HandlerFunc serves one method. params is the raw "params" member (nil
// when absent). The result is marshalled as the response "result".
//
// ctx is the connection's context: it is cancelled when the client closes
// the connection or the server shuts down.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

// Notification is a JSON-RPC notification the server pushes on a
// subscribed connection: {"jsonrpc":"2.0","method":...,"params":...},
// without id. Params is sent as given.
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// StreamFunc serves a stream method. On success the server writes result
// as the response, then forwards every notification from events to the
// connection, in order. ctx is the connection's context, cancelled when the
// client closes the connection or the server shuts down; the handler must
// stop feeding events (and release whatever feeds it) once ctx is done. The
// handler may close events to end the stream: the server then closes the
// connection, which tells the client (for example that it fell too far
// behind). A nil events channel streams nothing until the connection ends.
// On error nothing is streamed and the connection stays a plain one.
type StreamFunc func(ctx context.Context, params json.RawMessage) (result any, events <-chan Notification, err error)

type notificationMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// handler is one registered method: exactly one of plain and stream is set.
type handler struct {
	plain  HandlerFunc
	stream StreamFunc
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

var nullID = json.RawMessage("null")

// Server dispatches JSON-RPC requests to registered handlers.
type Server struct {
	log      *slog.Logger
	mu       sync.Mutex
	handlers map[string]handler
	conns    map[net.Conn]struct{}
}

// NewServer returns a Server that logs to log (nil discards). It never logs
// request params or results: they can hold prompt text and env values.
func NewServer(log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{log: log, handlers: map[string]handler{}, conns: map[net.Conn]struct{}{}}
}

// Handle registers h for method. Register all handlers before Serve.
func (s *Server) Handle(method string, h HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler{plain: h}
}

// HandleStream registers the stream method h for method. Register all
// handlers before Serve. A stream request sent as a notification (no id) is
// ignored without calling h. Once a connection is subscribed, the server
// reads it only to notice when it closes: further requests on it get a
// CodeInvalidRequest error and notifications are ignored.
func (s *Server) HandleStream(method string, h StreamFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler{stream: h}
}

// maxSocketPath is the longest Unix socket path the kernel accepts
// (sun_path is 108 bytes on Linux, 104 on macOS, including the NUL).
const maxSocketPath = 103

func checkSocketPath(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("socket path %s is too long (%d bytes, max %d); set XDG_STATE_HOME to a shorter directory",
			path, len(path), maxSocketPath)
	}
	return nil
}

// Listen creates the Unix socket at path with mode 0600, in a directory
// created with mode 0700 if needed. A stale socket file left by a crashed
// daemon is replaced; a socket another daemon answers on gives
// ErrDaemonRunning.
func Listen(path string) (net.Listener, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	// Tighten an existing directory too, so no other user can reach the
	// socket in the moment between bind and chmod.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod socket directory: %w", err)
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

// removeStaleSocket removes a socket file nobody listens on (left by a
// crashed daemon). It refuses to touch anything that is not a socket, and
// reports ErrDaemonRunning if a daemon answers.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; remove it by hand", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		_ = c.Close()
		return fmt.Errorf("%w on %s", ErrDaemonRunning, path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("check socket %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	return nil
}

// Serve accepts connections on ln until ctx is done, then closes ln and all
// open connections and returns nil.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
	})
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.mu.Lock()
		if ctx.Err() != nil {
			// Shutdown already closed the tracked connections; don't leak this one.
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConn(ctx, conn)
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = conn.Close()
		}()
	}
}

// connWriter serializes all writes to one connection, so responses and
// notifications never interleave mid-line.
type connWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (cw *connWriter) writeLine(data []byte) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	if _, err := cw.w.Write(append(data, '\n')); err != nil {
		return err
	}
	return cw.w.Flush()
}

func (cw *connWriter) writeResponse(resp response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		data, _ = json.Marshal(response{JSONRPC: "2.0", ID: resp.ID,
			Error: &Error{Code: CodeInternalError, Message: "cannot encode result"}})
	}
	return cw.writeLine(data)
}

// serveConn serves one connection until the client closes it or ctx is
// done. Handlers get a context that is cancelled when the read loop ends. It
// returns only after the connection's forwarding goroutine, if any, ended.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	var fwd sync.WaitGroup
	defer fwd.Wait()
	defer cancel()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessage)
	cw := &connWriter{w: bufio.NewWriter(conn)}
	subscribed := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if subscribed {
			if resp, ok := subscribedReply(line); ok {
				if err := cw.writeResponse(resp); err != nil {
					return
				}
			}
			continue
		}
		resp, ok, events := s.handleLine(ctx, line)
		if !ok {
			continue // notification
		}
		if err := cw.writeResponse(resp); err != nil {
			return
		}
		if events != nil {
			subscribed = true
			fwd.Add(1)
			go func() {
				defer fwd.Done()
				s.forward(ctx, cancel, conn, cw, events)
			}()
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		s.log.Warn("api connection read failed", "err", err)
	}
}

// forward writes the notifications from events to the connection until
// events is closed (then it closes the connection) or ctx is done.
func (s *Server) forward(ctx context.Context, cancel context.CancelFunc, conn net.Conn, cw *connWriter, events <-chan Notification) {
	end := func() {
		cancel() // before Close, so the read loop does not log the closed conn
		_ = conn.Close()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-events:
			if !ok {
				end()
				return
			}
			data, err := json.Marshal(notificationMsg{JSONRPC: "2.0", Method: n.Method, Params: n.Params})
			if err != nil {
				s.log.Warn("api notification not encodable", "method", n.Method, "err", err)
				continue
			}
			if err := cw.writeLine(data); err != nil {
				end()
				return
			}
		}
	}
}

// subscribedReply answers a line received on a subscribed connection: an
// error for a request with an id, nothing otherwise.
func subscribedReply(line []byte) (response, bool) {
	var req request
	if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 || !validID(req.ID) {
		return response{}, false
	}
	return response{JSONRPC: "2.0", ID: req.ID, Error: &Error{Code: CodeInvalidRequest,
		Message: "invalid request: connection is subscribed; use another connection for calls"}}, true
}

// handleLine returns the response for one line, and false for a
// notification (a request without id), which gets no response. For a
// successful stream method it also returns the handler's events (nil
// otherwise).
func (s *Server) handleLine(ctx context.Context, line []byte) (response, bool, <-chan Notification) {
	fail := func(id json.RawMessage, code int, msg string) (response, bool, <-chan Notification) {
		if len(id) == 0 {
			id = nullID
		}
		return response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}}, true, nil
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		if line[0] == '{' || line[0] == '[' {
			var v any
			if json.Unmarshal(line, &v) == nil { // valid JSON, wrong shape (batch etc.)
				return fail(nil, CodeInvalidRequest, "invalid request: "+err.Error())
			}
		}
		return fail(nil, CodeParseError, "parse error")
	}
	if !validID(req.ID) {
		return fail(nil, CodeInvalidRequest, "invalid request: id must be a string, number or null")
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return fail(req.ID, CodeInvalidRequest, `invalid request: want "jsonrpc":"2.0" and a method`)
	}
	s.mu.Lock()
	h, found := s.handlers[req.Method]
	s.mu.Unlock()
	notification := len(req.ID) == 0
	if !found {
		if notification {
			return response{}, false, nil
		}
		return fail(req.ID, CodeMethodNotFound, "method not found: "+req.Method)
	}
	if h.stream != nil && notification {
		return response{}, false, nil // nobody would read the stream
	}
	start := time.Now()
	var (
		result any
		events <-chan Notification
		err    error
	)
	if h.stream != nil {
		result, events, err = h.stream(ctx, req.Params)
	} else {
		result, err = h.plain(ctx, req.Params)
	}
	s.log.Debug("api call", "method", req.Method, "duration_ms", time.Since(start).Milliseconds(), "ok", err == nil)
	if notification {
		return response{}, false, nil
	}
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			s.log.Warn("api handler failed", "method", req.Method, "err", err)
			rpcErr = &Error{Code: CodeInternalError, Message: err.Error()}
		}
		return response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}, true, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fail(req.ID, CodeInternalError, "cannot encode result: "+err.Error())
	}
	if h.stream != nil && events == nil {
		events = make(chan Notification) // streams nothing until the connection ends
	}
	return response{JSONRPC: "2.0", ID: req.ID, Result: data}, true, events
}

// validID reports whether id is absent, a string, a number or null.
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	switch id[0] {
	case '"', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

// Client is a connection to the daemon. It is not safe for concurrent use,
// except that Close may be called while a subscription is running.
type Client struct {
	conn      net.Conn
	sc        *bufio.Scanner
	nextID    int64
	closed    chan struct{}
	closeOnce sync.Once
}

// Dial connects to the daemon socket at path. If no daemon listens there the
// error wraps ErrDaemonNotRunning.
func Dial(path string) (*Client, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w (no daemon on %s)", ErrDaemonNotRunning, path)
		}
		return nil, fmt.Errorf("connect to daemon on %s: %w", path, err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64<<10), maxMessage)
	return &Client{conn: conn, sc: sc, closed: make(chan struct{})}, nil
}

// Close closes the connection. It ends a running subscription.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.conn.Close()
}

// Call sends one request and decodes the result into result (which may be
// nil). A JSON-RPC error response is returned as *Error. The context
// deadline, if any, bounds the whole exchange.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	deadline, _ := ctx.Deadline() // zero means no deadline
	if err := c.conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	return c.exchange(ctx, method, params, result)
}

// Subscribe calls the stream method like Call, decoding its result into
// result, and then delivers the notifications the server pushes on the
// returned channel, in order. The context deadline, if any, bounds only the
// call; cancelling ctx ends the subscription and closes the connection. The
// channel is closed when the subscription ends: ctx done, Close, or the
// server closed the connection (for example because the client fell too far
// behind; subscribe again on a new connection). A JSON-RPC error response is
// returned as *Error, and the client stays usable for Call. After a
// successful Subscribe the client must not be used for Call; use another
// connection.
func (c *Client) Subscribe(ctx context.Context, method string, params, result any) (<-chan Notification, error) {
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	if err := c.exchange(ctx, method, params, result); err != nil {
		stop()
		return nil, err
	}
	if err := c.conn.SetDeadline(time.Time{}); err != nil {
		stop()
		return nil, c.wrap(ctx, err)
	}
	out := make(chan Notification, 64)
	go func() {
		defer close(out)
		defer stop()
		for c.sc.Scan() {
			var msg notificationMsg
			if json.Unmarshal(c.sc.Bytes(), &msg) != nil || msg.Method == "" {
				continue // not a notification (an error answer to a stray request)
			}
			select {
			case out <- Notification{Method: msg.Method, Params: msg.Params}:
			case <-ctx.Done():
				return
			case <-c.closed:
				return
			}
		}
	}()
	return out, nil
}

// exchange sends one request and reads its response; the caller sets the
// connection deadline.
func (c *Client) exchange(ctx context.Context, method string, params, result any) error {
	c.nextID++
	id := json.RawMessage(fmt.Sprint(c.nextID))
	req := request{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode params: %w", err)
		}
		req.Params = p
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return c.wrap(ctx, err)
	}
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return c.wrap(ctx, err)
		}
		return errors.New("daemon closed the connection")
	}
	var resp response
	if err := json.Unmarshal(c.sc.Bytes(), &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !bytes.Equal(resp.ID, id) {
		return fmt.Errorf("response id %s does not match request id %s", resp.ID, id)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func (c *Client) wrap(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
