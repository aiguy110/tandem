package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// fakeAdapter is an ACP-like adapter whose turns are held open until the test
// releases them, so a test controls exactly when a turn ends.
type fakeAdapter struct {
	events chan eventlog.Event
	done   chan struct{}
	gate   chan struct{}
	close  sync.Once

	mu      sync.Mutex
	prompts []string
	steers  []string
	// autoEnd makes every turn end immediately instead of waiting for release.
	autoEnd bool
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{events: make(chan eventlog.Event, 64), done: make(chan struct{}), gate: make(chan struct{}, 64)}
}

func (f *fakeAdapter) Capabilities() agentadapter.Capabilities {
	return agentadapter.Capabilities{Structured: true, Steering: true}
}
func (f *fakeAdapter) Events() <-chan eventlog.Event { return f.events }
func (f *fakeAdapter) Done() <-chan struct{}         { return f.done }
func (f *fakeAdapter) Prompt(ctx context.Context, blocks []agentadapter.PromptBlock) (string, error) {
	f.mu.Lock()
	for _, b := range blocks {
		f.prompts = append(f.prompts, b.Text)
	}
	auto := f.autoEnd
	f.mu.Unlock()
	if auto {
		return "end_turn", nil
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-f.gate:
	}
	return "end_turn", nil
}
func (f *fakeAdapter) Steer(_ context.Context, blocks []agentadapter.PromptBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range blocks {
		f.steers = append(f.steers, b.Text)
	}
	return nil
}
func (f *fakeAdapter) SendInput([]byte) error                 { return nil }
func (f *fakeAdapter) Resize(uint16, uint16) error            { return nil }
func (f *fakeAdapter) RespondPermission(string, string) error { return nil }
func (f *fakeAdapter) Interrupt() error                       { return nil }
func (f *fakeAdapter) Close(context.Context) error {
	f.close.Do(func() { close(f.events); close(f.done) })
	return nil
}
func (f *fakeAdapter) ExternalSessionID() string { return "ext" }
func (f *fakeAdapter) PID() int                  { return 1 }

// release ends the turn currently held open.
func (f *fakeAdapter) release() { f.gate <- struct{}{} }

func (f *fakeAdapter) promptTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}
func (f *fakeAdapter) steerTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steers...)
}

// fakeClock is a manually advanced clock shared by a test's services.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// testNet connects test hosts the way federation would: a call to a host
// executes the browser-protocol command on that host's messaging service, with
// the caller as the federation origin and the access level the test set.
type testNet struct {
	mu      sync.Mutex
	hosts   map[string]*testHost
	offline map[string]bool
	// level overrides the access level the caller has on the callee, keyed
	// "caller>callee"; the default is message.
	level map[string]federation.Level
	calls []string
}

func newNet() *testNet {
	return &testNet{hosts: map[string]*testHost{}, offline: map[string]bool{}, level: map[string]federation.Level{}}
}

func (n *testNet) setOffline(id string, off bool) { n.mu.Lock(); n.offline[id] = off; n.mu.Unlock() }
func (n *testNet) setLevel(from, to string, l federation.Level) {
	n.mu.Lock()
	n.level[from+">"+to] = l
	n.mu.Unlock()
}
func (n *testNet) callLog() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

// testFed is one host's view of the net; it implements Federation.
type testFed struct {
	net  *testNet
	self string
}

func (f *testFed) SelfID() string { return f.self }
func (f *testFed) Hosts() []federation.Host {
	f.net.mu.Lock()
	defer f.net.mu.Unlock()
	var out []federation.Host
	for id := range f.net.hosts {
		if id == f.self {
			continue
		}
		status := "connected"
		if f.net.offline[id] {
			status = "offline"
		}
		level, ok := f.net.level[f.self+">"+id]
		if !ok {
			level = federation.LevelMessage
		}
		out = append(out, federation.Host{ID: id, NodeID: id, Name: strings.ToUpper(id), Status: status, Access: level.String()})
	}
	return out
}

func (f *testFed) Call(ctx context.Context, hostID string, payload json.RawMessage) (json.RawMessage, error) {
	f.net.mu.Lock()
	target := f.net.hosts[hostID]
	off := f.net.offline[hostID]
	level, ok := f.net.level[f.self+">"+hostID]
	f.net.mu.Unlock()
	if !ok {
		level = federation.LevelMessage
	}
	var cmd struct {
		T        string            `json:"t"`
		Envelope *Envelope         `json:"envelope"`
		From     *Address          `json:"from"`
		To       *Address          `json:"to"`
		Reason   string            `json:"reason"`
		Req      *Address          `json:"requester"`
		Query    string            `json:"query"`
		Extra    map[string]string `json:"-"`
	}
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	f.net.mu.Lock()
	f.net.calls = append(f.net.calls, f.self+">"+hostID+":"+cmd.T)
	f.net.mu.Unlock()
	if target == nil || off {
		return nil, fmt.Errorf("federation: route to %q is offline", hostID)
	}
	if need := federation.CommandLevel(cmd.T); level < need {
		return nil, fmt.Errorf("federation: %s may not %s on this host (needs %s access, has %s)", f.self, cmd.T, need, level)
	}
	switch cmd.T {
	case "agent_message_deliver":
		res := target.svc.Deliver(ctx, f.self, *cmd.Envelope)
		reply := map[string]any{"t": "agent_message_result", "id": res.ID, "status": res.Status}
		if res.Error != "" {
			reply["error"], reply["message"] = res.Error, res.Message
		}
		return json.Marshal(reply)
	case "agent_link_request":
		res := target.svc.HandleLinkRequest(ctx, f.self, *cmd.From, *cmd.To, cmd.Reason)
		reply := map[string]any{"t": "agent_link_request_result", "status": res.Status}
		if res.Error != "" {
			reply["error"], reply["message"] = res.Error, res.Message
		}
		return json.Marshal(reply)
	case "agent_directory":
		return json.Marshal(map[string]any{"t": "agent_directory", "entries": target.svc.LocalDirectory(cmd.Req, cmd.Query)})
	}
	return nil, fmt.Errorf("unsupported test command %s", cmd.T)
}

// testSessions is a Sessions backed by a map.
type testSessions struct {
	mu sync.Mutex
	m  map[string]*session.Session
}

func (s *testSessions) Get(id string) *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id]
}
func (s *testSessions) List() []*session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session.Session, 0, len(s.m))
	for _, v := range s.m {
		out = append(out, v)
	}
	return out
}
func (s *testSessions) CWD(id string) string { return "/work/" + id }

// testHost is one daemon's worth of messaging: a store, sessions and a service.
type testHost struct {
	t        *testing.T
	id       string
	db       *store.Store
	sessions *testSessions
	svc      *Service
	clock    *fakeClock
	adapters map[string]*fakeAdapter

	summaryChanges int32
	linkChanges    []string
	mu             sync.Mutex
}

type hostOpts struct {
	net   *testNet
	clock *fakeClock
	db    *store.Store
}

func newTestHost(t *testing.T, id string, o hostOpts) *testHost {
	t.Helper()
	if o.clock == nil {
		o.clock = newClock()
	}
	if o.db == nil {
		db, err := store.Open(filepath.Join(t.TempDir(), id+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		o.db = db
	}
	h := &testHost{t: t, id: id, db: o.db, sessions: &testSessions{m: map[string]*session.Session{}}, clock: o.clock, adapters: map[string]*fakeAdapter{}}
	h.svc = h.newService(o.net)
	if o.net != nil {
		o.net.mu.Lock()
		o.net.hosts[id] = h
		o.net.mu.Unlock()
	}
	return h
}

func (h *testHost) newService(net *testNet) *Service {
	opts := Options{
		Store: h.db, Sessions: h.sessions, SelfID: h.id, LocalName: strings.ToUpper(h.id), Now: h.clock.Now, Token: "tok",
		OnLinksChanged: func(id string) { h.mu.Lock(); h.linkChanges = append(h.linkChanges, id); h.mu.Unlock() },
	}
	if net != nil {
		opts.Federation = &testFed{net: net, self: h.id}
	}
	svc, err := New(opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return svc
}

// restart replaces the service with a fresh one over the same store, as a
// daemon restart would.
func (h *testHost) restart(net *testNet) {
	h.svc.Close()
	h.svc = h.newService(net)
	for _, s := range h.sessions.List() {
		h.svc.Watch(s)
	}
}

func (h *testHost) addSession(id, name string) (*session.Session, *fakeAdapter) {
	h.t.Helper()
	log, err := eventlog.New(h.id+"-"+id, h.db, 64)
	if err != nil {
		h.t.Fatal(err)
	}
	a := newFakeAdapter()
	s, err := session.New(id, name, agentadapter.Spec{Agent: "claude"}, a, log)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = s.Dispose(context.Background()) })
	h.sessions.mu.Lock()
	h.sessions.m[id] = s
	h.sessions.mu.Unlock()
	h.adapters[id] = a
	h.svc.Watch(s)
	return s, a
}

// closeSession mimics Registry.Close.
func (h *testHost) closeSession(id string) {
	h.sessions.mu.Lock()
	s := h.sessions.m[id]
	delete(h.sessions.m, id)
	h.sessions.mu.Unlock()
	if s != nil {
		_ = s.Dispose(context.Background())
	}
	h.svc.SessionClosed(id)
}

func (h *testHost) addr(id string) Address {
	return Address{Host: h.id, Agent: id, Name: h.sessions.Get(id).DisplayName()}
}

// link grants from -> to (a local session) with default settings.
func (h *testHost) link(from Address, to string) {
	h.t.Helper()
	if err := h.svc.SetLink(to, LinkInput{From: from}); err != nil {
		h.t.Fatal(err)
	}
}

// events returns a session's logged agent_message events.
func (h *testHost) agentEvents(id string) []map[string]any {
	h.t.Helper()
	hist, err := h.sessions.Get(id).Log.FullHistory()
	if err != nil {
		h.t.Fatal(err)
	}
	var out []map[string]any
	for _, le := range hist {
		if le.Event.Kind != "agent_message" && le.Event.Kind != "agent_message_status" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(le.Event.Payload, &m); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// requestID extracts request-id="…" from a prompt.
func requestID(t *testing.T, prompt string) string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, `request-id="`)
	if !ok {
		t.Fatalf("no request-id in %q", prompt)
	}
	id, _, _ := strings.Cut(rest, `"`)
	return id
}

func rejectionCode(err error) string {
	if err == nil {
		return ""
	}
	if r, ok := err.(*Rejection); ok {
		return r.Code
	}
	return "unexpected: " + err.Error()
}
