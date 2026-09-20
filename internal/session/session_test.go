package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	validatedTypes        []string
	promptErr             error
}

func newFake() *fakeAdapter {
	return &fakeAdapter{events: make(chan eventlog.Event, 16), done: make(chan struct{}), gate: make(chan struct{}, 4)}
}

func TestStreamingChunksAreCoalescedAndRateLimited(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "chunks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log, err := eventlog.New("chunk-session", db, 32)
	if err != nil {
		t.Fatal(err)
	}
	adapter := newFake()
	s, err := New("chunk-session", "chunks", agentadapter.Spec{}, adapter, log)
	if err != nil {
		t.Fatal(err)
	}

	seen := make(chan eventlog.LoggedEvent, 4)
	s.OnEvent(func(event eventlog.LoggedEvent) { seen <- event })
	for _, text := range []string{"many ", "tiny ", "chunks"} {
		payload, _ := json.Marshal(map[string]any{"kind": "message_chunk", "text": text})
		adapter.events <- eventlog.Event{Kind: "message_chunk", Payload: payload}
	}

	select {
	case event := <-seen:
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Event.Payload, &payload); err != nil || payload.Text != "many tiny chunks" {
			t.Fatalf("coalesced payload=%q err=%v", payload.Text, err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced chunk")
	}
	select {
	case event := <-seen:
		t.Fatalf("unexpected extra event within rate limit: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}
func (f *fakeAdapter) Capabilities() agentadapter.Capabilities {
	return agentadapter.Capabilities{Structured: true, Image: f.image}
}

// ValidatePrompt mirrors the real ACP adapter: it only understands text/image
// blocks and rejects anything else, so quote blocks must be flattened before
// they reach it.
func (f *fakeAdapter) ValidatePrompt(blocks []agentadapter.PromptBlock) error {
	f.mu.Lock()
	f.validateCalls++
	for _, b := range blocks {
		f.validatedTypes = append(f.validatedTypes, b.Type)
	}
	f.mu.Unlock()
	for _, b := range blocks {
		switch b.Type {
		case "text", "image":
		default:
			return errors.New("invalid prompt block type " + b.Type)
		}
	}
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
	err := f.promptErr
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
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
func (f *fakeAdapter) ExternalSessionID() string { return "s" }
func (f *fakeAdapter) PID() int                  { return 1 }

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

func TestPromptFailureIsRecordedInTranscript(t *testing.T) {
	s, a, _ := testSession(t)
	a.promptErr = errors.New("ACP transport unavailable")
	a.gate <- struct{}{}
	if _, err := s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "hello"}}); err == nil {
		t.Fatal("expected prompt failure")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		history, historyErr := s.Log.FullHistory()
		if historyErr != nil {
			t.Fatal(historyErr)
		}
		for _, event := range history {
			if event.Event.Kind == "error" && strings.Contains(string(event.Event.Payload), "ACP transport unavailable") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("prompt failure was not logged")
}

func TestFlattenQuoteBlocks(t *testing.T) {
	blocks := []agentadapter.PromptBlock{
		{Type: "quote", RefSeq: 42, Role: "assistant", Quote: "the answer is 4", Comment: "is this right?"},
		{Type: "text", Text: "also please double-check the math"},
	}
	flat := flattenQuoteBlocks(blocks)
	if len(flat) != 2 {
		t.Fatalf("flattened len=%d, want 2: %+v", len(flat), flat)
	}
	if flat[0].Type != "text" {
		t.Fatalf("quote block did not flatten to text: %+v", flat[0])
	}
	want := "> [assistant] \"the answer is 4\"\n  is this right?"
	if flat[0].Text != want {
		t.Fatalf("flattened quote text = %q, want %q", flat[0].Text, want)
	}
	// The trailing free-text block passes through unchanged.
	if flat[1] != blocks[1] {
		t.Fatalf("trailing text block mutated: %+v", flat[1])
	}
	// Non-quote input is untouched (no accidental copy divergence).
	textOnly := []agentadapter.PromptBlock{{Type: "text", Text: "hi"}}
	if got := flattenQuoteBlocks(textOnly); len(got) != 1 || got[0] != textOnly[0] {
		t.Fatalf("text-only passthrough = %+v", got)
	}
}

func TestValidatePromptFlattensQuoteBeforeAdapterValidator(t *testing.T) {
	s, a, _ := testSession(t)
	blocks := []agentadapter.PromptBlock{
		{Type: "quote", RefSeq: 7, Role: "user", Quote: "original text", Comment: "expand on this"},
		{Type: "text", Text: "go ahead"},
	}
	if err := s.ValidatePrompt(blocks); err != nil {
		t.Fatalf("quote prompt should validate, got: %v", err)
	}
	// The adapter's validator must never see the raw "quote" type — only the
	// flattened text/image blocks — or it rejects it as an invalid block type.
	if got := a.validatedTypes; len(got) != 2 || got[0] != "text" || got[1] != "text" {
		t.Fatalf("adapter validated block types=%+v, want [text text]", got)
	}
}

func TestSwapAdapterClearsOldTurnAndIgnoresItsLateResult(t *testing.T) {
	s, old, _ := testSession(t)
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "stuck"}})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !s.ActiveTurn() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.ActiveTurn() {
		t.Fatal("prompt did not become active")
	}

	replacement := newFake()
	if err := s.SwapAdapter(context.Background(), func() (agentadapter.Adapter, error) {
		return replacement, nil
	}, "transcript", nil); err != nil {
		t.Fatal(err)
	}
	if s.ActiveTurn() || s.Status() != Idle || s.ControlMode() != "transcript" {
		t.Fatalf("replacement inherited old state: active=%v status=%s mode=%s", s.ActiveTurn(), s.Status(), s.ControlMode())
	}

	// Simulate the disposed adapter's Prompt call returning late. It should
	// resolve its caller without changing the replacement's state or appending
	// a misleading transcript error.
	old.gate <- struct{}{}
	if err := <-done; !errors.Is(err, errAdapterReplaced) {
		t.Fatalf("late prompt error=%v", err)
	}
	if s.ActiveTurn() || s.Status() != Idle {
		t.Fatalf("late prompt changed replacement: active=%v status=%s", s.ActiveTurn(), s.Status())
	}
	history, err := s.Log.FullHistory()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range history {
		if entry.Event.Kind == "error" {
			t.Fatalf("late old-adapter error leaked into transcript: %s", entry.Event.Payload)
		}
	}
}

func TestQuoteBlocksPersistRichButAdapterSeesFlattenedText(t *testing.T) {
	s, a, _ := testSession(t)
	blocks := []agentadapter.PromptBlock{
		{Type: "quote", RefSeq: 7, Role: "user", Quote: "original text", Comment: "expand on this"},
		{Type: "text", Text: "go ahead"},
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(context.Background(), blocks)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !s.ActiveTurn() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	a.gate <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// The adapter only ever sees flattened text — no "quote" type block, and the
	// citation rendering ahead of the trailing free text.
	if len(a.prompts) != 2 {
		t.Fatalf("adapter prompts=%+v", a.prompts)
	}
	wantQuote := "> [user] \"original text\"\n  expand on this"
	if a.prompts[0] != wantQuote || a.prompts[1] != "go ahead" {
		t.Fatalf("adapter prompts=%+v", a.prompts)
	}

	// The persisted user_message event keeps the original, structured quote
	// block so replay stays rich (not a flattened blob).
	history, err := s.Log.FullHistory()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range history {
		if event.Event.Kind != "user_message" {
			continue
		}
		var payload struct {
			Blocks []agentadapter.PromptBlock `json:"blocks"`
		}
		if err := json.Unmarshal(event.Event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Blocks) != 2 {
			t.Fatalf("persisted blocks=%+v", payload.Blocks)
		}
		q := payload.Blocks[0]
		if q.Type != "quote" || q.RefSeq != 7 || q.Role != "user" || q.Quote != "original text" || q.Comment != "expand on this" {
			t.Fatalf("persisted quote block=%+v", q)
		}
		if payload.Blocks[1].Type != "text" || payload.Blocks[1].Text != "go ahead" {
			t.Fatalf("persisted trailing block=%+v", payload.Blocks[1])
		}
		found = true
	}
	if !found {
		t.Fatal("no user_message event found in history")
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

func usageEvent(used, size float64) eventlog.Event {
	ev, err := eventlog.ParseNormalized([]byte(fmt.Sprintf(`{"kind":"usage","used":%g,"size":%g,"cost":null}`, used, size)))
	if err != nil {
		panic(err)
	}
	return ev
}

func decodeUsage(t *testing.T, ev eventlog.Event) usagePayload {
	t.Helper()
	var payload usagePayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// The usage timer must only move when the agent reports different totals:
// re-reported identical usage (turn boundaries, resumes, idle pings) keeps the
// original timestamp so the UI's "updated Xm ago" tracks real model output.
func TestUsageTimestampAdvancesOnlyOnChange(t *testing.T) {
	s, _, _ := testSession(t)
	first, err := s.append(usageEvent(1000, 200000))
	if err != nil {
		t.Fatal(err)
	}
	firstAt := decodeUsage(t, first.Event).UpdatedAt
	if firstAt <= 0 {
		t.Fatalf("expected a stamped updatedAt, got %d", firstAt)
	}
	repeat, err := s.append(usageEvent(1000, 200000))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeUsage(t, repeat.Event).UpdatedAt; got != firstAt {
		t.Fatalf("unchanged usage moved the timer: %d != %d", got, firstAt)
	}
	time.Sleep(2 * time.Millisecond)
	changed, err := s.append(usageEvent(1500, 200000))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeUsage(t, changed.Event).UpdatedAt; got <= firstAt {
		t.Fatalf("changed usage did not move the timer: %d <= %d", got, firstAt)
	}
	if got := decodeUsage(t, changed.Event).Used; got != 1500 {
		t.Fatalf("used was rewritten: %v", got)
	}
	if string(decodeUsage(t, changed.Event).Cost) != "null" {
		t.Fatalf("cost was dropped: %q", decodeUsage(t, changed.Event).Cost)
	}
}

// A daemon restart re-seeds the cached usage from SQLite, so re-reported
// identical usage after the restart still shows its original age.
func TestUsageTimestampSurvivesRestart(t *testing.T) {
	s, _, db := testSession(t)
	first, err := s.append(usageEvent(1000, 200000))
	if err != nil {
		t.Fatal(err)
	}
	firstAt := decodeUsage(t, first.Event).UpdatedAt
	log, err := eventlog.New("api-1", db, 2)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New("api-1", "api-1", agentadapter.Spec{}, newFake(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Dispose(context.Background()) })
	after, err := restarted.append(usageEvent(1000, 200000))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeUsage(t, after.Event).UpdatedAt; got != firstAt {
		t.Fatalf("restart reset the usage timer: %d != %d", got, firstAt)
	}
}
