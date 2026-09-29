package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
)

// seedForkSource records a closed ACP agent with a two-turn transcript:
//
//	1 user_message "first"   2 message_chunk (msg_a)   3 status
//	4 user_message "second" (quotes seq 2)   5 message_chunk (msg_b)
func seedForkSource(t *testing.T, db *store.Store, cwd string) {
	t.Helper()
	spec, _ := json.Marshal(agentadapter.Spec{Adapter: "acp", Agent: "fake"})
	ext := "sess_source"
	closed := time.Now().UnixMilli()
	if err := db.UpsertSession(store.Session{ID: "src", Name: "src", Spec: spec, CWD: cwd, ExternalSessionID: &ext, Status: "idle", CreatedAt: 1, ClosedAt: &closed}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ kind, payload string }{
		{"user_message", `{"kind":"user_message","text":"first","blocks":[{"type":"text","text":"first"}]}`},
		{"message_chunk", `{"kind":"message_chunk","text":"answer one","messageId":"msg_a"}`},
		{"status", `{"kind":"status","status":"idle"}`},
		{"user_message", `{"kind":"user_message","text":"second","blocks":[{"type":"quote","refSeq":2,"role":"assistant","quote":"answer","comment":"why"},{"type":"text","text":"second"}]}`},
		{"message_chunk", `{"kind":"message_chunk","text":"answer two","messageId":"msg_b"}`},
	} {
		if _, err := db.AppendEvent("src", e.kind, e.payload, 1); err != nil {
			t.Fatal(err)
		}
	}
}

func forkKinds(t *testing.T, db *store.Store, id string) []string {
	t.Helper()
	rows, err := db.RangeEvents(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, row := range rows {
		kinds = append(kinds, row.Kind)
	}
	return kinds
}

func TestForkAfterMessageCopiesTranscriptPrefixAndCutsAtItsTurn(t *testing.T) {
	factory := &phaseFactory{}
	r, db, _ := phaseSetup(t, factory, config.Launch{Cmd: "fake"})
	cwd := t.TempDir()
	seedForkSource(t, db, cwd)

	s, err := r.Fork(context.Background(), ForkRequest{SourceID: "src", AtSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "src-fork" {
		t.Fatalf("fork id = %q", s.ID)
	}
	req := factory.requests[len(factory.requests)-1]
	if req.Fork == nil || req.Fork.SessionID != "sess_source" || req.Fork.MessageID != "msg_a" {
		t.Fatalf("fork point = %+v, want sess_source cut at msg_a", req.Fork)
	}
	if req.CWD != cwd {
		t.Fatalf("fork cwd = %q, want the source workspace %q", req.CWD, cwd)
	}
	if got := strings.Join(forkKinds(t, db, s.ID)[:2], ","); got != "user_message,message_chunk" {
		t.Fatalf("copied prefix = %s", got)
	}
}

func TestForkEditEndsBeforeMessageAndSendsReplacement(t *testing.T) {
	factory := &phaseFactory{}
	r, db, _ := phaseSetup(t, factory, config.Launch{Cmd: "fake"})
	seedForkSource(t, db, t.TempDir())

	blocks := []agentadapter.PromptBlock{{Type: "quote", RefSeq: 2, Role: "assistant", Quote: "answer"}, {Type: "text", Text: "edited"}}
	s, err := r.Fork(context.Background(), ForkRequest{SourceID: "src", AtSeq: 4, Edit: true, Blocks: blocks})
	if err != nil {
		t.Fatal(err)
	}
	if req := factory.requests[len(factory.requests)-1]; req.Fork == nil || req.Fork.MessageID != "msg_a" {
		t.Fatalf("fork point = %+v, want cut at msg_a", req.Fork)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := db.RangeEvents(s.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		var edited *store.StoredEvent
		for i := range rows {
			if rows[i].Kind == "user_message" && strings.Contains(rows[i].Payload, "edited") {
				edited = &rows[i]
			}
		}
		if edited != nil {
			// The copied answer is the fork's seq 2, so the citation still
			// points at it.
			if !strings.Contains(edited.Payload, `"refSeq":2`) {
				t.Fatalf("edited prompt citation not remapped: %s", edited.Payload)
			}
			if strings.Contains(edited.Payload, "second") || rows[0].Kind != "user_message" || rows[1].Kind != "message_chunk" {
				t.Fatalf("fork log = %+v", rows)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("edited prompt never sent; fork log = %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForkEditOfFirstMessageStartsFreshSession(t *testing.T) {
	factory := &phaseFactory{}
	r, db, _ := phaseSetup(t, factory, config.Launch{Cmd: "fake"})
	seedForkSource(t, db, t.TempDir())

	if _, err := r.Fork(context.Background(), ForkRequest{SourceID: "src", AtSeq: 1, Edit: true, Blocks: []agentadapter.PromptBlock{{Type: "text", Text: "redo"}}}); err != nil {
		t.Fatal(err)
	}
	if req := factory.requests[len(factory.requests)-1]; req.Fork != nil || req.ResumeSessionID != "" {
		t.Fatalf("first-message edit should start a fresh session, got %+v", req)
	}
}

func TestForkWholeConversationHasNoMessageCut(t *testing.T) {
	factory := &phaseFactory{}
	r, db, _ := phaseSetup(t, factory, config.Launch{Cmd: "fake"})
	seedForkSource(t, db, t.TempDir())

	s, err := r.Fork(context.Background(), ForkRequest{SourceID: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if req := factory.requests[len(factory.requests)-1]; req.Fork == nil || req.Fork.MessageID != "" {
		t.Fatalf("whole-conversation fork point = %+v", req.Fork)
	}
	if got := strings.Join(forkKinds(t, db, s.ID)[:4], ","); got != "user_message,message_chunk,user_message,message_chunk" {
		t.Fatalf("copied transcript = %s (status must not be copied)", got)
	}
}

func TestForkRejectsNonUserAnchorAndUntrackedCut(t *testing.T) {
	factory := &phaseFactory{}
	r, db, _ := phaseSetup(t, factory, config.Launch{Cmd: "fake"})
	seedForkSource(t, db, t.TempDir())
	if _, err := r.Fork(context.Background(), ForkRequest{SourceID: "src", AtSeq: 2}); err == nil {
		t.Fatal("fork anchored on an agent message should fail")
	}
	// A transcript recorded before message ids were tracked can only be
	// forked from its end.
	if _, err := db.AppendEvent("src", "user_message", `{"kind":"user_message","text":"third"}`, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent("src", "message_chunk", `{"kind":"message_chunk","text":"old"}`, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent("src", "user_message", `{"kind":"user_message","text":"fourth"}`, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fork(context.Background(), ForkRequest{SourceID: "src", AtSeq: 6}); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("cut after an untracked message = %v, want a predates error", err)
	}
	// The end of the conversation needs no message cut.
	if _, err := r.Fork(context.Background(), ForkRequest{SourceID: "src", AtSeq: 8}); err != nil {
		t.Fatal(err)
	}
}
