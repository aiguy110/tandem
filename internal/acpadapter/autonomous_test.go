package acpadapter

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
)

func newAutoAdapter(quiet time.Duration) *Adapter {
	a := newUpdateAdapter([]string{"claudeCode", "parentToolUseId"})
	a.events = make(chan eventlog.Event, 256)
	a.sessionID = "s1"
	a.auto = newAutonomousTracker()
	a.auto.quiet = quiet
	a.auto.grace = 50 * time.Millisecond
	return a
}

func sendUpdate(t *testing.T, a *Adapter, sessionID, update string) {
	t.Helper()
	raw := fmt.Sprintf(`{"sessionId":%q,"update":%s}`, sessionID, update)
	if err := a.handleUpdate(json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
}

// statuses drains every queued event and returns the status transitions.
func statuses(t *testing.T, a *Adapter) []string {
	t.Helper()
	var out []string
	for {
		select {
		case ev := <-a.events:
			m := eventMap(t, ev)
			if m["kind"] == "status" {
				out = append(out, m["status"].(string))
			}
		default:
			return out
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const (
	chunkUpdate    = `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}`
	resultUsage    = `{"sessionUpdate":"usage_update","used":10,"size":100,"cost":{"amount":0.1,"currency":"USD"}}`
	compactUsage   = `{"sessionUpdate":"usage_update","used":10,"size":100}`
	bgToolDone     = `{"sessionUpdate":"tool_call_update","toolCallId":"bg-1","status":"completed"}`
	toolStart      = `{"sessionUpdate":"tool_call","toolCallId":"t-1","title":"Bash","status":"in_progress"}`
	toolFinish     = `{"sessionUpdate":"tool_call_update","toolCallId":"t-1","status":"completed"}`
	subagentStarts = `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"},"_meta":{"claudeCode":{"parentToolUseId":"task"}}}`
)

func TestAutonomousTurnStartsOnOutputAndEndsAtResultUsage(t *testing.T) {
	a := newAutoAdapter(time.Hour)
	// A background task's completion touches an old tool call: not a turn.
	sendUpdate(t, a, "s1", bgToolDone)
	if got := statuses(t, a); len(got) != 0 {
		t.Fatalf("tool_call_update alone changed status: %v", got)
	}
	sendUpdate(t, a, "s1", chunkUpdate)
	sendUpdate(t, a, "s1", toolStart)
	sendUpdate(t, a, "s1", compactUsage) // no cost: not a result
	if got := statuses(t, a); !equalStrings(got, []string{"working"}) {
		t.Fatalf("statuses = %v, want [working]", got)
	}
	sendUpdate(t, a, "s1", resultUsage)
	if got := statuses(t, a); !equalStrings(got, []string{"idle"}) {
		t.Fatalf("statuses = %v, want [idle]", got)
	}
}

func TestAutonomousTurnIgnoresOtherSessionsSubagentsAndReplay(t *testing.T) {
	a := newAutoAdapter(time.Hour)
	sendUpdate(t, a, "fork", chunkUpdate)
	sendUpdate(t, a, "s1", subagentStarts)
	a.loading = true
	sendUpdate(t, a, "s1", chunkUpdate)
	a.loading = false
	a.replaying = true
	sendUpdate(t, a, "s1", chunkUpdate)
	a.replaying = false
	if got := statuses(t, a); len(got) != 0 {
		t.Fatalf("statuses = %v, want none", got)
	}
}

func TestAutonomousTurnIgnoresUpdatesDuringAndJustAfterPrompt(t *testing.T) {
	a := newAutoAdapter(time.Hour)
	a.autoPromptStarted()
	sendUpdate(t, a, "s1", chunkUpdate)
	sendUpdate(t, a, "s1", resultUsage)
	a.autoPromptEnded()
	// A straggler right after the prompt returned belongs to that turn.
	sendUpdate(t, a, "s1", chunkUpdate)
	if got := statuses(t, a); len(got) != 0 {
		t.Fatalf("statuses = %v, want none", got)
	}
	time.Sleep(80 * time.Millisecond)
	sendUpdate(t, a, "s1", chunkUpdate)
	if got := statuses(t, a); !equalStrings(got, []string{"working"}) {
		t.Fatalf("statuses = %v, want [working]", got)
	}
	// A prompt takes the stretch over; its result usage must not report idle
	// mid-prompt, and the prompt's own idle is the end.
	a.autoPromptStarted()
	sendUpdate(t, a, "s1", resultUsage)
	if got := statuses(t, a); len(got) != 0 {
		t.Fatalf("statuses during prompt = %v, want none", got)
	}
}

func TestAutonomousTurnQuietFallbackWaitsForRunningTools(t *testing.T) {
	a := newAutoAdapter(40 * time.Millisecond)
	sendUpdate(t, a, "s1", toolStart)
	if got := statuses(t, a); !equalStrings(got, []string{"working"}) {
		t.Fatalf("statuses = %v, want [working]", got)
	}
	time.Sleep(120 * time.Millisecond)
	if got := statuses(t, a); len(got) != 0 {
		t.Fatalf("went idle while a tool ran: %v", got)
	}
	sendUpdate(t, a, "s1", toolFinish)
	time.Sleep(120 * time.Millisecond)
	if got := statuses(t, a); !equalStrings(got, []string{"idle"}) {
		t.Fatalf("statuses = %v, want [idle]", got)
	}
}

func TestAutonomousTurnInterruptReportsIdle(t *testing.T) {
	a := newAutoAdapter(time.Hour)
	sendUpdate(t, a, "s1", chunkUpdate)
	a.autoInterrupted()
	if got := statuses(t, a); !equalStrings(got, []string{"working", "idle"}) {
		t.Fatalf("statuses = %v, want [working idle]", got)
	}
	// With nothing running, an interrupt reports nothing.
	a.autoInterrupted()
	if got := statuses(t, a); len(got) != 0 {
		t.Fatalf("statuses = %v, want none", got)
	}
}
