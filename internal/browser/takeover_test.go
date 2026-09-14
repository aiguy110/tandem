package browser

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTakeoverEndpointAuthenticatesAndResolvesOnRelease(t *testing.T) {
	requested := false
	resolved := false
	h := NewTakeovers(TakeoverOptions{Token: "secret", AgentExists: func(id string) bool { return id == "one" }, OnRequest: func(sessionID, reqID, reason string) {
		requested = sessionID == "one" && reqID == "tk_1" && reason == "login"
	}, OnResolved: func(sessionID, reqID string) {
		resolved = sessionID == "one" && reqID == "tk_1"
	}})
	post := httptest.NewRequest(http.MethodPost, "/internal/browser/takeover?agentId=one", bytes.NewBufferString(`{"reason":"login"}`))
	post.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, post)
	if w.Code != http.StatusOK || !requested {
		t.Fatalf("post status=%d requested=%v", w.Code, requested)
	}
	var registered struct {
		ReqID string `json:"reqId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &registered)
	status := func() bool {
		r := httptest.NewRequest(http.MethodGet, "/internal/browser/takeover?reqId="+registered.ReqID, nil)
		r.Header.Set("Authorization", "Bearer secret")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		var v struct {
			Resolved bool `json:"resolved"`
		}
		_ = json.Unmarshal(rw.Body.Bytes(), &v)
		return v.Resolved
	}
	if status() {
		t.Fatal("resolved before release")
	}
	h.Release("one")
	if !status() || !resolved {
		t.Fatalf("release status=%v callback=%v", status(), resolved)
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/internal/browser/takeover?reqId=tk_1", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauth.Code)
	}
}

func TestBrokerReleaseResolvesTakeovers(t *testing.T) {
	h := NewTakeovers(TakeoverOptions{Token: "secret", AgentExists: func(string) bool { return true }})
	b := NewBroker(&fakeDriver{provisions: map[string]int{}, teardowns: map[string]int{}}, BrokerConfig{OnRelease: h.Release})
	post := httptest.NewRequest(http.MethodPost, "/internal/browser/takeover?agentId=one&token=secret", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, post)
	if err := b.Release("one"); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, "/internal/browser/takeover?reqId=tk_1&token=secret", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, get)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"resolved":true`)) {
		t.Fatalf("status = %s", w.Body.String())
	}
}
