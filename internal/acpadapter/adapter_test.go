package acpadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/terminalhost"
	"github.com/aiguy110/tandem/internal/workspacefs"
)

type fakeAssets struct{ stored assets.Stored }

type discardEvents struct{}

func (discardEvents) Append(event eventlog.Event) (eventlog.LoggedEvent, error) {
	return eventlog.LoggedEvent{Event: event}, nil
}

func (f fakeAssets) Get(sessionID, assetID string) (assets.Stored, error) {
	if sessionID != "api-1" || assetID != f.stored.AssetID {
		return assets.Stored{}, assets.ErrNotFound
	}
	return f.stored, nil
}

func (f fakeAssets) Put(string, []byte, string) (assets.Stored, error) {
	return assets.Stored{}, errors.New("unexpected asset write")
}

func mockPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate mock")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "testdata", "mock-acp-agent.mjs"))
}

func startMock(t *testing.T, mutate func(*AdapterConfig)) *Adapter {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the shared ACP mock")
	}
	cfg := AdapterConfig{
		SessionID: "api-1",
		Cwd:       t.TempDir(),
		Transport: acp.Config{
			Command: node,
			Args:    []string{mockPath(t)},
			Env:     os.Environ(),
			Stderr:  io.Discard,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	a, err := StartAdapter(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestConfiguredMCPServersAreDeclaredForNewAndLoadedSessions(t *testing.T) {
	for _, tc := range []struct {
		name, resumeID string
	}{
		{name: "new"},
		{name: "load", resumeID: "sess_external"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := startMock(t, func(cfg *AdapterConfig) {
				cfg.ResumeSessionID = tc.resumeID
				cfg.MCPServers = []MCPServer{{Name: "playwright", Command: "/tools/node", Args: []string{"playwright-mcp"}}}
				cfg.Transport.Env = append(cfg.Transport.Env, "TANDEM_MOCK_EXPECT_MCP=true")
			})
			if a.ExternalSessionID() == "" {
				t.Fatal("session was not initialized")
			}
		})
	}
}

func startServiceMock(t *testing.T) (*Adapter, string, *terminalhost.Host, *eventlog.Log) {
	t.Helper()
	workspace := t.TempDir()
	fsService, err := workspacefs.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsService.Close() })
	backing, err := store.Open(filepath.Join(t.TempDir(), "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backing.Close() })
	log, err := eventlog.New("api-1", backing, 4)
	if err != nil {
		t.Fatal(err)
	}
	host, err := terminalhost.New(terminalhost.Options{DefaultCwd: workspace, EventLog: log, ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = host.Close(ctx)
	})
	a := startMock(t, func(cfg *AdapterConfig) {
		cfg.Cwd = workspace
		cfg.WorkspaceFS = fsService
		cfg.Terminals = host
	})
	return a, workspace, host, log
}

func TestNormalizeToolImagesIntoDurableAssets(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assetStore, err := assets.Open(filepath.Join(root, "assets"), db)
	if err != nil {
		t.Fatal(err)
	}
	var pngData bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{cfg: AdapterConfig{SessionID: "api-1", Cwd: root, Assets: assetStore}}

	inline, _ := json.Marshal([]any{map[string]any{"type": "content", "content": map[string]any{
		"type": "image", "data": base64.StdEncoding.EncodeToString(pngData.Bytes()), "mimeType": "image/png",
	}}})
	normalized, ok := a.normalizeToolContent(inline, "")
	if !ok {
		t.Fatal("inline image content was dropped")
	}
	encoded, _ := json.Marshal(normalized)
	if bytes.Contains(encoded, []byte(`"data"`)) || !bytes.Contains(encoded, []byte(`"assetId"`)) {
		t.Fatalf("normalized inline content = %s", encoded)
	}

	file := filepath.Join(root, "shot.png")
	if err := os.WriteFile(file, pngData.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	resource, _ := json.Marshal([]any{map[string]any{"type": "content", "content": map[string]any{
		"type": "resource_link", "uri": file, "name": "shot.png",
	}}})
	normalized, ok = a.normalizeToolContent(resource, "")
	if !ok {
		t.Fatal("resource image content was dropped")
	}
	encoded, _ = json.Marshal(normalized)
	if bytes.Contains(encoded, []byte(`resource_link`)) || !bytes.Contains(encoded, []byte(`"name":"shot.png"`)) {
		t.Fatalf("normalized resource content = %s", encoded)
	}

	normalized, ok = a.normalizeToolContent(nil, "shot.png")
	if !ok {
		t.Fatal("Playwright filename image was dropped")
	}
	encoded, _ = json.Marshal(normalized)
	if !bytes.Contains(encoded, []byte(`"assetId"`)) {
		t.Fatalf("normalized filename content = %s", encoded)
	}
	rawInput := json.RawMessage(`{"arguments":{"filename":"shot.png","type":"png"},"server":"playwright"}`)
	if got := playwrightScreenshotFile("mcp.playwright.browser_take_screenshot", rawInput); got != "shot.png" {
		t.Fatalf("Playwright screenshot filename = %q", got)
	}
}

func eventMap(t *testing.T, event eventlog.Event) map[string]any {
	t.Helper()
	data, err := event.NormalizedJSON()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitEvent(t *testing.T, a *Adapter, kind string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-a.Events():
			if !ok {
				t.Fatalf("event stream closed waiting for %s", kind)
			}
			got := eventMap(t, event)
			if got["kind"] == kind && (match == nil || match(got)) {
				return got
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func TestLifecycleApprovalAndNormalizedUpdates(t *testing.T) {
	a := startMock(t, nil)
	if a.ExternalSessionID() != "sess_mock" {
		t.Fatalf("session ID = %q", a.ExternalSessionID())
	}
	if caps := a.Capabilities(); !caps.Structured || !caps.LoadSession || !caps.ForkSession || !caps.Image || !caps.Steering {
		t.Fatalf("capabilities = %#v", caps)
	}

	type result struct {
		stop string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		stop, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "do it"}})
		done <- result{stop, err}
	}()
	seen := map[string]bool{}
	var permission map[string]any
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	complete := func() bool {
		return permission != nil && seen["usage"] && seen["plan"] && seen["tool_call"]
	}
	for !complete() {
		select {
		case event := <-a.Events():
			got := eventMap(t, event)
			kind, _ := got["kind"].(string)
			seen[kind] = true
			if kind == "permission_request" {
				permission = got
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for approval flow")
		}
	}
	pending := a.PendingApprovals()
	if len(pending) != 1 || pending[0].ReqID != permission["reqId"] || pending[0].ToolCallID != "tc1" || len(pending[0].Options) != 2 {
		t.Fatalf("pending approvals = %#v", pending)
	}
	if err := a.RespondPermission(permission["reqId"].(string), "allow"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, a, "tool_call_update", func(e map[string]any) bool { return e["status"] == "done" })
	select {
	case got := <-done:
		if got.err != nil || got.stop != "end_turn" {
			t.Fatalf("prompt = %q, %v", got.stop, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not finish")
	}
}

func TestSteerUsesNegotiatedExtension(t *testing.T) {
	a := startMock(t, nil)
	if err := a.Steer(context.Background(), []PromptBlock{{Type: "text", Text: "change course"}}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, a, "message_chunk", func(e map[string]any) bool { return e["text"] == "Steered: change course" })
}

func TestSteerRequiresAdvertisedCapability(t *testing.T) {
	a := startMock(t, func(cfg *AdapterConfig) {
		cfg.Transport.Env = append(cfg.Transport.Env, "TANDEM_MOCK_STEERING_CAPABILITY=false")
	})
	if err := a.Steer(context.Background(), []PromptBlock{{Type: "text", Text: "change course"}}); err == nil {
		t.Fatal("Steer succeeded without advertised capability")
	}
}

func TestAsideForksAndWrapsForkUpdates(t *testing.T) {
	a := startMock(t, nil)
	stop, err := a.Aside(context.Background(), "aside-1", []PromptBlock{{Type: "text", Text: "DERISK_ASIDE"}})
	if err != nil || stop != "end_turn" {
		t.Fatalf("Aside() = %q, %v", stop, err)
	}
	got := waitEvent(t, a, "aside_event", func(event map[string]any) bool {
		inner, _ := event["event"].(map[string]any)
		return event["asideId"] == "aside-1" && inner["kind"] == "message_chunk"
	})
	inner := got["event"].(map[string]any)
	if inner["text"] != "Aside answer." {
		t.Fatalf("aside event = %#v", got)
	}
	if a.ExternalSessionID() != "sess_mock" {
		t.Fatalf("parent session changed to %q", a.ExternalSessionID())
	}
}

// An interrupt that lands before the prompt is dispatched settles the turn as
// cancelled, and must not carry over into the turn the user sends next.
func TestInterruptedTurnDoesNotLeakIntoTheNextPrompt(t *testing.T) {
	a := startMock(t, nil)
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	stop, err := a.Aside(context.Background(), "aside-after-interrupt", []PromptBlock{{Type: "text", Text: "DERISK_ASIDE"}})
	if err != nil || stop != "end_turn" {
		t.Fatalf("Aside() after an idle interrupt = %q, %v", stop, err)
	}
}

func TestInterruptCancelsActiveAsideFork(t *testing.T) {
	a := startMock(t, nil)
	done := make(chan error, 1)
	go func() {
		stop, err := a.Aside(context.Background(), "aside-cancel", []PromptBlock{{Type: "text", Text: "DERISK_FORK_CANCEL"}})
		if err == nil && stop != "cancelled" {
			err = fmt.Errorf("stop reason = %q", stop)
		}
		done <- err
	}()

	// Wait for the fork's turn to be in flight rather than for the local
	// "working" status, which the adapter emits before session/prompt is sent.
	waitEvent(t, a, "aside_event", func(event map[string]any) bool {
		inner, _ := event["event"].(map[string]any)
		return event["asideId"] == "aside-cancel" && inner["text"] == "Working on it."
	})
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not cancel the active aside fork")
	}
	got := waitEvent(t, a, "aside_event", func(event map[string]any) bool {
		inner, _ := event["event"].(map[string]any)
		return event["asideId"] == "aside-cancel" && inner["kind"] == "message_chunk"
	})
	inner := got["event"].(map[string]any)
	if inner["text"] != "Cancelled." {
		t.Fatalf("aside cancellation event = %#v", got)
	}
}

func TestPromptCancellationRestoresIdleStatus(t *testing.T) {
	a := startMock(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Prompt(ctx, []PromptBlock{{Type: "text", Text: "DERISK_CANCEL"}})
		done <- err
	}()

	waitEvent(t, a, "status", func(e map[string]any) bool { return e["status"] == "working" })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled prompt returned no error")
	}
	waitEvent(t, a, "status", func(e map[string]any) bool { return e["status"] == "idle" })
}

func TestLiveClientServicesRoundTripEscapeAndTerminalLifecycle(t *testing.T) {
	a, workspace, host, durable := startServiceMock(t)
	stop, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_SERVICES"}})
	if err != nil || stop != "end_turn" {
		t.Fatalf("services prompt = %q, %v", stop, err)
	}
	message := waitEvent(t, a, "message_chunk", func(e map[string]any) bool {
		text, _ := e["text"].(string)
		return strings.Contains(text, "SERVICES_DONE")
	})
	if !strings.Contains(message["text"].(string), "SERVICES_DONE") {
		t.Fatalf("services message = %#v", message)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "tandem-roundtrip.txt"))
	if err != nil || string(body) != "hello from the agent\nline two\n" {
		t.Fatalf("roundtrip body = %q, %v", body, err)
	}
	history, err := durable.FullHistory()
	if err != nil {
		t.Fatal(err)
	}
	var terminalID string
	for _, logged := range history {
		if logged.Event.Kind != "terminal_output" {
			continue
		}
		var payload struct {
			TermID string `json:"termId"`
		}
		if json.Unmarshal(logged.Event.Payload, &payload) == nil {
			terminalID = payload.TermID
			break
		}
	}
	if terminalID == "" {
		t.Fatal("terminal service produced no durable output event")
	}
	output, err := host.Output(terminalID)
	if err != nil || !strings.Contains(output.Output, "chunk-a") || output.ExitStatus == nil {
		t.Fatalf("released terminal output = %#v, %v", output, err)
	}

	outside := filepath.Join(filepath.Dir(workspace), "tandem-escape.txt")
	_ = os.Remove(outside)
	stop, err = a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_ESCAPE"}})
	if err != nil || stop != "end_turn" {
		t.Fatalf("escape prompt = %q, %v", stop, err)
	}
	escape := waitEvent(t, a, "message_chunk", func(e map[string]any) bool {
		text, _ := e["text"].(string)
		return strings.Contains(text, "ESCAPE_REJECTED")
	})
	if !strings.Contains(escape["text"].(string), "code=-32602") {
		t.Fatalf("escape response = %q", escape["text"])
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape touched outside file: %v", err)
	}
}

func TestInterruptCancelsPendingClientServiceOperation(t *testing.T) {
	a, _, _, _ := startServiceMock(t)
	done := make(chan error, 1)
	go func() {
		stop, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_SERVICE_CANCEL"}})
		if err == nil && stop != "cancelled" {
			err = fmt.Errorf("stop reason = %q", stop)
		}
		done <- err
	}()
	waitEvent(t, a, "tool_call", func(e map[string]any) bool { return e["title"] == "terminal: pending wait" })
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not settle prompt")
	}
	waitEvent(t, a, "message_chunk", func(e map[string]any) bool {
		text, _ := e["text"].(string)
		return strings.Contains(text, "SERVICE_WAIT_CANCELLED code=-32603")
	})
}

func TestServiceValidationAndSessionScoping(t *testing.T) {
	fsService, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fsService.Close()
	a := &Adapter{sessionID: "session-a", cfg: AdapterConfig{WorkspaceFS: fsService}}
	tests := []struct {
		method string
		params string
		code   int
	}{
		{"unknown/method", `{}`, methodNotFound},
		{"fs/read_text_file", `{`, invalidParams},
		{"fs/read_text_file", `{"sessionId":"session-b","path":"file"}`, invalidParams},
	}
	for _, test := range tests {
		_, err := a.dispatchService(context.Background(), test.method, json.RawMessage(test.params))
		if err == nil {
			t.Fatalf("%s unexpectedly succeeded", test.method)
		}
		code := internalError
		var coded rpcCodedError
		if errors.As(err, &coded) {
			code = coded.JSONRPCCode()
		}
		if code != test.code {
			t.Fatalf("%s code = %d (%v), want %d", test.method, code, err, test.code)
		}
	}
}

func TestImagePromptResolvesAssetInline(t *testing.T) {
	data := []byte("not decoded by the ACP boundary")
	digest := sha256.Sum256(data)
	id := hex.EncodeToString(digest[:])
	a := startMock(t, func(cfg *AdapterConfig) {
		cfg.Assets = fakeAssets{assets.Stored{AssetID: id, MIMEType: "image/png", Size: int64(len(data)), Data: data}}
	})
	stop, err := a.Prompt(context.Background(), []PromptBlock{
		{Type: "text", Text: "DERISK_IMAGE before"},
		{Type: "image", AssetID: id, MIMEType: "image/png"},
		{Type: "text", Text: "after"},
	})
	if err != nil || stop != "end_turn" {
		t.Fatalf("prompt = %q, %v", stop, err)
	}
	event := waitEvent(t, a, "message_chunk", func(e map[string]any) bool { return strings.Contains(e["text"].(string), "ACP_PROMPT_BLOCKS") })
	wantDigest := hex.EncodeToString(digest[:8])
	want := "text:DERISK_IMAGE before|image:image/png:" + stringInt(len(data)) + ":" + wantDigest + "|text:after"
	if !strings.Contains(event["text"].(string), want) {
		t.Fatalf("image echo %q does not contain %q", event["text"], want)
	}
}

func stringInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestInterruptCorrelatesPermissionAndCancelsLiveTools(t *testing.T) {
	a := startMock(t, nil)
	done := make(chan error, 1)
	go func() {
		stop, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_CANCEL"}})
		if err == nil && stop != "cancelled" {
			err = errors.New("unexpected stop reason: " + stop)
		}
		done <- err
	}()
	waitEvent(t, a, "permission_request", nil)
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, a, "tool_call_update", func(e map[string]any) bool { return e["status"] == "cancelled" })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled prompt did not finish")
	}
}

func TestLoadSessionCapturesIDAndSuppressesReplay(t *testing.T) {
	a := startMock(t, func(cfg *AdapterConfig) { cfg.ResumeSessionID = "sess_external" })
	if a.ExternalSessionID() != "sess_external" {
		t.Fatalf("loaded session = %q", a.ExternalSessionID())
	}
	if err := a.LoadSession(context.Background(), "sess_second", false); err != nil {
		t.Fatal(err)
	}
	if a.ExternalSessionID() != "sess_second" {
		t.Fatalf("reloaded session = %q", a.ExternalSessionID())
	}
}

// Regression: on resume the agent re-streams its whole history as session/update
// notifications before answering session/load. The transport enqueues all of
// them before it delivers the response that unblocks StartAdapter, so clearing
// the replay gate on the load-caller goroutine used to race readLoop draining
// the tail of that queue — re-logging the agent's last message(s). endReplay now
// routes the gate drop through readLoop, so the whole replayed burst is
// suppressed no matter the scheduling.
func TestResumeReplayBurstIsFullySuppressed(t *testing.T) {
	a := startMock(t, func(cfg *AdapterConfig) { cfg.ResumeSessionID = "sess_replay" })
	// StartAdapter has returned, so endReplay has flushed the replay queue under
	// the gate. Any leaked replay chunk is already on Events(); give a brief
	// settle for stragglers, then assert none escaped.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-a.Events():
			if ev.Kind == "message_chunk" && bytes.Contains(ev.Payload, []byte("replayed-chunk")) {
				t.Fatalf("replayed chunk leaked into the log: %s", ev.Payload)
			}
		case <-deadline:
			return
		}
	}
}

func TestUnknownUpdateIsDiagnosticButMalformedRequiredUpdateFails(t *testing.T) {
	t.Run("unknown optional", func(t *testing.T) {
		a := startMock(t, nil)
		stop, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_UNKNOWN_UPDATE"}})
		if err != nil || stop != "end_turn" {
			t.Fatalf("prompt = %q, %v", stop, err)
		}
		select {
		case err := <-a.Errors():
			var optional *OptionalUpdateError
			if !errors.As(err, &optional) || optional.Variant != "mock_future_optional_update" {
				t.Fatalf("diagnostic = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("unknown update was not logged")
		}
	})

	t.Run("malformed required", func(t *testing.T) {
		a := startMock(t, nil)
		_, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_MALFORMED_UPDATE"}})
		if err == nil {
			t.Fatal("malformed required update did not fail prompt")
		}
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case diagnostic := <-a.Errors():
				if strings.Contains(diagnostic.Error(), "toolCallId is required") {
					return
				}
			case <-deadline.C:
				t.Fatal("malformed required update was not diagnosed")
			}
		}
	})
}

func TestImageCapabilityAndMIMEAreEnforcedBeforeRPC(t *testing.T) {
	data := []byte("x")
	id := strings.Repeat("a", 64)
	a := startMock(t, func(cfg *AdapterConfig) {
		cfg.Transport.Env = append(os.Environ(), "TANDEM_MOCK_IMAGE_CAPABILITY=false")
		cfg.Assets = fakeAssets{assets.Stored{AssetID: id, MIMEType: "image/png", Size: 1, Data: data}}
	})
	_, err := a.Prompt(context.Background(), []PromptBlock{{Type: "image", AssetID: id, MIMEType: "image/png"}})
	if err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("capability error = %v", err)
	}
}

func TestEventPayloadsRemainNormalizedJSON(t *testing.T) {
	a := startMock(t, nil)
	event := waitEvent(t, a, "prompt_capabilities", nil)
	b, _ := json.Marshal(event)
	if !bytes.Contains(b, []byte(`"kind":"prompt_capabilities"`)) || bytes.Contains(b, []byte("jsonrpc")) {
		t.Fatalf("event leaked wire protocol: %s", b)
	}
}

func TestCloseWhilePromptIsInFlightDoesNotRaceEventShutdown(t *testing.T) {
	a := startMock(t, nil)
	done := make(chan error, 1)
	go func() {
		_, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_CANCEL"}})
		done <- err
	}()
	waitEvent(t, a, "permission_request", nil)
	_ = a.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closing an active prompt unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active prompt did not settle on close")
	}
}

func TestPromptsAreSerializedAndWaitingHonorsContext(t *testing.T) {
	a := startMock(t, nil)
	first := make(chan error, 1)
	go func() {
		_, err := a.Prompt(context.Background(), []PromptBlock{{Type: "text", Text: "DERISK_CANCEL"}})
		first <- err
	}()
	waitEvent(t, a, "permission_request", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := a.Prompt(ctx, []PromptBlock{{Type: "text", Text: "must not overlap"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued prompt error = %v", err)
	}
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first prompt did not settle")
	}
}

func TestConfigOptionsFlattenGroups(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`{"id":"model","name":"Model","type":"select","currentValue":"a","options":[{"group":"fast","name":"Fast","options":[{"value":"a","name":"A"}]},{"value":"b","name":"B"}]}`)}
	normalized := normalizeConfigOptions(raw)
	if len(normalized) != 1 {
		t.Fatalf("normalized options = %s", normalized)
	}
	var got struct {
		Type    string           `json:"type"`
		Options []map[string]any `json:"options"`
	}
	if err := json.Unmarshal(normalized[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "select" || len(got.Options) != 2 || got.Options[0]["value"] != "a" || got.Options[1]["value"] != "b" {
		t.Fatalf("normalized option = %s", normalized[0])
	}
}

// newUpdateAdapter builds an Adapter wired just enough to exercise handleUpdate
// directly (no subprocess): an event sink, a live context, and the maps the
// tool_call path writes into. parentPath mirrors what StartAdapter derives from
// AdapterConfig.ParentToolCallIDPath.
func newUpdateAdapter(parentPath []string) *Adapter {
	return &Adapter{
		cfg:           AdapterConfig{SessionID: "api-1"},
		ctx:           context.Background(),
		events:        make(chan eventlog.Event, 16),
		liveTools:     map[string]struct{}{},
		toolFiles:     map[string]string{},
		toolTerminals: map[string]string{},
		parentPath:    parentPath,
	}
}

func TestCompletedTerminalToolSnapshotsOutput(t *testing.T) {
	host, err := terminalhost.New(terminalhost.Options{DefaultCwd: t.TempDir(), EventLog: discardEvents{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	id, err := host.Create(context.Background(), terminalhost.CreateOptions{
		Command: "sh", Args: []string{"-c", "printf 'branch-name\\n'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.WaitForExit(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	a := newUpdateAdapter(nil)
	a.cfg.Terminals = host
	start := fmt.Sprintf(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"exec-1",`+
		`"title":"git branch --show-current","kind":"execute","status":"in_progress","content":[{"type":"terminal","terminalId":%q}]}}`, id)
	if err := a.handleUpdate(json.RawMessage(start)); err != nil {
		t.Fatal(err)
	}
	started := waitEvent(t, a, "tool_call", nil)
	if started["terminalId"] != id {
		t.Fatalf("tool call terminalId = %v, want %q", started["terminalId"], id)
	}

	done := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"exec-1","status":"completed"}}`
	if err := a.handleUpdate(json.RawMessage(done)); err != nil {
		t.Fatal(err)
	}
	got := waitEvent(t, a, "tool_call_update", nil)
	encoded, _ := json.Marshal(got["content"])
	if !strings.Contains(string(encoded), `branch-name`) {
		t.Fatalf("completed terminal content = %s", encoded)
	}
}

func TestCodexTerminalOutputMetaBecomesDurableTerminalEvents(t *testing.T) {
	a := newUpdateAdapter(nil)
	start := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"exec-1",` +
		`"title":"go test ./...","kind":"execute","status":"in_progress",` +
		`"content":[{"type":"terminal","terminalId":"exec-1"}]}}`
	if err := a.handleUpdate(json.RawMessage(start)); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, a, "tool_call", nil)

	for _, tc := range []struct {
		name, field, data string
	}{
		{name: "delta", field: "terminal_output_delta", data: "first line\n"},
		{name: "snapshot spelling", field: "terminal_output", data: "second line\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := fmt.Sprintf(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update",`+
				`"toolCallId":"exec-1","_meta":{%q:{"data":%q,"terminal_id":"exec-1"}}}}`, tc.field, tc.data)
			if err := a.handleUpdate(json.RawMessage(update)); err != nil {
				t.Fatal(err)
			}
			got := waitEvent(t, a, "terminal_output", nil)
			if got["termId"] != "exec-1" || got["chunk"] != tc.data || got["truncated"] != false {
				t.Fatalf("terminal output = %#v", got)
			}
			waitEvent(t, a, "tool_call_update", nil)
		})
	}

	done := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"exec-1","status":"completed"}}`
	if err := a.handleUpdate(json.RawMessage(done)); err != nil {
		t.Fatal(err)
	}
	if got := waitEvent(t, a, "tool_call_update", nil); got["status"] != "done" {
		t.Fatalf("completed update = %#v", got)
	}
}

func TestParentToolCallMetaAnnotatesSubagentUpdates(t *testing.T) {
	// A subagent tool call carries the spawning Task call's id under the
	// configured _meta path; it should surface as a normalized parentId.
	a := newUpdateAdapter([]string{"claudeCode", "parentToolUseId"})
	update := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"child-1",` +
		`"title":"Bash","status":"in_progress","_meta":{"claudeCode":{"parentToolUseId":"task-parent"}}}}`
	if err := a.handleUpdate(json.RawMessage(update)); err != nil {
		t.Fatal(err)
	}
	got := waitEvent(t, a, "tool_call", func(m map[string]any) bool { return m["id"] == "child-1" })
	if got["parentId"] != "task-parent" {
		t.Fatalf("parentId = %v, want task-parent (event: %v)", got["parentId"], got)
	}

	// A message chunk from the same subagent is attributed too, so the UI can
	// group its narration under the spawn alongside its tool calls.
	msg := `{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk",` +
		`"content":{"type":"text","text":"hi"},"_meta":{"claudeCode":{"parentToolUseId":"task-parent"}}}}`
	if err := a.handleUpdate(json.RawMessage(msg)); err != nil {
		t.Fatal(err)
	}
	got = waitEvent(t, a, "message_chunk", nil)
	if got["parentId"] != "task-parent" {
		t.Fatalf("message chunk parentId = %v, want task-parent", got["parentId"])
	}
}

func TestParentToolCallMetaAbsentLeavesToolCallFlat(t *testing.T) {
	// Unconfigured path, and a configured path that doesn't resolve, both leave
	// the event unannotated (today's flat rendering) rather than erroring.
	cases := []struct {
		name       string
		parentPath []string
		meta       string
	}{
		{"unconfigured", nil, `,"_meta":{"claudeCode":{"parentToolUseId":"task-parent"}}`},
		{"missing-segment", []string{"claudeCode", "parentToolUseId"}, `,"_meta":{"claudeCode":{}}`},
		{"no-meta", []string{"claudeCode", "parentToolUseId"}, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newUpdateAdapter(tc.parentPath)
			update := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"top-1",` +
				`"title":"Bash","status":"in_progress"` + tc.meta + `}}`
			if err := a.handleUpdate(json.RawMessage(update)); err != nil {
				t.Fatal(err)
			}
			got := waitEvent(t, a, "tool_call", func(m map[string]any) bool { return m["id"] == "top-1" })
			if _, ok := got["parentId"]; ok {
				t.Fatalf("unexpected parentId on flat tool call: %v", got)
			}
		})
	}
}

func TestMCPServerMarshalJSONKeepsRequiredArrays(t *testing.T) {
	http, err := json.Marshal(MCPServer{Name: "mem0", Type: "http", URL: "http://localhost:8081/mcp"})
	if err != nil {
		t.Fatalf("marshal http server: %v", err)
	}
	if got, want := string(http), `{"name":"mem0","type":"http","url":"http://localhost:8081/mcp","headers":[]}`; got != want {
		t.Fatalf("http server JSON = %s, want %s", got, want)
	}
	stdio, err := json.Marshal(MCPServer{Name: "tandem-control", Command: "/usr/bin/tandem"})
	if err != nil {
		t.Fatalf("marshal stdio server: %v", err)
	}
	if got, want := string(stdio), `{"name":"tandem-control","command":"/usr/bin/tandem","args":[],"env":[]}`; got != want {
		t.Fatalf("stdio server JSON = %s, want %s", got, want)
	}
}
