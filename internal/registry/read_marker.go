package registry

import (
	"errors"
	"log/slog"
	"sync"
)

// readMarkers records, per session, the highest transcript seq a user has
// acknowledged (by viewing the agent in some Tandem UI). Completed-turn
// notifications are raised by each browser from live events; publishing the
// marker in agent summaries lets every browser — including a federation
// parent's — clear a notification once the user has seen that turn anywhere.
// It is deliberately in-memory: browsers never resurrect a completed-turn
// notification from replay, so a restart has nothing stale to clear.
type readMarkers struct {
	mu   sync.Mutex
	seqs map[string]int64
}

func (m *readMarkers) get(id string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seqs[id]
}

// advance raises id's marker to seq and reports whether it moved. Markers
// never move backwards, so a stale or reordered request is a no-op.
func (m *readMarkers) advance(id string, seq int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seqs == nil {
		m.seqs = map[string]int64{}
	}
	if seq <= m.seqs[id] {
		return false
	}
	m.seqs[id] = seq
	return true
}

func (m *readMarkers) forget(id string) {
	m.mu.Lock()
	delete(m.seqs, id)
	m.mu.Unlock()
}

// MarkRead records that a user has seen sessionID's transcript through seq.
// It reports whether the marker advanced, so callers broadcast only changes.
func (r *Registry) MarkRead(sessionID string, seq int64) (bool, error) {
	if seq < 1 {
		return false, errors.New("invalid read seq")
	}
	if r.Get(sessionID) == nil {
		return false, errors.New("no such agent")
	}
	changed := r.read.advance(sessionID, seq)
	slog.Debug("session read marker", "session_id", sessionID, "seq", seq, "advanced", changed)
	return changed, nil
}
