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

// blockingChild blocks each call until release is closed or the child is
// closed, like a Playwright MCP stuck behind a page that never responds.
type blockingChild struct {
	mu      sync.Mutex
	calls   []string
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockingChild() *blockingChild {
	return &blockingChild{release: make(chan struct{}), closed: make(chan struct{})}
}

func (c *blockingChild) ToolDeclarations() []toolregistry.Declaration {
	return []toolregistry.Declaration{
		{Name: "browser_navigate", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "browser_tabs", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: HandleDialogTool, InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
}
func (c *blockingChild) CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	c.mu.Lock()
	c.calls = append(c.calls, name+" "+string(arguments))
	c.mu.Unlock()
	if name == HandleDialogTool {
		return json.RawMessage(`{"content":[{"type":"text","text":"cleared"}]}`), nil
	}
	select {
	case <-c.release:
		return json.RawMessage(`{"content":[{"type":"text","text":"done ` + name + `"}]}`), nil
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *blockingChild) Availability(context.Context) error {
	select {
	case <-c.closed:
		return io.EOF
	default:
		return nil
	}
}
func (c *blockingChild) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func (c *blockingChild) callLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// fakeDialogControl is the daemon's control surface as the router sees it.
type fakeDialogControl struct {
	mu      sync.Mutex
	owner   browser.ControlOwner
	dialogs []browser.PendingDialog
	handled []map[string]any
}

func (f *fakeDialogControl) set(owner browser.ControlOwner, dialogs []browser.PendingDialog) {
	f.mu.Lock()
	f.owner, f.dialogs = owner, dialogs
	f.mu.Unlock()
}

func (f *fakeDialogControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case browser.DialogsPath:
		owner := f.owner
		if owner == "" {
			owner = browser.ControlAgent
		}
		_ = json.NewEncoder(w).Encode(browser.DialogStatus{ControlOwner: owner, Dialogs: f.dialogs})
	case browser.DialogsPath + "/handle":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.handled = append(f.handled, body)
		f.dialogs = nil
		_ = json.NewEncoder(w).Encode(map[string]any{"handled": true, "remaining": []any{}})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": []browser.Instance{{ID: "default"}}})
	}
}

type routerHarness struct {
	t     *testing.T
	in    *io.PipeWriter
	out   *bufio.Scanner
	next  int
	mu    sync.Mutex
	kids  []*blockingChild
	start func() *blockingChild
}

func startRouter(t *testing.T, control http.Handler, cfg Config) *routerHarness {
	t.Helper()
	server := httptest.NewServer(control)
	t.Cleanup(server.Close)
	h := &routerHarness{t: t}
	inR, inW := pipe()
	outR, outW := pipe()
	h.in, h.out = inW, newScanner(outR)
	cfg.ControlURL, cfg.Token, cfg.SessionID, cfg.CDPBase = server.URL, "t", "s", "http://cdp/"
	cfg.StartChild = func(context.Context, string, string) (Child, error) {
		child := newBlockingChild()
		h.mu.Lock()
		h.kids = append(h.kids, child)
		h.mu.Unlock()
		return child, nil
	}
	go func() { _ = Run(context.Background(), inR, outW, cfg) }()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

func (h *routerHarness) children() []*blockingChild {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*blockingChild(nil), h.kids...)
}

func pipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

func newScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	return scanner
}

// callTool sends one tools/call and returns its text and whether the router
// flagged it as an error.
func (h *routerHarness) callTool(name string, args map[string]any) (string, bool) {
	h.t.Helper()
	return h.callToolWithin(name, args, 5*time.Second)
}

func (h *routerHarness) callToolWithin(name string, args map[string]any, within time.Duration) (string, bool) {
	h.t.Helper()
	h.next++
	encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": h.next, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	_, _ = h.in.Write(append(encoded, '\n'))
	done := make(chan map[string]any, 1)
	go func() {
		h.out.Scan()
		var response map[string]any
		_ = json.Unmarshal(h.out.Bytes(), &response)
		done <- response
	}()
	select {
	case response := <-done:
		result, _ := response["result"].(map[string]any)
		content, _ := result["content"].([]any)
		isError, _ := result["isError"].(bool)
		if len(content) == 0 {
			return "", isError
		}
		return content[0].(map[string]any)["text"].(string), isError
	case <-time.After(within):
		h.t.Fatalf("no response to %s within %s", name, within)
		return "", false
	}
}

func TestRouterRestartsAWedgedPlaywrightChild(t *testing.T) {
	control := &fakeDialogControl{}
	h := startRouter(t, control, Config{CallTimeout: 150 * time.Millisecond, DialogPollInterval: 10 * time.Millisecond})
	text, isError := h.callTool("browser_tabs", map[string]any{"browserId": "default", "action": "list"})
	if !isError || !strings.Contains(text, "restarted the Playwright connection") {
		t.Fatalf("wedged call = %q (error %v)", text, isError)
	}
	first := h.children()[0]
	select {
	case <-first.closed:
	default:
		t.Fatal("wedged child was not closed")
	}
	// The next call gets a fresh child instead of queueing behind the old one.
	go func() {
		time.Sleep(30 * time.Millisecond)
		for _, child := range h.children() {
			if child != first {
				close(child.release)
			}
		}
	}()
	text, isError = h.callTool("browser_tabs", map[string]any{"browserId": "default", "action": "list"})
	if isError || text != "done browser_tabs" || len(h.children()) != 2 {
		t.Fatalf("call after restart = %q (error %v), children %d", text, isError, len(h.children()))
	}
}

func TestRouterTimeoutIgnoresTimeHeldByTheHuman(t *testing.T) {
	control := &fakeDialogControl{}
	control.set(browser.ControlUser, nil)
	h := startRouter(t, control, Config{CallTimeout: 100 * time.Millisecond, DialogPollInterval: 10 * time.Millisecond})
	go func() {
		time.Sleep(300 * time.Millisecond)
		control.set(browser.ControlAgent, nil)
		close(h.children()[0].release)
	}()
	text, isError := h.callTool("browser_navigate", map[string]any{"browserId": "default", "url": "https://example.test"})
	if isError || text != "done browser_navigate" {
		t.Fatalf("held call = %q (error %v)", text, isError)
	}
}

func TestRouterFailsFastOnDialogsAndAnswersThem(t *testing.T) {
	control := &fakeDialogControl{}
	h := startRouter(t, control, Config{DialogPollInterval: 10 * time.Millisecond})
	// A dialog opening mid-call returns at once instead of waiting out the
	// navigation timeout.
	go func() {
		time.Sleep(50 * time.Millisecond)
		control.set(browser.ControlAgent, []browser.PendingDialog{{TargetID: "page-1", Type: "beforeunload", URL: "https://example.test/form"}})
	}()
	text, isError := h.callTool("browser_navigate", map[string]any{"browserId": "default", "url": "https://example.test/next"})
	if !isError || !strings.Contains(text, `"beforeunload" dialog is open`) || !strings.Contains(text, "browser_navigate opened it") || !strings.Contains(text, "discards unsaved form input") {
		t.Fatalf("navigate into dialog = %q (error %v)", text, isError)
	}
	// While it is open, other tools fail fast without reaching Playwright,
	// even though the navigation still holds the browser.
	text, isError = h.callTool("browser_tabs", map[string]any{"browserId": "default", "action": "list"})
	if !isError || !strings.Contains(text, "this tool call was not run") {
		t.Fatalf("tool during dialog = %q (error %v)", text, isError)
	}
	child := h.children()[0]
	if calls := child.callLog(); len(calls) != 1 {
		t.Fatalf("calls reached Playwright during dialog: %v", calls)
	}
	// browser_handle_dialog goes to the daemon, which can answer a dialog
	// Playwright never saw.
	text, isError = h.callTool(HandleDialogTool, map[string]any{"browserId": "default", "accept": true})
	if isError || !strings.Contains(text, `Accepted the "beforeunload" dialog on https://example.test/form`) {
		t.Fatalf("handle dialog = %q (error %v)", text, isError)
	}
	control.mu.Lock()
	handled := control.handled
	control.mu.Unlock()
	if len(handled) != 1 || handled[0]["targetId"] != "page-1" || handled[0]["accept"] != true || handled[0]["browserId"] != "default" {
		t.Fatalf("daemon handle requests = %v", handled)
	}
	// Answering lets the navigation settle; the next call first clears
	// Playwright's stale modal state by dismissing, then runs.
	close(child.release)
	text, isError = h.callTool("browser_tabs", map[string]any{"browserId": "default", "action": "list"})
	if isError || text != "done browser_tabs" {
		t.Fatalf("call after dialog = %q (error %v)", text, isError)
	}
	calls := child.callLog()
	if len(calls) != 3 || calls[1] != HandleDialogTool+` {"accept":false}` || !strings.HasPrefix(calls[2], "browser_tabs") {
		t.Fatalf("Playwright calls = %v", calls)
	}
}
