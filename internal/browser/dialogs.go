package browser

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DialogsPath prefixes the authenticated endpoints tandem mcp-browser uses to
// read a browser's pending JavaScript dialogs and answer one.
const DialogsPath = "/internal/browser/dialogs"

// ErrNoDialog reports that no JavaScript dialog is open (or that it closed
// before it could be answered).
var ErrNoDialog = errors.New("no JavaScript dialog is open")

// PendingDialog is a JavaScript dialog (alert, confirm, prompt or
// beforeunload) that is blocking a page until someone answers it.
type PendingDialog struct {
	TargetID      string `json:"targetId"`
	Type          string `json:"type"`
	Message       string `json:"message"`
	DefaultPrompt string `json:"defaultPrompt,omitempty"`
	URL           string `json:"url,omitempty"`
	OpenedAt      int64  `json:"openedAt"`
}

// dialogWatcher keeps its own direct CDP connection with a Page-enabled
// session on every page target, so it sees each dialog as it opens. A dialog
// blocks the page's renderer: a session attached afterwards cannot enable the
// Page domain and so can neither see nor answer it. That is why the watcher
// attaches to pages as they are created rather than when a dialog is
// suspected, and why it uses a connection separate from the human viewer,
// whose reconnects re-run Page.enable on the viewed page.
type dialogWatcher struct {
	wsURL, ownerID string
	onChange       func()

	mu       sync.Mutex
	conn     *websocket.Conn
	pending  map[int64]chan cdpResponse
	nextID   int64
	sessions map[string]string // CDP session -> target, current conn only
	targets  map[string]string // target -> CDP session ("" while attaching)
	dialogs  map[string]PendingDialog
	// observers maps a dialog's target to the session that saw it open. Only
	// that session can answer it: one attached later cannot enable Page.
	observers map[string]string
	closed    bool
	closedCh  chan struct{}
	writeMu   transportMutex
}

func newDialogWatcher(wsURL, ownerID string, onChange func()) *dialogWatcher {
	return &dialogWatcher{
		wsURL: wsURL, ownerID: ownerID, onChange: onChange,
		pending: make(map[int64]chan cdpResponse), sessions: make(map[string]string),
		targets: make(map[string]string), dialogs: make(map[string]PendingDialog), observers: make(map[string]string), closedCh: make(chan struct{}),
	}
}

func (w *dialogWatcher) start() { go w.run() }

func (w *dialogWatcher) run() {
	backoff := 100 * time.Millisecond
	for {
		done, err := w.connectOnce()
		if err == nil {
			backoff = 100 * time.Millisecond
			select {
			case <-done:
			case <-w.closedCh:
				return
			}
		} else {
			slog.Debug("browser dialog watcher connect failed", "session_id", w.ownerID, "error", err)
		}
		select {
		case <-w.closedCh:
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

func (w *dialogWatcher) connectOnce() (<-chan struct{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, w.wsURL, nil)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("dialog watcher closed")
	}
	w.conn = conn
	w.mu.Unlock()
	done := make(chan struct{})
	go w.readLoop(conn, done)
	if err := w.call(ctx, conn, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := w.call(ctx, conn, "", "Target.getTargets", map[string]any{}, &targets); err != nil {
		_ = conn.Close()
		return nil, err
	}
	live := make(map[string]bool)
	for _, target := range targets.TargetInfos {
		if target.Type == "page" {
			live[target.TargetID] = true
			go w.watch(conn, target.TargetID)
		}
	}
	// Dialogs survive a watcher reconnect (a new session cannot rediscover
	// them), but not the page that raised them.
	w.mu.Lock()
	changed := false
	for targetID := range w.dialogs {
		if !live[targetID] {
			delete(w.dialogs, targetID)
			delete(w.observers, targetID)
			changed = true
		}
	}
	w.mu.Unlock()
	if changed {
		w.notify()
	}
	return done, nil
}

func (w *dialogWatcher) watch(conn *websocket.Conn, targetID string) {
	w.mu.Lock()
	if w.conn != conn {
		w.mu.Unlock()
		return
	}
	if _, ok := w.targets[targetID]; ok {
		w.mu.Unlock()
		return
	}
	w.targets[targetID] = ""
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := w.call(ctx, conn, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		w.mu.Lock()
		if w.conn == conn && w.targets[targetID] == "" {
			delete(w.targets, targetID)
		}
		w.mu.Unlock()
		slog.Debug("browser dialog watcher attach failed", "session_id", w.ownerID, "target_id", targetID, "error", err)
		return
	}
	w.mu.Lock()
	if w.conn != conn {
		w.mu.Unlock()
		return
	}
	w.targets[targetID] = attached.SessionID
	w.sessions[attached.SessionID] = targetID
	w.mu.Unlock()
	if err := w.call(ctx, conn, attached.SessionID, "Page.enable", map[string]any{}, nil); err != nil {
		// Typically a dialog was already open when this page was first seen.
		slog.Warn("browser dialog watcher could not enable page events", "session_id", w.ownerID, "target_id", targetID, "error", err)
	}
}

func (w *dialogWatcher) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			w.mu.Lock()
			if w.conn == conn {
				w.conn = nil
				w.sessions, w.targets = make(map[string]string), make(map[string]string)
			}
			pending := w.pending
			w.pending = make(map[int64]chan cdpResponse)
			w.mu.Unlock()
			for _, ch := range pending {
				select {
				case ch <- cdpResponse{Err: err}:
				default:
				}
			}
			_ = conn.Close()
			return
		}
		var message cdpEnvelope
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message.ID != 0 {
			w.mu.Lock()
			ch := w.pending[message.ID]
			delete(w.pending, message.ID)
			w.mu.Unlock()
			if ch != nil {
				if message.Error != nil {
					ch <- cdpResponse{Err: fmt.Errorf("CDP %d: %s", message.Error.Code, message.Error.Message)}
				} else {
					ch <- cdpResponse{Result: message.Result}
				}
			}
			continue
		}
		w.handleEvent(conn, message)
	}
}

func (w *dialogWatcher) handleEvent(conn *websocket.Conn, message cdpEnvelope) {
	switch message.Method {
	case "Target.targetCreated":
		var params struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(message.Params, &params) == nil && params.TargetInfo.Type == "page" {
			go w.watch(conn, params.TargetInfo.TargetID)
		}
	case "Target.targetDestroyed":
		var params struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return
		}
		w.mu.Lock()
		session := w.targets[params.TargetID]
		delete(w.targets, params.TargetID)
		delete(w.sessions, session)
		_, had := w.dialogs[params.TargetID]
		delete(w.dialogs, params.TargetID)
		delete(w.observers, params.TargetID)
		w.mu.Unlock()
		if had {
			w.notify()
		}
	case "Target.detachedFromTarget":
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return
		}
		w.mu.Lock()
		if targetID, ok := w.sessions[params.SessionID]; ok {
			delete(w.sessions, params.SessionID)
			if w.targets[targetID] == params.SessionID {
				delete(w.targets, targetID)
			}
		}
		w.mu.Unlock()
	case "Page.javascriptDialogOpening":
		var params struct {
			URL           string `json:"url"`
			Message       string `json:"message"`
			Type          string `json:"type"`
			DefaultPrompt string `json:"defaultPrompt"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return
		}
		w.mu.Lock()
		targetID := w.sessions[message.SessionID]
		if targetID != "" {
			w.observers[targetID] = message.SessionID
			w.dialogs[targetID] = PendingDialog{TargetID: targetID, Type: params.Type, Message: params.Message, DefaultPrompt: params.DefaultPrompt, URL: params.URL, OpenedAt: time.Now().UnixMilli()}
		}
		w.mu.Unlock()
		if targetID == "" {
			return
		}
		// The message is page content; log only its size.
		slog.Info("browser dialog opened", "session_id", w.ownerID, "target_id", targetID, "type", params.Type, "message_bytes", len(params.Message))
		w.notify()
	case "Page.javascriptDialogClosed":
		var params struct {
			Result bool `json:"result"`
		}
		_ = json.Unmarshal(message.Params, &params)
		w.mu.Lock()
		targetID := w.sessions[message.SessionID]
		_, had := w.dialogs[targetID]
		delete(w.dialogs, targetID)
		delete(w.observers, targetID)
		w.mu.Unlock()
		if had {
			slog.Info("browser dialog closed", "session_id", w.ownerID, "target_id", targetID, "accepted", params.Result)
			w.notify()
		}
	}
}

func (w *dialogWatcher) notify() {
	if w.onChange != nil {
		w.onChange()
	}
}

func (w *dialogWatcher) call(ctx context.Context, conn *websocket.Conn, session, method string, params, out any) error {
	w.mu.Lock()
	w.nextID++
	id := w.nextID
	response := make(chan cdpResponse, 1)
	w.pending[id] = response
	w.mu.Unlock()
	message := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		message["sessionId"] = session
	}
	payload, err := json.Marshal(message)
	if err == nil {
		err = writeTransport(ctx, &w.writeMu, conn, websocket.TextMessage, payload)
	}
	if err != nil {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return err
	}
	select {
	case got := <-response:
		if got.Err != nil {
			return got.Err
		}
		if out != nil && len(got.Result) > 0 {
			return json.Unmarshal(got.Result, out)
		}
		return nil
	case <-ctx.Done():
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return ctx.Err()
	case <-w.closedCh:
		return errors.New("dialog watcher closed")
	}
}

// Dialogs returns the open dialogs, oldest first.
func (w *dialogWatcher) Dialogs() []PendingDialog {
	w.mu.Lock()
	out := make([]PendingDialog, 0, len(w.dialogs))
	for _, dialog := range w.dialogs {
		out = append(out, dialog)
	}
	w.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAt != out[j].OpenedAt {
			return out[i].OpenedAt < out[j].OpenedAt
		}
		return out[i].TargetID < out[j].TargetID
	})
	return out
}

// Handle answers the dialog on targetID (or the only open dialog when
// targetID is empty). accept confirms it — for beforeunload, leaves the page.
func (w *dialogWatcher) Handle(ctx context.Context, targetID string, accept bool, promptText string) (PendingDialog, error) {
	w.mu.Lock()
	if targetID == "" {
		if len(w.dialogs) > 1 {
			w.mu.Unlock()
			return PendingDialog{}, errors.New("several dialogs are open; name the page (targetId)")
		}
		for id := range w.dialogs {
			targetID = id
		}
	}
	dialog, ok := w.dialogs[targetID]
	conn, session := w.conn, w.observers[targetID]
	reachable := conn != nil && session != "" && w.sessions[session] == targetID
	w.mu.Unlock()
	if !ok {
		return PendingDialog{}, ErrNoDialog
	}
	if !reachable {
		return dialog, errors.New("Tandem lost its connection to this page after the dialog opened and cannot answer it; close the tab or restart the browser")
	}
	params := map[string]any{"accept": accept}
	if promptText != "" {
		params["promptText"] = promptText
	}
	err := w.call(ctx, conn, session, "Page.handleJavaScriptDialog", params, nil)
	if err != nil && !strings.Contains(err.Error(), "No dialog is showing") {
		slog.Warn("browser dialog answer failed", "session_id", w.ownerID, "target_id", targetID, "error", err)
		return dialog, err
	}
	w.mu.Lock()
	_, had := w.dialogs[targetID]
	delete(w.dialogs, targetID)
	delete(w.observers, targetID)
	w.mu.Unlock()
	if had {
		w.notify()
	}
	if err != nil {
		return dialog, ErrNoDialog
	}
	slog.Info("browser dialog answered", "session_id", w.ownerID, "target_id", targetID, "type", dialog.Type, "accept", accept)
	return dialog, nil
}

func (w *dialogWatcher) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	conn := w.conn
	w.conn = nil
	close(w.closedCh)
	w.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// DialogStatus is what the dialog endpoint reports for one browser instance.
type DialogStatus struct {
	ControlOwner ControlOwner    `json:"controlOwner"`
	Dialogs      []PendingDialog `json:"dialogs"`
}

// DialogControlOptions configures the agent-facing dialog endpoints.
type DialogControlOptions struct {
	Token       string
	Broker      *Broker
	AgentExists func(string) bool
}

// DialogControl serves DialogsPath: GET reports a browser's control owner and
// open dialogs, POST /handle answers one. Reading never provisions a browser.
type DialogControl struct{ opts DialogControlOptions }

func NewDialogControl(opts DialogControlOptions) *DialogControl { return &DialogControl{opts: opts} }

func (d *DialogControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := r.Header.Get("Authorization")
	if d.opts.Token == "" || subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+d.opts.Token)) != 1 {
		takeoverJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if d.opts.Broker == nil {
		takeoverJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "browser subsystem disabled"})
		return
	}
	keyFor := func(sessionID, browserID string) (string, bool) {
		if sessionID == "" || d.opts.AgentExists == nil || !d.opts.AgentExists(sessionID) {
			takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
			return "", false
		}
		if browserID == "" {
			browserID = DefaultBrowserID
		}
		if !ValidBrowserID(browserID) {
			takeoverJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid browserId"})
			return "", false
		}
		return InstanceKey(sessionID, browserID), true
	}
	switch {
	case r.URL.Path == DialogsPath && r.Method == http.MethodGet:
		key, ok := keyFor(r.URL.Query().Get("sessionId"), r.URL.Query().Get("browserId"))
		if !ok {
			return
		}
		takeoverJSON(w, http.StatusOK, DialogStatus{ControlOwner: d.opts.Broker.Owner(key), Dialogs: d.opts.Broker.Dialogs(key)})
	case r.URL.Path == DialogsPath+"/handle" && r.Method == http.MethodPost:
		var body struct {
			SessionID  string `json:"sessionId"`
			BrowserID  string `json:"browserId"`
			TargetID   string `json:"targetId"`
			Accept     bool   `json:"accept"`
			PromptText string `json:"promptText"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body) != nil {
			takeoverJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		key, ok := keyFor(body.SessionID, body.BrowserID)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		dialog, err := d.opts.Broker.HandleDialog(ctx, key, body.TargetID, body.Accept, body.PromptText)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, ErrNoDialog) {
				status = http.StatusNotFound
			}
			takeoverJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		takeoverJSON(w, http.StatusOK, map[string]any{"handled": true, "dialog": dialog, "remaining": d.opts.Broker.Dialogs(key)})
	default:
		takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}
