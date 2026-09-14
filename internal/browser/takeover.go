package browser

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type TakeoverOptions struct {
	Token       string
	AgentExists func(string) bool
	OnRequest   func(sessionID, reqID, reason string)
	OnResolved  func(sessionID, reqID string)
}

type takeover struct {
	sessionID  string
	resolved bool
}

// Takeovers is the authenticated HTTP state behind tandem mcp-control. The WS
// browser release command calls Release after returning broker control.
type Takeovers struct {
	mu       sync.Mutex
	opts     TakeoverOptions
	seq      uint64
	requests map[string]*takeover
}

func NewTakeovers(opts TakeoverOptions) *Takeovers {
	return &Takeovers{opts: opts, requests: make(map[string]*takeover)}
}

func (t *Takeovers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !t.authorized(r) {
		takeoverJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch r.Method {
	case http.MethodPost:
		sessionID := r.URL.Query().Get("agentId")
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
		reqID := fmt.Sprintf("tk_%d", t.seq)
		t.requests[reqID] = &takeover{sessionID: sessionID}
		t.mu.Unlock()
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
