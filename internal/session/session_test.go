package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/store"
)

type fakeAdapter struct {
	events                chan eventlog.Event
	done                  chan struct{}
	gate                  chan struct{}
	mu                    sync.Mutex
	inflight, maxInflight int
	close                 sync.Once
}

func newFake() *fakeAdapter {
	return &fakeAdapter{events: make(chan eventlog.Event, 16), done: make(chan struct{}), gate: make(chan struct{}, 4)}
}
func (f *fakeAdapter) Capabilities() agentadapter.Capabilities {
	return agentadapter.Capabilities{Structured: true}
}
func (f *fakeAdapter) Events() <-chan eventlog.Event { return f.events }
func (f *fakeAdapter) Done() <-chan struct{}         { return f.done }
func (f *fakeAdapter) Prompt(ctx context.Context, _ []agentadapter.PromptBlock) (string, error) {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.maxInflight {
		f.maxInflight = f.inflight
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-f.gate:
	}
	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()
	return "end_turn", nil
}
func (f *fakeAdapter) SendInput([]byte) error                 { return nil }
func (f *fakeAdapter) Resize(uint16, uint16) error            { return nil }
func (f *fakeAdapter) RespondPermission(string, string) error { return nil }
func (f *fakeAdapter) Interrupt() error                       { return nil }
func (f *fakeAdapter) Close(context.Context) error {
	f.close.Do(func() { close(f.events); close(f.done) })
	return nil
}
func (f *fakeAdapter) SessionID() string { return "s" }
func (f *fakeAdapter) PID() int          { return 1 }

func testSession(t *testing.T) (*Session, *fakeAdapter, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := eventlog.New("api-1", db, 2)
	if err != nil {
		t.Fatal(err)
	}
	a := newFake()
	s, err := New("api-1", "api-1", agentadapter.Spec{}, a, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Dispose(context.Background()); db.Close() })
	return s, a, db
}

func TestPromptSerializationAndIndependentDurableEvents(t *testing.T) {
	s, a, _ := testSession(t)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "hi"}})
			done <- err
		}()
	}
	deadline := time.Now().Add(time.Second)
	for !s.ActiveTurn() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	a.gate <- struct{}{}
	a.gate <- struct{}{}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if a.maxInflight != 1 {
		t.Fatalf("adapter saw %d concurrent prompts", a.maxInflight)
	}
	history, err := s.Log.FullHistory()
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%d err=%v", len(history), err)
	}
	if history[0].Seq != 1 || history[1].Seq != 2 {
		t.Fatalf("sequences: %+v", history)
	}
}

func TestApprovalsInterruptAndRepeatedDispose(t *testing.T) {
	s, a, _ := testSession(t)
	payload, _ := json.Marshal(map[string]any{"kind": "permission_request", "reqId": "p1", "toolCallId": "t1", "title": "run", "options": []map[string]string{{"optionId": "yes", "name": "Yes"}}})
	a.events <- eventlog.Event{Kind: "permission_request", Payload: payload}
	deadline := time.Now().Add(time.Second)
	for len(s.PendingApprovals()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Status() != Blocked {
		t.Fatalf("status=%s", s.Status())
	}
	if err := s.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingApprovals()) != 0 {
		t.Fatal("interrupt retained approvals")
	}
	if err := s.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
}
