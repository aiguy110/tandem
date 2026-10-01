package messaging

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Path is the prefix of the agent-facing HTTP bridge used by the
// tandem-messages MCP server (internal/messagesmcp).
const Path = "/internal/messages/"

// ServeHTTP serves POST /internal/messages/<tool> for ACP agents' MCP bridge.
// Every request carries the calling agent's session ID and the daemon's
// bearer token. A failure is {"error": "..."} with a 4xx status; the error
// text starts with the machine-readable code (for example "no_link: ...").
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	want, got := "Bearer "+s.opts.Token, r.Header.Get("Authorization")
	if s.opts.Token == "" || len(want) != len(got) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	tool := strings.TrimPrefix(r.URL.Path, Path)
	var req struct {
		SessionID      string `json:"sessionId"`
		Query          string `json:"query"`
		To             string `json:"to"`
		Body           string `json:"body"`
		ThreadID       string `json:"threadId"`
		TimeoutMinutes int    `json:"timeoutMinutes"`
		RequestID      string `json:"requestId"`
		Reason         string `json:"reason"`
		Card           string `json:"card"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": ErrInvalid + ": invalid request body"})
		return
	}
	log := slog.With("tool", tool, "session_id", req.SessionID)
	var (
		value any
		err   error
	)
	switch tool {
	case "directory":
		var entries []DirectoryEntry
		entries, err = s.Directory(r.Context(), req.SessionID, req.Query)
		value = map[string]any{"agents": nonNil(entries)}
	case "send":
		value, err = s.Send(r.Context(), req.SessionID, req.To, req.Body, req.ThreadID)
	case "ask":
		value, err = s.Ask(r.Context(), req.SessionID, req.To, req.Body, req.TimeoutMinutes)
	case "reply":
		value, err = s.Reply(r.Context(), req.SessionID, req.RequestID, req.Body)
	case "decline":
		value, err = s.Decline(r.Context(), req.SessionID, req.RequestID, req.Reason)
	case "request_link":
		value, err = s.RequestLinkWait(r.Context(), req.SessionID, req.To, req.Reason)
	case "set_card":
		if err = s.SetCard(req.SessionID, req.Card); err == nil {
			value = map[string]any{}
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown messages operation"})
		return
	}
	if err != nil {
		var rej *Rejection
		status := http.StatusInternalServerError
		msg := err.Error()
		if errors.As(err, &rej) {
			status = http.StatusBadRequest
		}
		log.Warn("agent messages tool failed", "status", status, "error", msg)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func nonNil(entries []DirectoryEntry) []DirectoryEntry {
	if entries == nil {
		return []DirectoryEntry{}
	}
	return entries
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
