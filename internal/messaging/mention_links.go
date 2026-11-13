package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// mentionLinkTimeout bounds linking every agent one prompt mentions.
const mentionLinkTimeout = 30 * time.Second

// linkMentioned links a session with every agent the user @-mentioned in a
// prompt to it, in both directions, so the agent can message them without
// asking for a link: the user's mention is the approval. Existing links are
// left as they are (a human may have paused or tuned them). When this host
// may not set links on a remote agent's host, it falls back to an ordinary
// link request, which the human approves there.
func (s *Service) linkMentioned(sess *session.Session, refs []string) {
	if s.Paused() {
		slog.Info("agent mention links skipped: messaging paused", "session_id", sess.ID, "refs", len(refs))
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, mentionLinkTimeout)
	defer cancel()
	from := s.addressOf(sess)
	for _, ref := range refs {
		log := slog.With("session_id", sess.ID, "ref", ref)
		dest, err := s.resolve(ctx, sess, ref)
		if err != nil {
			log.Info("agent mention not linked: reference did not resolve", "error", err)
			continue
		}
		if dest.Same(from) {
			continue
		}
		log = log.With("to", dest.String())
		// The mentioned agent may message this one.
		s.ensureLocalLink(dest, sess.ID, log)
		// This agent may message the mentioned one.
		if dest.Host == s.selfID() {
			if s.opts.Sessions.Get(dest.Agent) == nil {
				log.Info("agent mention target is not open here")
				continue
			}
			s.ensureLocalLink(from, dest.Agent, log)
			continue
		}
		if err := s.ensureRemoteLink(ctx, from, dest, log); err != nil {
			log.Info("agent mention could not set the remote link directly; requesting it", "error", err)
			reason := fmt.Sprintf("The user mentioned this agent in a prompt to %s.", firstNonEmpty(from.Name, from.Agent))
			if _, rerr := s.RequestLink(ctx, sess.ID, s.Ref(dest), reason); rerr != nil {
				log.Warn("agent mention link request failed", "error", rerr)
			}
		}
	}
}

// ensureLocalLink grants peer access to a local session unless a link exists.
func (s *Service) ensureLocalLink(peer Address, sessionID string, log *slog.Logger) {
	existing, err := s.st.AgentMsgLink(peer.Host, peer.Agent, sessionID)
	if err != nil {
		log.Error("agent mention link lookup failed", "to_session", sessionID, "error", err)
		return
	}
	if existing != nil {
		log.Debug("agent mention link already exists", "link_id", existing.ID, "to_session", sessionID, "paused", existing.Paused)
		return
	}
	stored, err := s.st.UpsertAgentMsgLink(store.AgentMsgLink{
		ID: newID("lnk_"), FromHost: peer.Host, FromAgent: peer.Agent, FromName: peer.Name, ToSession: sessionID,
		Delivery: DeliverySteer, BudgetPerHour: DefaultBudgetPerHour, MaxHops: DefaultMaxHops, Source: SourceMention,
	})
	if err != nil {
		log.Error("agent mention link creation failed", "to_session", sessionID, "error", err)
		return
	}
	log.Info("agent link granted by mention", "link_id", stored.ID, "from", peer.String(), "to_session", sessionID)
	s.invalidateDirectory()
	s.linksChanged(sessionID)
	if peer.Host != s.selfID() {
		// The peer's host may only be able to reach this one by pull.
		s.kickPull(false)
	}
}

// ensureRemoteLink grants from access to dest on dest's host through
// federation (list_agent_links, then set_agent_link if absent). It fails when
// this host lacks the access to do so.
func (s *Service) ensureRemoteLink(ctx context.Context, from, dest Address, log *slog.Logger) error {
	raw, denied, err := s.callFederation(ctx, dest.Host, map[string]any{"t": "list_agent_links", "sessionId": dest.Agent})
	if err != nil {
		return err
	}
	if denied != nil {
		return errors.New(firstNonEmpty(denied.Message, denied.Error))
	}
	var view struct {
		T     string `json:"t"`
		Error string `json:"error"`
		Links []Link `json:"links"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("invalid list_agent_links response: %w", err)
	}
	if view.Error != "" || view.T != "agent_links" {
		return fmt.Errorf("list_agent_links: %s", firstNonEmpty(view.Error, "unexpected response "+view.T))
	}
	for _, l := range view.Links {
		if l.From.Same(from) {
			log.Debug("agent mention remote link already exists", "link_id", l.ID, "paused", l.Paused)
			return nil
		}
	}
	link := LinkInput{From: from, Delivery: DeliverySteer, BudgetPerHour: DefaultBudgetPerHour, MaxHops: DefaultMaxHops}
	raw, denied, err = s.callFederation(ctx, dest.Host, map[string]any{"t": "set_agent_link", "sessionId": dest.Agent, "link": link})
	if err != nil {
		return err
	}
	if denied != nil {
		return errors.New(firstNonEmpty(denied.Message, denied.Error))
	}
	var ack struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return fmt.Errorf("invalid set_agent_link response: %w", err)
	}
	if ack.Error != "" {
		return fmt.Errorf("set_agent_link: %s", ack.Error)
	}
	log.Info("agent link granted by mention on remote host")
	s.invalidateDirectory()
	return nil
}
