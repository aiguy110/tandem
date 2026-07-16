package wsserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
	"github.com/gorilla/websocket"
)

type testBackend struct {
	mu       sync.RWMutex
	sessions map[string]*session.Session
}

func (b *testBackend) Get(id string) *session.Session {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sessions[id]
}
func (*testBackend) Summaries(context.Context) []registry.Summary {
	return []registry.Summary{{ID: "a", Name: "a", ControlMode: "transcript"}}
}
func (*testBackend) ListDirs(context.Context) ([]workspace.RepoInfo, error) {
	return []workspace.RepoInfo{{Path: "/repo", Name: "repo"}}, nil
}
func (*testBackend) AgentCatalog() registry.Catalog {
	return registry.Catalog{DefaultAgent: "codex", Agents: []registry.CatalogAgent{}}
}

type testAdapter struct {
	events chan eventlog.Event
	done   chan struct{}
}

func (a *testAdapter) Capabilities() agentadapter.Capabilities { return agentadapter.Capabilities{} }
func (a *testAdapter) Events() <-chan eventlog.Event           { return a.events }
func (a *testAdapter) Done() <-chan struct{}                   { return a.done }
func (*testAdapter) Prompt(context.Context, []agentadapter.PromptBlock) (string, error) {
	return "", nil
}
func (*testAdapter) SendInput([]byte) error                 { return nil }
func (*testAdapter) Resize(uint16, uint16) error            { return nil }
func (*testAdapter) RespondPermission(string, string) error { return nil }
func (*testAdapter) Interrupt() error                       { return nil }
func (*testAdapter) Close(context.Context) error            { return nil }
func (*testAdapter) SessionID() string                      { return "" }
func (*testAdapter) PID() int                               { return 0 }

func setupWS(t *testing.T, queue int) (*store.Store, *testBackend, *testAdapter, *httptest.Server, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := eventlog.New("a", db, 2)
	if err != nil {
		t.Fatal(err)
	}
	a := &testAdapter{make(chan eventlog.Event, 10000), make(chan struct{})}
	s, err := session.New("a", "a", agentadapter.Spec{}, a, log)
	if err != nil {
		t.Fatal(err)
	}
	b := &testBackend{sessions: map[string]*session.Session{"a": s}}
	server := httptest.NewServer(New(Options{Token: "secret", Registry: b, WriteQueue: queue}))
	t.Cleanup(func() { server.Close(); db.Close() })
	return db, b, a, server, "ws" + strings.TrimPrefix(server.URL, "http")
}
func event(text string) eventlog.Event {
	b, _ := json.Marshal(map[string]any{"kind": "message_chunk", "text": text})
	return eventlog.Event{Kind: "message_chunk", Payload: b}
}
func status() eventlog.Event {
	b, _ := json.Marshal(map[string]any{"kind": "status", "status": "working"})
	return eventlog.Event{Kind: "status", Payload: b}
}
func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url+"/?token=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	if err := c.WriteJSON(v); err != nil {
		t.Fatal(err)
	}
}
func recv(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var v map[string]any
	if err := c.ReadJSON(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func waitHead(t *testing.T, s *session.Session, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.Log.Head() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Log.Head() != n {
		t.Fatalf("head=%d want %d", s.Log.Head(), n)
	}
}

func TestAuthReadOperationsAndCorrelation(t *testing.T) {
	_, _, _, server, url := setupWS(t, 0)
	bad, _, err := websocket.DefaultDialer.Dial(url+"/?token=bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = bad.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok || ce.Code != 4401 {
		t.Fatalf("close=%v", err)
	}
	c := dial(t, url)
	for _, req := range []map[string]any{{"t": "list_agents", "corrId": "1"}, {"t": "list_dirs", "corrId": "2"}, {"t": "list_agent_catalog", "corrId": "3"}} {
		send(t, c, req)
		got := recv(t, c)
		if got["corrId"] != req["corrId"] {
			t.Fatalf("corr %#v", got)
		}
	}
	server.CloseClientConnections()
}

func TestSnapshotReplayChannelsMultipleClientsAndUnsubscribe(t *testing.T) {
	_, b, a, _, url := setupWS(t, 0)
	a.events <- event("one")
	a.events <- status()
	waitHead(t, b.Get("a"), 2)
	one, two := dial(t, url), dial(t, url)
	send(t, one, map[string]any{"t": "subscribe", "agentId": "a", "channels": []string{"transcript"}, "corrId": "s"})
	snap := recv(t, one)
	if snap["t"] != "snapshot" || snap["seq"] != float64(2) || len(snap["transcript"].([]any)) != 1 {
		t.Fatalf("snapshot %#v", snap)
	}
	if recv(t, one)["t"] != "ack" {
		t.Fatal("missing ack")
	}
	send(t, two, map[string]any{"t": "subscribe", "agentId": "a", "sinceSeq": 1})
	replayed := recv(t, two)
	if replayed["t"] != "event" || replayed["seq"] != float64(2) {
		t.Fatalf("replay %#v", replayed)
	}
	recv(t, two)
	a.events <- event("live")
	waitHead(t, b.Get("a"), 3)
	if recv(t, one)["seq"] != float64(3) || recv(t, two)["seq"] != float64(3) {
		t.Fatal("live fanout")
	}
	send(t, one, map[string]any{"t": "unsubscribe", "agentId": "a", "channels": []string{"transcript"}})
	recv(t, one)
	a.events <- event("gone")
	waitHead(t, b.Get("a"), 4)
	one.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if err := one.ReadJSON(&map[string]any{}); err == nil {
		t.Fatal("event after unsubscribe")
	}
}

func TestColdReplayAfterRestartAndSlowClientDoesNotBlockIngestion(t *testing.T) {
	db, b, a, _, url := setupWS(t, 4)
	for i := 0; i < 5; i++ {
		a.events <- event("old")
	}
	waitHead(t, b.Get("a"), 5)
	log, err := eventlog.New("a", db, 2)
	if err != nil {
		t.Fatal(err)
	}
	next := &testAdapter{make(chan eventlog.Event, 10000), make(chan struct{})}
	restored, err := session.New("a", "a", agentadapter.Spec{}, next, log)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.sessions["a"] = restored
	b.mu.Unlock()
	c := dial(t, url)
	send(t, c, map[string]any{"t": "subscribe", "agentId": "a", "sinceSeq": 1})
	for seq := 2; seq <= 5; seq++ {
		got := recv(t, c)
		if got["seq"] != float64(seq) {
			t.Fatalf("cold order %#v", got)
		}
	}
	recv(t, c)
	slow := dial(t, url)
	send(t, slow, map[string]any{"t": "subscribe", "agentId": "a", "sinceSeq": 5})
	time.Sleep(20 * time.Millisecond)
	payload := strings.Repeat("x", 128*1024)
	start := time.Now()
	for i := 0; i < 100; i++ {
		next.events <- event(payload)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow websocket blocked event producer")
	}
	waitHead(t, restored, 105)
}
