package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ControlOwner string

const (
	ControlAgent ControlOwner = "agent"
	ControlUser  ControlOwner = "user"
)

type BrokerConfig struct {
	Host                          string
	MaxPayloadBytes, MaxHeldBytes int64
	HTTPClient                    *http.Client
	OnRelease                     func(agentID string)
}
type heldFrame struct {
	kind int
	data []byte
}
type proxyLink struct {
	agent, upstream *websocket.Conn
	writeMu         sync.Mutex
	held            []heldFrame
	heldBytes       int64
	closed          chan struct{}
}
type agentBrowser struct {
	mu                sync.Mutex
	provisioned       bool
	provisioning      chan struct{}
	provisionErr      error
	cdpURL, browserWS string
	wsRoutes          map[string]string
	owner             ControlOwner
	links             map[*proxyLink]struct{}
	shared            *SharedBrowser
	nextListener      uint64
	stateListeners    map[uint64]func(BrowserState)
	frameListeners    map[uint64]func(ScreencastFrame)
}

type BrowserState struct {
	Active       bool         `json:"active"`
	ControlOwner ControlOwner `json:"controlOwner"`
}
type Broker struct {
	driver   Driver
	cfg      BrokerConfig
	server   *http.Server
	listener net.Listener
	mu       sync.Mutex
	agents   map[string]*agentBrowser
}

func NewBroker(driver Driver, cfg BrokerConfig) *Broker {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.MaxPayloadBytes == 0 {
		cfg.MaxPayloadBytes = 16 << 20
	}
	if cfg.MaxHeldBytes == 0 {
		cfg.MaxHeldBytes = 64 << 20
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	return &Broker{driver: driver, cfg: cfg, agents: make(map[string]*agentBrowser)}
}
func (b *Broker) Start() error {
	ln, err := net.Listen("tcp", net.JoinHostPort(b.cfg.Host, "0"))
	if err != nil {
		return err
	}
	b.listener = ln
	b.server = &http.Server{Handler: http.HandlerFunc(b.serveHTTP)}
	go func() { _ = b.server.Serve(ln) }()
	return nil
}
func (b *Broker) EndpointFor(id string) string {
	return "http://" + b.listener.Addr().String() + "/cdp/" + url.PathEscape(id)
}
func (b *Broker) DriverKind() string           { return b.driver.Kind() }
func (b *Broker) IsProvisioned(id string) bool { return b.driver.IsProvisioned(id) }
func (b *Broker) BrowserPID(id string) int     { return b.driver.PID(id) }
func (b *Broker) record(id string) *agentBrowser {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.agents[id]
	if a == nil {
		a = &agentBrowser{owner: ControlAgent, links: make(map[*proxyLink]struct{}), wsRoutes: make(map[string]string), stateListeners: make(map[uint64]func(BrowserState)), frameListeners: make(map[uint64]func(ScreencastFrame))}
		b.agents[id] = a
	}
	return a
}
func (b *Broker) State(id string) BrowserState {
	b.mu.Lock()
	a := b.agents[id]
	b.mu.Unlock()
	if a == nil {
		return BrowserState{ControlOwner: ControlAgent}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return BrowserState{Active: a.provisioned, ControlOwner: a.owner}
}

func (b *Broker) OnState(id string, callback func(BrowserState)) func() {
	a := b.record(id)
	a.mu.Lock()
	a.nextListener++
	listenerID := a.nextListener
	a.stateListeners[listenerID] = callback
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		delete(a.stateListeners, listenerID)
		a.mu.Unlock()
	}
}

func (b *Broker) AddFrameListener(id string, callback func(ScreencastFrame)) func() {
	a := b.record(id)
	a.mu.Lock()
	a.nextListener++
	listenerID := a.nextListener
	a.frameListeners[listenerID] = callback
	active := a.provisioned
	a.mu.Unlock()
	if active {
		go b.startCast(id)
	}
	return func() {
		a.mu.Lock()
		delete(a.frameListeners, listenerID)
		remaining := len(a.frameListeners)
		shared := a.shared
		a.mu.Unlock()
		if remaining == 0 && shared != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = shared.StopScreencast(ctx)
		}
	}
}

func (b *Broker) emitState(id string) {
	a := b.record(id)
	a.mu.Lock()
	state := BrowserState{Active: a.provisioned, ControlOwner: a.owner}
	callbacks := make([]func(BrowserState), 0, len(a.stateListeners))
	for _, callback := range a.stateListeners {
		callbacks = append(callbacks, callback)
	}
	a.mu.Unlock()
	for _, callback := range callbacks {
		callback(state)
	}
}

func (b *Broker) emitFrame(id string, frame ScreencastFrame) {
	a := b.record(id)
	a.mu.Lock()
	callbacks := make([]func(ScreencastFrame), 0, len(a.frameListeners))
	for _, callback := range a.frameListeners {
		callbacks = append(callbacks, callback)
	}
	a.mu.Unlock()
	for _, callback := range callbacks {
		callback(frame)
	}
}

func (b *Broker) startCast(id string) {
	a := b.record(id)
	a.mu.Lock()
	listeners := len(a.frameListeners)
	a.mu.Unlock()
	if listeners == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shared, err := b.SharedBrowser(ctx, id)
	if err != nil {
		return
	}
	a.mu.Lock()
	listeners = len(a.frameListeners)
	a.mu.Unlock()
	if listeners == 0 {
		return
	}
	_ = shared.StartScreencast(ctx, func(frame ScreencastFrame) { b.emitFrame(id, frame) })
}
func (b *Broker) Owner(id string) ControlOwner {
	a := b.record(id)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owner
}
func (b *Broker) ActiveConnections(id string) int {
	a := b.record(id)
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.links)
}

func (b *Broker) EnsureProvisioned(ctx context.Context, id string) error {
	a := b.record(id)
	a.mu.Lock()
	if a.provisioned {
		a.mu.Unlock()
		return nil
	}
	if wait := a.provisioning; wait != nil {
		a.mu.Unlock()
		select {
		case <-wait:
			a.mu.Lock()
			err := a.provisionErr
			a.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wait := make(chan struct{})
	a.provisioning = wait
	a.mu.Unlock()
	result, err := b.driver.Provision(ctx, id)
	var ws string
	if err == nil {
		ws, err = b.resolveBrowserWS(ctx, result.CDPURL)
	}
	a.mu.Lock()
	a.provisionErr = err
	if err == nil {
		a.provisioned = true
		a.cdpURL = result.CDPURL
		a.browserWS = ws
		// This client dials the real browser WebSocket, not the gated proxy.
		a.shared = NewSharedBrowser(ws)
	}
	a.provisioning = nil
	close(wait)
	listenerCount := len(a.frameListeners)
	a.mu.Unlock()
	if err != nil {
		_ = b.driver.Teardown(context.Background(), id)
	} else {
		b.emitState(id)
		if listenerCount > 0 {
			go b.startCast(id)
		}
	}
	return err
}

// SharedBrowser returns the daemon-owned direct CDP connection for the human
// browser pane. Provisioning remains lazy until this or the agent proxy is used.
func (b *Broker) SharedBrowser(ctx context.Context, id string) (*SharedBrowser, error) {
	if err := b.EnsureProvisioned(ctx, id); err != nil {
		return nil, err
	}
	a := b.record(id)
	a.mu.Lock()
	shared := a.shared
	a.mu.Unlock()
	if shared == nil {
		return nil, errors.New("shared browser unavailable")
	}
	if err := shared.Connect(ctx); err != nil {
		return nil, err
	}
	return shared, nil
}

// DevNavigate is the low-level development-only navigation hook. Its caller is
// responsible for enforcing the development-mode gate.
func (b *Broker) DevNavigate(ctx context.Context, id, targetURL string) error {
	shared, err := b.SharedBrowser(ctx, id)
	if err != nil {
		return err
	}
	return shared.Navigate(ctx, targetURL)
}

func (b *Broker) DispatchUserInput(ctx context.Context, id string, event BrowserInputEvent) error {
	if b.Owner(id) != ControlUser {
		return nil
	}
	shared, err := b.SharedBrowser(ctx, id)
	if err != nil {
		return err
	}
	return shared.Dispatch(ctx, event)
}
func (b *Broker) resolveBrowserWS(ctx context.Context, cdp string) (string, error) {
	if strings.HasPrefix(cdp, "ws://") || strings.HasPrefix(cdp, "wss://") {
		return cdp, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cdp, "/")+"/json/version", nil)
	r, err := b.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if r.StatusCode/100 != 2 {
		return "", fmt.Errorf("CDP metadata: %s", r.Status)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, b.cfg.MaxPayloadBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > b.cfg.MaxPayloadBytes {
		return "", errors.New("CDP metadata too large")
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err = json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	if v.WebSocketDebuggerURL == "" {
		return "", errors.New("CDP metadata missing webSocketDebuggerUrl")
	}
	return v.WebSocketDebuggerURL, nil
}

func parseAgentPath(path string) (id, rest string, ok bool) {
	if !strings.HasPrefix(path, "/cdp/") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/cdp/"), "/", 2)
	v, err := url.PathUnescape(parts[0])
	if err != nil || v == "" {
		return "", "", false
	}
	rest = "/"
	if len(parts) == 2 {
		rest = "/" + parts[1]
	}
	return v, rest, true
}
func (b *Broker) serveHTTP(w http.ResponseWriter, r *http.Request) {
	id, rest, ok := parseAgentPath(r.URL.EscapedPath())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		if r.URL.RawQuery != "" {
			rest += "?" + r.URL.RawQuery
		}
		b.upgrade(id, rest, w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := b.EnsureProvisioned(r.Context(), id); err != nil {
		http.Error(w, "broker: "+err.Error(), http.StatusBadGateway)
		return
	}
	a := b.record(id)
	a.mu.Lock()
	cdp := a.cdpURL
	browserWS := a.browserWS
	a.mu.Unlock()
	if strings.HasPrefix(cdp, "ws://") || strings.HasPrefix(cdp, "wss://") {
		if rest != "/" && strings.TrimRight(rest, "/") != "/json/version" {
			http.NotFound(w, r)
			return
		}
		b.writeSyntheticVersion(w, id, browserWS)
		return
	}
	path := rest
	if path == "/" {
		path = "/json/version"
	}
	up, err := url.Parse(strings.TrimRight(cdp, "/") + path)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	up.RawQuery = r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, up.String(), nil)
	resp, err := b.cfg.HTTPClient.Do(req)
	if err != nil {
		http.Error(w, "broker: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, b.cfg.MaxPayloadBytes+1))
	if err != nil || int64(len(body)) > b.cfg.MaxPayloadBytes {
		http.Error(w, "CDP metadata too large", http.StatusBadGateway)
		return
	}
	for k, vals := range resp.Header {
		if strings.EqualFold(k, "content-length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	body = b.rewriteMetadata(id, body, a)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
func (b *Broker) proxyWSURL(id string) string {
	return "ws://" + b.listener.Addr().String() + "/cdp/" + url.PathEscape(id) + "/devtools/browser"
}
func (b *Broker) writeSyntheticVersion(w http.ResponseWriter, id, upstream string) {
	a := b.record(id)
	a.mu.Lock()
	a.wsRoutes["/devtools/browser"] = upstream
	a.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": b.proxyWSURL(id)})
}
func (b *Broker) rewriteMetadata(id string, body []byte, a *agentBrowser) []byte {
	var data any
	if json.Unmarshal(body, &data) != nil {
		return body
	}
	changed := false
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, value := range x {
				if k == "webSocketDebuggerUrl" {
					if raw, ok := value.(string); ok {
						u, err := url.Parse(raw)
						if err == nil {
							route := u.EscapedPath()
							if u.RawQuery != "" {
								route += "?" + u.RawQuery
							}
							if route == "" {
								route = "/devtools/browser"
							}
							a.mu.Lock()
							a.wsRoutes[route] = raw
							a.mu.Unlock()
							x[k] = "ws://" + b.listener.Addr().String() + "/cdp/" + url.PathEscape(id) + route
							changed = true
						}
					}
				} else {
					walk(value)
				}
			}
		case []any:
			for _, value := range x {
				walk(value)
			}
		}
	}
	walk(data)
	if !changed {
		return body
	}
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: false}

func (b *Broker) upgrade(id, rest string, w http.ResponseWriter, r *http.Request) {
	agent, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// An upgraded connection outlives the HTTP request. Its lifetime is owned by
	// the two WebSockets and broker teardown, not Request.Context.
	go b.bridge(context.Background(), id, rest, agent)
}
func (b *Broker) bridge(ctx context.Context, id, rest string, agent *websocket.Conn) {
	if err := b.EnsureProvisioned(ctx, id); err != nil {
		_ = agent.Close()
		return
	}
	a := b.record(id)
	a.mu.Lock()
	upURL := a.wsRoutes[rest]
	if upURL == "" {
		upURL = a.browserWS
	}
	a.mu.Unlock()
	up, _, err := websocket.DefaultDialer.DialContext(ctx, upURL, nil)
	if err != nil {
		_ = agent.Close()
		return
	}
	link := &proxyLink{agent: agent, upstream: up, closed: make(chan struct{})}
	agent.SetReadLimit(b.cfg.MaxPayloadBytes)
	up.SetReadLimit(b.cfg.MaxPayloadBytes)
	a.mu.Lock()
	a.links[link] = struct{}{}
	a.mu.Unlock()
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.links, link)
			a.mu.Unlock()
			close(link.closed)
			_ = agent.Close()
			_ = up.Close()
		})
	}
	go func() {
		defer closeBoth()
		for {
			kind, data, err := up.ReadMessage()
			if err != nil {
				return
			}
			link.writeMu.Lock()
			err = agent.WriteMessage(kind, data)
			link.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	defer closeBoth()
	for {
		kind, data, err := agent.ReadMessage()
		if err != nil {
			return
		}
		frame := heldFrame{kind: kind, data: append([]byte(nil), data...)}
		a.mu.Lock()
		if a.owner == ControlUser {
			if link.heldBytes+int64(len(data)) > b.cfg.MaxHeldBytes {
				a.mu.Unlock()
				return
			}
			link.held = append(link.held, frame)
			link.heldBytes += int64(len(data))
			a.mu.Unlock()
			continue
		}
		a.mu.Unlock()
		link.writeMu.Lock()
		err = up.WriteMessage(kind, data)
		link.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}
func (b *Broker) Grab(id string) {
	a := b.record(id)
	a.mu.Lock()
	changed := a.owner != ControlUser
	a.owner = ControlUser
	a.mu.Unlock()
	if changed {
		b.emitState(id)
	}
}
func (b *Broker) Release(id string) error {
	a := b.record(id)
	a.mu.Lock()
	// Keep owner=user until every held frame is written. Readers take a.mu before
	// forwarding, so a newly arriving command cannot overtake the held queue.
	for l := range a.links {
		frames := l.held
		l.held = nil
		l.heldBytes = 0
		l.writeMu.Lock()
		for _, f := range frames {
			if err := l.upstream.WriteMessage(f.kind, f.data); err != nil {
				l.writeMu.Unlock()
				a.mu.Unlock()
				return err
			}
		}
		l.writeMu.Unlock()
	}
	a.owner = ControlAgent
	a.mu.Unlock()
	if b.cfg.OnRelease != nil {
		b.cfg.OnRelease(id)
	}
	b.emitState(id)
	return nil
}

// detach drops the daemon's local hold on an agent's browser — the direct CDP
// connection and any agent proxy links — and removes the in-memory record,
// without ending the underlying browser. For an externalized (Steel) session
// this leaves the session alive on the server so a later Detach-then-restart can
// re-attach to it.
func (b *Broker) detach(id string) {
	b.mu.Lock()
	a := b.agents[id]
	delete(b.agents, id)
	b.mu.Unlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	shared := a.shared
	a.shared = nil
	links := make([]*proxyLink, 0, len(a.links))
	for l := range a.links {
		links = append(links, l)
	}
	a.links = make(map[*proxyLink]struct{})
	a.mu.Unlock()
	if shared != nil {
		_ = shared.Close()
	}
	for _, l := range links {
		_ = l.agent.Close()
		_ = l.upstream.Close()
	}
}

// Detach releases only the daemon-local resources, keeping the underlying
// session alive. Used on daemon shutdown so a redeploy can re-attach.
func (b *Broker) Detach(id string) { b.detach(id) }

// Teardown ends the session for good: detaches locally, then tells the driver to
// destroy (release) the underlying browser. Used when an agent is closed.
func (b *Broker) Teardown(ctx context.Context, id string) error {
	b.detach(id)
	return b.driver.Teardown(ctx, id)
}
func (b *Broker) Stop(ctx context.Context) error {
	b.mu.Lock()
	ids := make([]string, 0, len(b.agents))
	for id := range b.agents {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	// Shutdown must not destroy sessions — detach so a restart can re-attach.
	for _, id := range ids {
		b.detach(id)
	}
	if b.server != nil {
		return b.server.Shutdown(ctx)
	}
	return nil
}
