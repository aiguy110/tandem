package browser

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTakeoverGoesStaleWhenCallerStopsPolling(t *testing.T) {
	now := time.Unix(1000, 0)
	h := NewTakeovers(TakeoverOptions{Token: "secret", AgentExists: func(string) bool { return true }, Now: func() time.Time { return now }})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/internal/browser/takeover?agentId=one&token=secret", bytes.NewBufferString(`{}`)))
	var registered struct {
		ReqID string `json:"reqId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &registered)
	poll := func() string {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/internal/browser/takeover?reqId="+registered.ReqID+"&token=secret", nil))
		return rw.Body.String()
	}
	if !h.Live(registered.ReqID) || !h.HasLive("one") {
		t.Fatal("fresh takeover is not live")
	}
	now = now.Add(15 * time.Second)
	poll()
	now = now.Add(15 * time.Second)
	if !h.Live(registered.ReqID) {
		t.Fatal("polled takeover went stale")
	}
	now = now.Add(time.Minute)
	if h.Live(registered.ReqID) || h.HasLive("one") {
		t.Fatal("unpolled takeover is still live")
	}
	h.Abandon(registered.ReqID)
	if !strings.Contains(poll(), `"resolved":true`) {
		t.Fatal("abandoned takeover does not report resolved")
	}
	if h.Live("tk_unknown") {
		t.Fatal("unknown takeover is live")
	}
}

func TestTakeoverEndpointAuthenticatesAndResolvesOnRelease(t *testing.T) {
	requested := false
	resolved := false
	requestedID := ""
	h := NewTakeovers(TakeoverOptions{Token: "secret", AgentExists: func(id string) bool { return id == "one" }, OnRequest: func(sessionID, reqID, reason string) {
		requested = sessionID == "one" && strings.HasPrefix(reqID, "tk_") && reason == "login"
		requestedID = reqID
	}, OnResolved: func(sessionID, reqID string) {
		resolved = sessionID == "one" && reqID == requestedID
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
	if registered.ReqID != requestedID {
		t.Fatalf("reqId = %q, callback saw %q", registered.ReqID, requestedID)
	}
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
	var registered struct {
		ReqID string `json:"reqId"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &registered)
	if err := b.Release("one"); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, "/internal/browser/takeover?reqId="+registered.ReqID+"&token=secret", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, get)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"resolved":true`)) {
		t.Fatalf("status = %s", w.Body.String())
	}
}
