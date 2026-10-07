package secretbroker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileRoundTripRedactsAndMaterializesSecret(t *testing.T) {
	root := t.TempDir()
	b := New(Options{
		AgentExists: func(id string) bool { return id == "a1" },
		Workspace:   func(id string) string { return root },
	})
	requestID, err := b.Register(Request{SessionID: "a1", Service: "demo", Usage: "file", Path: ".env"})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("never-in-model-context")
	if err := b.Resolve(requestID, secret, false); err != nil {
		t.Fatal(err)
	}
	_, _, grantID, err := b.Status("a1", requestID)
	if err != nil {
		t.Fatal(err)
	}
	read, err := b.FileRead(FileReadRequest{SessionID: "a1", Path: ".env"})
	if err != nil || read.Revision != "missing" {
		t.Fatalf("initial read = %#v, %v", read, err)
	}
	marker := "{{TANDEM_SECRET:" + grantID + "}}"
	if err := b.FileWrite(FileWriteRequest{SessionID: "a1", Path: ".env", Revision: read.Revision, Content: "API_KEY=" + marker + "\nMODE=dev\n"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "API_KEY=never-in-model-context\nMODE=dev\n" {
		t.Fatalf("materialized file = %q", raw)
	}
	info, _ := os.Stat(filepath.Join(root, ".env"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	redacted, err := b.FileRead(FileReadRequest{SessionID: "a1", Path: ".env"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(redacted.Content, "never-in-model-context") || !strings.Contains(redacted.Content, marker) {
		t.Fatalf("redacted content = %q", redacted.Content)
	}
	if err := b.FileWrite(FileWriteRequest{SessionID: "a1", Path: ".env", Revision: redacted.Revision, Content: redacted.Content + "EXTRA=1\n"}); err != nil {
		t.Fatal(err)
	}
}

func TestFileWriteRejectsTraversalUnknownMarkerAndStaleRevision(t *testing.T) {
	root := t.TempDir()
	b := New(Options{AgentExists: func(string) bool { return true }, Workspace: func(string) string { return root }})
	if _, err := b.Register(Request{SessionID: "a1", Service: "demo", Usage: "file", Path: "../outside"}); err == nil {
		t.Fatal("traversal request succeeded")
	}
	bad := FileWriteRequest{SessionID: "a1", Path: ".env", Revision: "missing", Content: "X={{TANDEM_SECRET:sg_00000000000000000000000000000000}}"}
	if err := b.FileWrite(bad); err == nil {
		t.Fatal("unknown marker write succeeded")
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.FileWrite(FileWriteRequest{SessionID: "a1", Path: ".env", Revision: "missing", Content: "new"}); err == nil {
		t.Fatal("stale write succeeded")
	}
}

func TestRegisterRejectsUnsafeOrigins(t *testing.T) {
	b := New(Options{AgentExists: func(id string) bool { return id == "a1" }})
	for _, origin := range []string{"http://api.example.com", "https://localhost", "https://127.0.0.1", "https://api.example.com/path", "https://user:pass@example.com"} {
		if _, err := b.Register(Request{SessionID: "a1", Service: "demo", Origin: origin}); err == nil {
			t.Errorf("Register(%q) succeeded", origin)
		}
	}
}

func TestResolveAndProxyNeverReturnSecret(t *testing.T) {
	const secret = "top-secret-value"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("X-Request-Id", secret)
		_, _ = io.WriteString(w, `{"echo":"`+secret+`"}`)
	}))
	defer upstream.Close()
	now := time.Now()
	b := New(Options{HTTPClient: upstream.Client(), Now: func() time.Time { return now }})
	b.requests["sr_test"] = &requestState{Request: Request{RequestID: "sr_test", SessionID: "a1", Origin: upstream.URL, HeaderName: "Authorization", Prefix: "Bearer "}}
	material := []byte(secret)
	if err := b.Resolve("sr_test", material, false); err != nil {
		t.Fatal(err)
	}
	_, _, grantID, err := b.Status("a1", "sr_test")
	if err != nil || grantID == "" {
		t.Fatalf("Status = %q, %v", grantID, err)
	}
	resp, err := b.Proxy(context.Background(), ProxyRequest{SessionID: "a1", GrantID: grantID, URL: upstream.URL + "/v1/test"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(resp)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("proxy response disclosed secret: %s", encoded)
	}
	if !strings.Contains(resp.Body, "[REDACTED SECRET]") {
		t.Fatalf("proxy response was not redacted: %s", resp.Body)
	}
}

func TestResolveEndpointDoesNotEchoSecret(t *testing.T) {
	b := New(Options{Token: "token"})
	b.requests["sr_test"] = &requestState{Request: Request{RequestID: "sr_test", SessionID: "a1", Origin: "https://api.example.com", HeaderName: "Authorization"}}
	req := httptest.NewRequest(http.MethodPost, "/internal/secrets/resolve", strings.NewReader(`{"requestId":"sr_test","secret":"never-echo-me"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "never-echo-me") {
		t.Fatalf("resolve response disclosed secret: %s", w.Body.String())
	}
}
