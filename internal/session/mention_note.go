package session

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/aiguy110/tandem/internal/agentadapter"
)

// agentMentionPattern matches an `@agent:<host>/<agent>` reference (see
// internal/messaging/ref.go) in text the user typed.
var agentMentionPattern = regexp.MustCompile(`@agent:\S`)

// agentMentionRefPattern extracts complete references. Both parts follow the
// reference token grammar (a display name or a stable ID).
var agentMentionRefPattern = regexp.MustCompile(`@agent:[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*`)

// agentMentionNote is appended, for the agent only, to a user prompt that
// mentions another Tandem agent. Harnesses such as Claude Code ship their own
// messaging/subagent tools and otherwise tend to reach for those, which cannot
// reach a Tandem agent.
const agentMentionNote = "[Tandem note — not shown to the user: `@agent:<host>/<agent>` above refers to another " +
	"Tandem-managed agent. To contact, ask, or check on it, use the `tandem-messages` MCP tools " +
	"(`messages_directory`, `messages_send`), not any messaging, team, or subagent " +
	"tools built into your harness — those cannot reach Tandem agents. Mentioning an agent links it to you " +
	"automatically, so message it directly with `messages_send`; use `messages_request_link` only if a send " +
	"reports that no link exists.]"

// AgentMentionRefs returns the distinct `@agent:<host>/<agent>` references in
// text, in order of first appearance. A trailing sentence period is not part
// of a reference.
func AgentMentionRefs(text string) []string {
	var refs []string
	seen := map[string]bool{}
	for _, m := range agentMentionRefPattern.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".")
		if strings.HasSuffix(m, "/") {
			continue
		}
		key := strings.ToLower(m)
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, m)
	}
	return refs
}

// withAgentMentionNote returns blocks with agentMentionNote appended when any
// text block mentions an agent, and tells OnAgentMention listeners which
// agents the user mentioned. The transcript keeps the user's original blocks;
// only what the adapter receives changes.
func (s *Session) withAgentMentionNote(blocks []agentadapter.PromptBlock) []agentadapter.PromptBlock {
	mentioned := false
	var refs []string
	seen := map[string]bool{}
	for _, block := range blocks {
		if block.Type != "text" || !agentMentionPattern.MatchString(block.Text) {
			continue
		}
		mentioned = true
		for _, ref := range AgentMentionRefs(block.Text) {
			if key := strings.ToLower(ref); !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
		}
	}
	if !mentioned {
		return blocks
	}
	slog.Debug("appending agent-mention messaging note to prompt", "session_id", s.ID, "refs", len(refs))
	if len(refs) > 0 {
		s.notifyAgentMention(refs)
	}
	out := append([]agentadapter.PromptBlock(nil), blocks...)
	return append(out, agentadapter.PromptBlock{Type: "text", Text: "\n\n" + agentMentionNote})
}

// OnAgentMention registers cb to be called with the agent references a user
// prompt to this session mentions (relayed agent messages are not included).
// cb runs on its own goroutine. The returned func unregisters it.
func (s *Session) OnAgentMention(cb func(refs []string)) func() {
	s.mu.Lock()
	if s.mentionListeners == nil {
		s.mentionListeners = map[uint64]func([]string){}
	}
	id := s.nextListener
	s.nextListener++
	s.mentionListeners[id] = cb
	s.mu.Unlock()
	return func() { s.mu.Lock(); delete(s.mentionListeners, id); s.mu.Unlock() }
}

func (s *Session) notifyAgentMention(refs []string) {
	s.mu.RLock()
	callbacks := make([]func([]string), 0, len(s.mentionListeners))
	for _, cb := range s.mentionListeners {
		callbacks = append(callbacks, cb)
	}
	s.mu.RUnlock()
	for _, cb := range callbacks {
		go cb(append([]string(nil), refs...))
	}
}
