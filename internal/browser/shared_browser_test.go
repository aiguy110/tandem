package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type directCDPFake struct {
	t        *testing.T
	server   *httptest.Server
	wsURL    string
	mu       sync.Mutex
	conns    map[*websocket.Conn]struct{}
	writers  map[*websocket.Conn]*sync.Mutex
	commands []cdpTestCommand
	created  int
}

type cdpTestCommand struct {
	ID        int64          `json:"id"`
	Method    string         `json:"method"`
	SessionID string         `json:"sessionId"`
	Params    map[string]any `json:"params"`
}

type fakeWriteTarget struct {
	conn *websocket.Conn
	mu   *sync.Mutex
}

func newDirectCDPFake(t *testing.T, pageExists bool) *directCDPFake {
	t.Helper()
	f := &directCDPFake{t: t, conns: make(map[*websocket.Conn]struct{}), writers: make(map[*websocket.Conn]*sync.Mutex)}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns[conn] = struct{}{}
		writeMu := &sync.Mutex{}
		f.writers[conn] = writeMu
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.conns, conn)
			delete(f.writers, conn)
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
			f.mu.Unlock()
			result := map[string]any{}
			switch command.Method {
			case "Target.getTargets":
				if pageExists {
					result["targetInfos"] = []map[string]any{{"targetId": "page-1", "type": "page", "url": "about:blank"}}
				} else {
					result["targetInfos"] = []map[string]any{{"targetId": "worker", "type": "service_worker"}}
				}
			case "Target.createTarget":
				f.mu.Lock()
				f.created++
				f.mu.Unlock()
				result["targetId"] = "created-page"
			case "Target.attachToTarget":
				result["sessionId"] = "human-session"
			}
			response, _ := json.Marshal(map[string]any{"id": command.ID, "result": result})
			writeMu.Lock()
			err = conn.WriteMessage(websocket.TextMessage, response)
			writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}))
	f.wsURL = "ws" + f.server.URL[len("http"):]
	return f
}

func (f *directCDPFake) close() {
	f.closeConnections()
	f.server.Close()
}

func (f *directCDPFake) closeConnections() {
	f.mu.Lock()
	connections := make([]fakeWriteTarget, 0, len(f.conns))
	for conn := range f.conns {
		connections = append(connections, fakeWriteTarget{conn: conn, mu: f.writers[conn]})
	}
	f.mu.Unlock()
	for _, target := range connections {
		_ = target.conn.Close()
	}
}

func (f *directCDPFake) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	methods := make([]string, len(f.commands))
	for i := range f.commands {
		methods[i] = f.commands[i].Method
	}
	return methods
}

func (f *directCDPFake) commandsFor(method string) []cdpTestCommand {
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

func (f *directCDPFake) sendEvent(method string, params any) {
	payload, _ := json.Marshal(map[string]any{"method": method, "sessionId": "human-session", "params": params})
	f.mu.Lock()
	connections := make([]fakeWriteTarget, 0, len(f.conns))
	for conn := range f.conns {
		connections = append(connections, fakeWriteTarget{conn: conn, mu: f.writers[conn]})
	}
	f.mu.Unlock()
	for _, target := range connections {
		target.mu.Lock()
		_ = target.conn.WriteMessage(websocket.TextMessage, payload)
		target.mu.Unlock()
	}
}

func TestSharedBrowserCreatesTargetScreencastsAndDoesNotLeakListeners(t *testing.T) {
	fake := newDirectCDPFake(t, false)
	defer fake.close()
	shared := NewSharedBrowser(fake.wsURL)
	defer shared.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	frames := make(chan ScreencastFrame, 4)
	if err := shared.StartScreencast(ctx, func(frame ScreencastFrame) { frames <- frame }); err != nil {
		t.Fatal(err)
	}
	if fake.created != 1 {
		t.Fatalf("created targets=%d", fake.created)
	}
	start := fake.commandsFor("Page.startScreencast")[0]
	if start.Params["quality"] != float64(90) {
		t.Fatalf("screencast quality=%v", start.Params["quality"])
	}
	fake.sendEvent("Page.screencastFrame", map[string]any{
		"data": "jpeg-one", "sessionId": 41,
		"metadata": map[string]any{"deviceWidth": 1280, "deviceHeight": 800, "offsetTop": 5, "timestamp": 1.25},
	})
	select {
	case frame := <-frames:
		if frame.DataB64 != "jpeg-one" || frame.Meta.DeviceWidth != 1280 || frame.Meta.OffsetTop != 5 {
			t.Fatalf("frame=%+v", frame)
		}
	case <-ctx.Done():
		t.Fatal("frame not delivered")
	}
	waitFor(t, "frame acknowledgement", func() bool { return len(fake.commandsFor("Page.screencastFrameAck")) == 1 })
	ack := fake.commandsFor("Page.screencastFrameAck")[0]
	if ack.SessionID != "human-session" || ack.Params["sessionId"] != float64(41) {
		t.Fatalf("ack=%+v", ack)
	}
	if err := shared.StopScreencast(ctx); err != nil {
		t.Fatal(err)
	}
	if err := shared.StartScreencast(ctx, func(frame ScreencastFrame) { frames <- frame }); err != nil {
		t.Fatal(err)
	}
	fake.sendEvent("Page.screencastFrame", map[string]any{"data": "jpeg-two", "sessionId": 42, "metadata": map[string]any{}})
	select {
	case <-frames:
	case <-ctx.Done():
		t.Fatal("second-cycle frame not delivered")
	}
	select {
	case duplicate := <-frames:
		t.Fatalf("duplicate listener delivered frame: %+v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
	if got := len(fake.commandsFor("Page.startScreencast")); got != 2 {
		t.Fatalf("start commands=%d", got)
	}
	// An unexpected socket loss automatically restores an active cast.
	fake.closeConnections()
	waitFor(t, "screencast reconnect", func() bool {
		return shared.IsConnected() && len(fake.commandsFor("Page.startScreencast")) == 3
	})
	fake.sendEvent("Page.screencastFrame", map[string]any{"data": "jpeg-reconnected", "sessionId": 43, "metadata": map[string]any{}})
	select {
	case frame := <-frames:
		if frame.DataB64 != "jpeg-reconnected" {
			t.Fatalf("reconnected frame=%+v", frame)
		}
	case <-ctx.Done():
		t.Fatal("reconnected frame not delivered")
	}
}

func TestSharedBrowserInputMappingNavigationReconnectAndCleanup(t *testing.T) {
	fake := newDirectCDPFake(t, true)
	defer fake.close()
	shared := NewSharedBrowser(fake.wsURL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	inputs := []BrowserInputEvent{
		{Kind: "mousemove", X: 12, Y: 34, Buttons: 2},
		{Kind: "mousedown", X: 12, Y: 34, Button: "right", ClickCount: 2, Buttons: 2},
		{Kind: "mouseup", X: 12, Y: 34, Button: "right"},
		{Kind: "click", X: 8, Y: 9},
		{Kind: "wheel", X: 4, Y: 5, DeltaX: 6, DeltaY: -7},
		{Kind: "keydown", Key: "A", Code: "KeyA", KeyCode: 65, Text: "a"},
		{Kind: "keyup", Key: "A", Code: "KeyA", KeyCode: 65},
		{Kind: "text", Text: "hello"},
	}
	for _, input := range inputs {
		if err := shared.Dispatch(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	mouse := fake.commandsFor("Input.dispatchMouseEvent")
	if len(mouse) != 6 || mouse[0].Params["type"] != "mouseMoved" || mouse[0].Params["x"] != float64(12) || mouse[4].Params["type"] != "mouseReleased" || mouse[5].Params["deltaY"] != float64(-7) {
		t.Fatalf("mouse mapping=%+v", mouse)
	}
	keys := fake.commandsFor("Input.dispatchKeyEvent")
	if len(keys) != 2 || keys[0].Params["text"] != "a" || keys[1].Params["type"] != "keyUp" {
		t.Fatalf("key mapping=%+v", keys)
	}
	if text := fake.commandsFor("Input.insertText"); len(text) != 1 || text[0].Params["text"] != "hello" {
		t.Fatalf("text mapping=%+v", text)
	}

	// A lost direct socket is recreated and reattached before navigation.
	fake.closeConnections()
	waitFor(t, "disconnect observation", func() bool { return !shared.IsConnected() })
	if err := shared.Navigate(ctx, "https://example.test/recovered"); err != nil {
		t.Fatal(err)
	}
	navigation := fake.commandsFor("Page.navigate")
	if len(navigation) != 1 || navigation[0].Params["url"] != "https://example.test/recovered" {
		t.Fatalf("navigation=%+v", navigation)
	}
	if len(fake.commandsFor("Target.attachToTarget")) != 2 {
		t.Fatalf("expected reattach, methods=%v", fake.methods())
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "direct connection cleanup", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.conns) == 0
	})
}
