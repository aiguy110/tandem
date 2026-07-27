package historyimport

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
)

type lifecycleImporter struct {
	mu      sync.Mutex
	calls   int
	fresh   int
	started chan struct{}
	release chan struct{}
	result  Result
	err     error
}

func (f *lifecycleImporter) Import(ctx context.Context, _ string, _ config.History) (Result, error) {
	return f.run(ctx, false)
}
func (f *lifecycleImporter) Reindex(ctx context.Context, _ string, _ config.History) (Result, error) {
	return f.run(ctx, true)
}
func (f *lifecycleImporter) run(ctx context.Context, fresh bool) (Result, error) {
	f.mu.Lock()
	f.calls++
	if fresh {
		f.fresh++
	}
	started, release := f.started, f.release
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	return f.result, f.err
}

func newLifecycleTest(t *testing.T, importer importer, options func(*LifecycleOptions)) (*Lifecycle, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	values := LifecycleOptions{
		Store: db, Importer: importer,
		Agents: map[string]config.Agent{"test": {
			History: &config.History{Parser: "/fixture.ts", Enabled: true},
		}},
		StaleAfter: time.Hour, ScanInterval: time.Hour, MissingGrace: time.Hour,
	}
	if options != nil {
		options(&values)
	}
	lifecycle, err := NewLifecycle(values)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lifecycle.Close)
	return lifecycle, db
}

func TestLifecycleRefreshIsNonblockingSingleflightAndReindexable(t *testing.T) {
	fake := &lifecycleImporter{started: make(chan struct{}, 2), release: make(chan struct{})}
	lifecycle, _ := newLifecycleTest(t, fake, nil)
	start := time.Now()
	scheduled, err := lifecycle.Refresh("test", false)
	if err != nil || !scheduled || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("refresh scheduled=%v err=%v duration=%v", scheduled, err, time.Since(start))
	}
	<-fake.started
	if scheduled, err := lifecycle.Refresh("test", true); err != nil || scheduled {
		t.Fatalf("overlap scheduled=%v err=%v", scheduled, err)
	}
	close(fake.release)
	eventually(t, func() bool {
		status, _ := lifecycle.Status("test")
		return len(status) == 1 && !status[0].Running
	})
	fake.release = nil
	if scheduled, err := lifecycle.Refresh("test", true); err != nil || !scheduled {
		t.Fatalf("reindex scheduled=%v err=%v", scheduled, err)
	}
	eventually(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.fresh == 1
	})
}

func TestLifecycleStartupPeriodicAndShutdownCancellation(t *testing.T) {
	fake := &lifecycleImporter{started: make(chan struct{}, 4), release: make(chan struct{})}
	lifecycle, _ := newLifecycleTest(t, fake, func(o *LifecycleOptions) {
		o.ScanInterval = 10 * time.Millisecond
	})
	parent, cancel := context.WithCancel(context.Background())
	lifecycle.Start(parent)
	<-fake.started
	// Periodic ticks fold into the running scan.
	time.Sleep(30 * time.Millisecond)
	fake.mu.Lock()
	if fake.calls != 1 {
		t.Fatalf("overlapping startup/periodic calls=%d", fake.calls)
	}
	fake.mu.Unlock()
	cancel()
	done := make(chan struct{})
	go func() { lifecycle.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel active importer")
	}
}

func TestLifecycleFailureIsPerAgentAndRetainsIndex(t *testing.T) {
	fake := &lifecycleImporter{err: errors.New("vendor unavailable")}
	lifecycle, db := newLifecycleTest(t, fake, nil)
	if err := db.ReplaceHistorySession(
		store.HistorySession{Source: "history", Agent: "test", ExternalID: "old", SourceKey: "old", Resumable: true},
		[]store.HistoryEntry{{ExternalID: "entry", Ordinal: 1, Text: "durable lifecycle sentinel"}},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Refresh("test", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		status, _ := lifecycle.Status("test")
		return len(status) == 1 && !status[0].Running
	})
	hits, err := db.SearchHistory("sentinel", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("failed scan removed last-good index: hits=%#v err=%v", hits, err)
	}
}

func TestLifecycleOnlySchedulesStaleAgents(t *testing.T) {
	fake := &lifecycleImporter{}
	now := time.Unix(10_000, 0)
	lifecycle, db := newLifecycleTest(t, fake, func(o *LifecycleOptions) {
		o.Now = func() time.Time { return now }
	})
	runID, err := db.StartHistoryImportRun("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishHistoryImportRun(runID, 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	// Store timestamps use wall clock, so make the lifecycle's clock relative
	// to the persisted completion for deterministic freshness checks.
	run, _ := db.LatestHistoryImportRun("test")
	now = time.UnixMilli(*run.CompletedAt)
	lifecycle.TriggerStale("test")
	time.Sleep(10 * time.Millisecond)
	fake.mu.Lock()
	if fake.calls != 0 {
		t.Fatalf("fresh agent was scanned %d times", fake.calls)
	}
	fake.mu.Unlock()
	now = now.Add(2 * time.Hour)
	lifecycle.TriggerStale("test")
	eventually(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.calls == 1
	})
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}
