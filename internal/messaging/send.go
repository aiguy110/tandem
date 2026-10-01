package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// SendResult is what the agent-facing send/ask/reply/decline tools return.
type SendResult struct {
	// Ref is the recipient: for send/ask the agent addressed, for reply/decline
	// the agent that asked.
	Ref       string `json:"ref,omitempty"`
	ID        string `json:"id"`
	ThreadID  string `json:"threadId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Status    string `json:"status"`
}

func (s *Service) liveSession(sessionID string) (*session.Session, error) {
	sess := s.opts.Sessions.Get(sessionID)
	if sess == nil {
		return nil, &Rejection{Code: ErrInvalid, Message: "unknown calling agent " + sessionID}
	}
	return sess, nil
}

// Send sends a one-way message from a local agent.
func (s *Service) Send(ctx context.Context, sessionID, to, body, threadID string) (SendResult, error) {
	sess, err := s.liveSession(sessionID)
	if err != nil {
		return SendResult{}, err
	}
	dest, err := s.resolve(ctx, sess, to)
	if err != nil {
		return SendResult{}, err
	}
	hop := 0
	if threadID == "" {
		threadID = newID("thr_")
	} else if known, ok, terr := s.st.AgentMsgThreadHop(sess.ID, threadID); terr != nil {
		slog.Warn("agent thread hop lookup failed", "session_id", sess.ID, "thread_id", threadID, "error", terr)
	} else if ok {
		hop = known + 1
	}
	env := Envelope{ThreadID: threadID, Kind: KindSend, To: dest, Body: body, Hop: hop}
	res, err := s.sendFrom(ctx, sess, env)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{Ref: s.Ref(dest), ID: res.ID, ThreadID: threadID, Status: res.Status}, nil
}

// Ask sends a question and returns immediately; the answer arrives later as
// an ordinary inbound message. timeoutMinutes <= 0 means the default.
func (s *Service) Ask(ctx context.Context, sessionID, to, body string, timeoutMinutes int) (SendResult, error) {
	sess, err := s.liveSession(sessionID)
	if err != nil {
		return SendResult{}, err
	}
	dest, err := s.resolve(ctx, sess, to)
	if err != nil {
		return SendResult{}, err
	}
	timeoutSec := DefaultAskTimeoutSec
	if timeoutMinutes > 0 {
		timeoutSec = timeoutMinutes * 60
	}
	if timeoutSec > MaxAskTimeoutSec {
		timeoutSec = MaxAskTimeoutSec
	}
	env := Envelope{ThreadID: newID("thr_"), Kind: KindAsk, RequestID: newID("req_"), To: dest, Body: body, TimeoutSec: timeoutSec}
	res, err := s.sendFrom(ctx, sess, env)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{Ref: s.Ref(dest), ID: res.ID, ThreadID: env.ThreadID, RequestID: env.RequestID, Status: res.Status}, nil
}

// Reply answers an inbound ask.
func (s *Service) Reply(ctx context.Context, sessionID, requestID, body string) (SendResult, error) {
	return s.answer(ctx, sessionID, requestID, KindReply, body)
}

// Decline refuses an inbound ask, with a reason.
func (s *Service) Decline(ctx context.Context, sessionID, requestID, reason string) (SendResult, error) {
	return s.answer(ctx, sessionID, requestID, KindDecline, reason)
}

func (s *Service) answer(ctx context.Context, sessionID, requestID, kind, body string) (SendResult, error) {
	sess, err := s.liveSession(sessionID)
	if err != nil {
		return SendResult{}, err
	}
	ob, err := s.st.AgentMsgObligation(requestID)
	if err != nil {
		return SendResult{}, &Rejection{Code: ErrHostUnreachable, Message: "request lookup failed"}
	}
	if ob == nil || ob.Session != sess.ID || ob.Status != "open" {
		return SendResult{}, &Rejection{Code: ErrUnknownRequest, Message: "you have no open request " + requestID + " to answer"}
	}
	env := Envelope{
		ThreadID: ob.ThreadID, Kind: kind, RequestID: requestID, Hop: ob.Hop + 1, Body: body,
		To: Address{Host: ob.AskerHost, Agent: ob.AskerAgent, Name: ob.AskerName},
	}
	res, err := s.sendFrom(ctx, sess, env)
	if err != nil {
		var rej *Rejection
		if errors.As(err, &rej) && answerRejectionFinal(rej.Code) {
			s.closeObligation(requestID)
		} else {
			slog.Info("agent answer rejected; request stays open for a retry", "session_id", sess.ID, "request_id", requestID, "error", err)
		}
		return SendResult{}, err
	}
	s.closeObligation(requestID)
	return SendResult{Ref: s.Ref(env.To), ID: res.ID, ThreadID: ob.ThreadID, RequestID: requestID, Status: res.Status}, nil
}

// answerRejectionFinal reports whether a rejected reply or decline means the
// asker can never be answered (it timed out, is gone, or refuses), so the
// obligation should close. A mistake in the answer itself (bad arguments), the
// kill switch, or a local or transient failure leaves it open: the agent can
// correct and retry, and the reminder and no_reply decline still apply.
func answerRejectionFinal(code string) bool {
	switch code {
	case ErrInvalid, ErrMessagingPaused, ErrHostUnreachable, ErrRecipientUnavailable:
		return false
	}
	return true
}

// closeObligation closes an inbound ask once it has been answered (or can no
// longer be).
func (s *Service) closeObligation(requestID string) {
	closed, err := s.st.CloseAgentMsgObligation(requestID, ms(s.now()))
	if err != nil {
		slog.Error("agent obligation close failed", "request_id", requestID, "error", err)
		return
	}
	if closed {
		slog.Info("agent obligation closed", "request_id", requestID)
		s.summaryChanged()
	}
}

// sendFrom completes env (id, sender, timestamp), sends it, records the
// sender's transcript event, and registers the open request for an ask. A
// rejection is returned as a *Rejection error.
func (s *Service) sendFrom(ctx context.Context, sess *session.Session, env Envelope) (Result, error) {
	env.ID = newID("msg_")
	env.From = s.addressOf(sess)
	env.SentAt = s.now().UTC().Format(time.RFC3339)
	log := slog.With("envelope_id", env.ID, "kind", env.Kind, "from", env.From.String(), "to", env.To.String(), "body_bytes", len(env.Body), "request_id", env.RequestID)
	reject := func(code, msg string) (Result, error) {
		log.Warn("agent message not sent", "code", code, "detail", msg)
		sess.PushEvent(agentMessageEvent("out", env, StatusRejected, code))
		return Result{}, &Rejection{Code: code, Message: msg}
	}
	if s.selfID() == "" {
		return reject(ErrHostUnreachable, "this host has no federation identity yet")
	}
	if s.Paused() {
		return reject(ErrMessagingPaused, "agent messaging is paused on this host")
	}
	if strings.TrimSpace(env.Body) == "" {
		return reject(ErrInvalid, "body is required: put the message text in the body argument")
	}
	if len(env.Body) > MaxBodyBytes {
		return reject(ErrInvalid, fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes))
	}
	if env.To.Same(env.From) {
		return reject(ErrInvalid, "an agent cannot message itself")
	}
	if (env.Kind == KindSend || env.Kind == KindAsk) && env.To.Host == "" {
		return reject(ErrInvalid, "recipient host is required")
	}
	if env.Kind == KindAsk {
		now := s.now()
		req := store.AgentMsgRequest{
			RequestID: env.RequestID, Session: sess.ID, ToHost: env.To.Host, ToAgent: env.To.Agent, ToName: env.To.Name,
			ThreadID: env.ThreadID, MessageID: env.ID, Status: "open", TimeoutSec: env.TimeoutSec,
			CreatedAt: ms(now), Deadline: ms(now.Add(time.Duration(env.TimeoutSec) * time.Second)),
		}
		if err := s.st.AddAgentMsgRequest(req); err != nil {
			log.Error("agent request record failed", "error", err)
			return reject(ErrHostUnreachable, "could not record the request")
		}
		s.summaryChanged()
		// An answer may only ever come by pull; start looking for it.
		s.kickPull(false)
	}
	if err := s.st.NoteAgentMsgThreadHop(sess.ID, env.ThreadID, env.Hop); err != nil {
		log.Warn("agent thread hop record failed", "error", err)
	}
	res := s.dispatch(ctx, env, sess.ID)
	if !res.OK() {
		if env.Kind == KindAsk {
			s.closeRequest(env.RequestID, "closed")
		}
		return reject(res.Error, firstNonEmpty(res.Message, res.Error))
	}
	log.Info("agent message sent", "status", res.Status)
	sess.PushEvent(agentMessageEvent("out", env, res.Status, ""))
	return res, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// dispatch sends env to its recipient: straight to the local router when the
// recipient is on this host (federation is never involved), through the
// outbox otherwise. fromSession is the sending session ("" for daemon
// notices), used to report later status changes.
func (s *Service) dispatch(ctx context.Context, env Envelope, fromSession string) Result {
	if env.To.Host == s.selfID() {
		return s.Deliver(ctx, "", env)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return rejected(env.ID, ErrInvalid, err.Error())
	}
	row := store.AgentMsgOutbox{
		ID: env.ID, FromSession: fromSession, ToHost: env.To.Host, ToAgent: env.To.Agent, Kind: env.Kind,
		RequestID: env.RequestID, Envelope: string(raw), Status: "pending", CreatedAt: ms(s.now()), NextAttemptAt: ms(s.now()),
	}
	if err := s.st.AddAgentMsgOutbox(row); err != nil {
		slog.Error("agent outbox persist failed", "envelope_id", env.ID, "error", err)
		return rejected(env.ID, ErrHostUnreachable, "could not persist the message")
	}
	return s.attempt(ctx, row, env)
}

// dispatchAsync is dispatch off the caller's goroutine, for daemon-generated
// envelopes sent from callbacks that must not block.
func (s *Service) dispatchAsync(env Envelope, fromSession string) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		res := s.dispatch(s.ctx, env, fromSession)
		if !res.OK() {
			slog.Warn("agent daemon-generated message rejected", "envelope_id", env.ID, "kind", env.Kind, "to", env.To.String(), "code", res.Error, "detail", res.Message)
		}
	}()
}

// holdForPull reports whether a federation access denial of an envelope of
// this kind is non-terminal. A reply, decline or system notice answers a
// request the other host itself made, so the other host holds rights on this
// one even when this one holds none on it: the envelope waits in the outbox to
// be pulled (agent_message_pull) instead of failing.
func holdForPull(kind string) bool {
	return kind == KindReply || kind == KindDecline || kind == KindSystem
}

// attempt tries to deliver one outbox row to its remote host. A transport
// failure leaves it pending (to be retried or pulled); a policy rejection is
// terminal, except that a federation access denial of a reply, decline or
// system notice is held for the other host to pull.
func (s *Service) attempt(ctx context.Context, row store.AgentMsgOutbox, env Envelope) Result {
	log := slog.With("envelope_id", env.ID, "kind", env.Kind, "to", env.To.String(), "attempt", row.Attempts+1)
	if s.Paused() {
		// Held, not failed: the kill switch must not consume retries.
		log.Info("agent outbox attempt skipped: messaging paused")
		return Result{ID: env.ID, Status: StatusPending}
	}
	res, fedDenied, err := s.callRemote(ctx, env)
	now := s.now()
	hold := func(reason string) Result {
		attempts := row.Attempts + 1
		next := now.Add(outboxRetryInterval)
		if uerr := s.st.UpdateAgentMsgOutbox(env.ID, "pending", "", reason, attempts, ms(next)); uerr != nil {
			log.Error("agent outbox update failed", "error", uerr)
		}
		// The first failure is the news; later retries of the same hold are
		// routine.
		level := slog.LevelInfo
		if row.Attempts == 0 {
			level = slog.LevelWarn
		}
		log.Log(context.Background(), level, "agent message not pushed; held for pull and retry", "host_id", env.To.Host, "envelope_id", env.ID, "kind", env.Kind, "reason", reason, "next_attempt", next.Format(time.RFC3339))
		return Result{ID: env.ID, Status: StatusPending}
	}
	if err != nil {
		return hold(err.Error())
	}
	if !res.OK() {
		if fedDenied && holdForPull(env.Kind) {
			return hold(firstNonEmpty(res.Message, res.Error))
		}
		if uerr := s.st.UpdateAgentMsgOutbox(env.ID, "failed", "", res.Error, row.Attempts+1, ms(now)); uerr != nil {
			log.Error("agent outbox update failed", "error", uerr)
		}
		log.Warn("agent message rejected by recipient host", "code", res.Error, "detail", res.Message)
		return res
	}
	if uerr := s.st.UpdateAgentMsgOutbox(env.ID, "delivered", res.Status, "", row.Attempts+1, ms(now)); uerr != nil {
		log.Error("agent outbox update failed", "error", uerr)
	}
	log.Info("agent message delivered to remote host", "status", res.Status)
	return res
}

// remoteHost finds the federation host whose stable node ID is hostID.
func (s *Service) remoteHost(hostID string) (federation.Host, bool) {
	if s.opts.Federation == nil {
		return federation.Host{}, false
	}
	var found federation.Host
	ok := false
	for _, h := range s.opts.Federation.Hosts() {
		if h.Local || h.NodeID != hostID {
			continue
		}
		if !ok || (h.Status == "connected" && found.Status != "connected") {
			found, ok = h, true
		}
	}
	return found, ok
}

// callFederation sends one browser-protocol command to the host with node ID
// hostID. A returned error is a transport failure (retry later); an
// access-policy denial is reported as a rejected Result via the second value.
func (s *Service) callFederation(ctx context.Context, hostID string, payload map[string]any) (json.RawMessage, *Result, error) {
	h, ok := s.remoteHost(hostID)
	if !ok {
		return nil, nil, fmt.Errorf("no route to host %s", hostID)
	}
	if h.Status != "connected" {
		return nil, nil, fmt.Errorf("host %s is %s", hostID, h.Status)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.CallTimeout)
	defer cancel()
	raw, err := s.opts.Federation.Call(callCtx, h.ID, data)
	if err != nil {
		if federation.IsAccessDenied(err) {
			r := rejected("", ErrAccessDenied, err.Error())
			return nil, &r, nil
		}
		return nil, nil, err
	}
	return raw, nil, nil
}

// callRemote delivers env to its (remote) recipient host. fedDenied reports
// that the rejection came from federation's access policy on the way, rather
// than from the recipient's messaging rules.
func (s *Service) callRemote(ctx context.Context, env Envelope) (res Result, fedDenied bool, err error) {
	raw, denied, err := s.callFederation(ctx, env.To.Host, map[string]any{"t": "agent_message_deliver", "envelope": env})
	if err != nil {
		return Result{}, false, err
	}
	if denied != nil {
		denied.ID = env.ID
		return *denied, true, nil
	}
	var resp struct {
		T       string `json:"t"`
		ID      string `json:"id"`
		Status  string `json:"status"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Result{}, false, fmt.Errorf("invalid response from %s: %w", env.To.Host, err)
	}
	if resp.T != "agent_message_result" {
		// An older daemon answers an unknown command with an error ack.
		return rejected(env.ID, ErrHostUnreachable, "host "+env.To.Host+" does not support agent messaging: "+resp.Error), false, nil
	}
	if resp.Error != "" {
		return Result{ID: env.ID, Status: StatusRejected, Error: resp.Error, Message: resp.Message}, false, nil
	}
	return Result{ID: env.ID, Status: resp.Status}, false, nil
}

// flushOutbox retries every pending outbox row. Rows older than 24 hours are
// failed and their sender told. Nothing is attempted while paused.
func (s *Service) flushOutbox(ctx context.Context, forced bool) {
	if s.Paused() {
		return
	}
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	rows, err := s.st.PendingAgentMsgOutbox()
	if err != nil {
		slog.Error("agent outbox scan failed", "error", err)
		return
	}
	now := s.now()
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal([]byte(row.Envelope), &env); err != nil {
			slog.Error("agent outbox row is unreadable; failing it", "envelope_id", row.ID, "error", err)
			_ = s.st.UpdateAgentMsgOutbox(row.ID, "failed", "", "unreadable envelope", row.Attempts, ms(now))
			continue
		}
		if now.Sub(time.UnixMilli(row.CreatedAt)) >= outboxMaxAge {
			s.failOutbox(row, env, "undeliverable for 24 hours", "undeliverable")
			continue
		}
		if !forced && ms(now) < row.NextAttemptAt {
			continue
		}
		res := s.attempt(ctx, row, env)
		switch {
		case res.Status == StatusPending:
		case res.OK():
			s.noteOutboxResolved(row, res.Status, "")
		default:
			s.failOutbox(row, env, firstNonEmpty(res.Message, res.Error), res.Error)
		}
	}
}

// noteOutboxResolved tells the sending session an envelope it had seen as
// pending was finally delivered.
func (s *Service) noteOutboxResolved(row store.AgentMsgOutbox, status, errCode string) {
	if row.FromSession == "" {
		return
	}
	if sess := s.opts.Sessions.Get(row.FromSession); sess != nil {
		sess.PushEvent(statusEvent(row.ID, status, errCode))
	}
}

// failOutbox gives up on an envelope: it is marked failed, a pending ask is
// closed, the sender's transcript is updated and the sender is told with a
// system/undeliverable notice.
func (s *Service) failOutbox(row store.AgentMsgOutbox, env Envelope, reason, code string) {
	slog.Warn("agent outbox giving up on envelope", "envelope_id", row.ID, "kind", row.Kind, "to_host", row.ToHost, "reason", reason, "code", code)
	if err := s.st.UpdateAgentMsgOutbox(row.ID, "failed", "", code, row.Attempts, ms(s.now())); err != nil {
		slog.Error("agent outbox update failed", "envelope_id", row.ID, "error", err)
	}
	if env.Kind == KindAsk {
		s.closeRequest(env.RequestID, "closed")
	}
	if row.FromSession == "" {
		return
	}
	status := "undeliverable"
	if code != "" && code != "undeliverable" {
		status = StatusRejected
	}
	s.noteOutboxResolved(row, status, code)
	if env.Kind == KindSystem {
		return
	}
	body := fmt.Sprintf("Your %s message %s to %s could not be delivered: %s.", env.Kind, env.ID, s.Ref(env.To), reason)
	s.deliverSystem(row.FromSession, EventUndeliverable, body, env.RequestID, env.ThreadID, s.systemAddress())
}
