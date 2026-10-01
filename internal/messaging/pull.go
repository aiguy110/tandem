package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/federation"
)

// Pull fallback (docs/agent-messaging.md, "Outbox").
//
// A host that answers a request may hold no federation rights on the host that
// made it (a parent's children have no access on it by default, and a parent
// that grants them no view is not even in their fleet view). The requester
// does hold rights on the responder, so it fetches the responses itself:
//
//	agent_message_pull      the responder serves its undelivered envelopes
//	                        addressed to the caller;
//	agent_message_pull_ack  the caller reports what became of them.
//
// Both are ordinary message-level browser commands. The executing host trusts
// only the federation origin as the caller's identity.

const (
	// pullInterval is the default Options.PullInterval: how often a host with
	// outstanding requests polls the hosts they were sent to.
	pullInterval = 5 * time.Second
	// pullBackoff is how long a host that does not implement (or refuses)
	// agent_message_pull is left alone.
	pullBackoff = 5 * time.Minute
	// pullFailuresBeforeBackoff is how many consecutive failed polls of a
	// connected host count as that host not supporting pull.
	pullFailuresBeforeBackoff = 3
	// PullBatchMax caps the envelopes served by one pull.
	PullBatchMax = 100
)

// PullAck is the puller's account of one pulled envelope: the delivery Result
// it got from its own router.
type PullAck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// federationAccess is the part of federation messaging uses to re-apply this
// host's own access policy to a host it pulls from.
type federationAccess interface {
	AccessLevelFor(hostID string) federation.Level
}

type pullState struct {
	mu       sync.Mutex
	until    map[string]time.Time // host -> do not poll before
	failures map[string]int
	run      sync.Mutex // one poll pass at a time
	kick     chan struct{}
	reset    bool // the next pass ignores back-offs (a host reconnected)
	last     time.Time
}

func newPullState() *pullState {
	return &pullState{until: map[string]time.Time{}, failures: map[string]int{}, kick: make(chan struct{}, 1)}
}

// kickPull asks the pull loop to poll soon. resetBackoff also forgets every
// back-off: a reconnecting host may have been upgraded.
func (s *Service) kickPull(resetBackoff bool) {
	if resetBackoff {
		s.pull.mu.Lock()
		s.pull.reset = true
		s.pull.mu.Unlock()
	}
	select {
	case s.pull.kick <- struct{}{}:
	default:
	}
}

func (s *Service) pullLoop() {
	t := time.NewTicker(s.opts.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.pull.kick:
			s.pullOutstanding(s.ctx)
		case <-t.C:
			s.pull.mu.Lock()
			due := s.now().Sub(s.pull.last) >= s.opts.PullInterval
			s.pull.mu.Unlock()
			if due {
				s.pullOutstanding(s.ctx)
			}
		}
	}
}

// outstandingHosts lists the remote hosts this host awaits a response from: a
// host an open ask was sent to (its reply, decline or terminal notice), or a
// host a pending link request was sent to (its link_approved/link_denied).
func (s *Service) outstandingHosts() map[string]struct{} {
	hosts := map[string]struct{}{}
	self := s.selfID()
	reqs, err := s.st.OpenAgentMsgRequests()
	if err != nil {
		slog.Warn("agent pull: open requests lookup failed", "error", err)
	}
	for _, r := range reqs {
		if r.ToHost != "" && r.ToHost != self {
			hosts[r.ToHost] = struct{}{}
		}
	}
	links, err := s.st.AgentMsgLinkRequests("out")
	if err != nil {
		slog.Warn("agent pull: pending link requests lookup failed", "error", err)
	}
	for _, l := range links {
		if l.PeerHost != "" && l.PeerHost != self {
			hosts[l.PeerHost] = struct{}{}
		}
	}
	return hosts
}

// pullOutstanding polls every host with something outstanding, skipping hosts
// in back-off. A host with nothing outstanding is never polled.
func (s *Service) pullOutstanding(ctx context.Context) {
	if !s.pull.run.TryLock() {
		return
	}
	defer s.pull.run.Unlock()
	s.pull.mu.Lock()
	s.pull.last = s.now()
	if s.pull.reset {
		s.pull.reset = false
		s.pull.until = map[string]time.Time{}
		s.pull.failures = map[string]int{}
		slog.Debug("agent pull: back-offs cleared after a host reconnected")
	}
	s.pull.mu.Unlock()
	if s.opts.Federation == nil || s.Paused() {
		return
	}
	for hostID := range s.outstandingHosts() {
		if ctx.Err() != nil {
			return
		}
		s.pull.mu.Lock()
		until := s.pull.until[hostID]
		s.pull.mu.Unlock()
		if s.now().Before(until) {
			slog.Debug("agent pull: host in back-off", "host_id", hostID, "until", until.Format(time.RFC3339))
			continue
		}
		s.pollHost(ctx, hostID)
	}
}

func (s *Service) backOffPull(hostID, why string) {
	until := s.now().Add(pullBackoff)
	s.pull.mu.Lock()
	s.pull.until[hostID] = until
	s.pull.failures[hostID] = 0
	s.pull.mu.Unlock()
	slog.Debug("agent pull: backing off from host", "host_id", hostID, "reason", why, "until", until.Format(time.RFC3339))
}

// pollHost pulls the envelopes hostID holds for this host, delivers each
// through the normal router and acknowledges the outcomes.
func (s *Service) pollHost(ctx context.Context, hostID string) {
	log := slog.With("host_id", hostID)
	// requester is an Address (the browser protocol's existing field); only
	// its host is meaningful, and the executing host trusts the federation
	// origin over it.
	raw, denied, err := s.callFederation(ctx, hostID, map[string]any{"t": "agent_message_pull", "requester": Address{Host: s.selfID()}})
	if err != nil {
		s.pull.mu.Lock()
		s.pull.failures[hostID]++
		failures := s.pull.failures[hostID]
		s.pull.mu.Unlock()
		log.Debug("agent pull failed", "error", err, "consecutive_failures", failures)
		if failures >= pullFailuresBeforeBackoff {
			s.backOffPull(hostID, "repeated failures: "+err.Error())
		}
		return
	}
	s.pull.mu.Lock()
	delete(s.pull.failures, hostID)
	s.pull.mu.Unlock()
	if denied != nil {
		log.Info("agent pull refused by host access policy", "detail", denied.Message)
		s.backOffPull(hostID, "access denied")
		return
	}
	var resp struct {
		T         string     `json:"t"`
		Envelopes []Envelope `json:"envelopes"`
		Error     string     `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.T != "agent_message_pull_result" {
		// An older daemon answers an unknown command with an error ack.
		log.Debug("agent pull not supported by host", "response_type", resp.T, "error", resp.Error)
		s.backOffPull(hostID, "not supported")
		return
	}
	if len(resp.Envelopes) == 0 {
		return
	}
	if len(resp.Envelopes) > PullBatchMax {
		resp.Envelopes = resp.Envelopes[:PullBatchMax]
	}
	ids := make([]string, 0, len(resp.Envelopes))
	acks := make([]PullAck, 0, len(resp.Envelopes))
	for _, env := range resp.Envelopes {
		if ctx.Err() != nil {
			return
		}
		res := s.deliverPulled(ctx, hostID, env)
		switch res.Error {
		case ErrMessagingPaused, ErrHostUnreachable:
			// Transient on this side: leave it with the responder to pull again.
			log.Info("agent pulled message not acknowledged; will pull again", "envelope_id", env.ID, "kind", env.Kind, "code", res.Error)
			continue
		}
		ids = append(ids, env.ID)
		acks = append(acks, PullAck{ID: env.ID, Status: res.Status, Error: res.Error, Message: res.Message})
	}
	log.Info("agent messages pulled", "received", len(resp.Envelopes), "acknowledging", len(acks))
	if len(acks) == 0 {
		return
	}
	raw, denied, err = s.callFederation(ctx, hostID, map[string]any{"t": "agent_message_pull_ack", "ids": ids, "results": acks})
	if err != nil || denied != nil {
		// The envelopes stay pending there and are served again; the router
		// deduplicates them on envelope id.
		log.Warn("agent pull acknowledgement failed; envelopes will be served again", "error", err, "denied", denied != nil, "count", len(acks))
		return
	}
	log.Debug("agent pull acknowledged", "count", len(acks), "response_bytes", len(raw))
}

// deliverPulled routes one pulled envelope exactly as a push from hostID
// would be: origin is hostID's stable node ID, so the sender, reply, link and
// kill-switch checks all apply. Federation's own access check is bypassed on
// a pull, so a send or ask additionally needs this host's policy to grant
// hostID message access.
func (s *Service) deliverPulled(ctx context.Context, hostID string, env Envelope) Result {
	if env.Kind == KindSend || env.Kind == KindAsk {
		if !s.mayReceiveFrom(hostID) {
			slog.Warn("agent pulled message rejected: host lacks message access here", "host_id", hostID, "envelope_id", env.ID, "kind", env.Kind)
			return rejected(env.ID, ErrAccessDenied, "this host does not grant "+hostID+" message access")
		}
	}
	return s.Deliver(ctx, hostID, env)
}

func (s *Service) mayReceiveFrom(hostID string) bool {
	access, ok := s.opts.Federation.(federationAccess)
	if !ok || hostID == "" {
		return false
	}
	return access.AccessLevelFor(hostID) >= federation.LevelMessage
}

// HandlePull serves agent_message_pull: the undelivered envelopes in this
// host's outbox addressed to origin, the federation origin of the command
// (the caller's identity; there is no other). At most PullBatchMax are
// returned, oldest first. The kill switch holds the outbox, so nothing is
// served while paused.
func (s *Service) HandlePull(origin string) ([]Envelope, error) {
	if origin == "" {
		return nil, errors.New("agent_message_pull must come from another host")
	}
	log := slog.With("host_id", origin)
	if s.Paused() {
		log.Info("agent pull served nothing: messaging paused")
		return []Envelope{}, nil
	}
	rows, err := s.st.PendingAgentMsgOutboxForHost(origin, PullBatchMax)
	if err != nil {
		log.Error("agent pull outbox lookup failed", "error", err)
		return nil, errors.New("outbox lookup failed")
	}
	now := s.now()
	out := make([]Envelope, 0, len(rows))
	for _, row := range rows {
		if now.Sub(time.UnixMilli(row.CreatedAt)) >= outboxMaxAge {
			continue // the retry loop is about to give up on it
		}
		var env Envelope
		if err := json.Unmarshal([]byte(row.Envelope), &env); err != nil {
			log.Error("agent pull skipped an unreadable outbox row", "envelope_id", row.ID, "error", err)
			continue
		}
		out = append(out, env)
	}
	if len(out) > 0 {
		ids := make([]string, len(out))
		for i, e := range out {
			ids[i] = e.ID
		}
		log.Info("agent outbox served for pull", "count", len(out), "envelope_ids", ids)
	} else {
		log.Debug("agent pull found nothing pending")
	}
	return out, nil
}

// HandlePullAck serves agent_message_pull_ack. results holds the puller's
// delivery outcome per envelope; an id without a result counts as delivered.
// Only pending outbox rows addressed to origin are touched, so repeating an
// acknowledgement is harmless. A delivered row stops being served; a rejected
// one fails terminally with the puller's error, exactly as a push rejection
// would. It returns how many rows changed.
func (s *Service) HandlePullAck(origin string, ids []string, results []PullAck) (int, error) {
	if origin == "" {
		return 0, errors.New("agent_message_pull_ack must come from another host")
	}
	log := slog.With("host_id", origin)
	byID := make(map[string]PullAck, len(results))
	for _, r := range results {
		byID[r.ID] = r
	}
	var all []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			all = append(all, id)
		}
	}
	for _, id := range ids {
		add(id)
	}
	for _, r := range results {
		add(r.ID)
	}
	// Serialise with the retry loop so it never acts on a row already settled here.
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	changed := 0
	for _, id := range all {
		row, err := s.st.AgentMsgOutbox(id)
		if err != nil {
			log.Error("agent pull ack outbox lookup failed", "envelope_id", id, "error", err)
			continue
		}
		if row == nil || row.ToHost != origin || row.Status != "pending" {
			log.Debug("agent pull ack ignored", "envelope_id", id, "known", row != nil)
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(row.Envelope), &env); err != nil {
			log.Error("agent pull ack: outbox row is unreadable; failing it", "envelope_id", id, "error", err)
			_ = s.st.UpdateAgentMsgOutbox(id, "failed", "", "unreadable envelope", row.Attempts, ms(s.now()))
			continue
		}
		res := byID[id]
		changed++
		if res.Error == "" {
			status := firstNonEmpty(res.Status, "delivered")
			if err := s.st.UpdateAgentMsgOutbox(id, "delivered", status, "", row.Attempts+1, ms(s.now())); err != nil {
				log.Error("agent outbox update failed", "envelope_id", id, "error", err)
			}
			log.Info("agent message delivered by pull", "envelope_id", id, "kind", row.Kind, "status", status)
			s.noteOutboxResolved(*row, status, "")
			continue
		}
		log.Warn("agent message rejected by puller", "envelope_id", id, "kind", row.Kind, "code", res.Error, "detail", res.Message)
		s.failOutbox(*row, env, firstNonEmpty(res.Message, res.Error), res.Error)
	}
	return changed, nil
}
