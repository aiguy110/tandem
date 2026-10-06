package secretbroker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
