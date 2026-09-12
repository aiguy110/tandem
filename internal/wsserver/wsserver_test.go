package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/historyimport"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
	"github.com/gorilla/websocket"
)

type testFederation struct {
	hosts      []federation.Host
	mu         sync.Mutex
	calls      []json.RawMessage
	subscriber func(string, json.RawMessage)
}

func (f *testFederation) Hosts() []federation.Host { return f.hosts }
func (f *testFederation) Subscribe(fn func(string, json.RawMessage)) func() {
	f.mu.Lock()
	f.subscriber = fn
	f.mu.Unlock()
	return func() {}
}
func (f *testFederation) Call(_ context.Context, hostID string, payload json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append(json.RawMessage(nil), payload...))
	subscriber := f.subscriber
	f.mu.Unlock()
	var command struct {
		T string `json:"t"`
	}
	_ = json.Unmarshal(payload, &command)
	switch command.T {
	case "spawn_agent", "resume_session":
		return json.RawMessage(`{"t":"ack","agentId":"remote-agent"}`), nil
	case "subscribe":
		if subscriber != nil {
			subscriber(hostID, json.RawMessage(`{"t":"snapshot","agentId":"remote-agent","seq":0,"transcript":[],"status":"idle","pendingApprovals":[]}`))
		}
		return json.RawMessage(`{"t":"ack","agentId":"remote-agent"}`), nil
	case "search_sessions":
		return json.RawMessage(`{"t":"session_search","query":"work","results":[]}`), nil
	default:
		return json.RawMessage(`{"t":"ack"}`), nil
	}
}

type testBackend struct {
	mu       sync.RWMutex
	sessions map[string]*session.Session
	resume   struct {
		sessionID, agent, cwd, source string
	}
	annotations    map[string][]store.Annotation
	audioPositions map[string]store.AudioPosition
	now            func() int64
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
func (*testBackend) ListWorkspaceEntries(context.Context, string, string) ([]registry.WorkspaceEntry, error) {
	return []registry.WorkspaceEntry{{Path: "src", IsDir: true}, {Path: "README.md"}}, nil
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
func (b *testBackend) Close(_ context.Context, id string, _, _, _ bool) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions[id] == nil {
		return false, nil
	}
	delete(b.sessions, id)
	return true, nil
}
func (*testBackend) Rename(string, string) error { return nil }
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
func (*testBackend) SetAudioEnabled(string, bool) error       { return nil }
func (*testBackend) SetAudioFocus(string, string, bool) error { return nil }

// SetAudioPosition and AudioPosition are an in-memory stand-in for the store,
// mirroring the ListAnnotations/UpsertAnnotation pattern above, so WS tests
// can exercise set_audio_position + the snapshot's audioPosition field
// without a real database.
func (b *testBackend) SetAudioPosition(agentID string, seq, positionMs int64) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	updatedAt := int64(0)
	if b.now != nil {
		updatedAt = b.now()
	}
	if b.audioPositions == nil {
		b.audioPositions = map[string]store.AudioPosition{}
	}
	if seq == 0 {
		delete(b.audioPositions, agentID)
		return updatedAt, nil
	}
	b.audioPositions[agentID] = store.AudioPosition{AgentID: agentID, Seq: seq, PositionMs: positionMs, UpdatedAt: updatedAt}
	return updatedAt, nil
}
func (b *testBackend) AudioPosition(agentID string) (*store.AudioPosition, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	pos, ok := b.audioPositions[agentID]
	if !ok {
		return nil, nil
	}
	return &pos, nil
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
func (*testBackend) ResumeCatalog(context.Context) (registry.ResumeCatalog, error) {
	return registry.ResumeCatalog{Sessions: []registry.ResumableSession{{
		SessionID: "vendor-session", Source: "history", Agent: "codex",
		Adapter: "pty", CWD: "/repo", Resumable: true,
	}}}, nil
}
func (*testBackend) SearchSessions(_ context.Context, query string, _, _ int) ([]registry.SessionSearchResult, error) {
	return []registry.SessionSearchResult{{
		Session: registry.ResumableSession{
			SessionID: "vendor-session", Source: "history", Agent: "codex",
			Adapter: "pty", CWD: "/repo", Resumable: true,
		},
		Score: -1,
		Hits: []registry.SessionSearchHit{{
			EntryID: "entry-1", Role: "assistant",
			Match: store.HistoryExcerpt{Text: "matched " + query, Highlights: []store.Highlight{{Start: 8, End: 8 + len([]rune(query))}}},
		}},
	}}, nil
}
func (b *testBackend) Resume(_ context.Context, sessionID, agent, cwd, source string) (*session.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resume.sessionID, b.resume.agent, b.resume.cwd, b.resume.source = sessionID, agent, cwd, source
	return b.sessions["a"], nil
}

// ListAnnotations, UpsertAnnotation, DeleteAnnotation, and ClearAnnotations are
// an in-memory stand-in for the store, exercising the wsserver protocol
// (add/update/delete/clear + snapshot + broadcast) without a real database.
func (b *testBackend) ListAnnotations(agentID string) ([]store.Annotation, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]store.Annotation, len(b.annotations[agentID]))
	copy(out, b.annotations[agentID])
	return out, nil
}
func (b *testBackend) UpsertAnnotation(a store.Annotation) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.annotations == nil {
		b.annotations = map[string][]store.Annotation{}
	}
	list := b.annotations[a.AgentID]
	for i := range list {
		if list[i].ID == a.ID {
			list[i] = a
			return nil
		}
	}
	b.annotations[a.AgentID] = append(list, a)
	return nil
}
func (b *testBackend) DeleteAnnotation(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for agentID, list := range b.annotations {
		for i := range list {
			if list[i].ID == id {
				b.annotations[agentID] = append(list[:i], list[i+1:]...)
				return nil
			}
		}
	}
	return nil
}
func (b *testBackend) ClearAnnotations(agentID string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.annotations[agentID])
	delete(b.annotations, agentID)
	return n, nil
}

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
	return setupWSHistory(t, queue, nil)
}

func setupWSHistory(t *testing.T, queue int, history HistoryLifecycle) (*store.Store, *testBackend, *testAdapter, *httptest.Server, string) {
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
	server := httptest.NewServer(New(Options{Token: "secret", Registry: b, WriteQueue: queue, History: history, Automation: db}))
	t.Cleanup(func() { server.Close(); db.Close() })
	return db, b, a, server, "ws" + strings.TrimPrefix(server.URL, "http")
}

func TestAutomationListAndEnableToggle(t *testing.T) {
	db, _, _, _, url := setupWS(t, 0)
	if err := db.UpsertAutomationJob(store.AutomationJob{ID: "job-1", RepositoryID: "/repo/.git", ScriptPath: ".tandem/scripts/check.ts", Name: "check", Cron: "every 5m", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	c := dial(t, url)
	send(t, c, map[string]any{"t": "list_automation", "corrId": "auto-1"})
	got := recv(t, c)
	if got["t"] != "automation" || got["corrId"] != "auto-1" || len(got["jobs"].([]any)) != 1 {
		t.Fatalf("automation=%#v", got)
	}
	send(t, c, map[string]any{"t": "set_automation_enabled", "id": "job-1", "enabled": false, "corrId": "auto-2"})
	got = recv(t, c)
	jobs := got["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["enabled"] != false {
		t.Fatalf("toggled=%#v", got)
	}
}

func TestSystemNotificationsSnapshotBroadcastAndAction(t *testing.T) {
	db, backend, _, _, _ := setupWS(t, 0)
	center := notifications.New()
	center.Upsert(notifications.Notification{ID: "update", Severity: "attention", Title: "Update available"})
	var actedID, actedAction string
	handler := New(Options{
		Token: "secret", Registry: backend, Automation: db, Notifications: center,
		NotificationAction: func(_ context.Context, id, action string) (string, error) {
			actedID, actedAction = id, action
			return "a", nil
		},
	})
	t.Cleanup(handler.Close)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := dial(t, "ws"+strings.TrimPrefix(server.URL, "http"))

	send(t, c, map[string]any{"t": "list_system_notifications", "corrId": "list"})
	got := recv(t, c)
	if got["t"] != "system_notifications" || got["corrId"] != "list" || len(got["notifications"].([]any)) != 1 {
		t.Fatalf("snapshot = %#v", got)
	}
	center.Upsert(notifications.Notification{ID: "update", Severity: "success", Title: "Ready"})
	if got = recv(t, c); got["t"] != "system_notifications" || got["notifications"].([]any)[0].(map[string]any)["title"] != "Ready" {
		t.Fatalf("broadcast = %#v", got)
	}
	send(t, c, map[string]any{"t": "system_notification_action", "notificationId": "update", "action": "restart", "corrId": "act"})
	if got = recv(t, c); got["t"] != "ack" || got["agentId"] != "a" || got["corrId"] != "act" {
		t.Fatalf("action ack = %#v", got)
	}
	if actedID != "update" || actedAction != "restart" {
		t.Fatalf("action = %q %q", actedID, actedAction)
	}
}

func TestFederationRoutesNamespacesAndRelaysRemoteProtocol(t *testing.T) {
	db, backend, _, _, _ := setupWS(t, 0)
	fed := &testFederation{hosts: []federation.Host{{
		ID: "host/one", Name: "builder", Status: "connected",
		Snapshot: json.RawMessage(`{"t":"agents","agents":[{"id":"remote-agent","name":"Remote","adapter":"acp","canHandoff":true,"status":"idle","controlMode":"transcript"}]}`),
	}}}
	handler := New(Options{Token: "secret", Registry: backend, Automation: db, Federation: fed})
	t.Cleanup(handler.Close)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := dial(t, "ws"+strings.TrimPrefix(server.URL, "http"))

	send(t, c, map[string]any{"t": "list_agents", "corrId": "agents"})
	got := recv(t, c)
	agents := got["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents = %#v", got)
	}
	remote := agents[1].(map[string]any)
	remoteID, _ := remote["id"].(string)
	if remote["hostId"] != "host/one" || remote["hostName"] != "builder" || strings.Contains(remoteID, "/") {
		t.Fatalf("remote summary = %#v", remote)
	}

	send(t, c, map[string]any{"t": "spawn_agent", "spec": map[string]any{"hostId": "host/one", "adapter": "acp", "workspace": map[string]any{"kind": "existing", "cwd": "/repo"}}, "corrId": "spawn"})
	got = recv(t, c)
	if got["t"] != "ack" || got["agentId"] != remoteID || got["hostId"] != "host/one" {
		t.Fatalf("spawn ack = %#v", got)
	}
	fed.mu.Lock()
	spawnPayload := append(json.RawMessage(nil), fed.calls[len(fed.calls)-1]...)
	fed.mu.Unlock()
	if strings.Contains(string(spawnPayload), "host/one") {
		t.Fatalf("slave payload retained federation route: %s", spawnPayload)
	}

	send(t, c, map[string]any{"t": "subscribe", "agentId": remoteID, "channels": []string{"transcript", "browser"}, "corrId": "sub"})
	got = recv(t, c)
	if got["t"] != "snapshot" || got["agentId"] != remoteID || got["hostId"] != "host/one" {
		t.Fatalf("remote snapshot = %#v", got)
	}
	if got = recv(t, c); got["t"] != "ack" || got["agentId"] != remoteID || got["corrId"] != "sub" {
		t.Fatalf("subscribe ack = %#v", got)
	}

	send(t, c, map[string]any{"t": "search_sessions", "hostId": "host/one", "query": "work", "corrId": "search"})
	if got = recv(t, c); got["t"] != "session_search" || got["hostId"] != "host/one" || got["corrId"] != "search" {
		t.Fatalf("remote history = %#v", got)
	}
}

func TestFederationWebSocketUpgradeUsesFallbackRouter(t *testing.T) {
	_, backend, _, _, _ := setupWS(t, 0)
	called := make(chan struct{}, 1)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		http.Error(w, "federation route", http.StatusTeapot)
	})
	server := httptest.NewServer(New(Options{Token: "browser-secret", Registry: backend, Fallback: fallback}))
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/federation/tunnel"
	_, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil || response == nil || response.StatusCode != http.StatusTeapot {
		t.Fatalf("dial err=%v response=%#v", err, response)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("federation fallback was not called")
	}
}

type testHistoryLifecycle struct {
	mu       sync.Mutex
	triggers int
	refresh  int
	reindex  bool
}

func (h *testHistoryLifecycle) TriggerStale(string) {
	h.mu.Lock()
	h.triggers++
	h.mu.Unlock()
}
func (h *testHistoryLifecycle) Refresh(_ string, reindex bool) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refresh++
	h.reindex = reindex
	return true, nil
}
func (*testHistoryLifecycle) Status(agent string) ([]historyimport.AgentStatus, error) {
	return []historyimport.AgentStatus{{Agent: agent}}, nil
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
	for _, req := range []map[string]any{{"t": "list_agents", "corrId": "1"}, {"t": "list_dirs", "corrId": "2"}, {"t": "list_workspace_entries", "agentId": "a", "path": "", "corrId": "entries"}, {"t": "list_agent_catalog", "corrId": "3"}, {"t": "list_git_refs", "repo": "/repo", "corrId": "4"}, {"t": "get_close_preview", "agentId": "a", "corrId": "5"}, {"t": "get_diff", "agentId": "a", "corrId": "6"}} {
		send(t, c, req)
		got := recv(t, c)
		if got["corrId"] != req["corrId"] {
			t.Fatalf("corr %#v", got)
		}
		if req["t"] == "list_git_refs" && (got["t"] != "git_refs" || len(got["refs"].([]any)) != 1) {
			t.Fatalf("refs %#v", got)
		}
		if req["t"] == "list_workspace_entries" && (got["t"] != "workspace_entries" || len(got["entries"].([]any)) != 2) {
			t.Fatalf("workspace entries %#v", got)
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

func TestResumeProtocolPreservesAgentIdentityAndSource(t *testing.T) {
	_, backend, _, _, url := setupWS(t, 0)
	c := dial(t, url)
	send(t, c, map[string]any{"t": "list_sessions", "corrId": "list"})
	got := recv(t, c)
	if got["t"] != "sessions" || got["corrId"] != "list" {
		t.Fatalf("catalog response %#v", got)
	}
	send(t, c, map[string]any{
		"t": "resume_session", "sessionId": "vendor-session", "agent": "codex",
		"cwd": "/repo", "source": "history", "corrId": "resume",
	})
	for {
		got = recv(t, c)
		if got["corrId"] == "resume" {
			break
		}
	}
	if got["t"] != "ack" || got["corrId"] != "resume" || got["agentId"] != "a" {
		t.Fatalf("resume response %#v", got)
	}
	backend.mu.RLock()
	call := backend.resume
	backend.mu.RUnlock()
	if call.sessionID != "vendor-session" || call.agent != "codex" ||
		call.cwd != "/repo" || call.source != "history" {
		t.Fatalf("resume call %#v", call)
	}
}

func TestSessionSearchProtocolPreservesCorrelationAndStructuredHighlights(t *testing.T) {
	_, _, _, _, url := setupWS(t, 0)
	c := dial(t, url)
	send(t, c, map[string]any{
		"t": "search_sessions", "query": "東京", "limit": 12,
		"maxHitsPerSession": 3, "corrId": "search-2",
	})
	got := recv(t, c)
	if got["t"] != "session_search" || got["corrId"] != "search-2" || got["query"] != "東京" {
		t.Fatalf("search response %#v", got)
	}
	results, ok := got["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results %#v", got["results"])
	}
	result := results[0].(map[string]any)
	hits := result["hits"].([]any)
	match := hits[0].(map[string]any)["match"].(map[string]any)
	if match["text"] != "matched 東京" {
		t.Fatalf("match %#v", match)
	}
	highlight := match["highlights"].([]any)[0].(map[string]any)
	if highlight["start"] != float64(8) || highlight["end"] != float64(10) {
		t.Fatalf("highlight %#v", highlight)
	}
}

func TestHistoryLifecycleProtocolTriggersNonblockingRefresh(t *testing.T) {
	history := &testHistoryLifecycle{}
	_, _, _, _, url := setupWSHistory(t, 0, history)
	c := dial(t, url)
	send(t, c, map[string]any{"t": "list_sessions", "corrId": "list"})
	if got := recv(t, c); got["t"] != "sessions" {
		t.Fatalf("list response %#v", got)
	}
	send(t, c, map[string]any{"t": "search_sessions", "query": "needle", "corrId": "search"})
	if got := recv(t, c); got["t"] != "session_search" {
		t.Fatalf("search response %#v", got)
	}
	send(t, c, map[string]any{
		"t": "refresh_history", "agent": "codex", "reindex": true, "corrId": "refresh",
	})
	if got := recv(t, c); got["t"] != "history_refresh" || got["scheduled"] != true {
		t.Fatalf("refresh response %#v", got)
	}
	send(t, c, map[string]any{"t": "history_status", "agent": "codex", "corrId": "status"})
	if got := recv(t, c); got["t"] != "history_status" {
		t.Fatalf("status response %#v", got)
	}
	history.mu.Lock()
	defer history.mu.Unlock()
	if history.triggers != 2 || history.refresh != 1 || !history.reindex {
		t.Fatalf("history lifecycle calls=%+v", history)
	}
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

func TestAnnotationsAddUpdateDeleteClearAndBroadcast(t *testing.T) {
	_, b, _, _, url := setupWS(t, 0)
	one, two := dial(t, url), dial(t, url)
	for _, c := range []*websocket.Conn{one, two} {
		send(t, c, map[string]any{"t": "subscribe", "agentId": "a"})
		snap := recv(t, c)
		if snap["t"] != "snapshot" {
			t.Fatalf("snapshot=%#v", snap)
		}
		if anns, ok := snap["annotations"].([]any); !ok || len(anns) != 0 {
			t.Fatalf("expected empty annotations in snapshot, got %#v", snap["annotations"])
		}
		if ack := recv(t, c); ack["t"] != "ack" {
			t.Fatalf("subscribe ack=%#v", ack)
		}
	}

	// add_annotation: the daemon broadcasts the full list to every subscriber —
	// including the sender, ahead of the sender's own command ack — then ack's.
	send(t, one, map[string]any{"t": "add_annotation", "agentId": "a", "seq": float64(5), "role": "assistant", "quote": "hello world", "comment": "please clarify", "corrId": "add"})
	var annID string
	if msg := recv(t, one); msg["t"] != "annotations" {
		t.Fatalf("broadcast to sender=%#v", msg)
	} else {
		row := msg["annotations"].([]any)[0].(map[string]any)
		annID, _ = row["id"].(string)
		if row["quote"] != "hello world" || row["comment"] != "please clarify" || row["role"] != "assistant" || row["seq"] != float64(5) || annID == "" {
			t.Fatalf("annotation row=%#v", row)
		}
	}
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "add" || ack["error"] != nil {
		t.Fatalf("add ack=%#v", ack)
	}
	if msg := recv(t, two); msg["t"] != "annotations" || msg["agentId"] != "a" || len(msg["annotations"].([]any)) != 1 {
		t.Fatalf("broadcast to peer=%#v", msg)
	}
	if got, _ := b.ListAnnotations("a"); len(got) != 1 || got[0].ID != annID {
		t.Fatalf("store after add=%#v", got)
	}

	// update_annotation edits only the comment, preserving quote/seq/role.
	send(t, one, map[string]any{"t": "update_annotation", "agentId": "a", "id": annID, "comment": "actually nvm", "corrId": "update"})
	if msg := recv(t, one); msg["t"] != "annotations" {
		t.Fatalf("broadcast to sender=%#v", msg)
	} else {
		row := msg["annotations"].([]any)[0].(map[string]any)
		if row["comment"] != "actually nvm" || row["quote"] != "hello world" || row["seq"] != float64(5) {
			t.Fatalf("updated annotation=%#v", row)
		}
	}
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "update" || ack["error"] != nil {
		t.Fatalf("update ack=%#v", ack)
	}
	recv(t, two) // broadcast to peer

	// update_annotation on an unknown id is a structured command error.
	send(t, one, map[string]any{"t": "update_annotation", "agentId": "a", "id": "missing", "comment": "x", "corrId": "update-missing"})
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "update-missing" || ack["error"] == nil {
		t.Fatalf("update-missing ack=%#v", ack)
	}

	// delete_annotation removes just the targeted row.
	send(t, one, map[string]any{"t": "delete_annotation", "agentId": "a", "id": annID, "corrId": "del"})
	if msg := recv(t, one); msg["t"] != "annotations" || len(msg["annotations"].([]any)) != 0 {
		t.Fatalf("broadcast after delete=%#v", msg)
	}
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "del" || ack["error"] != nil {
		t.Fatalf("delete ack=%#v", ack)
	}
	recv(t, two) // broadcast to peer

	// clear_annotations empties the tray for everyone; add two rows first.
	send(t, one, map[string]any{"t": "add_annotation", "agentId": "a", "seq": float64(1), "role": "user", "quote": "x", "corrId": "add2"})
	recv(t, one) // broadcast
	recv(t, one) // ack
	recv(t, two) // broadcast
	send(t, one, map[string]any{"t": "add_annotation", "agentId": "a", "seq": float64(2), "role": "user", "quote": "y", "corrId": "add3"})
	recv(t, one) // broadcast
	recv(t, one) // ack
	recv(t, two) // broadcast

	send(t, one, map[string]any{"t": "clear_annotations", "agentId": "a", "corrId": "clear"})
	if msg := recv(t, one); msg["t"] != "annotations" || len(msg["annotations"].([]any)) != 0 {
		t.Fatalf("broadcast after clear=%#v", msg)
	}
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "clear" || ack["error"] != nil {
		t.Fatalf("clear ack=%#v", ack)
	}
	recv(t, two) // broadcast to peer
	if got, _ := b.ListAnnotations("a"); len(got) != 0 {
		t.Fatalf("store not cleared: %#v", got)
	}
}

func TestSetAudioPositionPersistsAndBroadcastsWithoutEchoingSender(t *testing.T) {
	_, b, _, _, url := setupWS(t, 0)
	var clock int64 = 1000
	b.now = func() int64 { return clock }

	one, two := dial(t, url), dial(t, url)
	for _, c := range []*websocket.Conn{one, two} {
		send(t, c, map[string]any{"t": "subscribe", "agentId": "a"})
		snap := recv(t, c)
		if snap["t"] != "snapshot" {
			t.Fatalf("snapshot=%#v", snap)
		}
		if _, has := snap["audioPosition"]; has {
			t.Fatalf("expected no audioPosition before any write, got %#v", snap["audioPosition"])
		}
		if ack := recv(t, c); ack["t"] != "ack" {
			t.Fatalf("subscribe ack=%#v", ack)
		}
	}

	send(t, one, map[string]any{"t": "set_audio_position", "agentId": "a", "seq": float64(4), "positionMs": float64(12500), "corrId": "pos1"})

	// The sender gets its own ack but must NOT receive a broadcast echo of
	// the position it just sent — that would fight its own playback clock.
	// Proven without a read-deadline probe (which can leave the connection's
	// read side unusable in gorilla/websocket): send a distinguishable
	// follow-up command and require its reply to be the very next message,
	// meaning nothing else was queued in between.
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "pos1" || ack["error"] != nil {
		t.Fatalf("set_audio_position ack=%#v", ack)
	}
	send(t, one, map[string]any{"t": "list_dirs", "corrId": "probe"})
	if msg := recv(t, one); msg["t"] != "dirs" || msg["corrId"] != "probe" {
		t.Fatalf("expected the probe's own reply next (no echoed broadcast in between): %#v", msg)
	}

	// A different connection subscribed to the same agent does hear about it,
	// so a second device can follow along.
	if msg := recv(t, two); msg["t"] != "audio_position" || msg["agentId"] != "a" || msg["seq"] != float64(4) || msg["positionMs"] != float64(12500) || msg["updatedAt"] != float64(1000) {
		t.Fatalf("peer broadcast=%#v", msg)
	}

	if got, err := b.AudioPosition("a"); err != nil || got == nil || got.Seq != 4 || got.PositionMs != 12500 || got.UpdatedAt != 1000 {
		t.Fatalf("store after set=%#v err=%v", got, err)
	}

	// A fresh snapshot now carries audioPosition.
	three := dial(t, url)
	send(t, three, map[string]any{"t": "subscribe", "agentId": "a"})
	snap := recv(t, three)
	pos, _ := snap["audioPosition"].(map[string]any)
	if pos == nil || pos["seq"] != float64(4) || pos["positionMs"] != float64(12500) || pos["updatedAt"] != float64(1000) {
		t.Fatalf("snapshot audioPosition=%#v", snap["audioPosition"])
	}
	recv(t, three) // subscribe ack

	// seq: 0 clears the stored position; a later snapshot omits the field.
	clock = 2000
	send(t, one, map[string]any{"t": "set_audio_position", "agentId": "a", "seq": float64(0), "positionMs": float64(0), "corrId": "pos2"})
	if ack := recv(t, one); ack["t"] != "ack" || ack["corrId"] != "pos2" || ack["error"] != nil {
		t.Fatalf("clear ack=%#v", ack)
	}
	if msg := recv(t, two); msg["t"] != "audio_position" || msg["seq"] != float64(0) {
		t.Fatalf("peer clear broadcast=%#v", msg)
	}
	if got, err := b.AudioPosition("a"); err != nil || got != nil {
		t.Fatalf("expected cleared position, got %#v err=%v", got, err)
	}
	four := dial(t, url)
	send(t, four, map[string]any{"t": "subscribe", "agentId": "a"})
	snap = recv(t, four)
	if _, has := snap["audioPosition"]; has {
		t.Fatalf("expected omitted audioPosition after clear, got %#v", snap["audioPosition"])
	}
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

func TestSendFrameKeepsOnlyLatestPendingFramePerAgent(t *testing.T) {
	c := newConnection(New(Options{WriteQueue: 4}), nil)
	if !c.sendFrame("a", map[string]any{"t": "browser_frame", "dataB64": "old"}) {
		t.Fatal("first frame was rejected")
	}
	if !c.sendFrame("a", map[string]any{"t": "browser_frame", "dataB64": "latest"}) {
		t.Fatal("replacement frame was rejected")
	}
	if got := len(c.frameReady); got != 1 {
		t.Fatalf("frame notifications=%d, want 1", got)
	}
	c.frameMu.Lock()
	data := append([]byte(nil), c.latestFrames["a"]...)
	c.frameMu.Unlock()
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	if frame["dataB64"] != "latest" {
		t.Fatalf("pending frame=%v", frame)
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
