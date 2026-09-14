package federation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
)

// LoopbackLocal bridges the federation tunnel into one private, authenticated
// local browser WebSocket. It never reveals the browser token to the master.
// Correlation IDs are replaced while in flight, so concurrent remote browser
// clients cannot consume one another's replies; all uncorrelated envelopes
// remain a live EventSource (including PTY and browser frames).
type LoopbackLocal struct {
	url     string
	dial    *websocket.Dialer
	mu      sync.Mutex
	conn    *websocket.Conn
	writeMu sync.Mutex
	waiters map[string]loopbackWaiter
	next    uint64
	events  chan json.RawMessage
	closed  bool
}

type loopbackWaiter struct {
	conn  *websocket.Conn
	reply chan json.RawMessage
}

func NewLoopbackLocal(httpBaseURL, token string) (*LoopbackLocal, error) {
	u, err := url.Parse(httpBaseURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return nil, errors.New("loopback URL must be http or https")
	}
	u.Path = "/"
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return &LoopbackLocal{url: u.String(), dial: websocket.DefaultDialer, waiters: map[string]loopbackWaiter{}, events: make(chan json.RawMessage, 1024)}, nil
}
func (l *LoopbackLocal) Snapshot(ctx context.Context) (json.RawMessage, error) {
	return l.Execute(ctx, json.RawMessage(`{"t":"list_agents"}`))
}
func (l *LoopbackLocal) Events() <-chan json.RawMessage { return l.events }
func (l *LoopbackLocal) Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, err
	}
	if len(message["t"]) == 0 {
		return nil, errors.New("federation command has no type")
	}
	conn, err := l.connection(ctx)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.next++
	id := "federation-loopback-" + strconv.FormatUint(l.next, 10)
	wait := make(chan json.RawMessage, 1)
	l.waiters[id] = loopbackWaiter{conn: conn, reply: wait}
	l.mu.Unlock()
	defer func() { l.mu.Lock(); delete(l.waiters, id); l.mu.Unlock() }()
	message["corrId"], _ = json.Marshal(id)
	data, err := json.Marshal(message)
	if err != nil {
		slog.Warn("federation loopback command write failed", "command_id", id, "error", err)
		return nil, err
	}
	l.writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, data)
	l.writeMu.Unlock()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	select {
	case reply, ok := <-wait:
		if !ok {
			return nil, errors.New("federation loopback disconnected")
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (l *LoopbackLocal) connection(ctx context.Context) (*websocket.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, errors.New("federation loopback is closed")
	}
	if l.conn != nil {
		conn := l.conn
		l.mu.Unlock()
		return conn, nil
	}
	l.mu.Unlock()
	conn, _, err := l.dial.DialContext(ctx, l.url, nil)
	if err != nil {
		slog.Warn("federation loopback connection failed", "error", err)
		return nil, err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("federation loopback is closed")
	}
	if l.conn != nil {
		existing := l.conn
		l.mu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	l.conn = conn
	l.mu.Unlock()
	slog.Info("federation loopback connected")
	go l.read(conn)
	return conn, nil
}
func (l *LoopbackLocal) read(conn *websocket.Conn) {
	defer func() {
		failed := l.failWaiters(conn)
		_ = conn.Close()
		slog.Info("federation loopback disconnected", "failed_commands", failed)
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var e struct {
			CorrID string `json:"corrId"`
		}
		_ = json.Unmarshal(data, &e)
		l.mu.Lock()
		waiter, found := l.waiters[e.CorrID]
		l.mu.Unlock()
		if e.CorrID != "" && found && waiter.conn == conn {
			select {
			case waiter.reply <- append(json.RawMessage(nil), data...):
			default:
			}
			continue
		}
		select {
		case l.events <- append(json.RawMessage(nil), data...):
		default:
			slog.Warn("dropping federation loopback event because relay buffer is full", "bytes", len(data))
		}
	}
}

// failWaiters retires only calls that used conn. An old reader can exit after
// Reset has installed a replacement connection, so sweeping the whole waiter
// map here would randomly abort healthy commands on the replacement.
func (l *LoopbackLocal) failWaiters(conn *websocket.Conn) int {
	l.mu.Lock()
	if l.conn == conn {
		l.conn = nil
	}
	waiters := make([]chan json.RawMessage, 0, len(l.waiters))
	for id, waiter := range l.waiters {
		if waiter.conn == conn {
			waiters = append(waiters, waiter.reply)
			delete(l.waiters, id)
		}
	}
	l.mu.Unlock()
	for _, waiter := range waiters {
		close(waiter)
	}
	return len(waiters)
}
func (l *LoopbackLocal) Close() error {
	l.mu.Lock()
	l.closed = true
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()
	if conn != nil {
		slog.Info("closing federation loopback connection")
		return conn.Close()
	}
	return nil
}

// Reset drops the private browser-protocol connection without permanently
// closing the bridge. This releases every local subscription when an upstream
// tunnel ends; the next command reconnects and establishes only the master's
// current subscriptions.
func (l *LoopbackLocal) Reset() {
	l.mu.Lock()
	conn := l.conn
	l.conn = nil
	l.mu.Unlock()
	if conn != nil {
		slog.Info("resetting federation loopback connection")
		_ = conn.Close()
	}
}
