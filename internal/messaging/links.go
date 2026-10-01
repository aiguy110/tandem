package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

func (s *Service) linkView(l store.AgentMsgLink) Link {
	used, err := s.st.AgentMsgInboundSince(l.ID, ms(s.now().Add(-time.Hour)))
	if err != nil {
		slog.Warn("agent link usage lookup failed", "link_id", l.ID, "error", err)
	}
	return Link{
		ID: l.ID, From: Address{Host: l.FromHost, Agent: l.FromAgent, Name: l.FromName}, To: l.ToSession,
		Delivery: l.Delivery, BudgetPerHour: l.BudgetPerHour, MaxHops: l.MaxHops, Paused: l.Paused,
		Source: l.Source, CreatedAt: rfc3339(l.CreatedAt), UsedLastHour: used,
	}
}

// Links returns a session's inbound links, listing and card.
func (s *Service) Links(sessionID string) (LinksView, error) {
	if s.opts.Sessions.Get(sessionID) == nil {
		return LinksView{}, fmt.Errorf("no such agent: %s", sessionID)
	}
	links, err := s.st.AgentMsgLinks(sessionID)
	if err != nil {
		return LinksView{}, err
	}
	cfg, err := s.st.AgentMsgSettings(sessionID)
	if err != nil {
		return LinksView{}, err
	}
	view := LinksView{SessionID: sessionID, Links: make([]Link, 0, len(links)), Listed: cfg.Listed, Card: cfg.Card}
	for _, l := range links {
		view.Links = append(view.Links, s.linkView(l))
	}
	return view, nil
}

// SetLink creates or edits the link granting in.From access to a local agent.
func (s *Service) SetLink(sessionID string, in LinkInput) error {
	sess := s.opts.Sessions.Get(sessionID)
	if sess == nil {
		return fmt.Errorf("no such agent: %s", sessionID)
	}
	if in.From.Host == "" || in.From.Agent == "" {
		return errors.New("link sender needs a host and an agent")
	}
	if in.From.Host == s.selfID() && in.From.Agent == sessionID {
		return errors.New("an agent cannot be linked to itself")
	}
	switch in.Delivery {
	case "":
		in.Delivery = DeliverySteer
	case DeliverySteer, DeliveryQueue:
	default:
		return fmt.Errorf("delivery must be %q or %q", DeliverySteer, DeliveryQueue)
	}
	if in.BudgetPerHour < 0 || in.MaxHops < 0 {
		return errors.New("budgetPerHour and maxHops must not be negative")
	}
	if in.BudgetPerHour == 0 {
		in.BudgetPerHour = DefaultBudgetPerHour
	}
	if in.MaxHops == 0 {
		in.MaxHops = DefaultMaxHops
	}
	stored, err := s.st.UpsertAgentMsgLink(store.AgentMsgLink{
		ID: newID("lnk_"), FromHost: in.From.Host, FromAgent: in.From.Agent, FromName: in.From.Name, ToSession: sessionID,
		Delivery: in.Delivery, BudgetPerHour: in.BudgetPerHour, MaxHops: in.MaxHops, Paused: in.Paused, Source: SourceUser,
	})
	if err != nil {
		return err
	}
	slog.Info("agent link set", "link_id", stored.ID, "from", in.From.String(), "to_session", sessionID, "delivery", stored.Delivery,
		"budget_per_hour", stored.BudgetPerHour, "max_hops", stored.MaxHops, "paused", stored.Paused)
	s.invalidateDirectory()
	s.linksChanged(sessionID)
	return nil
}

// DeleteLink removes a link.
func (s *Service) DeleteLink(sessionID string, from Address) error {
	if s.opts.Sessions.Get(sessionID) == nil {
		return fmt.Errorf("no such agent: %s", sessionID)
	}
	deleted, err := s.st.DeleteAgentMsgLink(sessionID, from.Host, from.Agent)
	if err != nil {
		return err
	}
	slog.Info("agent link deleted", "from", from.String(), "to_session", sessionID, "existed", deleted)
	s.invalidateDirectory()
	s.linksChanged(sessionID)
	return nil
}

// SetListed shows or hides an agent in the directory.
func (s *Service) SetListed(sessionID string, listed bool) error {
	if s.opts.Sessions.Get(sessionID) == nil {
		return fmt.Errorf("no such agent: %s", sessionID)
	}
	if err := s.st.SetAgentMsgListed(sessionID, listed); err != nil {
		return err
	}
	slog.Info("agent directory listing set", "session_id", sessionID, "listed", listed)
	s.invalidateDirectory()
	s.linksChanged(sessionID)
	// Summaries carry "unlisted" so browsers offer only listed agents.
	s.summaryChanged()
	return nil
}

// RequestLink asks, on behalf of a local agent, for permission to message
// another agent and returns at once with status "pending". The outcome arrives
// later as a system link_approved or link_denied message; RequestLinkWait is
// the blocking form the agent-facing tool uses.
func (s *Service) RequestLink(ctx context.Context, sessionID, to, reason string) (LinkRequestResult, error) {
	return s.requestLink(ctx, sessionID, to, reason, nil)
}

// linkOutcome is the human's decision on a link request, as recorded on the
// requesting host.
type linkOutcome struct {
	approved bool
	env      Envelope
}

func (s *Service) outcomeResult(o linkOutcome) LinkRequestResult {
	status := "denied"
	if o.approved {
		status = "approved"
	}
	return LinkRequestResult{Status: status, Note: o.env.Body, Ref: s.Ref(o.env.From)}
}

// linkWaiter is one messages_request_link call blocked on a decision.
type linkWaiter struct {
	id string // the pending outbound link request
	ch chan linkOutcome
}

func (s *Service) addLinkWaiter(w *linkWaiter) {
	s.linkWaitMu.Lock()
	defer s.linkWaitMu.Unlock()
	if s.linkWaits[w.id] == nil {
		s.linkWaits[w.id] = map[*linkWaiter]struct{}{}
	}
	s.linkWaits[w.id][w] = struct{}{}
}

func (s *Service) removeLinkWaiter(w *linkWaiter) {
	if w == nil || w.id == "" {
		return
	}
	s.linkWaitMu.Lock()
	defer s.linkWaitMu.Unlock()
	delete(s.linkWaits[w.id], w)
	if len(s.linkWaits[w.id]) == 0 {
		delete(s.linkWaits, w.id)
	}
}

// claimLinkWaiters hands a recorded decision to every call blocked on request
// id and reports whether there was one; if so the calls carry the answer and
// the agent is not also sent a system message.
func (s *Service) claimLinkWaiters(id string, o linkOutcome) bool {
	s.linkWaitMu.Lock()
	defer s.linkWaitMu.Unlock()
	claimed := false
	for w := range s.linkWaits[id] {
		select {
		case w.ch <- o:
			claimed = true
		default:
		}
	}
	return claimed
}

// RequestLinkWait is RequestLink that blocks until the human decides: it
// returns status "approved" or "denied" once the requesting host records the
// outcome (however it arrived), or "pending" with a note after
// Options.LinkRequestWait, in which case the outcome is still delivered to the
// agent as a system message later. An outcome that arrives while this call is
// blocked is returned here instead, not sent to the agent a second time.
func (s *Service) RequestLinkWait(ctx context.Context, sessionID, to, reason string) (LinkRequestResult, error) {
	w := &linkWaiter{ch: make(chan linkOutcome, 1)}
	defer s.removeLinkWaiter(w)
	res, err := s.requestLink(ctx, sessionID, to, reason, w)
	if err != nil || res.Status != StatusPending {
		return res, err
	}
	log := slog.With("session_id", sessionID, "link_request_id", w.id)
	if w.id == "" {
		return res, nil
	}
	log.Info("agent link request blocking for the human's decision", "wait", s.opts.LinkRequestWait.String())
	timer := time.NewTimer(s.opts.LinkRequestWait)
	defer timer.Stop()
	select {
	case o := <-w.ch:
		log.Info("agent link request decided while blocked", "approved", o.approved)
		return s.outcomeResult(o), nil
	case <-timer.C:
	case <-s.ctx.Done():
	case <-ctx.Done():
		s.removeLinkWaiter(w)
		select {
		case o := <-w.ch:
			// The decision was claimed for a caller that is gone: the agent
			// must still learn of it.
			log.Warn("agent link request caller went away as the decision arrived; delivering it as a message", "approved", o.approved)
			event := EventLinkDenied
			if o.approved {
				event = EventLinkApproved
			}
			s.deliverSystem(sessionID, event, o.env.Body, "", o.env.ThreadID, o.env.From)
		default:
			log.Info("agent link request caller went away before a decision", "error", ctx.Err())
		}
		return LinkRequestResult{}, ctx.Err()
	}
	s.removeLinkWaiter(w)
	select {
	case o := <-w.ch:
		return s.outcomeResult(o), nil
	default:
	}
	log.Info("agent link request still pending after the wait", "wait", s.opts.LinkRequestWait.String())
	res.Note = "No decision yet: the human has not approved or denied this request. The outcome will be delivered to you as a message when it is decided."
	return res, nil
}

func (s *Service) requestLink(ctx context.Context, sessionID, to, reason string, w *linkWaiter) (LinkRequestResult, error) {
	sess, err := s.liveSession(sessionID)
	if err != nil {
		return LinkRequestResult{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return LinkRequestResult{}, &Rejection{Code: ErrInvalid, Message: "reason is required"}
	}
	reason = truncate(reason, maxReasonLen)
	dest, err := s.resolve(ctx, sess, to)
	if err != nil {
		return LinkRequestResult{}, err
	}
	from := s.addressOf(sess)
	if dest.Same(from) {
		return LinkRequestResult{}, &Rejection{Code: ErrInvalid, Message: "an agent cannot link to itself"}
	}
	if s.Paused() {
		return LinkRequestResult{}, &Rejection{Code: ErrMessagingPaused, Message: "agent messaging is paused on this host"}
	}
	log := slog.With("session_id", sess.ID, "to", dest.String())
	req := store.AgentMsgLinkRequest{ID: newID("lrq_"), Role: "out", Session: sess.ID, PeerHost: dest.Host, PeerAgent: dest.Agent, PeerName: dest.Name, Reason: reason, CreatedAt: ms(s.now())}
	added, err := s.st.AddAgentMsgLinkRequest(req)
	if err != nil {
		return LinkRequestResult{}, &Rejection{Code: ErrHostUnreachable, Message: "could not record the link request"}
	}
	if !added {
		log.Info("agent link request already pending")
		if w != nil {
			// Wait on the request that is already out.
			if existing, err := s.st.AgentMsgLinkRequestFor("out", sess.ID, dest.Host, dest.Agent); err == nil && existing != nil {
				w.id = existing.ID
				s.addLinkWaiter(w)
			}
		}
		s.kickPull(false)
		return LinkRequestResult{Status: StatusPending, Ref: s.Ref(dest)}, nil
	}
	if w != nil {
		// Registered before the request is sent, so an immediate decision
		// (an existing link is simply confirmed) cannot be missed.
		w.id = req.ID
		s.addLinkWaiter(w)
	}
	res := s.sendLinkRequest(ctx, from, dest, reason)
	if res.Error != "" {
		// The request never reached the other agent's host; forget it so a
		// later attempt is not mistaken for a duplicate.
		if _, derr := s.st.DeleteAgentMsgLinkRequest(req.ID); derr != nil {
			log.Warn("agent link request cleanup failed", "error", derr)
		}
		log.Warn("agent link request failed", "code", res.Error, "detail", res.Message)
		return LinkRequestResult{}, &Rejection{Code: res.Error, Message: firstNonEmpty(res.Message, res.Error)}
	}
	log.Info("agent link request sent", "request_id", req.ID)
	// The decision may come back by push or, when the other host cannot reach
	// this one, only by pull: start looking now.
	s.kickPull(false)
	return LinkRequestResult{Status: StatusPending, Ref: s.Ref(dest)}, nil
}

func (s *Service) sendLinkRequest(ctx context.Context, from, to Address, reason string) LinkRequestResult {
	if to.Host == s.selfID() {
		return s.HandleLinkRequest(ctx, "", from, to, reason)
	}
	raw, denied, err := s.callFederation(ctx, to.Host, map[string]any{"t": "agent_link_request", "from": from, "to": to, "reason": reason})
	if err != nil {
		return LinkRequestResult{Error: ErrHostUnreachable, Message: err.Error()}
	}
	if denied != nil {
		return LinkRequestResult{Error: ErrAccessDenied, Message: denied.Message}
	}
	return parseLinkRequestResult(raw)
}

func parseLinkRequestResult(raw []byte) LinkRequestResult {
	var resp struct {
		T       string `json:"t"`
		Status  string `json:"status"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return LinkRequestResult{Error: ErrHostUnreachable, Message: "invalid response: " + err.Error()}
	}
	if resp.T != "agent_link_request_result" {
		return LinkRequestResult{Error: ErrHostUnreachable, Message: "host does not support agent messaging: " + resp.Error}
	}
	if resp.Error != "" {
		return LinkRequestResult{Status: StatusRejected, Error: resp.Error, Message: resp.Message}
	}
	return LinkRequestResult{Status: firstNonEmpty(resp.Status, StatusPending)}
}

// HandleLinkRequest is the recipient host's side of a link request: it raises
// a daemon-owned approval on the asked agent's session and returns at once.
func (s *Service) HandleLinkRequest(ctx context.Context, origin string, from, to Address, reason string) LinkRequestResult {
	self := s.selfID()
	log := slog.With("from", from.String(), "to", to.String(), "origin", origin)
	fail := func(code, msg string) LinkRequestResult {
		log.Warn("agent link request rejected", "code", code, "detail", msg)
		return LinkRequestResult{Status: StatusRejected, Error: code, Message: msg}
	}
	if from.Host == "" || from.Agent == "" || to.Agent == "" {
		return fail(ErrInvalid, "from and to must name agents")
	}
	if to.Host != self {
		return fail(ErrRecipientGone, "this host is "+self+", not "+to.Host)
	}
	expect := self
	if origin != "" {
		expect = origin
	}
	if from.Host != expect {
		return fail(ErrAccessDenied, "link request sender does not match the requesting host")
	}
	if s.Paused() {
		return fail(ErrMessagingPaused, "agent messaging is paused on this host")
	}
	sess := s.opts.Sessions.Get(to.Agent)
	if sess == nil {
		return fail(ErrRecipientGone, "no open agent "+to.Agent)
	}
	reason = truncate(strings.TrimSpace(reason), maxReasonLen)
	if link, err := s.st.AgentMsgLink(from.Host, from.Agent, sess.ID); err == nil && link != nil && !link.Paused {
		// Already linked: just confirm.
		log.Info("agent link request for an existing link; confirming")
		s.answerLinkRequest(sess, from, true, "You already have a link to this agent.")
		return LinkRequestResult{Status: StatusPending}
	}
	pending, err := s.st.AgentMsgLinkRequestsForSession("in", sess.ID)
	if err != nil {
		return fail(ErrHostUnreachable, "could not read pending link requests")
	}
	for _, p := range pending {
		if p.PeerHost == from.Host && p.PeerAgent == from.Agent {
			log.Info("agent link request already awaiting approval")
			return LinkRequestResult{Status: StatusPending}
		}
	}
	if len(pending) >= maxPendingLinkReqs {
		return fail(ErrBudgetExceeded, "too many link requests are awaiting approval for this agent")
	}
	row := store.AgentMsgLinkRequest{ID: newID("lrq_"), Role: "in", Session: sess.ID, PeerHost: from.Host, PeerAgent: from.Agent, PeerName: from.Name, Reason: reason, CreatedAt: ms(s.now())}
	if _, err := s.st.AddAgentMsgLinkRequest(row); err != nil {
		return fail(ErrHostUnreachable, "could not record the link request")
	}
	log.Info("agent link request awaiting approval", "link_request_id", row.ID, "session_id", sess.ID)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.awaitLinkApproval(sess, row)
	}()
	return LinkRequestResult{Status: StatusPending}
}

// resumeLinkApprovals re-raises approvals that were pending when the daemon
// stopped; an approval cannot outlive its process.
func (s *Service) resumeLinkApprovals() {
	rows, err := s.st.AgentMsgLinkRequests("in")
	if err != nil {
		slog.Error("agent link request resume failed", "error", err)
		return
	}
	for _, row := range rows {
		row := row
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			// Get waits for a session that is still restoring.
			sess := s.opts.Sessions.Get(row.Session)
			if sess == nil {
				if claimed, _ := s.st.DeleteAgentMsgLinkRequest(row.ID); claimed {
					s.dispatchAsync(s.linkOutcomeEnvelope(Address{Host: s.selfID(), Agent: row.Session}, Address{Host: row.PeerHost, Agent: row.PeerAgent, Name: row.PeerName}, false, "The agent you asked is no longer open."), "")
				}
				return
			}
			slog.Info("agent link approval re-raised after restart", "link_request_id", row.ID, "session_id", row.Session)
			s.awaitLinkApproval(sess, row)
		}()
	}
}

// awaitLinkApproval raises the approval and applies the human's decision.
func (s *Service) awaitLinkApproval(sess *session.Session, row store.AgentMsgLinkRequest) {
	log := slog.With("link_request_id", row.ID, "session_id", sess.ID, "from", row.PeerHost+"~"+row.PeerAgent)
	peer := Address{Host: row.PeerHost, Agent: row.PeerAgent, Name: row.PeerName}
	title := fmt.Sprintf("%s wants to message this agent: %s", s.Ref(peer), row.Reason)
	ctx, cancel := context.WithTimeout(s.ctx, linkRequestMaxAge)
	defer cancel()
	choice, err := sess.RequestPermission(ctx, "msglink-"+row.ID, title, []agentadapter.ApprovalOption{
		{OptionID: "allow", Name: "Allow"}, {OptionID: "deny", Name: "Deny"},
	})
	if err != nil {
		if s.ctx.Err() != nil {
			// The daemon is stopping; the request stays stored and is
			// re-raised on the next start.
			log.Info("agent link approval interrupted by shutdown")
			return
		}
		log.Warn("agent link approval ended without a decision", "error", err)
		s.resolveLinkRequest(sess, row, false, "No decision was made on the link request.")
		return
	}
	if choice != "allow" {
		log.Info("agent link request denied")
		s.resolveLinkRequest(sess, row, false, "The link request was denied.")
		return
	}
	s.resolveLinkRequest(sess, row, true, "")
}

// resolveLinkRequest claims the pending row (so only one resolution wins),
// creates the link when approved, and tells the requester.
func (s *Service) resolveLinkRequest(sess *session.Session, row store.AgentMsgLinkRequest, approved bool, note string) {
	claimed, err := s.st.DeleteAgentMsgLinkRequest(row.ID)
	if err != nil || !claimed {
		return
	}
	peer := Address{Host: row.PeerHost, Agent: row.PeerAgent, Name: row.PeerName}
	if approved {
		stored, err := s.st.UpsertAgentMsgLink(store.AgentMsgLink{
			ID: newID("lnk_"), FromHost: peer.Host, FromAgent: peer.Agent, FromName: peer.Name, ToSession: sess.ID,
			Delivery: DeliverySteer, BudgetPerHour: DefaultBudgetPerHour, MaxHops: DefaultMaxHops, Source: SourceApproval,
		})
		if err != nil {
			slog.Error("agent link creation failed after approval", "link_request_id", row.ID, "error", err)
			s.answerLinkRequest(sess, peer, false, "The link could not be created.")
			return
		}
		slog.Info("agent link approved", "link_id", stored.ID, "from", peer.String(), "to_session", sess.ID)
		s.invalidateDirectory()
		s.linksChanged(sess.ID)
	}
	s.answerLinkRequest(sess, peer, approved, note)
}

func (s *Service) answerLinkRequest(sess *session.Session, peer Address, approved bool, note string) {
	s.dispatchAsync(s.linkOutcomeEnvelope(s.addressOf(sess), peer, approved, note), "")
}

func (s *Service) linkOutcomeEnvelope(from, to Address, approved bool, note string) Envelope {
	event, body := EventLinkDenied, note
	if approved {
		event = EventLinkApproved
		// The sender is named by the receiving host (the prompt's from
		// reference), not here: host names are viewer-relative.
		body = "Your link request was approved. You can now message this agent with messages_send or messages_ask."
	} else if body == "" {
		body = "The link request was denied."
	} else {
		body = "Your link request was not granted: " + note
	}
	return Envelope{
		ID: newID("msg_"), ThreadID: newID("thr_"), Kind: KindSystem, From: from, To: to, Body: body,
		SentAt: s.now().UTC().Format(time.RFC3339), System: &SystemInfo{Event: event},
	}
}

// expireLinkRequests gives up on requests nobody answered within 24 hours.
func (s *Service) expireLinkRequests(now time.Time) {
	rows, err := s.st.AgentMsgLinkRequests("out")
	if err != nil {
		slog.Warn("agent link request expiry scan failed", "error", err)
		return
	}
	for _, row := range rows {
		if now.Sub(time.UnixMilli(row.CreatedAt)) < linkRequestMaxAge {
			continue
		}
		if claimed, _ := s.st.DeleteAgentMsgLinkRequest(row.ID); !claimed {
			continue
		}
		slog.Info("agent link request expired", "link_request_id", row.ID, "session_id", row.Session, "peer", row.PeerHost+"~"+row.PeerAgent)
		peer := Address{Host: row.PeerHost, Agent: row.PeerAgent, Name: row.PeerName}
		s.deliverSystem(row.Session, EventLinkDenied, fmt.Sprintf("Your link request to %s got no answer within 24 hours.", s.Ref(peer)), "", "", peer)
	}
}
