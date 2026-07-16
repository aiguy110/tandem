package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeDriver struct {
	mu         sync.Mutex
	cdp        string
	provisions map[string]int
	teardowns  map[string]int
}

func (d *fakeDriver) Kind() string   { return "fake" }
func (d *fakeDriver) PID(string) int { return 0 }
func (d *fakeDriver) Provision(_ context.Context, id string) (ProvisionResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.provisions[id]++
	return ProvisionResult{CDPURL: d.cdp}, nil
}
func (d *fakeDriver) Teardown(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.teardowns[id]++
	return nil
}
func (d *fakeDriver) IsProvisioned(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.provisions[id] > d.teardowns[id]
}
func (d *fakeDriver) count(id string) int { d.mu.Lock(); defer d.mu.Unlock(); return d.provisions[id] }

type rawCDP struct {
	server   *httptest.Server
	wsURL    string
	received chan string
	event    chan string
}

func newRawCDP(t *testing.T) *rawCDP {
	r := &rawCDP{received: make(chan string, 20), event: make(chan string, 20)}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	r.server = httptest.NewServer(mux)
	r.wsURL = "ws" + r.server.URL[len("http"):] + "/devtools/browser/test"
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Browser": "Fake/1", "webSocketDebuggerUrl": r.wsURL})
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "target", "webSocketDebuggerUrl": "ws" + r.server.URL[len("http"):] + "/devtools/page/target"}})
	})
	mux.HandleFunc("/devtools/page/target", func(w http.ResponseWriter, req *http.Request) {
		c, err := up.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			r.received <- "target:" + string(data)
			if c.WriteMessage(kind, data) != nil {
				return
			}
		}
	})
	mux.HandleFunc("/devtools/browser/test", func(w http.ResponseWriter, req *http.Request) {
		c, err := up.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer c.Close()
		done := make(chan struct{})
		var once sync.Once
		var writes sync.Mutex
		go func() {
			for {
				select {
				case msg := <-r.event:
					writes.Lock()
					err := c.WriteMessage(websocket.TextMessage, []byte(msg))
					writes.Unlock()
					if err != nil {
						return
					}
				case <-done:
					return
				}
			}
		}()
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
			r.received <- string(data)
			writes.Lock()
			err = c.WriteMessage(kind, []byte(`{"ack":`+string(data)+`}`))
			writes.Unlock()
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
		}
	})
	return r
}
func (r *rawCDP) close() { r.server.Close() }
func dialEndpoint(t *testing.T, endpoint string) (*websocket.Conn, string) {
	resp, err := http.Get(endpoint + "/json/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var metadata struct {
		Browser string
		WS      string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Browser != "Fake/1" {
		t.Fatalf("metadata not proxied: %#v", metadata)
	}
	c, _, err := websocket.DefaultDialer.Dial(metadata.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, metadata.WS
}
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBrokerRawCDPColdPauseReleaseAndCleanup(t *testing.T) {
	raw := newRawCDP(t)
	defer raw.close()
	d := &fakeDriver{cdp: raw.server.URL, provisions: map[string]int{}, teardowns: map[string]int{}}
	b := NewBroker(d, BrokerConfig{})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	endpoint := b.EndpointFor("api/58")
	if d.count("api/58") != 0 {
		t.Fatal("endpoint lookup eagerly provisioned")
	}
	c, rewritten := dialEndpoint(t, endpoint)
	if u, _ := url.Parse(rewritten); u.Host == urlHost(raw.wsURL) {
		t.Fatal("webSocketDebuggerUrl was not rewritten through broker")
	}
	defer c.Close()
	if d.count("api/58") != 1 {
		t.Fatalf("provisions=%d", d.count("api/58"))
	}
	waitFor(t, "proxy registration", func() bool { return b.ActiveConnections("api/58") == 1 })
	b.Grab("api/58")
	if b.Owner("api/58") != ControlUser {
		t.Fatal("grab failed")
	}
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"id":1}`))
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"id":2}`))
	select {
	case got := <-raw.received:
		t.Fatalf("command passed hard pause: %s", got)
	case <-time.After(100 * time.Millisecond):
	}
	raw.event <- `{"method":"Page.event"}`
	_, event, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(event) != `{"method":"Page.event"}` {
		t.Fatalf("reverse event=%s", event)
	}
	if err := b.Release("api/58"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		select {
		case got := <-raw.received:
			want := fmt.Sprintf(`{"id":%d}`, i)
			if got != want {
				t.Fatalf("ordered frame=%s want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("held frame not released")
		}
	}
	_ = c.Close()
	waitFor(t, "proxy cleanup", func() bool { return b.ActiveConnections("api/58") == 0 })
	if err := b.Teardown(context.Background(), "api/58"); err != nil {
		t.Fatal(err)
	}
	if d.teardowns["api/58"] != 1 {
		t.Fatal("driver not torn down")
	}
}
func urlHost(raw string) string { u, _ := url.Parse(raw); return u.Host }

func TestBrokerIndependentAgentsAndConcurrentColdStart(t *testing.T) {
	raw := newRawCDP(t)
	defer raw.close()
	d := &fakeDriver{cdp: raw.server.URL, provisions: map[string]int{}, teardowns: map[string]int{}}
	b := NewBroker(d, BrokerConfig{})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.EnsureProvisioned(context.Background(), "one"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if d.count("one") != 1 {
		t.Fatalf("concurrent provisions=%d", d.count("one"))
	}
	one, _ := dialEndpoint(t, b.EndpointFor("one"))
	defer one.Close()
	two, _ := dialEndpoint(t, b.EndpointFor("two"))
	defer two.Close()
	waitFor(t, "two links", func() bool { return b.ActiveConnections("one") == 1 && b.ActiveConnections("two") == 1 })
	b.Grab("one")
	_ = one.WriteMessage(websocket.TextMessage, []byte(`{"agent":1}`))
	_ = two.WriteMessage(websocket.TextMessage, []byte(`{"agent":2}`))
	select {
	case got := <-raw.received:
		if got != `{"agent":2}` {
			t.Fatalf("agent isolation got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ungated agent blocked")
	}
	if d.count("two") != 1 {
		t.Fatal("second agent not independently provisioned")
	}
}

func TestBrokerHeldPayloadLimitClosesLink(t *testing.T) {
	raw := newRawCDP(t)
	defer raw.close()
	d := &fakeDriver{cdp: raw.server.URL, provisions: map[string]int{}, teardowns: map[string]int{}}
	b := NewBroker(d, BrokerConfig{MaxHeldBytes: 4})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	c, _ := dialEndpoint(t, b.EndpointFor("limited"))
	b.Grab("limited")
	_ = c.WriteMessage(websocket.TextMessage, []byte("12345"))
	waitFor(t, "oversize link close", func() bool { return b.ActiveConnections("limited") == 0 })
}

func TestBrokerRewritesAndProxiesTargetMetadata(t *testing.T) {
	raw := newRawCDP(t)
	defer raw.close()
	d := &fakeDriver{cdp: raw.server.URL, provisions: map[string]int{}, teardowns: map[string]int{}}
	b := NewBroker(d, BrokerConfig{})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background())
	resp, err := http.Get(b.EndpointFor("target-agent") + "/json/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var targets []struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || urlHost(targets[0].WS) == urlHost(raw.wsURL) {
		t.Fatalf("target metadata not rewritten: %#v", targets)
	}
	c, _, err := websocket.DefaultDialer.Dial(targets[0].WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"target":true}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-raw.received:
		if got != `target:{"target":true}` {
			t.Fatalf("target route got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("target command not proxied")
	}
}
