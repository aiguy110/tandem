package historyimport

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
)

const (
	defaultStaleAfter   = 5 * time.Minute
	defaultScanInterval = 30 * time.Minute
	defaultMissingGrace = 7 * 24 * time.Hour
)

type importer interface {
	Import(context.Context, string, config.History) (Result, error)
	Reindex(context.Context, string, config.History) (Result, error)
}

type LifecycleOptions struct {
	Store         *store.Store
	Importer      importer
	Agents        map[string]config.Agent
	EnsureRuntime func(context.Context) error
	StaleAfter    time.Duration
	ScanInterval  time.Duration
	MissingGrace  time.Duration
	Now           func() time.Time
}

type AgentStatus struct {
	Agent   string                  `json:"agent"`
	Running bool                    `json:"running"`
	LastRun *store.HistoryImportRun `json:"lastRun,omitempty"`
}

// Lifecycle schedules history scans while keeping all catalog/search paths
// read-only and immediate. It owns one cancellable scan per agent; failures are
// isolated and never remove the last successfully indexed transcript.
type Lifecycle struct {
	store                       *store.Store
	importer                    importer
	agents                      map[string]config.Agent
	ensure                      func(context.Context) error
	staleAfter, interval, grace time.Duration
	now                         func() time.Time

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	running  map[string]bool
	ensureMu sync.Mutex
	ensured  bool
}

func NewLifecycle(options LifecycleOptions) (*Lifecycle, error) {
	if options.Store == nil || options.Importer == nil {
		return nil, errors.New("history lifecycle requires store and importer")
	}
	if options.StaleAfter <= 0 {
		options.StaleAfter = defaultStaleAfter
	}
	if options.ScanInterval <= 0 {
		options.ScanInterval = defaultScanInterval
	}
	if options.MissingGrace <= 0 {
		options.MissingGrace = defaultMissingGrace
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Lifecycle{
		store: options.Store, importer: options.Importer, agents: options.Agents,
		ensure: options.EnsureRuntime, staleAfter: options.StaleAfter,
		interval: options.ScanInterval, grace: options.MissingGrace, now: options.Now,
		ctx: ctx, cancel: cancel, running: map[string]bool{},
	}, nil
}

// Start schedules an immediate nonblocking stale scan and periodic refreshes.
func (l *Lifecycle) Start(parent context.Context) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		select {
		case <-parent.Done():
			l.cancel()
			return
		default:
		}
		l.TriggerStale("")
		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				l.cancel()
				return
			case <-l.ctx.Done():
				return
			case <-ticker.C:
				l.TriggerStale("")
			}
		}
	}()
}

// Close cancels subprocess contexts and waits for scheduler/scan goroutines.
func (l *Lifecycle) Close() {
	l.cancel()
	l.wg.Wait()
}

// TriggerStale is safe on every Resume list/search request. It only schedules
// an import when the last completed run is stale, and returns immediately.
func (l *Lifecycle) TriggerStale(agent string) {
	for _, id := range l.agentIDs(agent) {
		run, err := l.store.LatestHistoryImportRun(id)
		if err != nil || (run != nil && run.CompletedAt != nil &&
			l.now().Sub(time.UnixMilli(*run.CompletedAt)) < l.staleAfter) {
			continue
		}
		_, _ = l.Refresh(id, false)
	}
}

// Refresh schedules one agent scan. reindex ignores checkpoints. The boolean
// reports whether a new scan was scheduled; an overlapping request is folded
// into the already-running scan.
func (l *Lifecycle) Refresh(agent string, reindex bool) (bool, error) {
	definition, ok := l.agents[agent]
	if !ok || definition.History == nil || !definition.History.Enabled {
		return false, errors.New("agent has no enabled history importer")
	}
	l.mu.Lock()
	if l.running[agent] {
		l.mu.Unlock()
		return false, nil
	}
	l.running[agent] = true
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			delete(l.running, agent)
			l.mu.Unlock()
		}()
		if err := l.ensureReady(l.ctx); err != nil {
			l.recordLifecycleFailure(agent, err)
			return
		}
		var result Result
		var err error
		if reindex {
			result, err = l.importer.Reindex(l.ctx, agent, *definition.History)
		} else {
			result, err = l.importer.Import(l.ctx, agent, *definition.History)
		}
		if err != nil || !result.Complete {
			return
		}
		if err := l.store.FinishHistoryImporterScan(agent, result.ImporterID, result.ImporterVersion, result.SourceKeys, l.grace); err != nil {
			l.recordLifecycleFailure(agent, err)
		}
	}()
	return true, nil
}

func (l *Lifecycle) ensureReady(ctx context.Context) error {
	if l.ensure == nil {
		return nil
	}
	l.ensureMu.Lock()
	defer l.ensureMu.Unlock()
	if l.ensured {
		return nil
	}
	if err := l.ensure(ctx); err != nil {
		return err
	}
	l.ensured = true
	return nil
}

func (l *Lifecycle) recordLifecycleFailure(agent string, runErr error) {
	id, err := l.store.StartHistoryImportRun(agent)
	if err == nil {
		_ = l.store.FinishHistoryImportRun(id, 0, 0, runErr)
	}
}

func (l *Lifecycle) Status(agent string) ([]AgentStatus, error) {
	ids := l.agentIDs(agent)
	out := make([]AgentStatus, 0, len(ids))
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		run, err := l.store.LatestHistoryImportRun(id)
		if err != nil {
			return nil, err
		}
		out = append(out, AgentStatus{Agent: id, Running: l.running[id], LastRun: run})
	}
	return out, nil
}

func (l *Lifecycle) agentIDs(agent string) []string {
	if agent != "" {
		return []string{agent}
	}
	ids := []string{}
	for id, definition := range l.agents {
		if definition.History != nil && definition.History.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
