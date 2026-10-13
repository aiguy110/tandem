package browser

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// InstanceRequestsPath prefixes the authenticated endpoints tandem
// mcp-browser uses to list, request and close browser instances.
const InstanceRequestsPath = "/internal/browser/instances"

// Source kinds an agent may request a new browser instance from.
const (
	SourceFresh        = "fresh"
	SourceSavedProfile = "saved_profile"
	SourceUserChoice   = "user_choice"
)

const (
	maxPendingInstanceRequests = 4
	maxInstanceReasonRunes     = 500
	maxInstanceNameRunes       = 60
)

// InstanceSource describes what state an agent asked a new browser to start
// from. user_choice leaves the decision (including "fresh") to the human.
type InstanceSource struct {
	Kind        string `json:"kind"`
	ProfileID   string `json:"profileId,omitempty"`
	ProfileName string `json:"profileName,omitempty"`
}

// InstanceRequest is one pending, human-gated request for an additional
// browser instance.
type InstanceRequest struct {
	RequestID string         `json:"requestId"`
	SessionID string         `json:"sessionId"`
	Source    InstanceSource `json:"source"`
	Reason    string         `json:"reason"`
	Name      string         `json:"name,omitempty"`
}

// InstanceService is the daemon-owned instance registry behind the endpoints.
type InstanceService interface {
	ListInstances(sessionID string) ([]Instance, error)
	ListSavedProfiles() ([]SavedProfileInfo, error)
	OpenInstance(ctx context.Context, sessionID, profileID, name string) (Instance, error)
	CloseInstance(ctx context.Context, sessionID, browserID string) error
}

type InstanceRequestOptions struct {
	Token       string
	Service     InstanceService
	AgentExists func(string) bool
	OnRequest   func(InstanceRequest)
	OnResolved  func(sessionID, requestID string, granted bool, browserID string)
	Now         func() time.Time
}

type pendingInstanceRequest struct {
	InstanceRequest
	done      bool
	resolving bool
	denied    bool
	instance  *Instance
}

// InstanceRequests holds pending instance requests in memory. Nothing here is
// granted until a human resolves the request through Resolve; agents can
// only list, ask, poll and close.
type InstanceRequests struct {
	opts     InstanceRequestOptions
	mu       sync.Mutex
	seq      uint64
	nonce    string
	requests map[string]*pendingInstanceRequest
}

func NewInstanceRequests(opts InstanceRequestOptions) *InstanceRequests {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	nonce := strconv.FormatInt(opts.Now().UnixNano(), 36)
	return &InstanceRequests{opts: opts, nonce: nonce, requests: make(map[string]*pendingInstanceRequest)}
}

func (q *InstanceRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !q.authorized(r) {
		takeoverJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if q.opts.Service == nil {
		takeoverJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "browser subsystem disabled"})
		return
	}
	route := strings.TrimPrefix(r.URL.Path, InstanceRequestsPath)
	switch {
	case route == "/saved-profiles" && r.Method == http.MethodGet:
		profiles, err := q.opts.Service.ListSavedProfiles()
		if err != nil {
			takeoverJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		takeoverJSON(w, http.StatusOK, map[string]any{"savedProfiles": profiles})
	case route == "" && r.Method == http.MethodGet:
		sessionID, ok := q.session(w, r.URL.Query().Get("sessionId"))
		if !ok {
			return
		}
		instances, err := q.opts.Service.ListInstances(sessionID)
		if err != nil {
			takeoverJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		takeoverJSON(w, http.StatusOK, map[string]any{"instances": instances})
	case route == "/request" && r.Method == http.MethodPost:
		var body InstanceRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body) != nil {
			takeoverJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		sessionID, ok := q.session(w, body.SessionID)
		if !ok {
			return
		}
		body.SessionID = sessionID
		requestID, err := q.request(body)
		if err != nil {
			takeoverJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		takeoverJSON(w, http.StatusOK, map[string]string{"requestId": requestID})
	case route == "/status" && r.Method == http.MethodGet:
		sessionID, requestID := r.URL.Query().Get("sessionId"), r.URL.Query().Get("requestId")
		q.mu.Lock()
		request := q.requests[requestID]
		if request == nil || request.SessionID != sessionID {
			q.mu.Unlock()
			takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "no such browser request"})
			return
		}
		status := map[string]any{"done": request.done, "denied": request.denied}
		if request.instance != nil {
			status["instance"] = request.instance
		}
		if request.done {
			// The poller has its answer; nothing else may read it.
			delete(q.requests, requestID)
		}
		q.mu.Unlock()
		takeoverJSON(w, http.StatusOK, status)
	case route == "/close" && r.Method == http.MethodPost:
		var body struct {
			SessionID string `json:"sessionId"`
			BrowserID string `json:"browserId"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body) != nil {
			takeoverJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		sessionID, ok := q.session(w, body.SessionID)
		if !ok {
			return
		}
		if err := q.opts.Service.CloseInstance(r.Context(), sessionID, body.BrowserID); err != nil {
			takeoverJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		takeoverJSON(w, http.StatusOK, map[string]bool{"closed": true})
	default:
		takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (q *InstanceRequests) session(w http.ResponseWriter, sessionID string) (string, bool) {
	if sessionID == "" || q.opts.AgentExists == nil || !q.opts.AgentExists(sessionID) {
		takeoverJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return "", false
	}
	return sessionID, true
}

func (q *InstanceRequests) request(body InstanceRequest) (string, error) {
	body.Reason = truncateRunes(strings.TrimSpace(body.Reason), maxInstanceReasonRunes)
	body.Name = truncateRunes(strings.TrimSpace(body.Name), maxInstanceNameRunes)
	if body.Reason == "" {
		return "", errors.New("reason is required")
	}
	source := body.Source
	switch source.Kind {
	case SourceFresh, SourceUserChoice:
		source.ProfileID, source.ProfileName = "", ""
	case SourceSavedProfile:
		profile, err := q.savedProfile(source.ProfileID)
		if err != nil {
			return "", err
		}
		source.ProfileName = profile.Name
	default:
		return "", fmt.Errorf("source must be %q, %q or %q", SourceFresh, SourceSavedProfile, SourceUserChoice)
	}
	body.Source = source
	q.mu.Lock()
	pending := 0
	for _, request := range q.requests {
		if request.SessionID == body.SessionID && !request.done {
			pending++
		}
	}
	if pending >= maxPendingInstanceRequests {
		q.mu.Unlock()
		return "", errors.New("too many browser requests are already waiting for the human")
	}
	q.seq++
	body.RequestID = fmt.Sprintf("bi_%s_%d", q.nonce, q.seq)
	q.requests[body.RequestID] = &pendingInstanceRequest{InstanceRequest: body}
	q.mu.Unlock()
	slog.Info("browser instance requested", "session_id", body.SessionID, "request_id", body.RequestID, "source", source.Kind, "saved_profile_id", source.ProfileID)
	if q.opts.OnRequest != nil {
		q.opts.OnRequest(body)
	}
	return body.RequestID, nil
}

func (q *InstanceRequests) savedProfile(id string) (SavedProfileInfo, error) {
	if id == "" {
		return SavedProfileInfo{}, errors.New("savedProfileId is required for a saved_profile source")
	}
	profiles, err := q.opts.Service.ListSavedProfiles()
	if err != nil {
		return SavedProfileInfo{}, err
	}
	for _, profile := range profiles {
		if profile.ID == id {
			if !profile.Compatible {
				return SavedProfileInfo{}, fmt.Errorf("saved browser profile %q was captured by a different browser driver", profile.Name)
			}
			return profile, nil
		}
	}
	return SavedProfileInfo{}, fmt.Errorf("no such saved browser profile: %s", id)
}

// Resolve records the human's decision. Approval opens the instance before
// the request is marked done, so a failed launch leaves the request pending
// for a retry or an explicit denial. For a user_choice request profileID is
// the human's pick ("" for fresh); otherwise it must match the request.
func (q *InstanceRequests) Resolve(ctx context.Context, sessionID, requestID string, granted bool, profileID string) (*Instance, error) {
	q.mu.Lock()
	request := q.requests[requestID]
	if request == nil || request.SessionID != sessionID || request.done {
		q.mu.Unlock()
		return nil, errors.New("no such pending browser request")
	}
	if request.resolving {
		q.mu.Unlock()
		return nil, errors.New("browser request is already being resolved")
	}
	request.resolving = true
	snapshot := request.InstanceRequest
	q.mu.Unlock()
	instance, err := q.resolve(ctx, snapshot, granted, profileID)
	q.mu.Lock()
	request.resolving = false
	if err != nil {
		q.mu.Unlock()
		return nil, err
	}
	request.done, request.denied, request.instance = true, !granted, instance
	q.mu.Unlock()
	browserID := ""
	if instance != nil {
		browserID = instance.ID
	}
	slog.Info("browser instance request resolved", "session_id", sessionID, "request_id", requestID, "granted", granted, "browser_id", browserID, "saved_profile_id", profileID)
	if q.opts.OnResolved != nil {
		q.opts.OnResolved(sessionID, requestID, granted, browserID)
	}
	return instance, nil
}

func (q *InstanceRequests) resolve(ctx context.Context, snapshot InstanceRequest, granted bool, profileID string) (*Instance, error) {
	sessionID := snapshot.SessionID
	var instance *Instance
	if granted {
		switch snapshot.Source.Kind {
		case SourceFresh:
			if profileID != "" {
				return nil, errors.New("this request is for a fresh browser")
			}
		case SourceSavedProfile:
			if profileID == "" {
				profileID = snapshot.Source.ProfileID
			}
			if profileID != snapshot.Source.ProfileID {
				return nil, errors.New("approve the requested saved profile or deny the request")
			}
		case SourceUserChoice:
			if profileID != "" {
				if _, err := q.savedProfile(profileID); err != nil {
					return nil, err
				}
			}
		}
		opened, err := q.opts.Service.OpenInstance(ctx, sessionID, profileID, snapshot.Name)
		if err != nil {
			slog.Warn("approved browser instance failed to open", "session_id", sessionID, "request_id", snapshot.RequestID, "error", err)
			return nil, err
		}
		instance = &opened
	}
	return instance, nil
}

// Pending reports whether requestID is still awaiting a decision.
func (q *InstanceRequests) Pending(requestID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	request := q.requests[requestID]
	return request != nil && !request.done
}

func (q *InstanceRequests) authorized(r *http.Request) bool {
	want := "Bearer " + q.opts.Token
	got := r.Header.Get("Authorization")
	return q.opts.Token != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
