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
	prompts               []string
	close                 sync.Once
	image                 bool
	validateCalls         int
}

func newFake() *fakeAdapter {
	return &fakeAdapter{events: make(chan eventlog.Event, 16), done: make(chan struct{}), gate: make(chan struct{}, 4)}
}
func (f *fakeAdapter) Capabilities() agentadapter.Capabilities {
	return agentadapter.Capabilities{Structured: true, Image: f.image}
}
func (f *fakeAdapter) ValidatePrompt([]agentadapter.PromptBlock) error {
	f.mu.Lock()
	f.validateCalls++
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) validationCount() int          { f.mu.Lock(); defer f.mu.Unlock(); return f.validateCalls }
func (f *fakeAdapter) Events() <-chan eventlog.Event { return f.events }
func (f *fakeAdapter) Done() <-chan struct{}         { return f.done }
func (f *fakeAdapter) Prompt(ctx context.Context, blocks []agentadapter.PromptBlock) (string, error) {
	f.mu.Lock()
	f.inflight++
	for _, block := range blocks {
		if block.Type == "text" {
			f.prompts = append(f.prompts, block.Text)
		}
	}
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
	if err != nil {
		t.Fatal(err)
	}
	userMessages := 0
	for _, event := range history {
		if event.Event.Kind == "user_message" {
			userMessages++
		}
	}
	if userMessages != 2 {
		t.Fatalf("user messages=%d history=%+v", userMessages, history)
	}
}

func TestExplicitPromptQueueIsFIFOAndRemovable(t *testing.T) {
	s, a, _ := testSession(t)
	first, err := s.EnqueuePrompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "first"}})
	if err != nil || first.Disposition != "started" || first.Position != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	deadline := time.Now().Add(time.Second)
	for !s.ActiveTurn() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second, err := s.EnqueuePrompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "second"}})
	if err != nil || second.Disposition != "queued" || second.Position != 1 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	third, err := s.EnqueuePrompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "third"}})
	if err != nil || third.Disposition != "queued" || third.Position != 2 {
		t.Fatalf("third=%+v err=%v", third, err)
	}
	if queued := s.QueuedPrompts(); len(queued) != 2 || queued[0].ID != second.ID || queued[1].ID != third.ID {
		t.Fatalf("queue=%+v", queued)
	}
	if !s.RemoveQueuedPrompt(second.ID) {
		t.Fatal("second prompt was not removed")
	}
	a.gate <- struct{}{}
	deadline = time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		startedThird := len(a.prompts) == 2
		a.mu.Unlock()
		if startedThird || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	a.gate <- struct{}{}
	if result := <-third.done; result.err != nil {
		t.Fatal(result.err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.prompts) != 2 || a.prompts[0] != "first" || a.prompts[1] != "third" {
		t.Fatalf("prompt order=%v", a.prompts)
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

func TestDaemonOwnedPermissionUsesSharedApprovalQueue(t *testing.T) {
	s, _, _ := testSession(t)
	result := make(chan struct {
		option string
		err    error
	}, 1)
	go func() {
		option, err := s.RequestPermission(context.Background(), "automation-grant-1", "Allow browser_snapshot for this repository?", []agentadapter.ApprovalOption{
			{OptionID: "allow", Name: "Allow"},
			{OptionID: "deny", Name: "Deny"},
		})
		result <- struct {
			option string
			err    error
		}{option, err}
	}()
	deadline := time.Now().Add(time.Second)
	for len(s.PendingApprovals()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.PendingApprovals(); len(got) != 1 || got[0].ReqID != "automation-grant-1" {
		t.Fatalf("pending=%+v", got)
	}
	if err := s.RespondPermission("automation-grant-1", "allow"); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || got.option != "allow" {
		t.Fatalf("result=%+v", got)
	}
	if len(s.PendingApprovals()) != 0 || s.Status() != Working {
		t.Fatalf("pending=%+v status=%s", s.PendingApprovals(), s.Status())
	}
}

func TestImageValidationRejectsBeforeLoggingAndCountsBeforeResolution(t *testing.T) {
	t.Run("negotiated capability false", func(t *testing.T) {
		s, a, _ := testSession(t)
		_, err := s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "image", AssetID: "asset", MIMEType: "image/png"}})
		if err == nil || err.Error() != "this agent does not support image prompts" {
			t.Fatalf("err=%v", err)
		}
		history, historyErr := s.Log.FullHistory()
		if historyErr != nil || len(history) != 0 || a.validationCount() != 0 {
			t.Fatalf("history=%v err=%v validate=%d", history, historyErr, a.validationCount())
		}
	})
	t.Run("image count precedes adapter byte resolution", func(t *testing.T) {
		s, a, _ := testSession(t)
		a.image = true
		blocks := make([]agentadapter.PromptBlock, 5)
		for i := range blocks {
			blocks[i] = agentadapter.PromptBlock{Type: "image", AssetID: "asset", MIMEType: "image/png"}
		}
		err := s.ValidatePrompt(blocks)
		if err == nil || err.Error() != "prompt may contain at most 4 images" || a.validationCount() != 0 {
			t.Fatalf("err=%v validate=%d", err, a.validationCount())
		}
	})
}
