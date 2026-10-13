package browsermcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/toolregistry"
)

type fakeChild struct {
	browserID string
	mu        sync.Mutex
	calls     []string
}

func (c *fakeChild) ToolDeclarations() []toolregistry.Declaration {
	return []toolregistry.Declaration{{Name: "browser_click", Description: "Click", InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"}},"required":["ref"]}`)}}
}
func (c *fakeChild) CallTool(_ context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	c.calls = append(c.calls, name+" "+string(arguments))
	c.mu.Unlock()
	return json.RawMessage(`{"content":[{"type":"text","text":"- button \"Go\" [ref=e7] in ` + c.browserID + `"}]}`), nil
}
func (c *fakeChild) Availability(context.Context) error { return nil }
func (c *fakeChild) Close() error                       { return nil }

func TestRouterAddsBrowserIDAndScopesRefs(t *testing.T) {
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": []browser.Instance{{ID: "default"}, {ID: "b2"}}})
	}))
	defer control.Close()
	children := map[string]*fakeChild{}
	var mu sync.Mutex
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		_ = Run(context.Background(), inR, outW, Config{
			ControlURL: control.URL, Token: "t", SessionID: "s", CDPBase: "http://cdp/",
			StartChild: func(_ context.Context, browserID, endpoint string) (Child, error) {
				if want := "http://cdp/" + browser.InstanceKey("s", browserID); endpoint != want {
					t.Errorf("endpoint = %q, want %q", endpoint, want)
				}
				mu.Lock()
				defer mu.Unlock()
				child := &fakeChild{browserID: browserID}
				children[browserID] = child
				return child, nil
			},
		})
	}()
	defer inW.Close()
	responses := bufio.NewScanner(outR)
	call := func(id int, method string, params any) map[string]any {
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		_, _ = inW.Write(append(encoded, '\n'))
		done := make(chan map[string]any, 1)
		go func() {
			responses.Scan()
			var response map[string]any
			_ = json.Unmarshal(responses.Bytes(), &response)
			done <- response
		}()
		select {
		case response := <-done:
			return response
		case <-time.After(5 * time.Second):
			t.Fatal("no MCP response")
			return nil
		}
	}
	text := func(response map[string]any) string {
		result, _ := response["result"].(map[string]any)
		content, _ := result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}

	listed := call(1, "tools/list", map[string]any{})
	encoded, _ := json.Marshal(listed)
	if !strings.Contains(string(encoded), `"required":["browserId","ref"]`) || !strings.Contains(string(encoded), RequestInstanceTool) {
		t.Fatalf("tools/list = %s", encoded)
	}
	if got := text(call(2, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"ref": "e7"}})); !strings.Contains(got, "browserId is required") {
		t.Fatalf("missing browserId = %q", got)
	}
	if got := text(call(3, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"browserId": "b2", "ref": "e7"}})); !strings.Contains(got, "not from browser") {
		t.Fatalf("bare ref against b2 = %q", got)
	}
	if got := text(call(4, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"browserId": "b2", "ref": "b2:e7"}})); !strings.Contains(got, "[ref=b2:e7] in b2") {
		t.Fatalf("b2 click = %q", got)
	}
	if got := text(call(5, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"browserId": "default", "ref": "b2:e7"}})); !strings.Contains(got, "belongs to browser") {
		t.Fatalf("cross-browser ref = %q", got)
	}
	if got := text(call(7, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"browserId": "b2", "target": "button:has-text('Go')"}})); !strings.Contains(got, "in b2") {
		t.Fatalf("selector target = %q", got)
	}
	if got := text(call(8, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"browserId": "b2", "target": "default:f1e3"}})); !strings.Contains(got, "belongs to browser") {
		t.Fatalf("cross-browser target = %q", got)
	}
	if got := text(call(6, "tools/call", map[string]any{"name": "browser_click", "arguments": map[string]any{"browserId": "b9", "ref": "e7"}})); !strings.Contains(got, "no browser") {
		t.Fatalf("unknown browser = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls := children["b2"].calls; len(calls) != 2 || calls[0] != `browser_click {"ref":"e7"}` {
		t.Fatalf("b2 child calls = %#v", calls)
	}
}
