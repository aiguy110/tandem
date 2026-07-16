// Package wsserver implements Tandem's authenticated, multiplexed browser
// WebSocket protocol.
package wsserver

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/workspace"
	"github.com/gorilla/websocket"
)

type Backend interface {
	Get(string) *session.Session
	Spawn(context.Context, agentadapter.Spec) (*session.Session, error)
	SpawnOptions(context.Context, string, string, []string, string) (registry.SpawnOptions, error)
	Close(context.Context, string, bool, bool) (bool, error)
	Summaries(context.Context) []registry.Summary
	ListDirs(context.Context) ([]workspace.RepoInfo, error)
	ListGitRefs(context.Context, string) ([]workspace.GitRefInfo, error)
	ClosePreview(context.Context, string) (*workspace.ClosePreview, error)
	AgentCatalog() registry.Catalog
}

type Options struct {
	Token      string
	Registry   Backend
	Fallback   http.Handler
	WriteQueue int
}

type Handler struct {
	opts        Options
	upgrader    websocket.Upgrader
	mu          sync.Mutex
	connections map[*connection]struct{}
}

func New(opts Options) *Handler {
	if opts.WriteQueue <= 0 {
		opts.WriteQueue = 8192
	}
	return &Handler{opts: opts, upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: false}, connections: map[*connection]struct{}{}}
}

// Close terminates upgraded sockets, which net/http intentionally no longer
// owns after hijacking them during the WebSocket handshake.
func (h *Handler) Close() {
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.close()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		if h.opts.Fallback != nil {
			h.opts.Fallback.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if !tokenMatches(r.URL.Query().Get("token"), h.opts.Token) {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4401, "unauthorized"), time.Now().Add(time.Second))
		_ = ws.Close()
		return
	}
	c := newConnection(h, ws)
	h.mu.Lock()
	h.connections[c] = struct{}{}
	h.mu.Unlock()
	c.run()
	h.mu.Lock()
	delete(h.connections, c)
	h.mu.Unlock()
}

func tokenMatches(got, want string) bool {
	return want != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type clientMessage struct {
	T              string                     `json:"t"`
	AgentID        string                     `json:"agentId"`
	Channels       []string                   `json:"channels"`
	SinceSeq       int64                      `json:"sinceSeq"`
	CorrID         json.RawMessage            `json:"corrId"`
	Text           string                     `json:"text"`
	Blocks         []agentadapter.PromptBlock `json:"blocks"`
	BytesB64       string                     `json:"bytesB64"`
	Cols           int                        `json:"cols"`
	Rows           int                        `json:"rows"`
	ReqID          string                     `json:"reqId"`
	OptionID       string                     `json:"optionId"`
	Spec           agentadapter.Spec          `json:"spec"`
	Agent          string                     `json:"agent"`
	Profile        string                     `json:"profile"`
	ACPArgs        []string                   `json:"acpArgs"`
	CWD            string                     `json:"cwd"`
	Repo           string                     `json:"repo"`
	Force          bool                       `json:"force"`
	DeleteWorktree *bool                      `json:"deleteWorktree"`
	SessionID      string                     `json:"sessionId"`
	InterruptFirst bool                       `json:"interrupt"`
}

type connection struct {
	server    *Handler
	ws        *websocket.Conn
	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	subs      map[string]*subscription
}

func newConnection(h *Handler, ws *websocket.Conn) *connection {
	return &connection{server: h, ws: ws, out: make(chan []byte, h.opts.WriteQueue), done: make(chan struct{}), subs: map[string]*subscription{}}
}

func (c *connection) run() {
	go c.writeLoop()
	c.ws.SetReadLimit(1 << 20)
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			break
		}
		var m clientMessage
		if json.Unmarshal(data, &m) != nil {
			c.send(map[string]any{"t": "ack", "error": "invalid message"})
			continue
		}
		c.handle(m)
	}
	c.close()
}

func (c *connection) writeLoop() {
	for {
		select {
		case data := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if c.ws.WriteMessage(websocket.TextMessage, data) != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *connection) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		for _, s := range c.subs {
			s.stop()
		}
		c.subs = map[string]*subscription{}
		c.mu.Unlock()
		_ = c.ws.Close()
	})
}

func (c *connection) send(value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	select {
	case c.out <- data:
		return true
	case <-c.done:
		return false
	default:
		c.close()
		return false
	}
}

func withCorr(base map[string]any, raw json.RawMessage) map[string]any {
	if len(raw) > 0 && string(raw) != "null" {
		base["corrId"] = raw
	}
	return base
}

func (c *connection) handle(m clientMessage) {
	switch m.T {
	case "subscribe":
		c.subscribe(m)
	case "unsubscribe":
		c.unsubscribe(m)
	case "list_agents":
		c.send(withCorr(map[string]any{"t": "agents", "agents": c.server.opts.Registry.Summaries(context.Background())}, m.CorrID))
	case "list_dirs":
		dirs, err := c.server.opts.Registry.ListDirs(context.Background())
		if err != nil {
			c.send(withCorr(map[string]any{"t": "ack", "error": err.Error()}, m.CorrID))
			return
		}
		if dirs == nil {
			dirs = []workspace.RepoInfo{}
		}
		c.send(withCorr(map[string]any{"t": "dirs", "dirs": dirs}, m.CorrID))
	case "list_git_refs":
		refs, err := c.server.opts.Registry.ListGitRefs(context.Background(), m.Repo)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "git_refs", "error": err.Error()}, m.CorrID))
			return
		}
		if refs == nil {
			refs = []workspace.GitRefInfo{}
		}
		c.send(withCorr(map[string]any{"t": "git_refs", "refs": refs}, m.CorrID))
	case "list_agent_catalog":
		c.send(withCorr(map[string]any{"t": "agent_catalog", "catalog": c.server.opts.Registry.AgentCatalog()}, m.CorrID))
	case "list_sessions":
		backend, ok := c.server.opts.Registry.(interface {
			ResumeCatalog(context.Context) (registry.ResumeCatalog, error)
		})
		if !ok {
			c.commandError(m, errors.New("session resume is unsupported"))
			return
		}
		catalog, err := backend.ResumeCatalog(context.Background())
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(map[string]any{"t": "sessions", "catalog": catalog}, m.CorrID))
	case "resume_session":
		backend, ok := c.server.opts.Registry.(interface {
			Resume(context.Context, string, string, string) (*session.Session, error)
		})
		if !ok {
			c.commandError(m, errors.New("session resume is unsupported"))
			return
		}
		sess, err := backend.Resume(context.Background(), m.SessionID, m.Agent, m.CWD)
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAgents()
		c.commandAck(m, sess.ID)
	case "enter_terminal":
		backend, ok := c.server.opts.Registry.(interface {
			EnterTerminal(context.Context, string, bool) error
		})
		if !ok {
			c.commandError(m, errors.New("terminal handoff is unsupported"))
			return
		}
		if err := backend.EnterTerminal(context.Background(), m.AgentID, m.InterruptFirst); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAgents()
		c.commandAck(m, m.AgentID)
	case "leave_terminal":
		backend, ok := c.server.opts.Registry.(interface {
			LeaveTerminal(context.Context, string) error
		})
		if !ok {
			c.commandError(m, errors.New("terminal handoff is unsupported"))
			return
		}
		if err := backend.LeaveTerminal(context.Background(), m.AgentID); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAgents()
		c.commandAck(m, m.AgentID)
	case "prompt":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		blocks := m.Blocks
		if blocks == nil {
			blocks = []agentadapter.PromptBlock{{Type: "text", Text: m.Text}}
		}
		if err := sess.ValidatePrompt(blocks); err != nil {
			c.commandError(m, err)
			return
		}
		go func() { _, _ = sess.Prompt(context.Background(), blocks) }()
		c.commandAck(m, sess.ID)
	case "input":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		data, err := base64.StdEncoding.DecodeString(m.BytesB64)
		if err != nil {
			c.commandError(m, errors.New("invalid base64 input"))
			return
		}
		if err := sess.SendInput(data); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "resize":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if m.Cols < 1 || m.Cols > 65535 || m.Rows < 1 || m.Rows > 65535 {
			c.commandError(m, errors.New("invalid terminal size"))
			return
		}
		if err := sess.Resize(uint16(m.Cols), uint16(m.Rows)); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "permission_response":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if err := sess.RespondPermission(m.ReqID, m.OptionID); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "interrupt":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if err := sess.Interrupt(); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "spawn_agent":
		sess, err := c.server.opts.Registry.Spawn(context.Background(), m.Spec)
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "get_spawn_options":
		options, err := c.server.opts.Registry.SpawnOptions(context.Background(), m.Agent, m.Profile, m.ACPArgs, m.CWD)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "spawn_options", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "spawn_options", "options": options}, m.CorrID))
	case "get_close_preview":
		preview, err := c.server.opts.Registry.ClosePreview(context.Background(), m.AgentID)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "close_preview", "error": err.Error()}, m.CorrID))
			return
		}
		if preview == nil {
			c.send(withCorr(map[string]any{"t": "close_preview", "error": "no such agent"}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "close_preview", "preview": preview}, m.CorrID))
	case "close_agent":
		deleteWorktree := true
		if m.DeleteWorktree != nil {
			deleteWorktree = *m.DeleteWorktree
		}
		closed, err := c.server.opts.Registry.Close(context.Background(), m.AgentID, m.Force, deleteWorktree)
		if err != nil {
			c.commandError(m, err)
			return
		}
		if !closed {
			c.commandError(m, errors.New("no such agent"))
			return
		}
		c.server.broadcastClosed(m.AgentID)
		c.commandAck(m, m.AgentID)
	case "merge_back":
		c.send(withCorr(map[string]any{"t": "ack", "agentId": m.AgentID, "error": "merge_back not implemented yet"}, m.CorrID))
	default:
		c.send(withCorr(map[string]any{"t": "ack", "error": m.T + " not implemented yet"}, m.CorrID))
	}
}

func (c *connection) commandAck(m clientMessage, agentID string) {
	c.send(withCorr(map[string]any{"t": "ack", "agentId": agentID}, m.CorrID))
}
func (c *connection) commandError(m clientMessage, err error) {
	value := map[string]any{"t": "ack", "error": err.Error()}
	if m.AgentID != "" {
		value["agentId"] = m.AgentID
	}
	c.send(withCorr(value, m.CorrID))
}
func (c *connection) requireSession(m clientMessage) (*session.Session, bool) {
	sess := c.server.opts.Registry.Get(m.AgentID)
	if sess == nil {
		c.commandError(m, errors.New("no such agent: "+m.AgentID))
		return nil, false
	}
	return sess, true
}

func (h *Handler) broadcastClosed(agentID string) {
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.mu.Lock()
		sub := c.subs[agentID]
		if sub != nil {
			sub.stop()
			delete(c.subs, agentID)
		}
		c.mu.Unlock()
		if sub != nil {
			c.send(map[string]any{"t": "agent_closed", "agentId": agentID})
		}
	}
}

func (h *Handler) broadcastAgents() {
	agents := h.opts.Registry.Summaries(context.Background())
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.send(map[string]any{"t": "agents", "agents": agents})
	}
}

var allChannels = []string{"transcript", "pty", "terminals", "browser", "status"}

func makeChannels(in []string) map[string]bool {
	if len(in) == 0 {
		in = allChannels
	}
	out := map[string]bool{}
	for _, v := range in {
		for _, known := range allChannels {
			if v == known {
				out[v] = true
			}
		}
	}
	return out
}
func channelOf(kind string) string {
	switch kind {
	case "raw_pty":
		return "pty"
	case "terminal_output":
		return "terminals"
	case "status":
		return "status"
	default:
		return "transcript"
	}
}

type subscription struct {
	c            *connection
	s            *session.Session
	channels     map[string]bool
	mu           sync.Mutex
	initializing bool
	pending      []eventlog.LoggedEvent
	active       atomic.Bool
	unlisten     func()
}

func (s *subscription) wants(ev eventlog.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channels[channelOf(ev.Kind)]
}
func (s *subscription) stop() {
	if s.active.Swap(false) && s.unlisten != nil {
		s.unlisten()
	}
}

func (c *connection) subscribe(m clientMessage) {
	sess := c.server.opts.Registry.Get(m.AgentID)
	if sess == nil {
		c.send(withCorr(map[string]any{"t": "ack", "error": "no such agent: " + m.AgentID}, m.CorrID))
		return
	}
	sub := &subscription{c: c, s: sess, channels: makeChannels(m.Channels), initializing: true}
	sub.active.Store(true)
	sub.unlisten = sess.OnEvent(func(le eventlog.LoggedEvent) {
		if !sub.active.Load() {
			return
		}
		sub.mu.Lock()
		if !sub.channels[channelOf(le.Event.Kind)] {
			sub.mu.Unlock()
			return
		}
		if sub.initializing {
			sub.pending = append(sub.pending, le)
			sub.mu.Unlock()
			return
		}
		sub.mu.Unlock()
		c.send(eventMessage(sess.ID, le))
	})
	c.mu.Lock()
	if old := c.subs[sess.ID]; old != nil {
		old.stop()
	}
	c.subs[sess.ID] = sub
	c.mu.Unlock()
	replay, err := sess.Log.ReplaySince(m.SinceSeq)
	if err != nil {
		sub.stop()
		c.send(withCorr(map[string]any{"t": "ack", "agentId": m.AgentID, "error": err.Error()}, m.CorrID))
		return
	}
	boundary := int64(0)
	if replay.Source != eventlog.ReplaySnapshot {
		boundary = m.SinceSeq
	}
	if len(replay.Events) > 0 {
		boundary = replay.Events[len(replay.Events)-1].Seq
	}
	if replay.Source == eventlog.ReplaySnapshot {
		transcript := make([]map[string]any, 0, len(replay.Events))
		for _, le := range replay.Events {
			if sub.wants(le.Event) {
				transcript = append(transcript, map[string]any{"seq": le.Seq, "event": wireEvent(le.Event)})
			}
		}
		c.send(map[string]any{"t": "snapshot", "agentId": sess.ID, "seq": boundary, "transcript": transcript, "status": sess.Status(), "controlMode": "transcript", "pendingApprovals": sess.PendingApprovals()})
	} else {
		for _, le := range replay.Events {
			if sub.wants(le.Event) {
				c.send(eventMessage(sess.ID, le))
			}
		}
	}
	sub.mu.Lock()
	pending := append([]eventlog.LoggedEvent{}, sub.pending...)
	sub.pending = nil
	sub.initializing = false
	sub.mu.Unlock()
	for _, le := range pending {
		if le.Seq > boundary {
			c.send(eventMessage(sess.ID, le))
		}
	}
	c.send(withCorr(map[string]any{"t": "ack", "agentId": m.AgentID}, m.CorrID))
}

func (c *connection) unsubscribe(m clientMessage) {
	c.mu.Lock()
	sub := c.subs[m.AgentID]
	if sub != nil && len(m.Channels) > 0 {
		sub.mu.Lock()
		for _, ch := range m.Channels {
			delete(sub.channels, ch)
		}
		empty := len(sub.channels) == 0
		sub.mu.Unlock()
		if empty {
			sub.stop()
			delete(c.subs, m.AgentID)
		}
	} else if sub != nil {
		sub.stop()
		delete(c.subs, m.AgentID)
	}
	c.mu.Unlock()
	c.send(withCorr(map[string]any{"t": "ack", "agentId": m.AgentID}, m.CorrID))
}

func eventMessage(id string, le eventlog.LoggedEvent) map[string]any {
	return map[string]any{"t": "event", "agentId": id, "seq": le.Seq, "event": wireEvent(le.Event)}
}
func wireEvent(ev eventlog.Event) json.RawMessage { b, _ := ev.NormalizedJSON(); return b }
