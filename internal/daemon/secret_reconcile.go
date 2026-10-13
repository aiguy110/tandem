package daemon

import (
	"encoding/json"
	"log/slog"

	"github.com/aiguy110/tandem/internal/store"
)

// reconcileOrphanedSecrets closes requests persisted by an earlier daemon.
// Secret material and MCP waiters are intentionally memory-only, so replaying
// an old request as actionable would present a form that can never complete.
func reconcileOrphanedSecrets(sessionID string, events func(string, ...string) ([]store.StoredEvent, error), resolve func(string, string)) {
	reconcileOrphanedRequests(sessionID, "secret_request", "secret_resolved", events, resolve)
}

// reconcileOrphanedRequests resolves every request event (keyed by
// requestId) that has no matching resolution event. The waiters for these
// human-gated requests live in daemon memory, so after a restart they can
// never complete.
func reconcileOrphanedRequests(sessionID, requestKind, resolvedKind string, events func(string, ...string) ([]store.StoredEvent, error), resolve func(string, string)) {
	stored, err := events(sessionID, requestKind, resolvedKind)
	if err != nil {
		slog.Warn("request reconcile: read events failed", "session", sessionID, "kind", requestKind, "error", err)
		return
	}
	pending := map[string]bool{}
	for _, event := range stored {
		var payload struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal([]byte(event.Payload), &payload) != nil || payload.RequestID == "" {
			continue
		}
		if event.Kind == requestKind {
			pending[payload.RequestID] = true
		} else {
			delete(pending, payload.RequestID)
		}
	}
	for requestID := range pending {
		slog.Info("resolving orphaned request", "session", sessionID, "kind", requestKind, "request_id", requestID)
		resolve(sessionID, requestID)
	}
}
