package handoff_test

import (
	"encoding/json"
	"strconv"
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
		event(t, 5, map[string]any{"kind": "tool_call", "id": "t1", "title": "Read", "status": "pending"}),
		event(t, 6, map[string]any{"kind": "tool_call_update", "id": "t1", "status": "completed",
			"rawInput": map[string]any{"file_path": "/w/repo/upload.go", "offset": 40, "limit": 20}}),
		event(t, 7, map[string]any{"kind": "tool_call", "id": "t2", "title": "Edit", "status": "failed",
			"rawInput": map[string]any{"file_path": "/w/repo/upload.go", "old_string": "x"}}),
		event(t, 8, map[string]any{"kind": "message_chunk", "text": "The edit failed; retrying."}),
	}
	got := handoff.Render(history, handoff.Source{SessionName: "tesla-13", Harness: "Claude", CWD: "/w/repo", Branch: "tandem/master/tesla-13"}, handoff.ModeFull)

	for _, want := range []string{
		"add a retry to the uploader",
		"Sure. Let me read the uploader.",  // chunks coalesce into one message
		"- Read — /w/repo/upload.go:40-59", // the read's target and span, from its raw input
		"- Edit — /w/repo/upload.go [failed]",
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
	if got := handoff.Render(history, handoff.Source{}, handoff.ModeFull); got != "" {
		t.Errorf("want empty render, got %q", got)
	}
}

func TestRenderCapsARunawayToolLoop(t *testing.T) {
	history := []eventlog.LoggedEvent{event(t, 0, map[string]any{"kind": "user_message", "text": "go"})}
	for i := 1; i <= 100; i++ {
		history = append(history, event(t, int64(i), map[string]any{
			"kind": "tool_call", "id": strconv.Itoa(i), "title": "Bash echo", "status": "completed",
		}))
	}
	got := handoff.Render(history, handoff.Source{}, handoff.ModeFull)
	if lines := strings.Count(got, "- Bash echo"); lines != 10 {
		t.Errorf("want 10 summarized tool lines, got %d", lines)
	}
	if !strings.Contains(got, "and 90 more tool calls") {
		t.Errorf("dropped tool calls were not accounted for:\n%s", got)
	}
}

func TestBriefModeKeepsOnlyTurnEndMessagesAndCountsTools(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "add a retry to the uploader"}),
		event(t, 2, map[string]any{"kind": "message_chunk", "text": "Reading the uploader first."}),
		event(t, 3, map[string]any{"kind": "tool_call", "id": "t1", "title": "Read upload.go", "status": "completed"}),
		event(t, 4, map[string]any{"kind": "message_chunk", "text": "Now editing."}),
		event(t, 5, map[string]any{"kind": "tool_call", "id": "t2", "title": "Edit upload.go", "status": "completed"}),
		event(t, 6, map[string]any{"kind": "tool_call", "id": "t3", "title": "Bash go test", "status": "completed"}),
		event(t, 7, map[string]any{"kind": "message_chunk", "text": "Done: retries with backoff, tests pass."}),
		event(t, 8, map[string]any{"kind": "user_message", "text": "now do the downloader"}),
		event(t, 9, map[string]any{"kind": "message_chunk", "text": "Starting on it."}),
	}
	got := handoff.Render(history, handoff.Source{SessionName: "tesla-13"}, handoff.ModeBrief)

	for _, want := range []string{
		"add a retry to the uploader",
		"3 tool calls",
		"Done: retries with backoff, tests pass.",
		"now do the downloader",
		"Starting on it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief hand-off is missing %q:\n%s", want, got)
		}
	}
	// Intermediate narration is exactly what brief mode trades away.
	for _, unwanted := range []string{"Reading the uploader first.", "Now editing.", "Read upload.go"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("brief hand-off should not contain %q:\n%s", unwanted, got)
		}
	}
	// The count is of the whole turn, so it is not subject to the full-mode cap.
	if strings.Contains(got, "more tool call") {
		t.Errorf("brief hand-off should count tool calls, not truncate them:\n%s", got)
	}
}

func TestBriefModeCountsPastTheFullModeCap(t *testing.T) {
	history := []eventlog.LoggedEvent{event(t, 0, map[string]any{"kind": "user_message", "text": "go"})}
	for i := 1; i <= 100; i++ {
		history = append(history, event(t, int64(i), map[string]any{
			"kind": "tool_call", "id": strconv.Itoa(i), "title": "Bash echo", "status": "completed",
		}))
	}
	got := handoff.Render(history, handoff.Source{}, handoff.ModeBrief)
	if !strings.Contains(got, "100 tool calls") {
		t.Errorf("want the true total, got:\n%s", got)
	}
}

func TestRenderGroupsDelegatedWorkByParentID(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "fix auth and tests"}),
		event(t, 2, map[string]any{"kind": "message_chunk", "text": "I will delegate the investigation."}),
		event(t, 3, map[string]any{"kind": "tool_call", "id": "task-1", "title": "Explore authentication flow", "status": "pending"}),
		event(t, 4, map[string]any{"kind": "message_chunk", "parentId": "task-1", "text": "I found the token check. "}),
		event(t, 5, map[string]any{"kind": "tool_call", "parentId": "task-1", "id": "read-1", "title": "Read", "status": "completed", "rawInput": map[string]any{"file_path": "/repo/auth.go"}}),
		event(t, 6, map[string]any{"kind": "message_chunk", "parentId": "task-1", "text": "The expiry comparison is reversed."}),
		event(t, 7, map[string]any{"kind": "tool_call_update", "id": "task-1", "status": "completed"}),
		event(t, 8, map[string]any{"kind": "message_chunk", "text": "I am applying that finding."}),
	}

	got := handoff.Render(history, handoff.Source{}, handoff.ModeFull)
	for _, want := range []string{
		"## Delegated work",
		"### Explore authentication flow — completed",
		"Child-agent narration:\n\nI found the token check.",
		"Final child response:\n\nThe expiry comparison is reversed.",
		"Tool activity:\n\n- Read — /repo/auth.go",
		"I am applying that finding.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hierarchical hand-off missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "I found the token check.") != 1 {
		t.Errorf("child output was duplicated into the parent transcript:\n%s", got)
	}
}

func TestRenderDelegationStatesAndMissingChildResponse(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "delegate these"}),
		event(t, 2, map[string]any{"kind": "tool_call", "id": "running", "title": "Update tests", "status": "in_progress"}),
		event(t, 3, map[string]any{"kind": "tool_call", "parentId": "running", "id": "write", "title": "Write tests", "status": "pending"}),
		event(t, 4, map[string]any{"kind": "tool_call", "id": "failed", "title": "Review schema", "status": "failed"}),
		event(t, 5, map[string]any{"kind": "message_chunk", "parentId": "failed", "text": "The schema file is malformed."}),
		event(t, 6, map[string]any{"kind": "tool_call", "id": "unknown", "title": "Check deployment"}),
		event(t, 7, map[string]any{"kind": "message_chunk", "parentId": "unknown", "text": "Still checking."}),
	}

	got := handoff.Render(history, handoff.Source{}, handoff.ModeFull)
	for _, want := range []string{
		"### Update tests — running",
		"_No child response was recorded before hand-off._",
		"### Review schema — failed",
		"Final child response:\n\nThe schema file is malformed.",
		"### Check deployment — unresolved",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("delegation state render missing %q:\n%s", want, got)
		}
	}
}

func TestBriefModeRetainsDelegationOutcome(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "investigate"}),
		event(t, 2, map[string]any{"kind": "tool_call", "id": "task", "title": "Investigate cache", "status": "completed"}),
		event(t, 3, map[string]any{"kind": "message_chunk", "parentId": "task", "text": "First I inspected it."}),
		event(t, 4, map[string]any{"kind": "tool_call", "parentId": "task", "id": "read", "title": "Read cache.go", "status": "completed"}),
		event(t, 5, map[string]any{"kind": "tool_call", "parentId": "task", "id": "search", "title": "Search invalidation", "status": "completed"}),
		event(t, 6, map[string]any{"kind": "message_chunk", "parentId": "task", "text": "The cache key omits the tenant ID."}),
	}

	got := handoff.Render(history, handoff.Source{}, handoff.ModeBrief)
	for _, want := range []string{
		"### Investigate cache — completed",
		"Final child response:\n\nThe cache key omits the tenant ID.",
		"2 tool calls (details omitted in brief mode)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief delegation render missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"First I inspected it.", "Read cache.go", "Search invalidation"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("brief delegation render should omit %q:\n%s", unwanted, got)
		}
	}
}

func TestRenderSupportsNestedDelegations(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "research"}),
		event(t, 2, map[string]any{"kind": "tool_call", "id": "outer", "title": "Research API", "status": "completed"}),
		event(t, 3, map[string]any{"kind": "tool_call", "parentId": "outer", "id": "inner", "title": "Inspect client", "status": "completed"}),
		event(t, 4, map[string]any{"kind": "message_chunk", "parentId": "inner", "text": "Client retries twice."}),
		event(t, 5, map[string]any{"kind": "message_chunk", "parentId": "outer", "text": "The API is safe to retry."}),
	}

	got := handoff.Render(history, handoff.Source{}, handoff.ModeFull)
	outer := strings.Index(got, "### Research API — completed")
	inner := strings.Index(got, "#### Inspect client — completed")
	if outer < 0 || inner < 0 || inner < outer {
		t.Errorf("nested delegation hierarchy is missing or out of order:\n%s", got)
	}
	for _, want := range []string{"The API is safe to retry.", "Client retries twice."} {
		if !strings.Contains(got, want) {
			t.Errorf("nested delegation missing %q:\n%s", want, got)
		}
	}
}

func TestRenderPreservesOrphanedChildEventsAsUnresolvedDelegation(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "user_message", "text": "continue"}),
		event(t, 2, map[string]any{"kind": "message_chunk", "parentId": "missing-task", "text": "I changed the parser but did not run tests."}),
		event(t, 3, map[string]any{"kind": "tool_call", "parentId": "missing-task", "id": "edit", "title": "Edit parser.go", "status": "completed"}),
	}

	got := handoff.Render(history, handoff.Source{}, handoff.ModeFull)
	for _, want := range []string{
		"### Delegated task missing-task — unresolved",
		"I changed the parser but did not run tests.",
		"- Edit parser.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("orphaned child output missing %q:\n%s", want, got)
		}
	}
}

func TestAnalyzeDelegationsIncludesNestedAndOrphanedTasks(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "tool_call", "id": "done", "title": "Done task", "status": "completed"}),
		event(t, 2, map[string]any{"kind": "message_chunk", "parentId": "done", "text": "done"}),
		event(t, 3, map[string]any{"kind": "tool_call", "parentId": "done", "id": "nested", "title": "Nested task", "status": "failed"}),
		event(t, 4, map[string]any{"kind": "message_chunk", "parentId": "nested", "text": "failed"}),
		event(t, 5, map[string]any{"kind": "tool_call", "id": "running", "title": "Running task", "status": "pending"}),
		event(t, 6, map[string]any{"kind": "tool_call", "parentId": "running", "id": "read", "title": "Read", "status": "completed"}),
		event(t, 7, map[string]any{"kind": "message_chunk", "parentId": "orphan", "text": "orphaned"}),
	}

	got := handoff.AnalyzeDelegations(history)
	if got.Completed != 1 || got.Failed != 1 || got.Running != 1 || got.Unresolved != 1 || got.Total() != 4 {
		t.Fatalf("AnalyzeDelegations = %+v, want one task in each state", got)
	}
}

func TestAnalyzeDelegationsTreatsCancelledWorkAsFailed(t *testing.T) {
	history := []eventlog.LoggedEvent{
		event(t, 1, map[string]any{"kind": "tool_call", "id": "cancelled", "title": "Cancelled task", "status": "cancelled"}),
		event(t, 2, map[string]any{"kind": "message_chunk", "parentId": "cancelled", "text": "Stopped during hand-off."}),
	}

	got := handoff.AnalyzeDelegations(history)
	if got.Failed != 1 || got.Total() != 1 {
		t.Fatalf("AnalyzeDelegations = %+v, want one failed task", got)
	}
}
