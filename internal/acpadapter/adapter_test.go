package acpadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
)

type fakeAssets struct{ stored assets.Stored }

func (f fakeAssets) Get(agentID, assetID string) (assets.Stored, error) {
	if agentID != "api-1" || assetID != f.stored.AssetID {
		return assets.Stored{}, assets.ErrNotFound
	}
	return f.stored, nil
}

func mockPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate mock")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "daemon", "src", "mock-acp-agent.mjs"))
}

func startMock(t *testing.T, mutate func(*AdapterConfig)) *Adapter {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the shared ACP mock")
	}
	cfg := AdapterConfig{
		AgentID: "api-1",
		Cwd:     t.TempDir(),
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
	if a.SessionID() != "sess_mock" {
		t.Fatalf("session ID = %q", a.SessionID())
	}
	if caps := a.Capabilities(); !caps.Structured || !caps.LoadSession || !caps.Image {
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
	if a.SessionID() != "sess_external" {
		t.Fatalf("loaded session = %q", a.SessionID())
	}
	if err := a.LoadSession(context.Background(), "sess_second", false); err != nil {
		t.Fatal(err)
	}
	if a.SessionID() != "sess_second" {
		t.Fatalf("reloaded session = %q", a.SessionID())
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
