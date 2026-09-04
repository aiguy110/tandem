// Package wsserver implements Tandem's authenticated, multiplexed browser
// WebSocket protocol.
package wsserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/historyimport"
	"github.com/aiguy110/tandem/internal/registry"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
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
	ListWorkspaceEntries(context.Context, string, string) ([]registry.WorkspaceEntry, error)
	ListGitRefs(context.Context, string) ([]workspace.GitRefInfo, error)
	ClosePreview(context.Context, string) (*workspace.ClosePreview, error)
	Diff(context.Context, string) (*workspace.Diff, error)
	AgentCatalog() registry.Catalog
	SetMode(context.Context, string, string) error
	SetConfigOption(context.Context, string, string, any) error
	CaptureSnapshot(context.Context, string, string) (store.BrowserSnapshot, error)
	ListSnapshots() ([]store.BrowserSnapshot, error)
	DeleteSnapshot(string) error
	RestartBrowser(context.Context, string, string) error
	ListProfiles(string) ([]store.Profile, []string, error)
	RenameProfile(string, string) error
	DeleteProfile(string) error
	Rename(string, string) error
	ListAnnotations(string) ([]store.Annotation, error)
	UpsertAnnotation(store.Annotation) error
	DeleteAnnotation(string) error
	ClearAnnotations(string) (int, error)
	SetAudioEnabled(string, bool) error
	SetAudioFocus(string, string, bool) error
	SetAudioPosition(string, int64, int64) (int64, error)
	AudioPosition(string) (*store.AudioPosition, error)
}

type Options struct {
	Token      string
	Registry   Backend
	Fallback   http.Handler
	WriteQueue int
	Browser    *browser.Broker
	History    HistoryLifecycle
	Automation AutomationStore
	// AudioReadySeqs returns durable rendered-audio metadata for snapshot
	// hydration. Audio bytes are still fetched from the authenticated API.
	AudioReadySeqs func(string) []int64
	// AudioReady is AudioReadySeqs plus each clip's known duration (0 means
	// unknown), for spacing timeline tick marks without downloading audio.
	AudioReady func(string) []store.MessageAudioClip
}

type AutomationStore interface {
	AutomationJobs(string) ([]store.AutomationJob, error)
	AutomationRuns(string, int) ([]store.AutomationRun, error)
	SetAutomationJobEnabled(string, bool) error
}

type HistoryLifecycle interface {
	TriggerStale(string)
	Refresh(string, bool) (bool, error)
	Status(string) ([]historyimport.AgentStatus, error)
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
	PromptID       string                     `json:"promptId"`
	OptionID       string                     `json:"optionId"`
	ModeID         string                     `json:"modeId"`
	ConfigID       string                     `json:"configId"`
	Value          any                        `json:"value"`
	Spec           agentadapter.Spec          `json:"spec"`
	Agent          string                     `json:"agent"`
	Harness        string                     `json:"harness"`
	ACPArgs        []string                   `json:"acpArgs"`
	CWD            string                     `json:"cwd"`
	Repo           string                     `json:"repo"`
	Force          bool                       `json:"force"`
	DeleteWorktree *bool                      `json:"deleteWorktree"`
	SessionID      string                     `json:"sessionId"`
	Source         string                     `json:"source"`
	Query          string                     `json:"query"`
	Limit          int                        `json:"limit"`
	MaxHits        int                        `json:"maxHitsPerSession"`
	Reindex        bool                       `json:"reindex"`
	InterruptFirst bool                       `json:"interrupt"`
	Action         string                     `json:"action"`
	Event          browser.BrowserInputEvent  `json:"event"`
	Name           string                     `json:"name"`
	ID             string                     `json:"id"`
	SnapshotID     string                     `json:"snapshotId"`
	Project        string                     `json:"project"`
	RepositoryID   string                     `json:"repositoryId"`
	Enabled        bool                       `json:"enabled"`
	Focused        bool                       `json:"focused"`
	Seq            int64                      `json:"seq"`
	Role           string                     `json:"role"`
	Quote          string                     `json:"quote"`
	Comment        string                     `json:"comment"`
	Path           string                     `json:"path"`
	PositionMs     int64                      `json:"positionMs"`
}

type connection struct {
	server       *Handler
	ws           *websocket.Conn
	out          chan []byte
	frameReady   chan string
	frameMu      sync.Mutex
	latestFrames map[string][]byte
	done         chan struct{}
	closeOnce    sync.Once
	mu           sync.Mutex
	subs         map[string]*subscription
	audioFocusID string
}

func newConnection(h *Handler, ws *websocket.Conn) *connection {
	return &connection{
		server: h, ws: ws,
		out:        make(chan []byte, h.opts.WriteQueue),
		frameReady: make(chan string, 16), latestFrames: make(map[string][]byte),
		done: make(chan struct{}), subs: map[string]*subscription{},
	}
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
		// Control/state messages take priority over lossy screencast frames.
		select {
		case data := <-c.out:
			if !c.write(data) {
				return
			}
			continue
		default:
		}
		select {
		case data := <-c.out:
			if !c.write(data) {
				return
			}
		case agentID := <-c.frameReady:
			c.frameMu.Lock()
			data := c.latestFrames[agentID]
			delete(c.latestFrames, agentID)
			c.frameMu.Unlock()
			if len(data) > 0 && !c.write(data) {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *connection) write(data []byte) bool {
	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if c.ws.WriteMessage(websocket.TextMessage, data) != nil {
		c.close()
		return false
	}
	return true
}

func (c *connection) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		focused := c.audioFocusID
		c.audioFocusID = ""
		for _, s := range c.subs {
			s.stop()
		}
		c.subs = map[string]*subscription{}
		c.mu.Unlock()
		if focused != "" {
			_ = c.server.opts.Registry.SetAudioFocus(focused, fmt.Sprintf("%p", c), false)
		}
		_ = c.ws.Close()
	})
}

func (c *connection) setAudioFocus(m clientMessage) {
	clientID := fmt.Sprintf("%p", c)
	c.mu.Lock()
	previous := c.audioFocusID
	if m.Focused {
		c.audioFocusID = m.AgentID
	} else if previous == m.AgentID {
		c.audioFocusID = ""
	}
	c.mu.Unlock()
	if previous != "" && previous != m.AgentID {
		_ = c.server.opts.Registry.SetAudioFocus(previous, clientID, false)
	}
	if err := c.server.opts.Registry.SetAudioFocus(m.AgentID, clientID, m.Focused); err != nil {
		c.commandError(m, err)
		return
	}
	c.commandAck(m, m.AgentID)
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

// sendFrame retains at most one unsent frame per agent. Screencast frames are
// snapshots, so delivering stale intermediate frames only increases latency.
func (c *connection) sendFrame(agentID string, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	c.frameMu.Lock()
	_, alreadyPending := c.latestFrames[agentID]
	c.latestFrames[agentID] = data
	c.frameMu.Unlock()
	if alreadyPending {
		return true
	}
	select {
	case c.frameReady <- agentID:
		return true
	case <-c.done:
		return false
	default:
		// Only subscribed browser panes enqueue frames, but avoid ever blocking
		// the CDP reader if that invariant changes.
		c.frameMu.Lock()
		delete(c.latestFrames, agentID)
		c.frameMu.Unlock()
		return false
	}
}

func withCorr(base map[string]any, raw json.RawMessage) map[string]any {
	if len(raw) > 0 && string(raw) != "null" {
		base["corrId"] = raw
	}
	return base
}

func (c *connection) sendAutomation(repositoryID string, corrID json.RawMessage) {
	if c.server.opts.Automation == nil {
		c.send(withCorr(map[string]any{"t": "automation", "error": "automation unavailable"}, corrID))
		return
	}
	jobs, err := c.server.opts.Automation.AutomationJobs(repositoryID)
	if err != nil {
		c.send(withCorr(map[string]any{"t": "automation", "error": err.Error()}, corrID))
		return
	}
	runs, err := c.server.opts.Automation.AutomationRuns("", 100)
	if err != nil {
		c.send(withCorr(map[string]any{"t": "automation", "error": err.Error()}, corrID))
		return
	}
	if jobs == nil {
		jobs = []store.AutomationJob{}
	}
	if runs == nil {
		runs = []store.AutomationRun{}
	}
	c.send(withCorr(map[string]any{"t": "automation", "jobs": jobs, "runs": runs}, corrID))
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
	case "list_workspace_entries":
		if m.AgentID == "" {
			c.send(withCorr(map[string]any{"t": "workspace_entries", "error": "agentId is required"}, m.CorrID))
			return
		}
		entries, err := c.server.opts.Registry.ListWorkspaceEntries(context.Background(), m.AgentID, m.Path)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "workspace_entries", "error": err.Error()}, m.CorrID))
			return
		}
		if entries == nil {
			entries = []registry.WorkspaceEntry{}
		}
		c.send(withCorr(map[string]any{"t": "workspace_entries", "entries": entries}, m.CorrID))
	case "list_automation":
		c.sendAutomation(m.RepositoryID, m.CorrID)
	case "set_automation_enabled":
		if c.server.opts.Automation == nil {
			c.send(withCorr(map[string]any{"t": "automation", "error": "automation unavailable"}, m.CorrID))
			return
		}
		if m.ID == "" {
			c.send(withCorr(map[string]any{"t": "automation", "error": "id is required"}, m.CorrID))
			return
		}
		if err := c.server.opts.Automation.SetAutomationJobEnabled(m.ID, m.Enabled); err != nil {
			c.send(withCorr(map[string]any{"t": "automation", "error": err.Error()}, m.CorrID))
			return
		}
		c.sendAutomation(m.RepositoryID, m.CorrID)
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
		if c.server.opts.History != nil {
			c.server.opts.History.TriggerStale("")
		}
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
	case "search_sessions":
		if c.server.opts.History != nil {
			c.server.opts.History.TriggerStale("")
		}
		backend, ok := c.server.opts.Registry.(interface {
			SearchSessions(context.Context, string, int, int) ([]registry.SessionSearchResult, error)
		})
		if !ok {
			c.send(withCorr(map[string]any{"t": "session_search", "error": "session history search is unsupported"}, m.CorrID))
			return
		}
		results, err := backend.SearchSessions(context.Background(), m.Query, m.Limit, m.MaxHits)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "session_search", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "session_search", "query": m.Query, "results": results}, m.CorrID))
	case "refresh_history":
		if c.server.opts.History == nil {
			c.commandError(m, errors.New("session history import is unsupported"))
			return
		}
		scheduled, err := c.server.opts.History.Refresh(m.Agent, m.Reindex)
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(map[string]any{"t": "history_refresh", "agent": m.Agent, "reindex": m.Reindex, "scheduled": scheduled}, m.CorrID))
	case "history_status":
		if c.server.opts.History == nil {
			c.send(withCorr(map[string]any{"t": "history_status", "agents": []any{}}, m.CorrID))
			return
		}
		status, err := c.server.opts.History.Status(m.Agent)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "history_status", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "history_status", "agents": status}, m.CorrID))
	case "resume_session":
		backend, ok := c.server.opts.Registry.(interface {
			Resume(context.Context, string, string, string, string) (*session.Session, error)
		})
		if !ok {
			c.commandError(m, errors.New("session resume is unsupported"))
			return
		}
		sess, err := backend.Resume(context.Background(), m.SessionID, m.Agent, m.CWD, m.Source)
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
	case "shell_open":
		backend, ok := c.server.opts.Registry.(interface {
			OpenUserShell(string, uint16, uint16) error
		})
		if !ok {
			c.commandError(m, errors.New("user shell is unsupported"))
			return
		}
		if m.Cols < 0 || m.Cols > 65535 || m.Rows < 0 || m.Rows > 65535 {
			c.commandError(m, errors.New("invalid terminal size"))
			return
		}
		if err := backend.OpenUserShell(m.AgentID, uint16(m.Cols), uint16(m.Rows)); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, m.AgentID)
	case "shell_input":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		data, err := base64.StdEncoding.DecodeString(m.BytesB64)
		if err != nil {
			c.commandError(m, errors.New("invalid base64 input"))
			return
		}
		if err := sess.UserShellInput(data); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "shell_resize":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if m.Cols < 1 || m.Cols > 65535 || m.Rows < 1 || m.Rows > 65535 {
			c.commandError(m, errors.New("invalid terminal size"))
			return
		}
		if err := sess.UserShellResize(uint16(m.Cols), uint16(m.Rows)); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "shell_close":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		sess.CloseUserShell()
		c.commandAck(m, sess.ID)
	case "prompt":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		blocks := m.Blocks
		if blocks == nil {
			blocks = []agentadapter.PromptBlock{{Type: "text", Text: m.Text}}
		}
		receipt, err := sess.EnqueuePrompt(context.Background(), blocks)
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(map[string]any{"t": "ack", "agentId": sess.ID, "promptId": receipt.ID, "disposition": receipt.Disposition, "position": receipt.Position}, m.CorrID))
	case "aside":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		receipt, err := sess.EnqueueAside(context.Background(), m.Text)
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(map[string]any{"t": "ack", "agentId": sess.ID, "promptId": receipt.ID, "disposition": receipt.Disposition, "position": receipt.Position}, m.CorrID))
	case "remove_queued_prompt":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if m.PromptID == "" || !sess.RemoveQueuedPrompt(m.PromptID) {
			c.commandError(m, errors.New("queued prompt not found"))
			return
		}
		c.commandAck(m, sess.ID)
	case "clear_prompt_queue":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		count := sess.ClearPromptQueue()
		c.send(withCorr(map[string]any{"t": "ack", "agentId": sess.ID, "cleared": count}, m.CorrID))
	case "interrupt_and_clear_queue":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		count := sess.ClearPromptQueue()
		if err := sess.Interrupt(); err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(map[string]any{"t": "ack", "agentId": sess.ID, "cleared": count}, m.CorrID))
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
	case "set_mode":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if m.ModeID == "" {
			c.commandError(m, errors.New("modeId is required"))
			return
		}
		if err := c.server.opts.Registry.SetMode(context.Background(), sess.ID, m.ModeID); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, sess.ID)
	case "set_config_option":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if m.ConfigID == "" || m.Value == nil {
			c.commandError(m, errors.New("configId and value are required"))
			return
		}
		if err := c.server.opts.Registry.SetConfigOption(context.Background(), sess.ID, m.ConfigID, m.Value); err != nil {
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
		options, err := c.server.opts.Registry.SpawnOptions(context.Background(), m.Agent, m.Harness, m.ACPArgs, m.CWD)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "spawn_options", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "spawn_options", "options": options}, m.CorrID))
	case "capture_snapshot":
		snap, err := c.server.opts.Registry.CaptureSnapshot(context.Background(), m.AgentID, m.Name)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "snapshots", "error": err.Error()}, m.CorrID))
			return
		}
		snaps, _ := c.server.opts.Registry.ListSnapshots()
		c.send(withCorr(map[string]any{"t": "snapshots", "captured": snap, "snapshots": snaps}, m.CorrID))
	case "list_snapshots":
		snaps, err := c.server.opts.Registry.ListSnapshots()
		if err != nil {
			c.send(withCorr(map[string]any{"t": "snapshots", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "snapshots", "snapshots": snaps}, m.CorrID))
	case "delete_snapshot":
		if err := c.server.opts.Registry.DeleteSnapshot(m.ID); err != nil {
			c.send(withCorr(map[string]any{"t": "snapshots", "error": err.Error()}, m.CorrID))
			return
		}
		snaps, _ := c.server.opts.Registry.ListSnapshots()
		c.send(withCorr(map[string]any{"t": "snapshots", "snapshots": snaps}, m.CorrID))
	case "list_profiles":
		profiles, recent, err := c.server.opts.Registry.ListProfiles(m.Project)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "profiles", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "profiles", "profiles": profiles, "recent": recent, "project": m.Project}, m.CorrID))
	case "rename_profile":
		if err := c.server.opts.Registry.RenameProfile(m.ID, m.Name); err != nil {
			c.send(withCorr(map[string]any{"t": "profiles", "error": err.Error()}, m.CorrID))
			return
		}
		profiles, recent, _ := c.server.opts.Registry.ListProfiles(m.Project)
		c.send(withCorr(map[string]any{"t": "profiles", "profiles": profiles, "recent": recent, "project": m.Project}, m.CorrID))
	case "rename_agent":
		if err := c.server.opts.Registry.Rename(m.AgentID, m.Name); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAgents()
		c.commandAck(m, m.AgentID)
	case "set_audio_enabled":
		if err := c.server.opts.Registry.SetAudioEnabled(m.AgentID, m.Enabled); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, m.AgentID)
	case "set_audio_focus":
		c.setAudioFocus(m)
	case "set_audio_position":
		updatedAt, err := c.server.opts.Registry.SetAudioPosition(m.AgentID, m.Seq, m.PositionMs)
		if err != nil {
			c.commandError(m, err)
			return
		}
		// The sending client already knows its own position (it's the one
		// driving playback); broadcasting back to it would just fight its
		// local clock with a slightly-stale echo. Other connections watching
		// the same agent (a second device, or a background rail view) do
		// need this to keep a resumed player in sync, so they get it.
		c.server.broadcastAudioPosition(m.AgentID, m.Seq, m.PositionMs, updatedAt, c)
		c.commandAck(m, m.AgentID)
	case "delete_profile":
		if err := c.server.opts.Registry.DeleteProfile(m.ID); err != nil {
			c.send(withCorr(map[string]any{"t": "profiles", "error": err.Error()}, m.CorrID))
			return
		}
		profiles, recent, _ := c.server.opts.Registry.ListProfiles(m.Project)
		c.send(withCorr(map[string]any{"t": "profiles", "profiles": profiles, "recent": recent, "project": m.Project}, m.CorrID))
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
	case "get_diff":
		diff, err := c.server.opts.Registry.Diff(context.Background(), m.AgentID)
		if err != nil {
			c.send(withCorr(map[string]any{"t": "diff", "error": err.Error()}, m.CorrID))
			return
		}
		c.send(withCorr(map[string]any{"t": "diff", "diff": diff}, m.CorrID))
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
	case "browser_control":
		if c.server.opts.Browser == nil {
			c.commandError(m, errors.New("browser subsystem disabled"))
			return
		}
		if _, ok := c.requireSession(m); !ok {
			return
		}
		switch m.Action {
		case "grab":
			c.server.opts.Browser.Grab(m.AgentID)
		case "release":
			if err := c.server.opts.Browser.Release(m.AgentID); err != nil {
				c.commandError(m, err)
				return
			}
		default:
			c.commandError(m, errors.New("invalid browser control action"))
			return
		}
		c.commandAck(m, m.AgentID)
	case "restart_browser":
		if _, ok := c.requireSession(m); !ok {
			return
		}
		if err := c.server.opts.Registry.RestartBrowser(context.Background(), m.AgentID, m.SnapshotID); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, m.AgentID)
	case "browser_input":
		if c.server.opts.Browser == nil {
			c.commandError(m, errors.New("browser subsystem disabled"))
			return
		}
		if _, ok := c.requireSession(m); !ok {
			return
		}
		// Preserve input order. In particular, a release must never overtake a
		// press or race a synthetic click on separate goroutines.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := c.server.opts.Browser.DispatchUserInput(ctx, m.AgentID, m.Event)
		cancel()
		if err != nil {
			c.commandError(m, err)
			return
		}
		if len(m.CorrID) > 0 && string(m.CorrID) != "null" {
			c.commandAck(m, m.AgentID)
		}
	case "add_annotation":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		now := time.Now().UnixMilli()
		ann := store.Annotation{
			ID:        "ann-" + randHex(8),
			AgentID:   sess.ID,
			Seq:       m.Seq,
			Role:      m.Role,
			Quote:     m.Quote,
			Comment:   m.Comment,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := c.server.opts.Registry.UpsertAnnotation(ann); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAnnotations(sess.ID)
		c.commandAck(m, sess.ID)
	case "update_annotation":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		annotations, err := c.server.opts.Registry.ListAnnotations(sess.ID)
		if err != nil {
			c.commandError(m, err)
			return
		}
		var found *store.Annotation
		for i := range annotations {
			if annotations[i].ID == m.ID {
				found = &annotations[i]
				break
			}
		}
		if found == nil {
			c.commandError(m, errors.New("no such annotation: "+m.ID))
			return
		}
		found.Comment = m.Comment
		found.UpdatedAt = time.Now().UnixMilli()
		if err := c.server.opts.Registry.UpsertAnnotation(*found); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAnnotations(sess.ID)
		c.commandAck(m, sess.ID)
	case "delete_annotation":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if err := c.server.opts.Registry.DeleteAnnotation(m.ID); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAnnotations(sess.ID)
		c.commandAck(m, sess.ID)
	case "clear_annotations":
		sess, ok := c.requireSession(m)
		if !ok {
			return
		}
		if _, err := c.server.opts.Registry.ClearAnnotations(sess.ID); err != nil {
			c.commandError(m, err)
			return
		}
		c.server.broadcastAnnotations(sess.ID)
		c.commandAck(m, sess.ID)
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

// randHex returns n random bytes hex-encoded, for daemon-assigned annotation
// ids (mirrors internal/registry's id-generation pattern).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:n*2]
	}
	return hex.EncodeToString(b)
}

// broadcastAnnotations sends the current annotation list for agentID to every
// connection subscribed to it, for cross-device tray sync after a mutation.
// Modeled on broadcastClosed.
func (h *Handler) broadcastAnnotations(agentID string) {
	annotations, err := h.opts.Registry.ListAnnotations(agentID)
	if err != nil {
		return
	}
	if annotations == nil {
		annotations = []store.Annotation{}
	}
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.mu.Lock()
		_, subscribed := c.subs[agentID]
		c.mu.Unlock()
		if subscribed {
			c.send(map[string]any{"t": "annotations", "agentId": agentID, "annotations": annotations})
		}
	}
}

// broadcastAudioPosition tells every other connection subscribed to agentID
// (never the sender — see the set_audio_position handler) where playback
// currently stands, so a second device's player can resume from the same
// spot. It never touches the store itself: the caller already wrote the
// position and hands over the exact values written, so this stays a cheap
// in-memory fan-out even though the client throttles these to roughly one
// every 5 seconds during playback.
func (h *Handler) broadcastAudioPosition(agentID string, seq, positionMs, updatedAt int64, sender *connection) {
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		if c == sender {
			continue
		}
		c.mu.Lock()
		_, subscribed := c.subs[agentID]
		c.mu.Unlock()
		if subscribed {
			c.send(map[string]any{"t": "audio_position", "agentId": agentID, "seq": seq, "positionMs": positionMs, "updatedAt": updatedAt})
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
	case "raw_pty", "shell_pty", "shell_exit":
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
	// browserStateOff is registered for every subscription (even base, no
	// screencast) so the Browser tab enables the moment the agent provisions a
	// browser, without waiting for the pane to be opened. browserFrameOff is the
	// screencast, gated on the 'browser' channel (the focus/bandwidth rule).
	browserStateOff func()
	browserFrameOff func()
}

func (s *subscription) wants(ev eventlog.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channels[channelOf(ev.Kind)]
}
func (s *subscription) stop() {
	if s.active.Swap(false) {
		if s.unlisten != nil {
			s.unlisten()
		}
		s.mu.Lock()
		stateOff, frameOff := s.browserStateOff, s.browserFrameOff
		s.browserStateOff, s.browserFrameOff = nil, nil
		s.mu.Unlock()
		if frameOff != nil {
			frameOff()
		}
		if stateOff != nil {
			stateOff()
		}
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
		// Status transitions (turn start/end, blocked/unblocked) are the
		// moments the workspace's git state is likely to have changed, so
		// refresh every client's agent list rather than waiting for the next
		// spawn/resume/terminal action to happen to recompute it.
		if le.Event.Kind == "status" {
			go c.server.broadcastAgents()
		}
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
		annotations, err := c.server.opts.Registry.ListAnnotations(sess.ID)
		if err != nil || annotations == nil {
			annotations = []store.Annotation{}
		}
		readySeqs := []int64{}
		if c.server.opts.AudioReadySeqs != nil {
			readySeqs = c.server.opts.AudioReadySeqs(sess.ID)
		}
		audioReady := []map[string]any{}
		if c.server.opts.AudioReady != nil {
			for _, clip := range c.server.opts.AudioReady(sess.ID) {
				audioReady = append(audioReady, map[string]any{"seq": clip.Seq, "durationMs": clip.DurationMs})
			}
		}
		snapshot := map[string]any{"t": "snapshot", "agentId": sess.ID, "seq": boundary, "transcript": transcript, "status": sess.Status(), "controlMode": sess.ControlMode(), "pendingApprovals": sess.PendingApprovals(), "queuedPrompts": sess.QueuedPrompts(), "annotations": annotations, "audioReadySeqs": readySeqs, "audioReady": audioReady}
		if pos, err := c.server.opts.Registry.AudioPosition(sess.ID); err == nil && pos != nil {
			snapshot["audioPosition"] = map[string]any{"seq": pos.Seq, "positionMs": pos.PositionMs, "updatedAt": pos.UpdatedAt}
		}
		c.send(snapshot)
	} else {
		for _, le := range replay.Events {
			if sub.wants(le.Event) {
				c.send(eventMessage(sess.ID, le))
			}
		}
	}
	// Queue events are durable for live replay, but waiting goroutines are not
	// resumable across a daemon restart. When queue history is in this replay,
	// end it with the daemon's authoritative current queue before releasing
	// buffered events.
	queuedPrompts := sess.QueuedPrompts()
	queueRelevant := len(queuedPrompts) > 0
	for _, le := range replay.Events {
		if le.Event.Kind == "prompt_queued" || le.Event.Kind == "prompt_started" || le.Event.Kind == "prompt_removed" {
			queueRelevant = true
			break
		}
	}
	if queueRelevant {
		c.send(map[string]any{"t": "prompt_queue", "agentId": sess.ID, "queuedPrompts": queuedPrompts})
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
	if c.server.opts.Browser != nil {
		// Browser active/owner state flows to every subscription so the Browser
		// tab enables (and survives a page refresh) as soon as the agent has a
		// browser, independent of whether this client is viewing the pane.
		offState := c.server.opts.Browser.OnState(sess.ID, func(state browser.BrowserState) {
			c.send(map[string]any{"t": "browser_state", "agentId": sess.ID, "active": state.Active, "controlOwner": state.ControlOwner})
		})
		sub.mu.Lock()
		sub.browserStateOff = offState
		sub.mu.Unlock()
		// The screencast itself stays gated on the 'browser' channel (only the
		// focused, browser-viewing client streams frames).
		if sub.channels["browser"] {
			offFrames := c.server.opts.Browser.AddFrameListener(sess.ID, func(frame browser.ScreencastFrame) {
				c.sendFrame(sess.ID, map[string]any{"t": "browser_frame", "agentId": sess.ID, "dataB64": frame.DataB64, "meta": frame.Meta})
			})
			sub.mu.Lock()
			sub.browserFrameOff = offFrames
			sub.mu.Unlock()
		}
	}
	c.send(withCorr(map[string]any{"t": "ack", "agentId": m.AgentID}, m.CorrID))
}

func (c *connection) unsubscribe(m clientMessage) {
	c.mu.Lock()
	sub := c.subs[m.AgentID]
	if sub != nil && len(m.Channels) > 0 {
		sub.mu.Lock()
		removeBrowser := false
		for _, ch := range m.Channels {
			delete(sub.channels, ch)
			if ch == "browser" {
				removeBrowser = true
			}
		}
		empty := len(sub.channels) == 0
		// Dropping the 'browser' channel stops only the screencast; the tab's
		// active-state feed stays live on the remaining base subscription.
		frameOff := sub.browserFrameOff
		if removeBrowser {
			sub.browserFrameOff = nil
		}
		sub.mu.Unlock()
		if removeBrowser && frameOff != nil {
			frameOff()
		}
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
