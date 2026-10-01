package messaging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/messagesmcp"
)

func postTool(t *testing.T, h http.Handler, token, tool string, body map[string]any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, Path+tool, strings.NewReader(string(data)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestHTTPBridgeToolsEnforceAuthAndLinks(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.svc.opts.LinkRequestWait = 30 * time.Millisecond
	h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")

	if code, _ := postTool(t, h.svc, "wrong", "send", map[string]any{"sessionId": "alice", "to": "bob", "body": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d", code)
	}
	if code, _ := postTool(t, h.svc, "tok", "bogus", map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("unknown tool status = %d", code)
	}
	code, out := postTool(t, h.svc, "tok", "send", map[string]any{"sessionId": "alice", "to": "bob", "body": "x"})
	if code != http.StatusBadRequest || !strings.HasPrefix(out["error"].(string), "no_link") {
		t.Fatalf("send without link = %d %v", code, out)
	}
	h.link(h.addr("alice"), "bob")
	code, out = postTool(t, h.svc, "tok", "ask", map[string]any{"sessionId": "alice", "to": "@bob", "body": "q?", "timeoutMinutes": 2})
	if code != http.StatusOK || out["requestId"] == nil || out["status"] != "started" {
		t.Fatalf("ask = %d %v", code, out)
	}
	eventually(t, "bob prompt", func() bool { return len(bobAd.promptTexts()) == 1 })
	code, out = postTool(t, h.svc, "tok", "reply", map[string]any{"sessionId": "bob", "requestId": out["requestId"], "body": "a"})
	if code != http.StatusOK || out["id"] == nil {
		t.Fatalf("reply = %d %v", code, out)
	}
	code, out = postTool(t, h.svc, "tok", "directory", map[string]any{"sessionId": "alice", "query": "bob"})
	if agents, _ := out["agents"].([]any); code != http.StatusOK || len(agents) != 1 {
		t.Fatalf("directory = %d %v", code, out)
	}
	if code, _ = postTool(t, h.svc, "tok", "set_card", map[string]any{"sessionId": "alice", "card": "reviews PRs"}); code != http.StatusOK {
		t.Fatalf("set_card = %d", code)
	}
	code, out = postTool(t, h.svc, "tok", "directory", map[string]any{"sessionId": "bob"})
	if agents, _ := out["agents"].([]any); len(agents) != 1 || agents[0].(map[string]any)["card"] != "reviews PRs" {
		t.Fatalf("directory after card = %d %v", code, out)
	}
	code, out = postTool(t, h.svc, "tok", "request_link", map[string]any{"sessionId": "bob", "to": "alice", "reason": "just because"})
	// Nobody decides within the (shortened) wait: still pending, with a note.
	if code != http.StatusOK || out["status"] != "pending" || out["note"] == nil {
		t.Fatalf("request_link = %d %v", code, out)
	}
	if code, _ := postTool(t, h.svc, "tok", "decline", map[string]any{"sessionId": "alice", "requestId": "req_nope", "reason": "x"}); code != http.StatusBadRequest {
		t.Fatalf("decline unknown = %d", code)
	}
}

// The real MCP bridge talking to the real HTTP handler.
func TestMCPBridgeEndToEnd(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	srv := httptest.NewServer(h.svc)
	defer srv.Close()
	var out strings.Builder
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"messages_send","arguments":{"to":"@bob","body":"through the bridge"}}}` + "\n"
	if err := messagesmcp.Run(context.Background(), strings.NewReader(in), &out, messagesmcp.Config{ControlURL: srv.URL, Token: "tok", SessionID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("bridge reported an error: %s", out.String())
	}
	eventually(t, "bob prompt", func() bool { return len(bobAd.promptTexts()) == 1 })
	if p := bobAd.promptTexts()[0]; !strings.Contains(p, "through the bridge") || strings.Contains(p, "messages_reply") {
		t.Fatalf("prompt = %s", p)
	}
}

// messages_request_link blocks in the HTTP handler until the human decides.
func TestHTTPRequestLinkBlocksUntilDecision(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	bob, _ := h.addSession("bob", "bob")
	h.svc.Start(context.Background())
	t.Cleanup(h.svc.Close)
	type reply struct {
		code int
		out  map[string]any
	}
	done := make(chan reply, 1)
	go func() {
		code, out := postTool(t, h.svc, "tok", "request_link", map[string]any{"sessionId": "alice", "to": "bob", "reason": "pls"})
		done <- reply{code, out}
	}()
	eventually(t, "approval on bob", func() bool { return len(bob.PendingApprovals()) == 1 })
	select {
	case r := <-done:
		t.Fatalf("returned before the decision: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if err := bob.RespondPermission(bob.PendingApprovals()[0].ReqID, "allow"); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.code != http.StatusOK || r.out["status"] != "approved" {
		t.Fatalf("request_link = %+v", r)
	}
}
