package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/browser"
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

type inertBrowserDriver struct{}

func (inertBrowserDriver) Kind() string { return "test" }
func (inertBrowserDriver) Provision(context.Context, string) (browser.ProvisionResult, error) {
	return browser.ProvisionResult{}, errors.New("not provisioned by this test")
}
func (inertBrowserDriver) Teardown(context.Context, string) error { return nil }
func (inertBrowserDriver) IsProvisioned(string) bool              { return false }
func (inertBrowserDriver) PID(string) int                         { return 0 }

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
func (*testBackend) ListGitRefs(context.Context, string) ([]workspace.GitRefInfo, error) {
	return []workspace.GitRefInfo{{Ref: "refs/heads/main", DisplayName: "main", Kind: workspace.RefLocalBranch, Commit: "abc", IsCurrent: true, IsDefault: true}}, nil
}
func (*testBackend) ClosePreview(context.Context, string) (*workspace.ClosePreview, error) {
	ahead, behind := 1, 0
	return &workspace.ClosePreview{Kind: workspace.KindWorktree, Uncommitted: "?? dirty", Unmerged: "abc work", TargetRef: "refs/heads/main", Ahead: &ahead, Behind: &behind}, nil
}
func (*testBackend) Diff(context.Context, string) (*workspace.Diff, error) {
	return &workspace.Diff{Uncommitted: "diff --git a/a b/a", Committed: "diff --git a/b b/b", TargetRef: "refs/heads/main"}, nil
}
func (*testBackend) AgentCatalog() registry.Catalog {
	return registry.Catalog{DefaultAgent: "codex", Agents: []registry.CatalogAgent{}}
}
func (b *testBackend) Spawn(context.Context, agentadapter.Spec) (*session.Session, error) {
	return b.Get("a"), nil
}
func (*testBackend) SpawnOptions(context.Context, string, string, []string, string) (registry.SpawnOptions, error) {
	return registry.SpawnOptions{Modes: json.RawMessage(`null`), ConfigOptions: []json.RawMessage{}}, nil
}
func (b *testBackend) Close(_ context.Context, id string, _, _ bool) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions[id] == nil {
		return false, nil
	}
	delete(b.sessions, id)
	return true, nil
}
func (b *testBackend) SetMode(ctx context.Context, id, mode string) error {
	s := b.Get(id)
	if s == nil {
		return errors.New("no such agent")
	}
	return s.SetMode(ctx, mode)
}
func (b *testBackend) SetConfigOption(ctx context.Context, id, configID string, value any) error {
	s := b.Get(id)
	if s == nil {
		return errors.New("no such agent")
	}
	return s.SetConfigOption(ctx, configID, value)
}

func (b *testBackend) CaptureSnapshot(context.Context, string, string) (store.BrowserSnapshot, error) {
	return store.BrowserSnapshot{}, nil
}
func (b *testBackend) ListSnapshots() ([]store.BrowserSnapshot, error)        { return nil, nil }
func (b *testBackend) DeleteSnapshot(string) error                            { return nil }
func (b *testBackend) RestartBrowser(context.Context, string, string) error   { return nil }
func (b *testBackend) ListProfiles(string) ([]store.Profile, []string, error) { return nil, nil, nil }
func (b *testBackend) RenameProfile(string, string) error                     { return nil }
func (b *testBackend) DeleteProfile(string) error                             { return nil }

type testAdapter struct {
	events        chan eventlog.Event
	done          chan struct{}
	mu            sync.Mutex
	prompts       [][]agentadapter.PromptBlock
	inputs        [][]byte
	cols, rows    uint16
	permissions   [][2]string
	modes         []string
	configOptions []struct {
		id    string
		value any
	}
	interrupts, closes int
	promptGate         chan struct{}
}

func (a *testAdapter) Capabilities() agentadapter.Capabilities { return agentadapter.Capabilities{} }
func (a *testAdapter) Events() <-chan eventlog.Event           { return a.events }
func (a *testAdapter) Done() <-chan struct{}                   { return a.done }
func (a *testAdapter) Prompt(ctx context.Context, blocks []agentadapter.PromptBlock) (string, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, blocks)
	gate := a.promptGate
	a.mu.Unlock()
	if gate != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-gate:
		}
	}
	return "", nil
}
func (a *testAdapter) SendInput(v []byte) error {
	a.mu.Lock()
	a.inputs = append(a.inputs, append([]byte{}, v...))
	a.mu.Unlock()
	return nil
}
func (a *testAdapter) Resize(cols, rows uint16) error {
	a.mu.Lock()
	a.cols, a.rows = cols, rows
	a.mu.Unlock()
	return nil
}
func (a *testAdapter) RespondPermission(req, option string) error {
	a.mu.Lock()
	a.permissions = append(a.permissions, [2]string{req, option})
	a.mu.Unlock()
	return nil
}
func (a *testAdapter) Interrupt() error { a.mu.Lock(); a.interrupts++; a.mu.Unlock(); return nil }
func (a *testAdapter) SetMode(_ context.Context, mode string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.modes = append(a.modes, mode)
	return nil
}
func (a *testAdapter) SetConfigOption(_ context.Context, id string, value any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.configOptions = append(a.configOptions, struct {
		id    string
		value any
	}{id, value})
	return nil
}
func (a *testAdapter) Close(context.Context) error {
	a.mu.Lock()
	a.closes++
	a.mu.Unlock()
	return nil
}
func (*testAdapter) SessionID() string { return "" }
func (*testAdapter) PID() int          { return 0 }

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
	a := &testAdapter{events: make(chan eventlog.Event, 10000), done: make(chan struct{})}
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
	for _, req := range []map[string]any{{"t": "list_agents", "corrId": "1"}, {"t": "list_dirs", "corrId": "2"}, {"t": "list_agent_catalog", "corrId": "3"}, {"t": "list_git_refs", "repo": "/repo", "corrId": "4"}, {"t": "get_close_preview", "agentId": "a", "corrId": "5"}, {"t": "get_diff", "agentId": "a", "corrId": "6"}} {
		send(t, c, req)
		got := recv(t, c)
		if got["corrId"] != req["corrId"] {
			t.Fatalf("corr %#v", got)
		}
		if req["t"] == "list_git_refs" && (got["t"] != "git_refs" || len(got["refs"].([]any)) != 1) {
			t.Fatalf("refs %#v", got)
		}
		if req["t"] == "get_close_preview" && (got["t"] != "close_preview" || got["preview"] == nil) {
			t.Fatalf("preview %#v", got)
		}
		if req["t"] == "get_diff" && (got["t"] != "diff" || got["diff"] == nil) {
			t.Fatalf("diff %#v", got)
		}
	}
	server.CloseClientConnections()
}

func TestCoreCommandsAndDisconnectDoesNotDisposeAgent(t *testing.T) {
	_, _, a, _, url := setupWS(t, 0)
	c := dial(t, url)
	commands := []map[string]any{
		{"t": "prompt", "agentId": "a", "text": "hello", "corrId": "prompt"},
		{"t": "input", "agentId": "a", "bytesB64": "aGk=", "corrId": "input"},
		{"t": "resize", "agentId": "a", "cols": 120, "rows": 40, "corrId": "resize"},
		{"t": "permission_response", "agentId": "a", "reqId": "r1", "optionId": "allow", "corrId": "permission"},
		{"t": "interrupt", "agentId": "a", "corrId": "interrupt"},
		{"t": "set_mode", "agentId": "a", "modeId": "full-access", "corrId": "mode"},
		{"t": "set_config_option", "agentId": "a", "configId": "model", "value": "gpt-5", "corrId": "model"},
		{"t": "set_config_option", "agentId": "a", "configId": "reasoning", "value": true, "corrId": "reasoning"},
		{"t": "spawn_agent", "spec": map[string]any{}, "corrId": "spawn"},
	}
	for _, command := range commands {
		send(t, c, command)
		got := recv(t, c)
		if got["t"] != "ack" || got["corrId"] != command["corrId"] || got["error"] != nil {
			t.Fatalf("command %#v: %#v", command, got)
		}
	}
	send(t, c, map[string]any{"t": "get_spawn_options", "agent": "codex", "cwd": "/repo", "corrId": "options"})
	if got := recv(t, c); got["t"] != "spawn_options" || got["corrId"] != "options" || got["options"] == nil {
		t.Fatalf("spawn options %#v", got)
	}
	send(t, c, map[string]any{"t": "merge_back", "agentId": "a", "mode": "merge", "corrId": "merge"})
	if got := recv(t, c); got["error"] != "merge_back not implemented yet" || got["agentId"] != "a" {
		t.Fatalf("merge %#v", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		ready := len(a.prompts) == 1
		if ready {
			if a.prompts[0][0].Text != "hello" || string(a.inputs[0]) != "hi" || a.cols != 120 || a.rows != 40 || a.permissions[0] != [2]string{"r1", "allow"} || a.interrupts != 1 || len(a.modes) != 1 || a.modes[0] != "full-access" || len(a.configOptions) != 2 || a.configOptions[0].id != "model" || a.configOptions[0].value != "gpt-5" || a.configOptions[1].value != true {
				t.Fatalf("adapter calls: %#v %#v %dx%d %#v interrupts=%d", a.prompts, a.inputs, a.cols, a.rows, a.permissions, a.interrupts)
			}
		}
		a.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prompt was not dispatched")
		}
		time.Sleep(time.Millisecond)
	}
	c.Close()
	time.Sleep(20 * time.Millisecond)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closes != 0 {
		t.Fatalf("disconnect disposed adapter %d times", a.closes)
	}
}

func TestPromptQueueAcknowledgementSnapshotAndRemoval(t *testing.T) {
	_, b, a, _, url := setupWS(t, 0)
	a.promptGate = make(chan struct{}, 1)
	c := dial(t, url)
	send(t, c, map[string]any{"t": "prompt", "agentId": "a", "text": "first", "corrId": "first"})
	first := recv(t, c)
	if first["disposition"] != "started" || first["position"] != float64(0) || first["promptId"] == nil {
		t.Fatalf("first ack=%#v", first)
	}
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		started := len(a.prompts) == 1
		a.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first prompt did not start")
		}
		time.Sleep(time.Millisecond)
	}
	send(t, c, map[string]any{"t": "prompt", "agentId": "a", "text": "second", "corrId": "second"})
	second := recv(t, c)
	if second["disposition"] != "queued" || second["position"] != float64(1) {
		t.Fatalf("second ack=%#v", second)
	}
	promptID, _ := second["promptId"].(string)
	send(t, c, map[string]any{"t": "subscribe", "agentId": "a", "corrId": "sub"})
	snapshot := recv(t, c)
	queued, _ := snapshot["queuedPrompts"].([]any)
	if snapshot["t"] != "snapshot" || len(queued) != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if state := recv(t, c); state["t"] != "prompt_queue" || len(state["queuedPrompts"].([]any)) != 1 {
		t.Fatalf("queue state=%#v", state)
	}
	recv(t, c) // subscribe ack
	send(t, c, map[string]any{"t": "remove_queued_prompt", "agentId": "a", "promptId": promptID, "corrId": "remove"})
	for {
		message := recv(t, c)
		if message["t"] == "ack" && message["corrId"] == "remove" {
			break
		}
	}
	if len(b.Get("a").QueuedPrompts()) != 0 {
		t.Fatal("queued prompt remained after removal")
	}
	a.promptGate <- struct{}{}
}

func TestCloseBroadcastsSubscribersAndErrorsAreStructured(t *testing.T) {
	_, _, _, _, url := setupWS(t, 0)
	one, two, closer := dial(t, url), dial(t, url), dial(t, url)
	for _, c := range []*websocket.Conn{one, two} {
		send(t, c, map[string]any{"t": "subscribe", "agentId": "a"})
		recv(t, c) // snapshot
		recv(t, c) // ack
	}
	send(t, closer, map[string]any{"t": "resize", "agentId": "missing", "cols": 1, "rows": 1, "corrId": "missing"})
	if got := recv(t, closer); got["error"] != "no such agent: missing" || got["agentId"] != "missing" {
		t.Fatalf("missing agent %#v", got)
	}
	send(t, closer, map[string]any{"t": "close_agent", "agentId": "a", "corrId": "close"})
	if got := recv(t, closer); got["t"] != "ack" || got["error"] != nil {
		t.Fatalf("close ack %#v", got)
	}
	for _, c := range []*websocket.Conn{one, two} {
		if got := recv(t, c); got["t"] != "agent_closed" || got["agentId"] != "a" {
			t.Fatalf("closed %#v", got)
		}
	}
}

func TestSnapshotReplayChannelsMultipleClientsAndUnsubscribe(t *testing.T) {
	_, b, a, _, url := setupWS(t, 0)
	a.events <- event("one")
	a.events <- status()
	waitHead(t, b.Get("a"), 2)
	b.Get("a").SetControlMode("terminal")
	one, two := dial(t, url), dial(t, url)
	send(t, one, map[string]any{"t": "subscribe", "agentId": "a", "channels": []string{"transcript"}, "corrId": "s"})
	snap := recv(t, one)
	if snap["t"] != "snapshot" || snap["seq"] != float64(3) || snap["controlMode"] != "terminal" || len(snap["transcript"].([]any)) != 2 {
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
	if control := recv(t, two); control["t"] != "event" || control["seq"] != float64(3) {
		t.Fatalf("control replay %#v", control)
	}
	recv(t, two)
	a.events <- event("live")
	waitHead(t, b.Get("a"), 4)
	if recv(t, one)["seq"] != float64(4) || recv(t, two)["seq"] != float64(4) {
		t.Fatal("live fanout")
	}
	send(t, one, map[string]any{"t": "unsubscribe", "agentId": "a", "channels": []string{"transcript"}})
	recv(t, one)
	a.events <- event("gone")
	waitHead(t, b.Get("a"), 5)
	one.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if err := one.ReadJSON(&map[string]any{}); err == nil {
		t.Fatal("event after unsubscribe")
	}
}

func TestPTYChannelKeepsAgentCLIAndUserShellEventsTagged(t *testing.T) {
	_, b, a, _, url := setupWS(t, 0)
	a.events <- eventlog.RawPTY([]byte("agent-cli"))
	a.events <- eventlog.ShellPTY([]byte("user-shell"))
	a.events <- eventlog.ShellExit("exited (code 0)")
	a.events <- event("transcript")
	waitHead(t, b.Get("a"), 4)

	pty := dial(t, url)
	send(t, pty, map[string]any{"t": "subscribe", "agentId": "a", "channels": []string{"pty"}})
	snap := recv(t, pty)
	events := snap["transcript"].([]any)
	if snap["t"] != "snapshot" || len(events) != 3 {
		t.Fatalf("pty snapshot %#v", snap)
	}
	want := []string{"raw_pty", "shell_pty", "shell_exit"}
	for i, entry := range events {
		event := entry.(map[string]any)["event"].(map[string]any)
		if event["kind"] != want[i] {
			t.Fatalf("event %d = %#v, want kind %s", i, event, want[i])
		}
	}
	if recv(t, pty)["t"] != "ack" {
		t.Fatal("missing pty subscribe ack")
	}

	transcript := dial(t, url)
	send(t, transcript, map[string]any{"t": "subscribe", "agentId": "a", "channels": []string{"transcript"}})
	transcriptSnap := recv(t, transcript)
	transcriptEvents := transcriptSnap["transcript"].([]any)
	if len(transcriptEvents) != 1 {
		t.Fatalf("transcript snapshot %#v", transcriptSnap)
	}
	event := transcriptEvents[0].(map[string]any)["event"].(map[string]any)
	if event["kind"] != "message_chunk" {
		t.Fatalf("transcript event %#v", event)
	}
}

func TestBrowserSubscriptionControlAndInput(t *testing.T) {
	_, backend, _, old, _ := setupWS(t, 0)
	old.Close()
	broker := browser.NewBroker(inertBrowserDriver{}, browser.BrokerConfig{})
	server := httptest.NewServer(New(Options{Token: "secret", Registry: backend, Browser: broker}))
	defer server.Close()
	c := dial(t, "ws"+strings.TrimPrefix(server.URL, "http"))
	send(t, c, map[string]any{"t": "subscribe", "agentId": "a", "channels": []string{"browser"}})
	if got := recv(t, c); got["t"] != "snapshot" {
		t.Fatalf("snapshot = %#v", got)
	}
	if got := recv(t, c); got["t"] != "browser_state" || got["active"] != false || got["controlOwner"] != "agent" {
		t.Fatalf("cold browser state = %#v", got)
	}
	if got := recv(t, c); got["t"] != "ack" {
		t.Fatalf("subscribe ack = %#v", got)
	}
	send(t, c, map[string]any{"t": "browser_control", "agentId": "a", "action": "grab", "corrId": "grab"})
	if got := recv(t, c); got["t"] != "browser_state" || got["controlOwner"] != "user" {
		t.Fatalf("grab state = %#v", got)
	}
	if got := recv(t, c); got["t"] != "ack" || got["corrId"] != "grab" {
		t.Fatalf("grab ack = %#v", got)
	}
	send(t, c, map[string]any{"t": "browser_input", "agentId": "a", "event": map[string]any{"kind": "keydown", "key": "Enter"}, "corrId": "input"})
	if got := recv(t, c); got["t"] != "ack" || got["corrId"] != "input" {
		t.Fatalf("input ack = %#v", got)
	}
	send(t, c, map[string]any{"t": "browser_control", "agentId": "a", "action": "release", "corrId": "release"})
	if got := recv(t, c); got["t"] != "browser_state" || got["controlOwner"] != "agent" {
		t.Fatalf("release state = %#v", got)
	}
}

// A base subscription (no 'browser' channel, so no screencast) must still get
// browser_state, so the Browser tab enables the moment the agent provisions a
// browser — and after a page refresh — without the pane being opened first.
func TestBrowserStateFlowsOnBaseSubscription(t *testing.T) {
	_, backend, _, old, _ := setupWS(t, 0)
	old.Close()
	broker := browser.NewBroker(inertBrowserDriver{}, browser.BrokerConfig{})
	server := httptest.NewServer(New(Options{Token: "secret", Registry: backend, Browser: broker}))
	defer server.Close()
	c := dial(t, "ws"+strings.TrimPrefix(server.URL, "http"))
	send(t, c, map[string]any{"t": "subscribe", "agentId": "a", "channels": []string{"transcript", "status"}})
	if got := recv(t, c); got["t"] != "snapshot" {
		t.Fatalf("snapshot = %#v", got)
	}
	if got := recv(t, c); got["t"] != "browser_state" || got["active"] != false || got["controlOwner"] != "agent" {
		t.Fatalf("base browser state = %#v", got)
	}
	if got := recv(t, c); got["t"] != "ack" {
		t.Fatalf("subscribe ack = %#v", got)
	}
	// A later state change (grab) reaches the base subscriber even though it never
	// subscribed to the browser channel.
	broker.Grab("a")
	if got := recv(t, c); got["t"] != "browser_state" || got["controlOwner"] != "user" {
		t.Fatalf("grab state on base sub = %#v", got)
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
	next := &testAdapter{events: make(chan eventlog.Event, 10000), done: make(chan struct{})}
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
