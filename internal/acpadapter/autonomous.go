package acpadapter

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// Out-of-turn ("autonomous") activity tracking.
//
// ACP v1 only marks a turn by the lifetime of a session/prompt request, but
// some harnesses start turns on their own: Claude Code resumes when a
// background task finishes, a scheduled wakeup fires, or a monitor reports.
// The bridge streams that turn's session/update notifications with no prompt
// open, so without this tracker the session looks idle while the agent works.
//
// Heuristic, deliberately conservative:
//   - start: top-level agent output (message/thought chunk or a new tool call)
//     on the main session while no prompt is in flight, not during a
//     session/load replay, and not in the short grace window right after a
//     prompt returned (late stragglers of the turn that just ended);
//   - keep-alive: any further out-of-turn update while working;
//   - end: a usage_update carrying a cost (Claude sends one at the result of
//     every model cycle, autonomous ones included), an interrupt, the next
//     prompt, or — for harnesses without that signal — a quiet period with
//     no update and no tool call of the stretch still running.
//
// (claude-agent-acp also reports autonomous turns as AIR `async_task_*`
// updates, but only to clients that declare themselves JetBrains AIR, which
// switches many unrelated rendering behaviors, so Tandem does not opt in.)

const (
	autonomousQuietTimeout = 30 * time.Second
	autonomousPromptGrace  = 1500 * time.Millisecond
)

type autonomousTracker struct {
	mu            sync.Mutex
	promptActive  int
	lastPromptEnd time.Time
	working       bool
	startedAt     time.Time
	updates       int
	tools         map[string]struct{}
	timer         *time.Timer
	gen           uint64
	// Overridable in tests.
	quiet time.Duration
	grace time.Duration
	now   func() time.Time
}

func newAutonomousTracker() *autonomousTracker {
	return &autonomousTracker{tools: make(map[string]struct{}), quiet: autonomousQuietTimeout, grace: autonomousPromptGrace, now: time.Now}
}

// stopLocked ends the current stretch's bookkeeping. Caller holds t.mu.
func (t *autonomousTracker) stopLocked() {
	t.working = false
	t.updates = 0
	t.tools = make(map[string]struct{})
	t.gen++
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// autoPromptStarted marks a prompt in flight. A prompt reports its own
// working/idle transitions, so it takes over any autonomous stretch.
func (a *Adapter) autoPromptStarted() {
	t := a.auto
	if t == nil {
		return
	}
	t.mu.Lock()
	t.promptActive++
	if t.working {
		slog.Info("acp autonomous turn taken over by prompt", "session", a.cfg.SessionID, "elapsed_ms", t.now().Sub(t.startedAt).Milliseconds(), "updates", t.updates)
		t.stopLocked()
	}
	t.mu.Unlock()
}

func (a *Adapter) autoPromptEnded() {
	t := a.auto
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.promptActive > 0 {
		t.promptActive--
	}
	t.lastPromptEnd = t.now()
	t.mu.Unlock()
}

// autoInterrupted ends an autonomous stretch the user interrupted: the bridge
// drops the stopped work's output, so nothing else would report idle.
func (a *Adapter) autoInterrupted() {
	t := a.auto
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.working && t.promptActive == 0 {
		slog.Info("acp autonomous turn interrupted", "session", a.cfg.SessionID, "elapsed_ms", t.now().Sub(t.startedAt).Milliseconds(), "updates", t.updates)
		t.stopLocked()
		// Emitted under t.mu so a prompt starting concurrently cannot have its
		// "working" overwritten by this idle.
		a.emit(map[string]any{"kind": "status", "status": "idle"})
	}
}

// autoEmitEnded reports idle for a stretch observeOutOfTurn ended, after the
// update that ended it was pushed, unless a prompt took over in between.
func (a *Adapter) autoEmitEnded() {
	t := a.auto
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.promptActive == 0 && !t.working {
		a.emit(map[string]any{"kind": "status", "status": "idle"})
	}
}

// observeOutOfTurn inspects one session/update before it is normalized. It
// emits "working" when it starts an autonomous stretch, and returns whether
// the caller must emit "idle" after pushing the update (the stretch ended).
// Status events are emitted while holding t.mu so they cannot interleave with
// a concurrently starting prompt's own "working".
func (a *Adapter) observeOutOfTurn(sessionID, variant string, u map[string]json.RawMessage, parentID string) (endAfter bool) {
	a.mu.RLock()
	main := sessionID != "" && sessionID == a.sessionID
	replaying := a.replaying || a.loading
	a.mu.RUnlock()
	t := a.auto
	if t == nil || !main || replaying {
		return false
	}
	t.mu.Lock()
	if t.promptActive > 0 {
		t.mu.Unlock()
		return false
	}
	starter := parentID == "" && (variant == "agent_message_chunk" || variant == "agent_thought_chunk" || variant == "tool_call")
	if !t.working {
		if !starter || t.now().Sub(t.lastPromptEnd) < t.grace {
			t.mu.Unlock()
			return false
		}
		t.working = true
		t.startedAt = t.now()
		t.updates = 0
		slog.Info("acp autonomous turn started", "session", a.cfg.SessionID, "trigger", variant)
		a.emit(map[string]any{"kind": "status", "status": "working"})
	}
	t.updates++
	switch variant {
	case "tool_call", "tool_call_update":
		id := rawString(u["toolCallId"])
		status := rawString(u["status"])
		if id != "" {
			switch {
			case variant == "tool_call" && (status == "" || status == "pending" || status == "in_progress"):
				t.tools[id] = struct{}{}
			case status == "completed" || status == "failed":
				delete(t.tools, id)
			}
		}
	case "usage_update":
		// Claude's result-time usage carries the cycle's cost; a compaction's
		// usage does not. Either way, ignore usage of a subagent.
		if raw, ok := u["cost"]; ok && len(raw) != 0 && string(raw) != "null" && parentID == "" {
			slog.Info("acp autonomous turn ended", "session", a.cfg.SessionID, "reason", "result", "elapsed_ms", t.now().Sub(t.startedAt).Milliseconds(), "updates", t.updates)
			t.stopLocked()
			t.mu.Unlock()
			return true
		}
	}
	t.armQuietLocked(a)
	t.mu.Unlock()
	return false
}

// armQuietLocked (re)starts the quiet-period fallback. Caller holds t.mu.
func (t *autonomousTracker) armQuietLocked(a *Adapter) {
	t.gen++
	gen := t.gen
	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(t.quiet, func() { a.autoQuiet(gen) })
}

func (a *Adapter) autoQuiet(gen uint64) {
	t := a.auto
	if t == nil {
		return
	}
	t.mu.Lock()
	if gen != t.gen || !t.working || t.promptActive > 0 {
		t.mu.Unlock()
		return
	}
	if len(t.tools) > 0 {
		// A tool call of this stretch is still running (e.g. a long sleep);
		// its completion will arrive as an update and re-arm the timer.
		slog.Debug("acp autonomous turn quiet but tools running", "session", a.cfg.SessionID, "tools", len(t.tools))
		t.timer = time.AfterFunc(t.quiet, func() { a.autoQuiet(gen) })
		t.mu.Unlock()
		return
	}
	slog.Info("acp autonomous turn ended", "session", a.cfg.SessionID, "reason", "quiet", "elapsed_ms", t.now().Sub(t.startedAt).Milliseconds(), "updates", t.updates)
	t.stopLocked()
	a.emit(map[string]any{"kind": "status", "status": "idle"})
	t.mu.Unlock()
}

// autoClose stops the fallback timer when the adapter shuts down.
func (a *Adapter) autoClose() {
	t := a.auto
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stopLocked()
	t.mu.Unlock()
}
