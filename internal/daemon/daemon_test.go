package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/httpserver"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/voice"
	"github.com/gorilla/websocket"
)

func TestTakeoverResolvedStatusDoesNotResurrectCancelledTurn(t *testing.T) {
	tests := []struct {
		name    string
		active  bool
		current session.Status
		want    session.Status
	}{
		{name: "active takeover resumes turn", active: true, current: session.Blocked, want: session.Working},
		{name: "cancelled takeover becomes idle", active: false, current: session.Blocked, want: session.Idle},
		{name: "error is retained", active: false, current: session.Error, want: session.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := takeoverResolvedStatus(tt.active, tt.current); got != tt.want {
				t.Fatalf("takeoverResolvedStatus(%v, %q) = %q, want %q", tt.active, tt.current, got, tt.want)
			}
		})
	}
}

func TestConfiguredMCPServersUsesLiveGlobalAndProjectConfiguration(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	if err := os.WriteFile(config.ConfigFilePath(home), []byte("mcpServers:\n  global:\n    command: global-server\n  remote:\n    type: http\n    url: http://localhost:8081/mcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := config.ProjectMCPConfigPath(project)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("mcpServers:\n  project:\n    command: project-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	servers, err := configuredMCPServers(browser.MCPWiring{}, home, "agent-1", project)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, server := range servers {
		got[server.Name] = server.Command
	}
	if got["global"] != "global-server" || got["project"] != "project-server" {
		t.Fatalf("configured servers = %#v", got)
	}
	var remote *browser.MCPServer
	for i := range servers {
		if servers[i].Name == "remote" {
			remote = &servers[i]
		}
	}
	if remote == nil || remote.Type != "http" || remote.URL != "http://localhost:8081/mcp" {
		t.Fatalf("HTTP server was not converted into an ACP declaration: %#v", remote)
	}
	// The same resolver observes a write after daemon startup, which is the
	// no-restart behavior used for subsequent spawned agent sessions.
	if _, err := config.AddMCPServer(home, project, "later", config.MCPServer{Command: "later-server"}, false); err != nil {
		t.Fatal(err)
	}
	servers, err = configuredMCPServers(browser.MCPWiring{}, home, "agent-2", project)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, server := range servers {
		found = found || (server.Name == "later" && server.Command == "later-server")
	}
	if !found {
		t.Fatalf("live configuration change was not applied: %#v", servers)
	}
}

// mp3FixtureBytes returns a small, valid MPEG1 Layer III CBR clip (three
// 128kbps/44100Hz frames, no padding) that internal/voice.Duration can parse.
// The exact byte layout mirrors the production frame-length formula; see
// internal/voice/duration_test.go for the parser's own dedicated tests.
func mp3FixtureBytes(frameCount int) []byte {
	const frameLen = 417 // 144 * 128000 / 44100, no padding
	frame := make([]byte, frameLen)
	frame[0] = 0xFF
	frame[1] = 0xFB // MPEG1, Layer III, no CRC
	frame[2] = 0x90 // bitrate idx 9 (128kbps), sample rate idx 0 (44100Hz), no padding
	var data []byte
	for i := 0; i < frameCount; i++ {
		data = append(data, frame...)
	}
	return data
}

type failIfCalledRenderer struct{ t *testing.T }

func (f failIfCalledRenderer) Render(context.Context, string) (voice.Audio, error) {
	f.t.Fatal("renderer invoked for an already-cached clip")
	return voice.Audio{}, nil
}

func TestMessageAudioCacheBackfillsUnknownDuration(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	clip := mp3FixtureBytes(3)
	wantMs, ok := voice.Duration("audio/mpeg", clip)
	if !ok || wantMs <= 0 {
		t.Fatalf("fixture did not parse: ok=%v ms=%d", ok, wantMs)
	}
	if err := db.PutMessageAudio(store.MessageAudio{SessionID: "agent", Seq: 3, MIMEType: "audio/mpeg", Data: clip}); err != nil {
		t.Fatal(err)
	}
	if row, err := db.MessageAudio("agent", 3); err != nil || row.DurationMs != 0 {
		t.Fatalf("expected a freshly written pre-duration row: row=%#v err=%v", row, err)
	}

	cache := newMessageAudioCache(context.Background(), db, failIfCalledRenderer{t})

	// Direct backfill helper.
	cached, err := db.MessageAudio("agent", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := cache.backfillDuration("agent", 3, cached); got != wantMs {
		t.Fatalf("backfillDuration = %d, want %d", got, wantMs)
	}
	if row, err := db.MessageAudio("agent", 3); err != nil || row.DurationMs != wantMs {
		t.Fatalf("backfill was not persisted: row=%#v err=%v", row, err)
	}

	// render()'s cache-hit path must also backfill (for a separate row) and
	// must not invoke the renderer, since the clip is already cached.
	if err := db.PutMessageAudio(store.MessageAudio{SessionID: "agent", Seq: 5, MIMEType: "audio/mpeg", Data: clip}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.render(context.Background(), "agent", 5); err != nil {
		t.Fatal(err)
	}
	if row, err := db.MessageAudio("agent", 5); err != nil || row.DurationMs != wantMs {
		t.Fatalf("render() cache-hit path did not backfill: row=%#v err=%v", row, err)
	}
}

func TestServeLoadsEmbeddedUIAndStopsCleanly(t *testing.T) {
	home := t.TempDir()
	port := availablePort(t)
	cfg := config.Config{
		Home: home, DBPath: filepath.Join(home, "tandem.db"), TokenPath: filepath.Join(home, "token"),
		AssetsDir: filepath.Join(home, "assets"), Host: "127.0.0.1", Port: port,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, io.Discard) }()

	client := &http.Client{Timeout: time.Second}
	var response *http.Response
	var err error
	url := fmt.Sprintf("http://127.0.0.1:%d/agents/api-1", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err = client.Get(url)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		<-done
		t.Fatalf("load embedded UI: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `<div id="root"></div>`) || response.Header.Get("Cache-Control") == "" {
		t.Fatalf("UI response status=%d headers=%v body=%q err=%v", response.StatusCode, response.Header, body, readErr)
	}
	if mode := fileMode(t, cfg.TokenPath); mode.Perm() != 0o600 {
		t.Fatalf("token mode=%o, want 600", mode.Perm())
	}
	token, err := os.ReadFile(cfg.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/?token=%s", port, token), nil)
	if err != nil {
		t.Fatalf("dial daemon websocket: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("websocket remained open after daemon shutdown")
	}
}

func TestTranscriptMessageTextCollectsOnlySelectedContiguousMessage(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, event := range []struct{ kind, payload string }{
		{"message_chunk", `{"kind":"message_chunk","text":"Hello "}`},
		{"message_chunk", `{"kind":"message_chunk","text":"world"}`},
		{"audio_state", `{"kind":"audio_state","state":"ready","seq":1}`},
		{"message_chunk", `{"kind":"message_chunk","text":" again"}`},
		{"tool_call", `{"kind":"tool_call","id":"one","title":"tool","status":"done"}`},
		{"message_chunk", `{"kind":"message_chunk","text":"Second message"}`},
	} {
		if _, err := db.AppendEvent("agent", event.kind, event.payload, 1); err != nil {
			t.Fatal(err)
		}
	}
	got, err := transcriptMessageText(db, "agent", 1)
	if err != nil || got != "Hello world again" {
		t.Fatalf("first message = %q, %v", got, err)
	}
	got, err = transcriptMessageText(db, "agent", 6)
	if err != nil || got != "Second message" {
		t.Fatalf("second message = %q, %v", got, err)
	}
	if _, err = transcriptMessageText(db, "agent", 2); !errors.Is(err, httpserver.ErrMessageNotFound) {
		t.Fatalf("non-anchor error = %v", err)
	}
}

func TestMessageAudioPreparationClaimDeduplicatesAndAllowsRetry(t *testing.T) {
	cache := newMessageAudioCache(context.Background(), nil, nil)
	if !cache.claimPreparation("agent", 7) {
		t.Fatal("first preparation claim was rejected")
	}
	if cache.claimPreparation("agent", 7) {
		t.Fatal("duplicate preparation claim was accepted")
	}
	if !cache.claimPreparation("agent", 8) || !cache.claimPreparation("other", 7) {
		t.Fatal("claim key did not include both agent and message sequence")
	}
	cache.releasePreparation("agent", 7)
	if !cache.claimPreparation("agent", 7) {
		t.Fatal("released failed preparation could not be retried")
	}
}

func TestCompletedMessageSeqsPreparesClosedBlocksBeforeTurnCompletion(t *testing.T) {
	history := []eventlog.LoggedEvent{
		{Seq: 1, Event: eventlog.Event{Kind: "user_message"}},
		{Seq: 2, Event: eventlog.Event{Kind: "message_chunk"}},
		{Seq: 3, Event: eventlog.Event{Kind: "message_chunk"}},
		{Seq: 4, Event: eventlog.Event{Kind: "tool_call"}},
		{Seq: 5, Event: eventlog.Event{Kind: "message_chunk"}},
	}

	if got := completedMessageSeqs(history, true, 0, false); len(got) != 1 || got[0] != 2 {
		t.Fatalf("working turn message seqs = %v, want [2]", got)
	}
	if got := completedMessageSeqs(history, true, 0, true); len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Fatalf("idle turn message seqs = %v, want [2 5]", got)
	}
	if got := completedMessageSeqs(history, false, 0, false); len(got) != 1 || got[0] != 2 {
		t.Fatalf("unfocused working turn message seqs = %v, want [2]", got)
	}
}

func TestCompletedMessageSeqsDoesNotSplitOnAudioState(t *testing.T) {
	history := []eventlog.LoggedEvent{
		{Seq: 1, Event: eventlog.Event{Kind: "user_message"}},
		{Seq: 2, Event: eventlog.Event{Kind: "message_chunk"}},
		{Seq: 3, Event: eventlog.Event{Kind: "audio_state"}},
		{Seq: 4, Event: eventlog.Event{Kind: "message_chunk"}},
		{Seq: 5, Event: eventlog.Event{Kind: "tool_call"}},
	}

	if got := completedMessageSeqs(history, true, 0, false); len(got) != 1 || got[0] != 2 {
		t.Fatalf("message seqs = %v, want [2]", got)
	}
}

func TestMessageBlockBoundaryRecognizesAgentOutputTransitions(t *testing.T) {
	if !messageBlockBoundary("tool_call") || !messageBlockBoundary("thought_chunk") {
		t.Fatal("agent output transitions did not close a message block")
	}
	if messageBlockBoundary("audio_state") || messageBlockBoundary("message_chunk") {
		t.Fatal("auxiliary or streaming events unexpectedly closed a message block")
	}
}

func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func fileMode(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

// federatedAudioLocal stands in for a slave's browser-protocol bridge: it
// answers render_message_audio the way that host's own wsserver would.
type federatedAudioLocal struct {
	err string
}

func (federatedAudioLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"t":"agents","agents":[]}`), nil
}
func (l federatedAudioLocal) Execute(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var m struct {
		T         string `json:"t"`
		SessionID string `json:"sessionId"`
		Seq       int64  `json:"seq"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.T != "render_message_audio" {
		return nil, fmt.Errorf("unexpected command %q", m.T)
	}
	envelope := map[string]any{"t": "message_audio", "sessionId": m.SessionID, "seq": m.Seq}
	if l.err != "" {
		envelope["error"] = l.err
	} else {
		envelope["mimeType"] = "audio/mpeg"
		envelope["data"] = base64.StdEncoding.EncodeToString([]byte("remote-" + m.SessionID))
	}
	return json.Marshal(envelope)
}

// connectFederatedHost performs the real registration handshake so the test
// exercises Call over a live tunnel rather than a stubbed transport.
func connectFederatedHost(t *testing.T, local federation.Local) (*federation.Service, string) {
	t.Helper()
	masterStore, err := store.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = masterStore.Close() })
	center := notifications.New()
	master, err := federation.New(federation.Options{Store: masterStore, Notifications: center})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(master)
	t.Cleanup(server.Close)
	slaveStore, err := store.Open(filepath.Join(t.TempDir(), "slave.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slaveStore.Close() })
	slave, err := federation.New(federation.Options{Store: slaveStore, MasterURL: server.URL, Name: "build-host", Local: local, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- slave.RunSlave(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	var notification notifications.Notification
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := center.List(); len(got) > 0 {
			notification = got[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if notification.ID == "" {
		t.Fatal("slave never requested registration")
	}
	if _, _, err := master.HandleNotificationAction(context.Background(), notification.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	hostID := strings.TrimPrefix(notification.ID, "federation-registration-")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if peer, _ := masterStore.FederationSlave(hostID); peer != nil && peer.Status == "connected" {
			return master, hostID
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("slave never reached connected")
	return nil, ""
}

func TestRemoteMessageAudioRendersOnTheOwningHost(t *testing.T) {
	master, hostID := connectFederatedHost(t, federatedAudioLocal{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	audio, err := remoteMessageAudio(ctx, master, hostID, "faraday-66", 12)
	if err != nil {
		t.Fatal(err)
	}
	if string(audio.Data) != "remote-faraday-66" || audio.MIMEType != "audio/mpeg" {
		t.Fatalf("audio=%q %q", audio.Data, audio.MIMEType)
	}
}

// federatedAssetLocal answers get_asset the way a slave's wsserver would.
type federatedAssetLocal struct{}

func (federatedAssetLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"t":"agents","agents":[]}`), nil
}
func (federatedAssetLocal) Execute(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var m struct {
		T         string `json:"t"`
		SessionID string `json:"sessionId"`
		AssetID   string `json:"assetId"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.T != "get_asset" {
		return nil, fmt.Errorf("unexpected command %q", m.T)
	}
	envelope := map[string]any{"t": "asset", "sessionId": m.SessionID, "assetId": m.AssetID}
	if m.AssetID != "img-1" {
		envelope["error"] = "asset not found"
	} else {
		envelope["mimeType"] = "image/png"
		envelope["data"] = base64.StdEncoding.EncodeToString([]byte("png-" + m.SessionID))
	}
	return json.Marshal(envelope)
}

func TestFederatedAssetStoreFetchesFromTheOwningHost(t *testing.T) {
	master, hostID := connectFederatedHost(t, federatedAssetLocal{})
	localStore, err := store.Open(filepath.Join(t.TempDir(), "assets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localStore.Close() })
	local, err := assets.Open(filepath.Join(t.TempDir(), "assets"), localStore)
	if err != nil {
		t.Fatal(err)
	}
	s := federatedAssetStore{local: local, federation: master}

	stored, err := s.Get("fed~"+hostID+"~faraday-66", "img-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Data) != "png-faraday-66" || stored.MIMEType != "image/png" || stored.Size != int64(len(stored.Data)) {
		t.Fatalf("stored=%#v", stored)
	}
	if _, err := s.Get("fed~"+hostID+"~faraday-66", "img-2"); err == nil || err.Error() != "asset not found" {
		t.Fatalf("missing err=%v", err)
	}
	if _, err := s.Get("faraday-66", "img-1"); err == nil {
		t.Fatal("local session unexpectedly fetched remotely")
	}
}

type federatedUploadLocal struct{}

func (federatedUploadLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"t":"agents","agents":[]}`), nil
}
func (federatedUploadLocal) Execute(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var m struct {
		T         string `json:"t"`
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
		MIMEType  string `json:"mimeType"`
		BytesB64  string `json:"bytesB64"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(m.BytesB64)
	if err != nil {
		return nil, err
	}
	switch m.T {
	case "has_upload_directory":
		return json.Marshal(map[string]any{"t": m.T, "configured": m.SessionID == "faraday-66"})
	case "save_upload":
		return json.Marshal(map[string]any{"t": m.T, "path": ".tandem/uploads/" + m.Name + "-" + string(data)})
	case "put_asset":
		return json.Marshal(map[string]any{"t": m.T, "assetId": "remote-img", "mimeType": m.MIMEType, "size": len(data)})
	default:
		return nil, fmt.Errorf("unexpected command %q", m.T)
	}
}

func TestFederatedUploadsAreStoredOnTheOwningHost(t *testing.T) {
	master, hostID := connectFederatedHost(t, federatedUploadLocal{})
	remoteID := "fed~" + hostID + "~faraday-66"
	uploads := federatedUploadStore{federation: master}
	configured, err := uploads.HasConfiguredDirectory(remoteID)
	if err != nil || !configured {
		t.Fatalf("configured=%v err=%v", configured, err)
	}
	path, err := uploads.Save(remoteID, "photo.png", []byte("workspace"))
	if err != nil || path != ".tandem/uploads/photo.png-workspace" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	assetStore := federatedAssetStore{federation: master}
	stored, err := assetStore.Put(remoteID, []byte("image"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AssetID != "remote-img" || stored.MIMEType != "image/png" || stored.Size != 5 {
		t.Fatalf("stored=%#v", stored)
	}
}

func TestRemoteMessageAudioSurfacesTheOwningHostsError(t *testing.T) {
	master, hostID := connectFederatedHost(t, federatedAudioLocal{err: "voice rendering is not configured; run tandem setup"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := remoteMessageAudio(ctx, master, hostID, "faraday-66", 12)
	if err == nil || err.Error() != "voice rendering is not configured; run tandem setup" {
		t.Fatalf("err=%v", err)
	}
}
