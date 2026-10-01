package messagesmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runMCP(t *testing.T, cfg Config, requests ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n"), &out, cfg); err != nil {
		t.Fatal(err)
	}
	var replies []map[string]any
	dec := json.NewDecoder(&out)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, m)
	}
	return replies
}

func TestToolsListDeclaresEverySpecifiedTool(t *testing.T) {
	replies := runMCP(t, Config{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, r := range replies {
		if r["id"] != float64(2) {
			continue
		}
		for _, tool := range r["result"].(map[string]any)["tools"].([]any) {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
	}
	want := []string{ToolDirectory, ToolSend, ToolAsk, ToolReply, ToolDecline, ToolRequestLink, ToolSetCard}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestToolCallPostsToDaemonWithSessionAndToken(t *testing.T) {
	type seen struct {
		path, auth string
		body       map[string]any
	}
	got := make(chan seen, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- seen{r.URL.Path, r.Header.Get("Authorization"), body}
		if body["to"] == "nobody" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"no_link: no link"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","status":"started"}`)
	}))
	defer srv.Close()
	cfg := Config{ControlURL: srv.URL, Token: "tok", SessionID: "sess-1"}
	replies := runMCP(t, cfg,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"messages_ask","arguments":{"to":"@bob","body":"hi","timeoutMinutes":5}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"messages_send","arguments":{"to":"nobody","body":"hi"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"messages_bogus","arguments":{}}}`)
	// Tool calls are served concurrently, so requests may arrive in any order.
	var first seen
	for i := 0; i < 2; i++ {
		if r := <-got; r.path == "/internal/messages/ask" {
			first = r
		}
	}
	if first.path != "/internal/messages/ask" || first.auth != "Bearer tok" || first.body["sessionId"] != "sess-1" || first.body["to"] != "@bob" || first.body["timeoutMinutes"] != float64(5) {
		t.Fatalf("request = %+v", first)
	}
	byID := map[float64]map[string]any{}
	for _, r := range replies {
		byID[r["id"].(float64)] = r
	}
	if res := byID[1]["result"].(map[string]any); res["isError"] != nil || res["structuredContent"].(map[string]any)["status"] != "started" {
		t.Fatalf("ask result = %+v", res)
	}
	denied := byID[2]["result"].(map[string]any)
	if denied["isError"] != true || !strings.Contains(denied["content"].([]any)[0].(map[string]any)["text"].(string), "no_link") {
		t.Fatalf("denied result = %+v", denied)
	}
	if byID[3]["error"] == nil {
		t.Fatalf("unknown tool = %+v", byID[3])
	}
}
