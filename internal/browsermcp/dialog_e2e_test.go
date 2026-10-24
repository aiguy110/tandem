package browsermcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/browser"
)

type existingBrowserDriver struct{ cdp string }

func (d existingBrowserDriver) Kind() string { return "existing" }
func (d existingBrowserDriver) Provision(context.Context, string) (browser.ProvisionResult, error) {
	return browser.ProvisionResult{CDPURL: d.cdp}, nil
}
func (existingBrowserDriver) Teardown(context.Context, string) error { return nil }
func (existingBrowserDriver) IsProvisioned(string) bool              { return true }
func (existingBrowserDriver) PID(string) int                         { return 0 }

// TestRouterBeforeunloadEndToEnd drives real Chromium through the real
// Playwright MCP, broker and dialog endpoints: navigating away from a page
// with a beforeunload handler must report the dialog instead of hanging, and
// answering it must leave the browser usable.
func TestRouterBeforeunloadEndToEnd(t *testing.T) {
	chromium, bun, cli := os.Getenv("TANDEM_TEST_CHROMIUM"), os.Getenv("TANDEM_TEST_BUN"), os.Getenv("TANDEM_TEST_PLAYWRIGHT_MCP")
	if chromium == "" || bun == "" || cli == "" {
		t.Skip("set TANDEM_TEST_CHROMIUM, TANDEM_TEST_BUN and TANDEM_TEST_PLAYWRIGHT_MCP (cli.js) to run")
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/form" {
			fmt.Fprint(w, `<title>Form</title><input id="t"><button id="b">b</button><script>addEventListener('beforeunload',e=>{e.preventDefault();e.returnValue=''})</script>`)
			return
		}
		fmt.Fprint(w, `<title>Next</title>next`)
	}))
	defer site.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	profile := t.TempDir()
	chrome := exec.Command(chromium, "--headless=new", "--no-sandbox", "--disable-gpu", "--no-first-run", fmt.Sprintf("--remote-debugging-port=%d", port), "--user-data-dir="+profile, "about:blank")
	if err := chrome.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = chrome.Process.Kill(); _ = chrome.Wait(); time.Sleep(200 * time.Millisecond) }()
	cdp := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if response, err := http.Get(cdp + "/json/version"); err == nil {
			response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chromium did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}

	broker := browser.NewBroker(existingBrowserDriver{cdp: cdp}, browser.BrokerConfig{WatchDialogs: true})
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	defer broker.Stop(context.Background())
	dialogs := browser.NewDialogControl(browser.DialogControlOptions{Token: "t", Broker: broker, AgentExists: func(id string) bool { return id == "s" }})
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, browser.DialogsPath) {
			dialogs.ServeHTTP(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": []browser.Instance{{ID: browser.DefaultBrowserID}}})
	})
	server := httptest.NewServer(control)
	defer server.Close()
	// The watcher must be attached before the dialog opens.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := broker.EnsureProvisioned(ctx, "s"); err != nil {
		t.Fatal(err)
	}

	h := &routerHarness{t: t}
	inR, inW := pipe()
	outR, outW := pipe()
	h.in, h.out = inW, newScanner(outR)
	go func() {
		_ = Run(context.Background(), inR, outW, Config{
			ControlURL: server.URL, Token: "t", SessionID: "s", CDPBase: broker.CDPBase(),
			JSRuntime: bun, PlaywrightCLI: cli, OutputDir: t.TempDir(), DialogPollInterval: 100 * time.Millisecond,
		})
	}()
	defer inW.Close()
	call := func(name string, args map[string]any, within time.Duration) (string, bool) {
		t.Helper()
		started := time.Now()
		text, isError := h.callToolWithin(name, args, within)
		t.Logf("%s (%s): error=%v %.200q", name, time.Since(started).Round(time.Millisecond), isError, text)
		return text, isError
	}
	if _, isError := call("browser_navigate", map[string]any{"browserId": "default", "url": site.URL + "/form"}, 30*time.Second); isError {
		t.Fatal("open form")
	}
	call("browser_type", map[string]any{"browserId": "default", "target": "#t", "text": "typed"}, 20*time.Second)
	call("browser_click", map[string]any{"browserId": "default", "target": "#b"}, 20*time.Second)
	text, isError := call("browser_navigate", map[string]any{"browserId": "default", "url": site.URL + "/next"}, 20*time.Second)
	if !isError || !strings.Contains(text, `"beforeunload" dialog is open`) {
		t.Fatalf("navigate into beforeunload = %q", text)
	}
	if text, isError = call("browser_tabs", map[string]any{"browserId": "default", "action": "list"}, 5*time.Second); !isError || !strings.Contains(text, "was not run") {
		t.Fatalf("tabs during dialog = %q", text)
	}
	if text, isError = call(HandleDialogTool, map[string]any{"browserId": "default", "accept": true}, 10*time.Second); isError {
		t.Fatalf("handle dialog = %q", text)
	}
	text, isError = call("browser_tabs", map[string]any{"browserId": "default", "action": "list"}, 30*time.Second)
	if isError || !strings.Contains(text, "/next") || strings.Contains(text, "Modal state") {
		t.Fatalf("tabs after dialog = %q", text)
	}
	if text, isError = call("browser_evaluate", map[string]any{"browserId": "default", "function": "() => location.pathname"}, 30*time.Second); isError || !strings.Contains(text, "/next") {
		t.Fatalf("evaluate after dialog = %q", text)
	}
}
