package wsserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aiguy110/tandem/internal/agentadapter"
)

func (c *connection) handlePromptRequest(m clientMessage) {
	sess, ok := c.requireSession(m)
	if !ok {
		return
	}
	db := c.server.opts.PromptRequests
	if m.RequestID != "" {
		if len(m.RequestID) > 128 || db == nil {
			c.commandError(m, errors.New("prompt deduplication unavailable"))
			return
		}
		payload, _ := json.Marshal([]any{m.T, m.Text, m.Blocks, m.IncludeQueue})
		hash := sha256.Sum256(payload)
		claimed, result, err := db.ClaimPromptRequest(sess.ID, m.RequestID, hex.EncodeToString(hash[:]))
		if err != nil {
			slog.Warn("prompt request claim failed", "session_id", sess.ID, "request_id", m.RequestID, "error", err)
			c.commandError(m, err)
			return
		}
		if !claimed {
			slog.Info("duplicate prompt request reconciled", "session_id", sess.ID, "request_id", m.RequestID)
			var reply map[string]any
			if err := json.Unmarshal([]byte(result), &reply); err != nil {
				c.commandError(m, err)
				return
			}
			c.send(withCorr(reply, m.CorrID))
			return
		}
	}
	reply := map[string]any{"t": "ack", "sessionId": sess.ID}
	blocks := m.Blocks
	if blocks == nil {
		blocks = []agentadapter.PromptBlock{{Type: "text", Text: m.Text}}
	}
	var err error
	switch m.T {
	case "prompt", "aside":
		if m.T == "prompt" {
			receipt, e := sess.EnqueuePrompt(context.Background(), blocks)
			err = e
			reply["promptId"], reply["disposition"], reply["position"] = receipt.ID, receipt.Disposition, receipt.Position
		} else {
			receipt, e := sess.EnqueueAside(context.Background(), m.Text)
			err = e
			reply["promptId"], reply["disposition"], reply["position"] = receipt.ID, receipt.Disposition, receipt.Position
		}
	case "steer":
		if m.IncludeQueue {
			var cleared int
			cleared, err = sess.SteerWithQueue(context.Background(), m.Blocks)
			reply["cleared"] = cleared
		} else {
			err = sess.Steer(context.Background(), blocks)
		}
		reply["disposition"] = "steered"
	}
	if err != nil {
		reply["error"] = err.Error()
	}
	if m.RequestID != "" {
		encoded, _ := json.Marshal(reply)
		if err := db.CompletePromptRequest(sess.ID, m.RequestID, string(encoded)); err != nil {
			slog.Error("prompt receipt persistence failed", "session_id", sess.ID, "request_id", m.RequestID, "error", err)
			c.commandError(m, errors.New("prompt receipt could not be saved; check the chat and queue before retrying"))
			return
		}
		slog.Info("prompt request completed", "session_id", sess.ID, "request_id", m.RequestID, "type", m.T, "failed", err != nil)
	}
	c.send(withCorr(reply, m.CorrID))
}
