package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

const takeoverSweepInterval = 10 * time.Second

// takeoverReconciler resolves browser takeover notifications that nobody can
// answer any more. Takeover requests are persisted in the event log, but the
// mcp-control tool call waiting on them is not: a daemon restart, a cancelled
// turn, or a dead agent process leaves a durable takeover_request with no
// matching takeover_resolved, and the UI would show it forever.
type takeoverReconciler struct {
	events func(sessionID string, kinds ...string) ([]store.StoredEvent, error)
	live   func(reqID string) bool
	// abandon unblocks any late poller for the request in memory.
	abandon func(reqID string)
	// resolve records the resolution durably for the session.
	resolve func(sessionID, reqID string)

	mu          sync.Mutex
	outstanding map[string]string // reqID -> sessionID
}

func newTakeoverReconciler() *takeoverReconciler {
	return &takeoverReconciler{outstanding: make(map[string]string)}
}

func (r *takeoverReconciler) track(sessionID, reqID string) {
	r.mu.Lock()
	r.outstanding[reqID] = sessionID
	r.mu.Unlock()
}

func (r *takeoverReconciler) forget(reqID string) {
	r.mu.Lock()
	delete(r.outstanding, reqID)
	r.mu.Unlock()
}

// scan loads a session's unresolved takeover requests from the event log so
// requests left behind by a previous daemon process are swept too.
func (r *takeoverReconciler) scan(sessionID string) {
	events, err := r.events(sessionID, "takeover_request", "takeover_resolved")
	if err != nil {
		slog.Warn("takeover reconcile: read events failed", "session", sessionID, "err", err)
		return
	}
	pending := map[string]bool{}
	for _, event := range events {
		var payload struct {
			ReqID string `json:"reqId"`
		}
		if json.Unmarshal([]byte(event.Payload), &payload) != nil || payload.ReqID == "" {
			continue
		}
		if event.Kind == "takeover_request" {
			pending[payload.ReqID] = true
		} else {
			delete(pending, payload.ReqID)
		}
	}
	for reqID := range pending {
		r.track(sessionID, reqID)
	}
}

// sweep resolves every outstanding request whose tool call is no longer live.
func (r *takeoverReconciler) sweep() {
	r.mu.Lock()
	var orphaned [][2]string
	for reqID, sessionID := range r.outstanding {
		if !r.live(reqID) {
			orphaned = append(orphaned, [2]string{sessionID, reqID})
			delete(r.outstanding, reqID)
		}
	}
	r.mu.Unlock()
	for _, o := range orphaned {
		slog.Info("resolving orphaned browser takeover", "session", o[0], "reqId", o[1])
		r.abandon(o[1])
		r.resolve(o[0], o[1])
	}
}

func (r *takeoverReconciler) run(ctx context.Context, restored <-chan struct{}, sessionIDs func() []string) {
	select {
	case <-ctx.Done():
		return
	case <-restored:
	}
	for _, id := range sessionIDs() {
		r.scan(id)
	}
	r.sweep()
	ticker := time.NewTicker(takeoverSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep()
		}
	}
}

// orphanedTakeoverStatus is the status to restore after an abandoned takeover,
// or "" when the session should keep its current status. Only a session that
// is blocked solely on takeovers is unblocked.
func orphanedTakeoverStatus(s *session.Session, otherLive bool) session.Status {
	if s.Status() != session.Blocked || otherLive || len(s.PendingApprovals()) > 0 {
		return ""
	}
	return takeoverResolvedStatus(s.ActiveTurn(), s.Status())
}
