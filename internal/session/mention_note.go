package session

import (
	"log/slog"
	"regexp"

	"github.com/aiguy110/tandem/internal/agentadapter"
)

// agentMentionPattern matches an `@agent:<host>/<agent>` reference (see
// internal/messaging/ref.go) in text the user typed.
var agentMentionPattern = regexp.MustCompile(`@agent:\S`)

// agentMentionNote is appended, for the agent only, to a user prompt that
// mentions another Tandem agent. Harnesses such as Claude Code ship their own
// messaging/subagent tools and otherwise tend to reach for those, which cannot
// reach a Tandem agent.
const agentMentionNote = "[Tandem note — not shown to the user: `@agent:<host>/<agent>` above refers to another " +
	"Tandem-managed agent. To contact, ask, or check on it, use the `tandem-messages` MCP tools " +
	"(`messages_directory`, `messages_send`, `messages_request_link`), not any messaging, team, or subagent " +
	"tools built into your harness — those cannot reach Tandem agents.]"

// withAgentMentionNote returns blocks with agentMentionNote appended when any
// text block mentions an agent. The transcript keeps the user's original
// blocks; only what the adapter receives changes.
func (s *Session) withAgentMentionNote(blocks []agentadapter.PromptBlock) []agentadapter.PromptBlock {
	for _, block := range blocks {
		if block.Type == "text" && agentMentionPattern.MatchString(block.Text) {
			slog.Debug("appending agent-mention messaging note to prompt", "session_id", s.ID)
			out := append([]agentadapter.PromptBlock(nil), blocks...)
			return append(out, agentadapter.PromptBlock{Type: "text", Text: "\n\n" + agentMentionNote})
		}
	}
	return blocks
}
