package wsserver

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aiguy110/tandem/internal/messaging"
)

// Messaging is the agent-messaging service the browser protocol drives
// (docs/agent-messaging.md). Federation carries these commands like any other;
// origin is the federation origin of a relayed command ("" for this host's own
// browsers).
type Messaging interface {
	Deliver(ctx context.Context, origin string, env messaging.Envelope) messaging.Result
	LocalDirectory(requester *messaging.Address, query string) []messaging.DirectoryEntry
	HandleLinkRequest(ctx context.Context, origin string, from, to messaging.Address, reason string) messaging.LinkRequestResult
	Links(sessionID string) (messaging.LinksView, error)
	SetLink(sessionID string, in messaging.LinkInput) error
	DeleteLink(sessionID string, from messaging.Address) error
	SetListed(sessionID string, listed bool) error
	State() messaging.State
	SetPaused(paused bool) error
}

// BroadcastAgentLinks sends a session's current links to every browser
// subscribed to it.
func (h *Handler) BroadcastAgentLinks(sessionID string) {
	if h.opts.Messaging == nil {
		return
	}
	view, err := h.opts.Messaging.Links(sessionID)
	if err != nil {
		slog.Debug("agent links broadcast skipped", "session_id", sessionID, "error", err)
		return
	}
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.mu.Lock()
		_, subscribed := c.subs[sessionID]
		c.mu.Unlock()
		if subscribed {
			c.send(agentLinksEnvelope(view))
		}
	}
}

// BroadcastMessagingState sends the host-wide messaging state (kill switch) to
// every connected browser.
func (h *Handler) BroadcastMessagingState() {
	if h.opts.Messaging == nil {
		return
	}
	state := h.opts.Messaging.State()
	h.mu.Lock()
	connections := make([]*connection, 0, len(h.connections))
	for c := range h.connections {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	for _, c := range connections {
		c.send(messagingStateEnvelope(state))
	}
}

func agentLinksEnvelope(view messaging.LinksView) map[string]any {
	return map[string]any{"t": "agent_links", "sessionId": view.SessionID, "links": view.Links, "listed": view.Listed, "card": view.Card}
}

// handleMessaging serves the agent-messaging commands. Browser-originated
// commands are trusted like any other; commands relayed by federation have
// already passed the executing host's access-level check, and carry the
// requesting host as FederationOrigin.
func (c *connection) handleMessaging(m clientMessage) {
	svc := c.server.opts.Messaging
	if svc == nil {
		c.commandError(m, errors.New("agent messaging is unavailable"))
		return
	}
	switch m.T {
	case "list_agent_links":
		view, err := svc.Links(m.SessionID)
		if err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(agentLinksEnvelope(view), m.CorrID))
	case "set_agent_link":
		if m.Link == nil {
			c.commandError(m, errors.New("link is required"))
			return
		}
		if err := svc.SetLink(m.SessionID, *m.Link); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, m.SessionID)
		c.server.BroadcastAgentLinks(m.SessionID)
	case "delete_agent_link":
		if m.From == nil {
			c.commandError(m, errors.New("from is required"))
			return
		}
		if err := svc.DeleteLink(m.SessionID, *m.From); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, m.SessionID)
		c.server.BroadcastAgentLinks(m.SessionID)
	case "set_agent_listed":
		if m.Listed == nil {
			c.commandError(m, errors.New("listed is required"))
			return
		}
		if err := svc.SetListed(m.SessionID, *m.Listed); err != nil {
			c.commandError(m, err)
			return
		}
		c.commandAck(m, m.SessionID)
		c.server.BroadcastAgentLinks(m.SessionID)
	case "get_messaging_state":
		state := svc.State()
		c.send(withCorr(messagingStateEnvelope(state), m.CorrID))
	case "set_messaging_paused":
		if m.Paused == nil {
			c.commandError(m, errors.New("paused is required"))
			return
		}
		if err := svc.SetPaused(*m.Paused); err != nil {
			c.commandError(m, err)
			return
		}
		c.send(withCorr(map[string]any{"t": "ack"}, m.CorrID))
		c.server.BroadcastMessagingState()
	case "agent_directory":
		entries := svc.LocalDirectory(m.Requester, m.Query)
		c.send(withCorr(map[string]any{"t": "agent_directory", "entries": entries}, m.CorrID))
	case "agent_message_deliver":
		if m.Envelope == nil {
			c.commandError(m, errors.New("envelope is required"))
			return
		}
		res := svc.Deliver(context.Background(), m.FederationOrigin, *m.Envelope)
		reply := map[string]any{"t": "agent_message_result", "id": res.ID, "status": res.Status}
		if res.Error != "" {
			reply["error"] = res.Error
			if res.Message != "" {
				reply["message"] = res.Message
			}
		}
		c.send(withCorr(reply, m.CorrID))
	case "agent_link_request":
		if m.From == nil || m.To == nil {
			c.commandError(m, errors.New("from and to are required"))
			return
		}
		res := svc.HandleLinkRequest(context.Background(), m.FederationOrigin, *m.From, *m.To, m.Reason)
		reply := map[string]any{"t": "agent_link_request_result", "status": res.Status}
		if res.Error != "" {
			reply["error"] = res.Error
			if res.Message != "" {
				reply["message"] = res.Message
			}
		}
		c.send(withCorr(reply, m.CorrID))
	}
}

// messagingStateEnvelope omits hostId: browsers key host state by their own
// route ID, which a relaying parent stamps on the way through; an unstamped
// message is this daemon's own.
func messagingStateEnvelope(state messaging.State) map[string]any {
	return map[string]any{"t": "messaging_state", "paused": state.Paused}
}
