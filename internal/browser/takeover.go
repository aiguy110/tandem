package browser

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// takeoverStaleAfter is how long a registered takeover may go without its
// mcp-control caller polling before the request is considered abandoned. The
// caller polls every second while its tool call is alive, so silence this long
// means the agent turn (or the whole MCP process) has gone away.
const takeoverStaleAfter = 20 * time.Second

type TakeoverOptions struct {
	Token string
	// Now overrides the clock for tests.
	Now         func() time.Time
	AgentExists func(string) bool
	OnRequest   func(sessionID, reqID, reason string)
	OnResolved  func(sessionID, reqID string)
}

type takeover struct {
	sessionID string
	resolved  bool
	lastSeen  time.Time
}

// Takeovers is the authenticated HTTP state behind tandem mcp-control. The WS
// browser release command calls Release after returning broker control.
type Takeovers struct {
	mu       sync.Mutex
	opts     TakeoverOptions
	seq      uint64
	nonce    string
	requests map[string]*takeover
}

func NewTakeovers(opts TakeoverOptions) *Takeovers {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Request ids are persisted in the event log, so they must not repeat
	// across daemon restarts or an old unresolved id could alias a new one.
	nonce := strconv.FormatInt(opts.Now().UnixNano(), 36)
	return &Takeovers{opts: opts, nonce: nonce, requests: make(map[string]*takeover)}
}

func (t *Takeovers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !t.authorized(r) {
		takeoverJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch r.Method {
	case http.MethodPost:
		sessionID := r.URL.Query().Get("sessionId")
		if sessionID == "" {
			sessionID = r.URL.Query().Get("agentId")
		}
		if sessionID == "" || t.opts.AgentExists == nil || !t.opts.AgentExists(sessionID) {
			takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent or browser disabled"})
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
		if body.Reason == "" {
			body.Reason = "the agent needs you"
		}
		t.mu.Lock()
		t.seq++
		reqID := fmt.Sprintf("tk_%s_%d", t.nonce, t.seq)
		t.requests[reqID] = &takeover{sessionID: sessionID, lastSeen: t.opts.Now()}
		t.mu.Unlock()
		slog.Info("browser takeover requested", "session", sessionID, "reqId", reqID)
		if t.opts.OnRequest != nil {
			t.opts.OnRequest(sessionID, reqID, body.Reason)
		}
		takeoverJSON(w, http.StatusOK, map[string]string{"reqId": reqID})
	case http.MethodGet:
		t.mu.Lock()
		request := t.requests[r.URL.Query().Get("reqId")]
		resolved := false
		if request != nil {
			resolved = request.resolved
			request.lastSeen = t.opts.Now()
		}
		t.mu.Unlock()
		if request == nil {
			takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "no such takeover"})
			return
		}
		takeoverJSON(w, http.StatusOK, map[string]bool{"resolved": resolved})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (t *Takeovers) Release(sessionID string) {
	var resolved []string
	t.mu.Lock()
	for reqID, request := range t.requests {
		if request.sessionID == sessionID && !request.resolved {
			request.resolved = true
			resolved = append(resolved, reqID)
		}
	}
	t.mu.Unlock()
	if t.opts.OnResolved != nil {
		for _, reqID := range resolved {
			t.opts.OnResolved(sessionID, reqID)
		}
	}
}

// Live reports whether reqID is a registered, unresolved takeover whose
// mcp-control caller is still polling for it.
func (t *Takeovers) Live(reqID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	request := t.requests[reqID]
	return request != nil && !request.resolved && t.opts.Now().Sub(request.lastSeen) < takeoverStaleAfter
}

// HasLive reports whether sessionID has any live takeover request.
func (t *Takeovers) HasLive(sessionID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.opts.Now()
	for _, request := range t.requests {
		if request.sessionID == sessionID && !request.resolved && now.Sub(request.lastSeen) < takeoverStaleAfter {
			return true
		}
	}
	return false
}

// Abandon marks a takeover resolved without firing OnResolved; the caller is
// responsible for recording the resolution. It drops requests whose tool call
// has gone away so a late poll still unblocks rather than 404ing forever.
func (t *Takeovers) Abandon(reqID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if request := t.requests[reqID]; request != nil {
		request.resolved = true
	}
}

func (t *Takeovers) authorized(r *http.Request) bool {
	want := "Bearer " + t.opts.Token
	got := r.Header.Get("Authorization")
	// Query auth remains accepted for compatibility with the former Node MCP.
	if got == "" && r.URL.Query().Get("token") != "" {
		got = "Bearer " + r.URL.Query().Get("token")
	}
	return t.opts.Token != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func takeoverJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
