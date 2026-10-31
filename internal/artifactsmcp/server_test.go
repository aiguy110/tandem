package artifactsmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runMCP(t *testing.T, cfg Config, requests ...string) map[float64]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n"), &out, cfg); err != nil {
		t.Fatal(err)
	}
	replies := map[float64]map[string]any{}
	dec := json.NewDecoder(&out)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		id, _ := m["id"].(float64)
		replies[id] = m
	}
	return replies
}

func call(id int, name string, args map[string]any) string {
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return string(data)
}

func TestToolsListAndDocs(t *testing.T) {
	replies := runMCP(t, Config{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, call(2, ToolDocs, nil))
	var names []string
	for _, tool := range replies[1]["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != ToolPublish+","+ToolDocs {
		t.Fatalf("tools=%v", names)
	}
	text := replies[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "sandboxed iframe") {
		t.Fatalf("docs=%q", text)
	}
}

func TestPublishSendsInlineAndFileContent(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/artifacts/publish" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "artifact", "artifactId": "abc", "title": body["title"]})
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.html"), []byte("<h1>file</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ControlURL: srv.URL, Token: "tok", SessionID: "s-1", WorkspaceCWD: dir}
	// Sequential runs keep the request order deterministic.
	r1 := runMCP(t, cfg, call(1, ToolPublish, map[string]any{"html": "<p>inline</p>", "title": "T"}))
	r2 := runMCP(t, cfg, call(2, ToolPublish, map[string]any{"path": "r.html"}))
	for _, r := range []map[string]any{r1[1], r2[2]} {
		result := r["result"].(map[string]any)
		if result["isError"] == true {
			t.Fatalf("result=%v", result)
		}
		if sc := result["structuredContent"].(map[string]any); sc["artifactId"] != "abc" || sc["status"] != "published" || sc["kind"] != nil {
			t.Fatalf("structured=%v", sc)
		}
	}
	if len(bodies) != 2 || bodies[0]["content"] != "<p>inline</p>" || bodies[0]["sessionId"] != "s-1" || bodies[0]["sourcePath"] != "" {
		t.Fatalf("inline body=%v", bodies)
	}
	if bodies[1]["content"] != "<h1>file</h1>" || bodies[1]["sourcePath"] != filepath.Join(dir, "r.html") {
		t.Fatalf("file body=%v", bodies[1])
	}
}

func TestPublishArgumentErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ControlURL: "http://127.0.0.1:1", WorkspaceCWD: dir}
	replies := runMCP(t, cfg,
		call(1, ToolPublish, map[string]any{}),
		call(2, ToolPublish, map[string]any{"html": "x", "path": "y"}),
		call(3, ToolPublish, map[string]any{"path": "missing.html"}),
		call(4, ToolPublish, map[string]any{"path": "."}),
	)
	for id := 1.0; id <= 4; id++ {
		if replies[id]["result"].(map[string]any)["isError"] != true {
			t.Fatalf("call %v: %v", id, replies[id])
		}
	}
}
