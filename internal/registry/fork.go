package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// ForkRequest describes a conversation fork. Forks copy conversation context
// only: the new agent shares the source's workspace exactly as it is now, and
// no file changes are rolled back to the fork point.
type ForkRequest struct {
	SourceID string
	// AtSeq anchors the fork to one of the source's user_message events. Zero
	// forks the whole conversation.
	AtSeq int64
	// Edit replaces the anchored message: the fork ends just before it and
	// Blocks is sent as the fork's next prompt. Otherwise the fork ends after
	// the anchored message's turn.
	Edit   bool
	Blocks []agentadapter.PromptBlock
}

// forkedEventKinds are the transcript events copied into a fork. Session
// runtime state (status, approvals, terminal bytes, capabilities, config) is
// re-established by the fork's own adapter instead.
var forkedEventKinds = map[string]bool{
	"user_message":             true,
	"message_chunk":            true,
	"thought_chunk":            true,
	"tool_call":                true,
	"tool_call_update":         true,
	"terminal_output":          true,
	"plan":                     true,
	"usage":                    true,
	"compaction":               true,
	"compaction_summary_chunk": true,
	"aside_started":            true,
	"aside_event":              true,
	"aside_completed":          true,
	"error":                    true,
}

// Fork starts a new agent whose conversation is a prefix of the source's.
// The agent harness forks its own session history (ACP session/fork, cut at
// the last agent message before the fork point), while Tandem copies the same
// prefix of its event log so the fork's transcript renders exactly as the
// source's did. The source agent may be live or closed.
func (r *Registry) Fork(ctx context.Context, req ForkRequest) (*session.Session, error) {
	rec, spec, err := r.sourceAgent(req.SourceID)
	if err != nil {
		return nil, err
	}
	if spec.Adapter != "acp" {
		return nil, errors.New("only structured (ACP) agents can be forked")
	}
	if req.Edit && (req.AtSeq <= 0 || len(req.Blocks) == 0) {
		return nil, errors.New("an edit needs the edited message and its replacement")
	}
	rows, err := r.store.RangeEvents(rec.ID, 0)
	if err != nil {
		return nil, err
	}
	cutoff, err := forkCutoff(rows, req.AtSeq, req.Edit)
	if err != nil {
		return nil, err
	}
	tail := len(rows) == 0 || cutoff > rows[len(rows)-1].Seq
	if live := r.Get(rec.ID); live != nil && tail && (live.ActiveTurn() || live.Status() == session.Working) {
		return nil, errors.New("wait for the current turn to finish (or interrupt it) before forking from its end")
	}
	if _, err := os.Stat(rec.CWD); err != nil {
		return nil, fmt.Errorf("source workspace is unavailable: %w", err)
	}

	var point *agentadapter.ForkPoint
	if hasUserMessageBefore(rows, cutoff) {
		if rec.ExternalSessionID == nil || *rec.ExternalSessionID == "" {
			return nil, errors.New("source agent has no agent session to fork")
		}
		point = &agentadapter.ForkPoint{SessionID: *rec.ExternalSessionID}
		if !tail {
			point.MessageID = lastMessageIDBefore(rows, cutoff)
			if point.MessageID == "" {
				return nil, errors.New("this message predates fork-point tracking; only the end of this conversation can be forked")
			}
		}
	}

	forked := cloneSpec(spec)
	forked.Name, forked.Task, forked.HandoffFrom, forked.HandoffMode = "", "", "", ""
	name := r.nextName(rec.Name + "-fork")
	forked.Name = name
	raw, err := json.Marshal(forked)
	if err != nil {
		r.releaseKnown(name)
		return nil, err
	}
	// The fork joins the source's checkout and inherits its workspace
	// descriptor, the same way a spawn into an occupied worktree does.
	newRec := store.Session{ID: name, Name: name, Spec: raw, CWD: rec.CWD, Status: "idle", CreatedAt: time.Now().UnixMilli()}
	fail := func(err error) (*session.Session, error) {
		_ = r.store.DeleteSession(name)
		r.releaseKnown(name)
		slog.Warn("fork failed", "source_session", rec.ID, "fork_session", name, "at_seq", req.AtSeq, "edit", req.Edit, "error", err)
		return nil, err
	}
	if err := r.store.UpsertSession(newRec); err != nil {
		return fail(err)
	}
	seqMap, err := r.seedFork(name, rows, cutoff)
	if err != nil {
		return fail(err)
	}
	slog.Info("forking agent", "source_session", rec.ID, "fork_session", name, "at_seq", req.AtSeq, "edit", req.Edit, "cutoff_seq", cutoff, "copied_events", len(seqMap), "fork_point", point != nil, "message_id", messageIDOf(point))
	s, err := r.startWith(ctx, newRec, forked, agentadapter.StartRequest{Fork: point})
	if err != nil {
		return fail(err)
	}
	if req.Edit {
		blocks := remapQuoteRefs(req.Blocks, seqMap)
		if point == nil {
			blocks = withSessionPreamble(blocks)
		}
		if _, err := s.EnqueuePrompt(context.Background(), blocks); err != nil {
			slog.Warn("fork edited prompt was rejected", "fork_session", name, "error", err)
			return s, fmt.Errorf("fork created, but the edited message was rejected: %w", err)
		}
	}
	return s, nil
}

func messageIDOf(point *agentadapter.ForkPoint) string {
	if point == nil {
		return ""
	}
	return point.MessageID
}

// forkCutoff returns the first source sequence excluded from the fork.
func forkCutoff(rows []store.StoredEvent, atSeq int64, edit bool) (int64, error) {
	end := int64(1)
	if len(rows) > 0 {
		end = rows[len(rows)-1].Seq + 1
	}
	if atSeq <= 0 {
		return end, nil
	}
	anchor := -1
	for i, row := range rows {
		if row.Seq == atSeq {
			anchor = i
			break
		}
	}
	if anchor < 0 || rows[anchor].Kind != "user_message" {
		return 0, fmt.Errorf("no user message at seq %d", atSeq)
	}
	if edit {
		return atSeq, nil
	}
	for _, row := range rows[anchor+1:] {
		if row.Kind == "user_message" {
			return row.Seq, nil
		}
	}
	return end, nil
}

func hasUserMessageBefore(rows []store.StoredEvent, cutoff int64) bool {
	for _, row := range rows {
		if row.Seq >= cutoff {
			break
		}
		if row.Kind == "user_message" {
			return true
		}
	}
	return false
}

// lastMessageIDBefore returns the ACP id of the last top-level agent chunk
// before cutoff, or "" when that chunk carries none (it was recorded before
// ids were tracked) — an earlier id would silently drop turns from the fork's
// agent context. Subagent chunks (parentId set) are not part of the parent
// conversation the harness forks.
func lastMessageIDBefore(rows []store.StoredEvent, cutoff int64) string {
	id := ""
	for _, row := range rows {
		if row.Seq >= cutoff {
			break
		}
		if row.Kind != "message_chunk" && row.Kind != "thought_chunk" {
			continue
		}
		var payload struct {
			MessageID string `json:"messageId"`
			ParentID  string `json:"parentId"`
		}
		if json.Unmarshal([]byte(row.Payload), &payload) == nil && payload.ParentID == "" {
			id = payload.MessageID
		}
	}
	return id
}

// seedFork copies the source's transcript prefix into the fork's event log and
// returns the source→fork sequence mapping.
func (r *Registry) seedFork(id string, rows []store.StoredEvent, cutoff int64) (map[int64]int64, error) {
	_, base, err := r.store.EventBounds(id)
	if err != nil {
		return nil, err
	}
	var copied []store.StoredEvent
	seqMap := map[int64]int64{}
	for _, row := range rows {
		if row.Seq >= cutoff {
			break
		}
		if !forkedEventKinds[row.Kind] {
			continue
		}
		seqMap[row.Seq] = base + int64(len(copied)) + 1
		copied = append(copied, row)
	}
	for i := range copied {
		if copied[i].Kind == "user_message" {
			copied[i].Payload = remapUserMessageRefs(copied[i].Payload, seqMap)
		}
	}
	seqs, err := r.store.SeedEvents(id, copied)
	if err != nil {
		return nil, err
	}
	for i, seq := range seqs {
		if want := base + int64(i) + 1; seq != want {
			return nil, fmt.Errorf("fork event log was not empty (seq %d, want %d)", seq, want)
		}
	}
	return seqMap, nil
}

// remapUserMessageRefs points a copied user_message's quote citations at the
// fork's own sequence numbers.
func remapUserMessageRefs(payload string, seqMap map[int64]int64) string {
	var msg map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &msg) != nil || len(msg["blocks"]) == 0 {
		return payload
	}
	var blocks []agentadapter.PromptBlock
	if json.Unmarshal(msg["blocks"], &blocks) != nil {
		return payload
	}
	raw, err := json.Marshal(remapQuoteRefs(blocks, seqMap))
	if err != nil {
		return payload
	}
	msg["blocks"] = raw
	out, err := json.Marshal(msg)
	if err != nil {
		return payload
	}
	return string(out)
}

func remapQuoteRefs(blocks []agentadapter.PromptBlock, seqMap map[int64]int64) []agentadapter.PromptBlock {
	out := append([]agentadapter.PromptBlock(nil), blocks...)
	for i := range out {
		if out[i].Type != "quote" || out[i].RefSeq == 0 {
			continue
		}
		// A citation of something outside the fork keeps its quoted text but
		// loses the now-meaningless link.
		out[i].RefSeq = seqMap[out[i].RefSeq]
	}
	return out
}

// withSessionPreamble gives a fork with no prior history the same first-turn
// framing a fresh spawn's task gets.
func withSessionPreamble(blocks []agentadapter.PromptBlock) []agentadapter.PromptBlock {
	out := append([]agentadapter.PromptBlock(nil), blocks...)
	for i := range out {
		if out[i].Type == "text" && out[i].Text != "" {
			out[i].Text = firstPrompt("", out[i].Text)
			break
		}
	}
	return out
}
