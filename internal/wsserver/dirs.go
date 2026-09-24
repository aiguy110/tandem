package wsserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

// dirsFreshFor is how long a completed repository scan satisfies list_dirs
// without starting another; reopening the spawn palette in quick succession
// should not rescan every project root.
var dirsFreshFor = 10 * time.Second

// dirsScanTimeout bounds one scan of the project roots (a walk plus two git
// calls per repository), so a wedged filesystem cannot pin a scan forever.
const dirsScanTimeout = 2 * time.Minute

// dirsCache holds the last repository scan. list_dirs answers from it at once
// and revalidates in the background; every completed scan is broadcast, which
// also reaches a federation master through the loopback event stream.
type dirsCache struct {
	mu       sync.Mutex
	dirs     []workspace.RepoInfo
	loaded   bool
	at       time.Time
	scanning chan struct{}
	err      error
}

// profileCatalog is the batch profile listing; optional so test backends
// need not implement it.
type profileCatalog interface {
	ListAllProfiles() ([]store.Profile, map[string][]string, error)
}

// WarmDirs scans the project roots in the background so the first spawn
// palette after a restart opens against a populated list.
func (h *Handler) WarmDirs() {
	go func() { _, _ = h.refreshDirs("boot", nil) }()
}

func (h *Handler) cachedDirs() ([]workspace.RepoInfo, bool, bool) {
	h.dirs.mu.Lock()
	defer h.dirs.mu.Unlock()
	fresh := h.dirs.loaded && time.Since(h.dirs.at) < dirsFreshFor
	return h.dirs.dirs, h.dirs.loaded, fresh
}

// refreshDirs runs one scan, or joins the scan already in flight, and
// broadcasts a successful result to every connection but skip, which answers
// its own correlated request instead.
func (h *Handler) refreshDirs(reason string, skip *connection) ([]workspace.RepoInfo, error) {
	h.dirs.mu.Lock()
	if wait := h.dirs.scanning; wait != nil {
		h.dirs.mu.Unlock()
		<-wait
		h.dirs.mu.Lock()
		defer h.dirs.mu.Unlock()
		return h.dirs.dirs, h.dirs.err
	}
	done := make(chan struct{})
	h.dirs.scanning = done
	h.dirs.mu.Unlock()

	started := time.Now()
	slog.Info("repository scan started", "reason", reason)
	ctx, cancel := context.WithTimeout(context.Background(), dirsScanTimeout)
	dirs, err := h.opts.Registry.ListDirs(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	cancel()
	if dirs == nil {
		dirs = []workspace.RepoInfo{}
	}

	h.dirs.mu.Lock()
	h.dirs.scanning = nil
	h.dirs.err = err
	if err == nil {
		h.dirs.dirs, h.dirs.loaded, h.dirs.at = dirs, true, time.Now()
	}
	h.dirs.mu.Unlock()
	close(done)

	if err != nil {
		slog.Warn("repository scan failed", "reason", reason, "elapsed_ms", time.Since(started).Milliseconds(), "error", err)
		return dirs, err
	}
	slog.Info("repository scan finished", "reason", reason, "repos", len(dirs), "elapsed_ms", time.Since(started).Milliseconds())
	h.broadcast(map[string]any{"t": "dirs", "dirs": dirs}, skip)
	return dirs, nil
}

// listDirs answers list_dirs off the read loop. A cached list is returned at
// once, flagged as refreshing while a background scan revalidates it; the
// scan's broadcast then supersedes it.
func (c *connection) listDirs(m clientMessage) {
	cached, loaded, fresh := c.server.cachedDirs()
	if fresh {
		c.send(withCorr(map[string]any{"t": "dirs", "dirs": cached}, m.CorrID))
		return
	}
	if loaded {
		c.send(withCorr(map[string]any{"t": "dirs", "dirs": cached, "refreshing": true}, m.CorrID))
		go func() { _, _ = c.server.refreshDirs("list_dirs", nil) }()
		return
	}
	dirs, err := c.server.refreshDirs("list_dirs", c)
	if err != nil {
		c.send(withCorr(map[string]any{"t": "ack", "error": err.Error()}, m.CorrID))
		return
	}
	c.send(withCorr(map[string]any{"t": "dirs", "dirs": dirs}, m.CorrID))
}

// allProfilesMessage is the batch profile snapshot: every profile plus each
// project's recency list.
func (h *Handler) allProfilesMessage() (map[string]any, error) {
	catalog, ok := h.opts.Registry.(profileCatalog)
	if !ok {
		return nil, errors.New("batch profile listing unavailable")
	}
	profiles, recent, err := catalog.ListAllProfiles()
	if err != nil {
		return nil, err
	}
	if recent == nil {
		recent = map[string][]string{}
	}
	return map[string]any{"t": "profiles", "profiles": profiles, "recentByProject": recent}, nil
}

// broadcastProfiles pushes the batch profile snapshot after a profile change,
// so every client's cached palette (including a master's, via the loopback)
// sees new recency without asking.
func (h *Handler) broadcastProfiles(reason string) {
	msg, err := h.allProfilesMessage()
	if err != nil {
		slog.Warn("profile broadcast skipped", "reason", reason, "error", err)
		return
	}
	h.broadcast(msg, nil)
}

func (h *Handler) broadcast(value map[string]any, skip *connection) {
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		if c != skip {
			connections = append(connections, c)
		}
	}
	h.mu.Unlock()
	for _, c := range connections {
		// Each connection's send normalizes the envelope in place; give each
		// its own copy.
		copied := make(map[string]any, len(value))
		for k, v := range value {
			copied[k] = v
		}
		c.send(copied)
	}
}
