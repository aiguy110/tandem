package registry

import "github.com/aiguy110/tandem/internal/store"

// SetAudioPosition persists (or, for seq == 0, clears) an agent's audio
// player position. It is a thin pass-through — position tracking has no
// live-session dependency, unlike SetAudioEnabled/SetAudioFocus, so it works
// the same whether or not the agent is currently running.
func (r *Registry) SetAudioPosition(sessionID string, seq, positionMs int64) (int64, error) {
	return r.store.SetAudioPosition(sessionID, seq, positionMs)
}

// AudioPosition returns an agent's persisted audio player position, or nil
// if nothing is stored.
func (r *Registry) AudioPosition(sessionID string) (*store.AudioPosition, error) {
	return r.store.AudioPosition(sessionID)
}
