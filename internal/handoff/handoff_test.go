package handoff_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/handoff"
)

func event(t *testing.T, seq int64, payload map[string]any) eventlog.LoggedEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return eventlog.LoggedEvent{Seq: seq, Event: eventlog.Event{Kind: payload["kind"].(string), Payload: raw}}
}

func TestRenderCarriesMessagesVerbatimAndSummarizesTools(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "add a retry to the uploader"}),
		event(t, 2, map[string]any{"kind": "thought_chunk", "text": "the user probably wants exponential backoff"}),
		event(t, 3, map[string]any{"kind": "message_chunk", "text": "Sure. Let me "}),
		event(t, 4, map[string]any{"kind": "message_chunk", "text": "read the uploader."}),
		event(t, 5, map[string]any{"kind": "tool_call", "id": "t1", "title": "Read upload.go", "status": "pending"}),
		event(t, 6, map[string]any{"kind": "tool_call_update", "id": "t1", "status": "completed"}),
		event(t, 7, map[string]any{"kind": "tool_call", "id": "t2", "title": "Edit upload.go", "status": "failed"}),
		event(t, 8, map[string]any{"kind": "message_chunk", "text": "The edit failed; retrying."}),
	}
	got := handoff.Render(history, handoff.Source{AgentName: "tesla-13", Harness: "Claude", CWD: "/w/repo", Branch: "tandem/master/tesla-13"})

	for _, want := range []string{
		"add a retry to the uploader",
		"Sure. Let me read the uploader.", // chunks coalesce into one message
		"- Read upload.go",                // a completed call needs no status suffix
		"- Edit upload.go [failed]",
		"The edit failed; retrying.",
		"tesla-13",
		"Claude",
		"/w/repo",
		"tandem/master/tesla-13",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered transcript is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "exponential backoff") {
		t.Errorf("private reasoning leaked into the hand-off:\n%s", got)
	}
	if strings.Contains(got, "[pending]") {
		t.Errorf("a later tool_call_update did not refine the summary line in place:\n%s", got)
	}
	if user, assistant := strings.Index(got, "add a retry"), strings.Index(got, "Sure. Let me"); user > assistant {
		t.Errorf("messages are out of order:\n%s", got)
	}
}

func TestRenderReturnsEmptyForATranscriptWithNothingToCarry(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "status", "status": "idle"}),
		event(t, 2, map[string]any{"kind": "thought_chunk", "text": "hmm"}),
	}
	if got := handoff.Render(history, handoff.Source{}); got != "" {
		t.Errorf("want empty render, got %q", got)
	}
}

func TestRenderCapsARunawayToolLoop(t *testing.T) {
	history := []eventlog.LoggedEvent{event(t, 0, map[string]any{"kind": "user_message", "text": "go"})}
	for i := 1; i <= 100; i++ {
		history = append(history, event(t, int64(i), map[string]any{
			"kind": "tool_call", "id": string(rune('a'+i%26)) + strings.Repeat("x", i), "title": "Bash echo", "status": "completed",
		}))
	}
	got := handoff.Render(history, handoff.Source{})
	if lines := strings.Count(got, "- Bash echo"); lines != 40 {
		t.Errorf("want 40 summarized tool lines, got %d", lines)
	}
	if !strings.Contains(got, "and 60 more tool call(s)") {
		t.Errorf("dropped tool calls were not accounted for:\n%s", got)
	}
}
