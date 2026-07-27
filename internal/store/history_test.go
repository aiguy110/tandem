package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func historyTime(value int64) *int64 { return &value }

func TestReplaceAndSearchHistoryWithContext(t *testing.T) {
	s, _ := openTestStore(t)
	session := HistorySession{
		Source: "fixture", Agent: "pi", ExternalID: "session-1", CWD: "/work/repo",
		Title: "Search fixture", CreatedAt: historyTime(10), UpdatedAt: historyTime(20),
		Resumable: true, SourceKey: "/history/session-1.jsonl", SourceMeta: json.RawMessage(`{"version":3}`),
	}
	entries := []HistoryEntry{
		{ExternalID: "one", Ordinal: 1, Role: "user", Kind: "message", Text: "Please inspect the deployment logs."},
		{ExternalID: "two", Ordinal: 2, Role: "assistant", Kind: "message", Text: "The frobnicator failed during startup.", Timestamp: historyTime(15)},
		{ExternalID: "three", Ordinal: 3, Role: "user", Kind: "message", Text: "Repair it and verify the service."},
	}
	if err := s.ReplaceHistorySession(session, entries); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchHistory(`frobn"`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%#v", hits)
	}
	hit := hits[0]
	if hit.Session.Agent != "pi" || hit.ExternalID != "two" || hit.Role != "assistant" {
		t.Fatalf("unexpected hit: %#v", hit)
	}
	if hit.Match.Text != "The frobnicator failed during startup." || len(hit.Match.Highlights) != 1 {
		t.Fatalf("match=%#v", hit.Match)
	}
	if got := hit.Match.Text[hit.Match.Highlights[0].Start:hit.Match.Highlights[0].End]; got != "frobnicator" {
		t.Fatalf("highlight=%q", got)
	}
	if hit.Before == nil || !strings.Contains(hit.Before.Text, "deployment") || hit.After == nil || !strings.Contains(hit.After.Text, "Repair") {
		t.Fatalf("context before=%#v after=%#v", hit.Before, hit.After)
	}
	if hits, err := s.SearchHistory(`" OR * : NEAR(`, 10); err != nil || len(hits) != 0 {
		t.Fatalf("syntax-like input hits=%#v err=%v", hits, err)
	}

	entries[1].Text = "The replacement transcript no longer contains that term."
	if err := s.ReplaceHistorySession(session, entries); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.SearchHistory("frobnicator", 10); err != nil || len(hits) != 0 {
		t.Fatalf("stale FTS hit=%#v err=%v", hits, err)
	}
}

func TestReplaceHistorySessionRollsBackOnDuplicateEntry(t *testing.T) {
	s, _ := openTestStore(t)
	session := HistorySession{Source: "fixture", Agent: "claude", ExternalID: "s1", SourceKey: "s1", Resumable: true}
	if err := s.ReplaceHistorySession(session, []HistoryEntry{{ExternalID: "old", Ordinal: 1, Text: "durable sentinel"}}); err != nil {
		t.Fatal(err)
	}
	err := s.ReplaceHistorySession(session, []HistoryEntry{
		{ExternalID: "duplicate", Ordinal: 1, Text: "new material"},
		{ExternalID: "duplicate", Ordinal: 2, Text: "conflicting material"},
	})
	if err == nil {
		t.Fatal("duplicate entry replacement unexpectedly succeeded")
	}
	hits, err := s.SearchHistory("sentinel", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("previous transcript was not preserved: hits=%#v err=%v", hits, err)
	}
}

func TestTandemEventsAreIndexedLiveAndOnMigration(t *testing.T) {
	s, path := openTestStore(t)
	agent := Agent{
		ID: "api-42", Name: "local transcript", Spec: json.RawMessage(`{"adapter":"acp","agent":"codex"}`),
		CWD: "/repo", Status: "idle", CreatedAt: 100,
	}
	if err := s.UpsertAgent(agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(agent.ID, "user_message", `{"kind":"user_message","text":"locate the ultraviolet semaphore"}`, 110); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(agent.ID, "tool_call", `{"kind":"tool_call","title":"ultraviolet should not be indexed twice"}`, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(agent.ID, "message_chunk", `{"kind":"message_chunk","text":"The semaphore is in registry.go."}`, 130); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(agent.ID, "message_chunk", `{"kind":"message_chunk","text":" Cross-chunk frob"}`, 140); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(agent.ID, "message_chunk", `{"kind":"message_chunk","text":"nicator search works."}`, 150); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchHistory("ultraviolet", 10)
	if err != nil || len(hits) != 1 || hits[0].Session.Source != tandemHistorySource || hits[0].Session.Agent != "codex" {
		t.Fatalf("live indexed hits=%#v err=%v", hits, err)
	}
	if hits, err := s.SearchHistory("frobnicator", 10); err != nil || len(hits) != 1 {
		t.Fatalf("cross-chunk hits=%#v err=%v", hits, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	hits, err = s.SearchHistory("semaphore", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("backfilled/reopened hits=%#v err=%v", hits, err)
	}
}

func TestSearchHistoryUnicodeAndLimits(t *testing.T) {
	s, _ := openTestStore(t)
	session := HistorySession{Source: "fixture", Agent: "opencode", ExternalID: "unicode", SourceKey: "unicode", Resumable: true}
	if err := s.ReplaceHistorySession(session, []HistoryEntry{{ExternalID: "1", Ordinal: 1, Text: "Résumé diagnostics 東京"}}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"résu", "東京"} {
		hits, err := s.SearchHistory(query, 1)
		if err != nil || len(hits) != 1 {
			t.Fatalf("query %q hits=%#v err=%v", query, hits, err)
		}
	}
}

func BenchmarkSearchHistoryGenerated(b *testing.B) {
	s, _ := openTestStore(b)
	const sessionCount = 100
	const entriesPerSession = 100
	for sessionIndex := 0; sessionIndex < sessionCount; sessionIndex++ {
		entries := make([]HistoryEntry, entriesPerSession)
		for entryIndex := range entries {
			entries[entryIndex] = HistoryEntry{
				ExternalID: fmt.Sprintf("%d", entryIndex), Ordinal: int64(entryIndex),
				Role: "assistant", Kind: "message",
				Text: fmt.Sprintf("generated transcript %d entry %d ordinary searchable content needle-%d", sessionIndex, entryIndex, entryIndex%17),
			}
		}
		session := HistorySession{
			Source: "benchmark", Agent: "codex", ExternalID: fmt.Sprintf("session-%d", sessionIndex),
			SourceKey: fmt.Sprintf("source-%d", sessionIndex), Resumable: true,
		}
		if err := s.ReplaceHistorySession(session, entries); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.SearchHistory("ordinary searchable needle", 30); err != nil {
			b.Fatal(err)
		}
	}
}
