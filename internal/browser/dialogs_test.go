package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialogCDPFake is a browser-level CDP endpoint that attaches "w-<target>"
// sessions and records commands, for the dialog watcher.
type dialogCDPFake struct {
	server    *httptest.Server
	wsURL     string
	mu        sync.Mutex
	conns     map[*websocket.Conn]*sync.Mutex
	commands  []cdpTestCommand
	pages     []string
	connSeq   int
	noDialogs bool // answer Page.handleJavaScriptDialog with "No dialog is showing"
}

func newDialogCDPFake(t *testing.T, pages ...string) *dialogCDPFake {
	t.Helper()
	f := &dialogCDPFake{conns: make(map[*websocket.Conn]*sync.Mutex), pages: pages}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		writeMu := &sync.Mutex{}
		f.mu.Lock()
		f.conns[conn] = writeMu
		f.connSeq++
		connSeq := f.connSeq
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.conns, conn)
			f.mu.Unlock()
			_ = conn.Close()
		}()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var command cdpTestCommand
			if json.Unmarshal(data, &command) != nil {
				continue
			}
			f.mu.Lock()
			f.commands = append(f.commands, command)
			pages, noDialogs := append([]string(nil), f.pages...), f.noDialogs
			f.mu.Unlock()
			reply := map[string]any{"id": command.ID, "result": map[string]any{}}
			switch command.Method {
			case "Target.getTargets":
				infos := []map[string]any{{"targetId": "worker", "type": "service_worker"}}
				for _, page := range pages {
					infos = append(infos, map[string]any{"targetId": page, "type": "page"})
				}
				reply["result"] = map[string]any{"targetInfos": infos}
			case "Target.attachToTarget":
				// Like Chrome, a new connection gets new session IDs.
				reply["result"] = map[string]any{"sessionId": fmt.Sprintf("w-%s-%d", command.Params["targetId"], connSeq)}
			case "Page.handleJavaScriptDialog":
				if noDialogs {
					delete(reply, "result")
					reply["error"] = map[string]any{"code": -32000, "message": "No dialog is showing"}
				}
			}
			payload, _ := json.Marshal(reply)
			writeMu.Lock()
			err = conn.WriteMessage(websocket.TextMessage, payload)
			writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}))
	f.wsURL = "ws" + strings.TrimPrefix(f.server.URL, "http")
	t.Cleanup(func() { f.dropConnections(); f.server.Close() })
	return f
}

func (f *dialogCDPFake) event(method, session string, params any) {
	payload, _ := json.Marshal(map[string]any{"method": method, "sessionId": session, "params": params})
	f.mu.Lock()
	defer f.mu.Unlock()
	for conn, writeMu := range f.conns {
		writeMu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, payload)
		writeMu.Unlock()
	}
}

func (f *dialogCDPFake) dropConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for conn := range f.conns {
		_ = conn.Close()
	}
}

func (f *dialogCDPFake) commandsFor(method string) []cdpTestCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cdpTestCommand
	for _, command := range f.commands {
		if command.Method == method {
			out = append(out, command)
		}
	}
	return out
}

func (f *dialogCDPFake) pageEnabled(session string) bool {
	for _, command := range f.commandsFor("Page.enable") {
		if command.SessionID == session {
			return true
		}
	}
	return false
}

func TestBrokerTracksAndAnswersDialogs(t *testing.T) {
	fake := newDialogCDPFake(t, "page-1")
	broker := NewBroker(&fakeDriver{cdp: fake.wsURL, provisions: map[string]int{}, teardowns: map[string]int{}}, BrokerConfig{WatchDialogs: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var states []BrowserState
	off := broker.OnState("s", func(state BrowserState) {
		mu.Lock()
		states = append(states, state)
		mu.Unlock()
	})
	defer off()
	lastState := func() BrowserState {
		mu.Lock()
		defer mu.Unlock()
		return states[len(states)-1]
	}
	if err := broker.EnsureProvisioned(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	defer broker.Teardown(context.Background(), "s")
	waitFor(t, "page watch session", func() bool { return fake.pageEnabled("w-page-1-1") })
	if len(fake.commandsFor("Target.setDiscoverTargets")) != 1 {
		t.Fatal("watcher does not follow new targets")
	}

	fake.event("Page.javascriptDialogOpening", "w-page-1-1", map[string]any{"type": "beforeunload", "message": "", "url": "https://example.test/form"})
	waitFor(t, "dialog in browser state", func() bool { return len(lastState().Dialogs) == 1 })
	if dialog := lastState().Dialogs[0]; dialog.TargetID != "page-1" || dialog.Type != "beforeunload" || dialog.URL != "https://example.test/form" {
		t.Fatalf("dialog = %+v", dialog)
	}

	// The agent-facing endpoint reports it without provisioning anything.
	control := NewDialogControl(DialogControlOptions{Token: "tok", Broker: broker, AgentExists: func(id string) bool { return id == "s" }})
	request := httptest.NewRequest(http.MethodGet, DialogsPath+"?sessionId=s&browserId=default", nil)
	request.Header.Set("Authorization", "Bearer tok")
	recorder := httptest.NewRecorder()
	control.ServeHTTP(recorder, request)
	var status DialogStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil || len(status.Dialogs) != 1 || status.ControlOwner != ControlAgent {
		t.Fatalf("status %d %s", recorder.Code, recorder.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	control.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, DialogsPath+"?sessionId=s", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthorized.Code)
	}

	// A watcher reconnect keeps the dialog: the new session cannot see it.
	fake.dropConnections()
	waitFor(t, "watcher reconnect", func() bool { return fake.pageEnabled("w-page-1-2") })
	if len(broker.Dialogs("s")) != 1 {
		t.Fatal("dialog lost across watcher reconnect")
	}
	// ...but only the session that saw it open can answer it.
	if _, err := broker.HandleDialog(ctx, "s", "page-1", true, ""); err == nil || !strings.Contains(err.Error(), "lost its connection") {
		t.Fatalf("answer through a later session = %v", err)
	}
	if len(fake.commandsFor("Page.handleJavaScriptDialog")) != 0 {
		t.Fatal("answered through a session that cannot see the dialog")
	}
	fake.event("Page.javascriptDialogClosed", "w-page-1-2", map[string]any{"result": true})
	waitFor(t, "dialog closed", func() bool { return len(broker.Dialogs("s")) == 0 })
	fake.event("Page.javascriptDialogOpening", "w-page-1-2", map[string]any{"type": "beforeunload", "message": "", "url": "https://example.test/form"})
	waitFor(t, "dialog on the new session", func() bool { return len(lastState().Dialogs) == 1 })

	body, _ := json.Marshal(map[string]any{"sessionId": "s", "targetId": "page-1", "accept": true})
	request = httptest.NewRequest(http.MethodPost, DialogsPath+"/handle", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer tok")
	recorder = httptest.NewRecorder()
	control.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("handle %d %s", recorder.Code, recorder.Body.String())
	}
	answers := fake.commandsFor("Page.handleJavaScriptDialog")
	if len(answers) != 1 || answers[0].SessionID != "w-page-1-2" || answers[0].Params["accept"] != true {
		t.Fatalf("answers = %+v", answers)
	}
	waitFor(t, "dialog cleared from state", func() bool { return len(lastState().Dialogs) == 0 })
	if _, err := broker.HandleDialog(ctx, "s", "", false, ""); err != ErrNoDialog {
		t.Fatalf("answering with no dialog = %v", err)
	}
}

func TestDialogWatcherFollowsTargetsAndStaleDialogs(t *testing.T) {
	fake := newDialogCDPFake(t)
	changes := make(chan struct{}, 16)
	watcher := newDialogWatcher(fake.wsURL, "s", func() { changes <- struct{}{} })
	watcher.start()
	defer watcher.close()
	waitFor(t, "watcher connected", func() bool { return len(fake.commandsFor("Target.setDiscoverTargets")) == 1 })

	// Pages opened later are watched as they appear.
	fake.event("Target.targetCreated", "", map[string]any{"targetInfo": map[string]any{"targetId": "page-2", "type": "page"}})
	fake.event("Target.targetCreated", "", map[string]any{"targetInfo": map[string]any{"targetId": "sw", "type": "service_worker"}})
	waitFor(t, "new page watched", func() bool { return fake.pageEnabled("w-page-2-1") })
	if len(fake.commandsFor("Target.attachToTarget")) != 1 {
		t.Fatalf("attached non-page targets: %+v", fake.commandsFor("Target.attachToTarget"))
	}

	fake.event("Page.javascriptDialogOpening", "w-page-2-1", map[string]any{"type": "confirm", "message": "Sure?"})
	waitFor(t, "dialog seen", func() bool { return len(watcher.Dialogs()) == 1 })
	fake.event("Page.javascriptDialogClosed", "w-page-2-1", map[string]any{"result": false})
	waitFor(t, "dialog closed elsewhere", func() bool { return len(watcher.Dialogs()) == 0 })

	// A dialog that closed without an event is cleared when answering finds
	// nothing showing.
	fake.event("Page.javascriptDialogOpening", "w-page-2-1", map[string]any{"type": "alert", "message": "Hi"})
	waitFor(t, "second dialog", func() bool { return len(watcher.Dialogs()) == 1 })
	fake.mu.Lock()
	fake.noDialogs = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := watcher.Handle(ctx, "page-2", true, ""); err != ErrNoDialog {
		t.Fatalf("stale answer = %v", err)
	}
	if len(watcher.Dialogs()) != 0 {
		t.Fatal("stale dialog kept")
	}

	// Closing the page drops its dialog.
	fake.mu.Lock()
	fake.noDialogs = false
	fake.mu.Unlock()
	fake.event("Page.javascriptDialogOpening", "w-page-2-1", map[string]any{"type": "prompt", "message": "Name?", "defaultPrompt": "x"})
	waitFor(t, "prompt dialog", func() bool { return len(watcher.Dialogs()) == 1 && watcher.Dialogs()[0].DefaultPrompt == "x" })
	if _, err := watcher.Handle(ctx, "", true, "typed"); err != nil {
		t.Fatal(err)
	}
	answers := fake.commandsFor("Page.handleJavaScriptDialog")
	if last := answers[len(answers)-1]; last.Params["promptText"] != "typed" {
		t.Fatalf("prompt answer = %+v", last)
	}
	fake.event("Page.javascriptDialogOpening", "w-page-2-1", map[string]any{"type": "alert"})
	waitFor(t, "third dialog", func() bool { return len(watcher.Dialogs()) == 1 })
	fake.event("Target.targetDestroyed", "", map[string]any{"targetId": "page-2"})
	waitFor(t, "closed page clears dialog", func() bool { return len(watcher.Dialogs()) == 0 })
}

// TestDialogWatcherRealChromium checks the CDP behaviour the watcher relies
// on: a beforeunload dialog raised by a navigation is reported to a session
// that enabled Page beforehand and can be accepted through it.
func TestDialogWatcherRealChromium(t *testing.T) {
	executable := os.Getenv("TANDEM_TEST_CHROMIUM")
	if executable == "" {
		t.Skip("set TANDEM_TEST_CHROMIUM to exercise real dialogs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	address, stop := startTestChromium(t, ctx, executable)
	defer stop()
	broker := NewBroker(&fakeDriver{cdp: "http://" + address, provisions: map[string]int{}, teardowns: map[string]int{}}, BrokerConfig{WatchDialogs: true})
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	defer broker.Stop(context.Background())
	shared, err := broker.SharedBrowser(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	page := `data:text/html,<input id=t><script>addEventListener('beforeunload',e=>{e.preventDefault();e.returnValue=''})</script>`
	if err := shared.Navigate(ctx, page); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	// beforeunload dialogs need a user gesture on the page first.
	for _, kind := range []string{"mousedown", "mouseup"} {
		if err := shared.Dispatch(ctx, BrowserInputEvent{Kind: kind, X: 10, Y: 10}); err != nil {
			t.Fatal(err)
		}
	}
	go func() { _ = shared.Navigate(context.Background(), "data:text/html,next") }()
	waitFor(t, "beforeunload dialog", func() bool {
		dialogs := broker.Dialogs("test")
		return len(dialogs) == 1 && dialogs[0].Type == "beforeunload"
	})
	if _, err := broker.HandleDialog(ctx, "test", "", true, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "navigation after accepting", func() bool {
		pages, err := shared.Pages(ctx)
		return err == nil && len(pages.Pages) == 1 && pages.Pages[0].URL == "data:text/html,next"
	})
	// Let Chromium flush and reap its children before TempDir cleanup.
	shared.mu.Lock()
	conn := shared.conn
	shared.mu.Unlock()
	_ = shared.callOn(ctx, conn, "", "Browser.close", map[string]any{}, nil)
}

func startTestChromium(t *testing.T, ctx context.Context, executable string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	args := append([]string{"--headless=new", "--disable-extensions", "--no-sandbox"}, localLaunchArgs(port, t.TempDir(), false, "")...)
	command := exec.CommandContext(ctx, executable, args...)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Chrome CDP endpoint", func() bool {
		response, err := http.Get("http://" + address + "/json/version")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	return address, func() {
		exited := make(chan struct{})
		go func() { _ = command.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
	}
}
