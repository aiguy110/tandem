package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// Watch starts observing a session's turns so unanswered asks can be
// reminded. It is called for every session the registry starts or restores
// and is idempotent.
func (s *Service) Watch(sess *session.Session) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if prev, ok := s.watched[sess.ID]; ok {
		if prev.sess == sess {
			return
		}
		prev.unsub()
	}
	unsub := sess.OnTurnEnd(func(end session.TurnEnd) { s.onTurnEnd(sess, end) })
	s.watched[sess.ID] = watchedSession{sess: sess, unsub: unsub}
}

func (s *Service) unwatch(sessionID string) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if w, ok := s.watched[sessionID]; ok {
		w.unsub()
		delete(s.watched, sessionID)
	}
}

// onTurnEnd implements the reply-reminder rule. When a turn ends with an
// inbound ask still open the agent gets one reminder prompt; if the turn that
// reminder starts also ends with the ask open, the host declines on the
// agent's behalf (system no_reply) and closes it.
func (s *Service) onTurnEnd(sess *session.Session, end session.TurnEnd) {
	lock := s.lockFor(sess.ID)
	lock.Lock()
	defer lock.Unlock()
	obs, err := s.st.OpenAgentMsgObligations(sess.ID)
	if err != nil {
		slog.Error("agent obligation scan failed", "session_id", sess.ID, "error", err)
		return
	}
	if len(obs) == 0 {
		return
	}
	queued := map[string]bool{}
	for _, q := range sess.QueuedPrompts() {
		queued[q.ID] = true
	}
	var remind []store.AgentMsgObligation
	for _, ob := range obs {
		if ob.Reminded {
			// One reminder only. The ask stays open: the agent may still
			// answer later, and the asker's own deadline bounds the wait.
			continue
		}
		// An ask carried by a different, still-queued prompt has not had its
		// turn yet.
		if ob.ArmedPrompt == "" || ob.ArmedPrompt == end.PromptID || !queued[ob.ArmedPrompt] {
			remind = append(remind, ob)
		}
	}
	if len(remind) == 0 {
		return
	}
	s.remindAgent(sess, remind)
}

func (s *Service) remindAgent(sess *session.Session, obs []store.AgentMsgObligation) {
	var b strings.Builder
	b.WriteString("You ended your turn without answering ")
	if len(obs) == 1 {
		b.WriteString("a request from another agent:\n")
	} else {
		b.WriteString("these requests from other agents:\n")
	}
	for _, ob := range obs {
		fmt.Fprintf(&b, "- request %s from %s\n", ob.RequestID, s.Ref(Address{Host: ob.AskerHost, Agent: ob.AskerAgent, Name: ob.AskerName}))
	}
	b.WriteString("Answer with messages_send(to, body). If you cannot answer yet, send a short status update now and follow up when you can; you may send as many messages as the conversation needs.")
	env := Envelope{
		ID: newID("msg_"), ThreadID: obs[0].ThreadID, Kind: KindSystem, RequestID: obs[0].RequestID,
		From: s.systemAddress(), To: s.addressOf(sess), Body: b.String(), SentAt: s.now().UTC().Format(time.RFC3339),
		System: &SystemInfo{Event: EventReplyReminder},
	}
	log := slog.With("session_id", sess.ID, "requests", len(obs))
	blocks := []agentadapter.PromptBlock{{Type: "text", Text: s.promptText(env)}}
	receipt, err := sess.EnqueuePromptWithEvent(context.Background(), blocks, func(d string) eventlog.Event {
		return agentMessageEvent("in", env, d, "")
	})
	if err != nil {
		log.Warn("agent reply reminder could not be enqueued", "error", err)
		return
	}
	for _, ob := range obs {
		if err := s.st.RemindAgentMsgObligation(ob.RequestID, receipt.ID); err != nil {
			log.Error("agent obligation reminder record failed", "request_id", ob.RequestID, "error", err)
		}
	}
	log.Info("agent reply reminder enqueued", "prompt_id", receipt.ID, "disposition", receipt.Disposition)
}

// obligationMaxAge is how long an unanswered ask is held for its agent: the
// longest ask timeout, after which the asker has certainly stopped waiting.
const obligationMaxAge = MaxAskTimeoutSec * time.Second

// expireObligations closes unanswered asks older than obligationMaxAge so
// they stop counting as open on the agent.
func (s *Service) expireObligations(now time.Time) {
	obs, err := s.st.OpenAgentMsgObligations("")
	if err != nil {
		slog.Warn("agent obligation expiry scan failed", "error", err)
		return
	}
	changed := false
	for _, ob := range obs {
		if now.Sub(time.UnixMilli(ob.CreatedAt)) < obligationMaxAge {
			continue
		}
		if closed, err := s.st.CloseAgentMsgObligation(ob.RequestID, ms(now)); err == nil && closed {
			slog.Info("agent obligation lapsed unanswered", "session_id", ob.Session, "request_id", ob.RequestID, "asker", ob.AskerHost+"~"+ob.AskerAgent)
			changed = true
		}
	}
	if changed {
		s.summaryChanged()
	}
}

// SessionClosed is called after a user closes a session (not at daemon
// shutdown). Asks it had not answered are reported to their askers with
// recipient_gone, and its own pending requests are dropped.
func (s *Service) SessionClosed(sessionID string) {
	log := slog.With("session_id", sessionID)
	s.unwatch(sessionID)
	obs, err := s.st.OpenAgentMsgObligations(sessionID)
	if err != nil {
		log.Error("agent closing: obligation scan failed", "error", err)
	}
	for _, ob := range obs {
		closed, err := s.st.CloseAgentMsgObligation(ob.RequestID, ms(s.now()))
		if err != nil || !closed {
			continue
		}
		log.Info("agent closed with an unanswered ask; notifying asker", "request_id", ob.RequestID, "asker", ob.AskerHost+"~"+ob.AskerAgent)
		env := Envelope{
			ID: newID("msg_"), ThreadID: ob.ThreadID, Kind: KindSystem, RequestID: ob.RequestID,
			From: Address{Host: s.selfID(), Agent: sessionID}, To: Address{Host: ob.AskerHost, Agent: ob.AskerAgent, Name: ob.AskerName},
			Body:   "The agent you asked was closed before it answered.",
			Hop:    ob.Hop + 1,
			SentAt: s.now().UTC().Format(time.RFC3339), System: &SystemInfo{Event: EventRecipientGone},
		}
		s.dispatchAsync(env, "")
	}
	reqs, err := s.st.OpenAgentMsgRequests()
	if err != nil {
		log.Error("agent closing: request scan failed", "error", err)
	}
	for _, r := range reqs {
		if r.Session == sessionID {
			s.closeRequest(r.RequestID, "closed")
		}
	}
	in, _ := s.st.AgentMsgLinkRequestsForSession("in", sessionID)
	for _, row := range in {
		if claimed, _ := s.st.DeleteAgentMsgLinkRequest(row.ID); claimed {
			s.dispatchAsync(s.linkOutcomeEnvelope(Address{Host: s.selfID(), Agent: sessionID}, Address{Host: row.PeerHost, Agent: row.PeerAgent, Name: row.PeerName}, false, "The agent you asked was closed."), "")
		}
	}
	out, _ := s.st.AgentMsgLinkRequestsForSession("out", sessionID)
	for _, row := range out {
		_, _ = s.st.DeleteAgentMsgLinkRequest(row.ID)
	}
	s.summaryChanged()
}

// expireRequests closes asks past their deadline and tells the asker.
func (s *Service) expireRequests(ctx context.Context, now time.Time) {
	due, err := s.st.DueAgentMsgRequests(ms(now))
	if err != nil {
		slog.Error("agent request deadline scan failed", "error", err)
		return
	}
	for _, r := range due {
		if ctx.Err() != nil {
			return
		}
		if !s.closeRequest(r.RequestID, "timeout") {
			continue
		}
		slog.Warn("agent ask timed out", "request_id", r.RequestID, "session_id", r.Session, "to", r.ToHost+"~"+r.ToAgent, "timeout_sec", r.TimeoutSec)
		// The ask may still sit in the outbox (recipient host offline): it
		// must not be delivered after the asker has been told it timed out.
		if row, err := s.st.AgentMsgOutbox(r.MessageID); err == nil && row != nil && row.Status == "pending" {
			if err := s.st.UpdateAgentMsgOutbox(row.ID, "failed", "", "expired", row.Attempts, ms(now)); err != nil {
				slog.Warn("agent outbox cancel failed", "envelope_id", row.ID, "error", err)
			}
		}
		peer := Address{Host: r.ToHost, Agent: r.ToAgent, Name: r.ToName}
		body := fmt.Sprintf("No answer from %s to request %s within %s.", s.Ref(peer), r.RequestID, (time.Duration(r.TimeoutSec) * time.Second).String())
		s.deliverSystem(r.Session, EventTimeout, body, r.RequestID, r.ThreadID, peer)
	}
}
