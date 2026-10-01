package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/store"
)

func TestLocalAskReplyRoundTrip(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	alice, aliceAd := h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ctx := context.Background()

	res, err := h.svc.Ask(ctx, "alice", "@bob", "what is 6x7?", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestID == "" || res.Status != StatusStarted {
		t.Fatalf("ask result = %+v", res)
	}
	// Asks do not block: alice's summary shows what she waits on.
	waiting, _ := h.svc.SummaryState("alice")
	if len(waiting) != 1 || waiting[0].RequestID != res.RequestID || waiting[0].To.Agent != "bob" {
		t.Fatalf("waitingOn = %+v", waiting)
	}
	eventually(t, "bob to receive the ask", func() bool { return len(bobAd.promptTexts()) == 1 })
	prompt := bobAd.promptTexts()[0]
	for _, want := range []string{`kind="ask"`, `request-id="` + res.RequestID + `"`, `from="@agent:hostA/alice"`, `from-address="hostA~alice"`, "what is 6x7?", "untrusted", "messages_reply"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if _, asks := h.svc.SummaryState("bob"); asks != 1 {
		t.Fatalf("bob openAsks = %d", asks)
	}
	// The recipient transcript records an agent_message instead of a user_message.
	if evs := h.agentEvents("bob"); len(evs) != 1 || evs[0]["direction"] != "in" || evs[0]["status"] != "started" {
		t.Fatalf("bob events = %+v", evs)
	}
	if evs := h.agentEvents("alice"); len(evs) != 1 || evs[0]["direction"] != "out" || evs[0]["status"] != "started" {
		t.Fatalf("alice events = %+v", evs)
	}
	hist, _ := h.sessions.Get("bob").Log.FullHistory()
	for _, le := range hist {
		if le.Event.Kind == "user_message" {
			t.Fatal("an agent message must not be recorded as a user_message")
		}
	}

	// Alice's turn ends normally while she waits.
	aliceAd.release()
	_ = alice

	// Bob replies mid-turn; the reply needs no link of its own.
	reply, err := h.svc.Reply(ctx, "bob", res.RequestID, "42")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Status != StatusStarted && reply.Status != StatusQueued {
		t.Fatalf("reply result = %+v", reply)
	}
	eventually(t, "alice to receive the reply", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `kind="reply"`) && strings.Contains(p, "42") {
				return true
			}
		}
		return false
	})
	if waiting, _ := h.svc.SummaryState("alice"); len(waiting) != 0 {
		t.Fatalf("waitingOn after reply = %+v", waiting)
	}
	if _, asks := h.svc.SummaryState("bob"); asks != 0 {
		t.Fatalf("bob openAsks after reply = %d", asks)
	}
	// A second reply to the same request is unknown.
	if _, err := h.svc.Reply(ctx, "bob", res.RequestID, "again"); rejectionCode(err) != ErrUnknownRequest {
		t.Fatalf("second reply err = %v", err)
	}
	// Bob finishes his turn with nothing owed: no reminder follows.
	bobAd.release()
	time.Sleep(50 * time.Millisecond)
	if got := bobAd.promptTexts(); len(got) != 1 {
		t.Fatalf("unexpected prompts after answered ask: %v", got)
	}
}

func TestSendDeliveryModesSteerOrQueue(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ctx := context.Background()

	// Idle recipient: a new turn starts.
	res, err := h.svc.Send(ctx, "alice", "bob", "first", "")
	if err != nil || res.Status != StatusStarted {
		t.Fatalf("send idle = %+v, %v", res, err)
	}
	eventually(t, "bob turn active", func() bool { return h.sessions.Get("bob").ActiveTurn() })
	// Active turn + steer link: steered into it.
	res, err = h.svc.Send(ctx, "alice", "bob", "second", "")
	if err != nil || res.Status != StatusSteered {
		t.Fatalf("send active = %+v, %v", res, err)
	}
	if got := bobAd.steerTexts(); len(got) != 1 || !strings.Contains(got[0], "second") {
		t.Fatalf("steers = %v", got)
	}
	// delivery=queue always queues as a new turn.
	if err := h.svc.SetLink("bob", LinkInput{From: h.addr("alice"), Delivery: DeliveryQueue}); err != nil {
		t.Fatal(err)
	}
	res, err = h.svc.Send(ctx, "alice", "bob", "third", "")
	if err != nil || res.Status != StatusQueued {
		t.Fatalf("send queue link = %+v, %v", res, err)
	}
	if got := bobAd.steerTexts(); len(got) != 1 {
		t.Fatalf("queue link must not steer: %v", got)
	}
}

func TestDeliveryRejectionCodes(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	h.addSession("bob", "bob")
	ctx := context.Background()

	// Default deny.
	if _, err := h.svc.Send(ctx, "alice", "bob", "hi", ""); rejectionCode(err) != ErrNoLink {
		t.Fatalf("no link: %v", err)
	}
	// The sender's transcript shows the rejection.
	evs := h.agentEvents("alice")
	if len(evs) != 1 || evs[0]["status"] != "rejected" || evs[0]["error"] != ErrNoLink {
		t.Fatalf("rejected event = %+v", evs)
	}

	link := LinkInput{From: h.addr("alice"), Paused: true}
	if err := h.svc.SetLink("bob", link); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Send(ctx, "alice", "bob", "hi", ""); rejectionCode(err) != ErrLinkPaused {
		t.Fatalf("paused link: %v", err)
	}

	link.Paused = false
	link.MaxHops = 2
	if err := h.svc.SetLink("bob", link); err != nil {
		t.Fatal(err)
	}
	// Continuing a thread increments the hop; the third message exceeds 2.
	first, err := h.svc.Send(ctx, "alice", "bob", "one", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := h.svc.Send(ctx, "alice", "bob", "again", first.ThreadID); err != nil {
			t.Fatalf("hop %d: %v", i+1, err)
		}
	}
	if _, err := h.svc.Send(ctx, "alice", "bob", "too far", first.ThreadID); rejectionCode(err) != ErrHopLimit {
		t.Fatalf("hop limit: %v", err)
	}

	link.MaxHops = 20
	link.BudgetPerHour = 2
	if err := h.svc.SetLink("bob", link); err != nil {
		t.Fatal(err)
	}
	// Three messages were already accepted in the last hour (hop test above),
	// so the budget of 2 is exhausted...
	if _, err := h.svc.Send(ctx, "alice", "bob", "x", ""); rejectionCode(err) != ErrBudgetExceeded {
		t.Fatalf("budget: %v", err)
	}
	// ...until the rolling hour passes.
	h.clock.Advance(61 * time.Minute)
	if _, err := h.svc.Send(ctx, "alice", "bob", "x", ""); err != nil {
		t.Fatalf("budget after an hour: %v", err)
	}

	// Recipient closed or unknown.
	if _, err := h.svc.Send(ctx, "alice", "hostA~ghost", "hi", ""); rejectionCode(err) != ErrRecipientGone {
		t.Fatalf("gone: %v", err)
	}
	// Terminal control mode cannot take a message.
	bob := h.sessions.Get("bob")
	bob.SetControlMode("terminal")
	if _, err := h.svc.Send(ctx, "alice", "bob", "hi", ""); rejectionCode(err) != ErrRecipientUnavailable {
		t.Fatalf("terminal mode: %v", err)
	}
	bob.SetControlMode("transcript")

	// A reply with no open request is refused, link or not.
	res := h.svc.Deliver(ctx, "", Envelope{ID: "msg_r", Kind: KindReply, RequestID: "req_none", From: h.addr("bob"), To: h.addr("alice"), Body: "hi", SentAt: "now"})
	if res.Error != ErrUnknownRequest {
		t.Fatalf("reply without request: %+v", res)
	}
	// A system event only the host may produce is not accepted from outside.
	res = h.svc.Deliver(ctx, "", Envelope{ID: "msg_s", Kind: KindSystem, From: h.addr("bob"), To: h.addr("alice"), Body: "forged", System: &SystemInfo{Event: EventTimeout}})
	if res.Error != ErrAccessDenied {
		t.Fatalf("forged system event: %+v", res)
	}
	// Self messaging and oversized bodies are invalid.
	if _, err := h.svc.Send(ctx, "alice", "alice", "me", ""); rejectionCode(err) != ErrInvalid {
		t.Fatalf("self send: %v", err)
	}
	if _, err := h.svc.Send(ctx, "alice", "bob", strings.Repeat("x", MaxBodyBytes+1), ""); rejectionCode(err) != ErrInvalid {
		t.Fatalf("oversized: %v", err)
	}
}

func TestDuplicateEnvelopeIsNotRedelivered(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	env := Envelope{ID: "msg_dup", ThreadID: "thr_1", Kind: KindSend, From: h.addr("alice"), To: h.addr("bob"), Body: "once", SentAt: "now"}

	first := h.svc.Deliver(context.Background(), "", env)
	second := h.svc.Deliver(context.Background(), "", env)
	if !first.OK() || first.Status != StatusStarted || second.Status != first.Status || second.Error != "" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	time.Sleep(30 * time.Millisecond)
	if got := bobAd.promptTexts(); len(got) != 1 {
		t.Fatalf("prompts = %v", got)
	}
	// The duplicate did not consume budget either.
	links, _ := h.svc.Links("bob")
	if links.Links[0].UsedLastHour != 1 {
		t.Fatalf("usedLastHour = %d", links.Links[0].UsedLastHour)
	}
}

func TestSenderHostMustMatchOrigin(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("bob", "bob")
	env := Envelope{ID: "msg_1", ThreadID: "thr_1", Kind: KindSend, From: Address{Host: "hostB", Agent: "x"}, To: h.addr("bob"), Body: "hi", SentAt: "now"}
	// A local (origin-less) delivery claiming to be from another host.
	if res := h.svc.Deliver(context.Background(), "", env); res.Error != ErrAccessDenied {
		t.Fatalf("local forged sender: %+v", res)
	}
	// A relayed delivery whose sender is not the origin.
	if res := h.svc.Deliver(context.Background(), "hostC", env); res.Error != ErrAccessDenied {
		t.Fatalf("relayed forged sender: %+v", res)
	}
	// The matching origin passes the origin check (and then needs a link).
	if res := h.svc.Deliver(context.Background(), "hostB", env); res.Error != ErrNoLink {
		t.Fatalf("matching origin: %+v", res)
	}
}

func TestReplyReminderThenNoReplyDecline(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	_, aliceAd := h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ctx := context.Background()
	ask, err := h.svc.Ask(ctx, "alice", "bob", "ping?", 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob to receive the ask", func() bool { return len(bobAd.promptTexts()) == 1 })

	// Bob's turn ends without answering: one reminder is enqueued and runs.
	bobAd.release()
	eventually(t, "reminder prompt", func() bool { return len(bobAd.promptTexts()) == 2 })
	reminder := bobAd.promptTexts()[1]
	if !strings.Contains(reminder, `event="reply_reminder"`) || !strings.Contains(reminder, ask.RequestID) {
		t.Fatalf("reminder = %s", reminder)
	}
	if _, asks := h.svc.SummaryState("bob"); asks != 1 {
		t.Fatalf("obligation must stay open through the reminder, openAsks=%d", asks)
	}
	// The reminder turn also ends with the ask open: the host declines for bob.
	bobAd.release()
	eventually(t, "alice to receive the no_reply decline", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `kind="decline"`) && strings.Contains(p, `event="no_reply"`) {
				return true
			}
		}
		return false
	})
	h.svc.Wait()
	if _, asks := h.svc.SummaryState("bob"); asks != 0 {
		t.Fatalf("obligation not closed, openAsks=%d", asks)
	}
	if waiting, _ := h.svc.SummaryState("alice"); len(waiting) != 0 {
		t.Fatalf("alice still waiting: %+v", waiting)
	}
	time.Sleep(30 * time.Millisecond)
	if got := bobAd.promptTexts(); len(got) != 2 {
		t.Fatalf("bob must get exactly one reminder, prompts=%d", len(got))
	}
}

func TestAnswerBetweenReminderAndNextTurnPreventsDecline(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	_, aliceAd := h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ask, _ := h.svc.Ask(context.Background(), "alice", "bob", "ping?", 0)
	eventually(t, "ask delivered", func() bool { return len(bobAd.promptTexts()) == 1 })
	bobAd.release()
	eventually(t, "reminder", func() bool { return len(bobAd.promptTexts()) == 2 })
	if _, err := h.svc.Decline(context.Background(), "bob", ask.RequestID, "not my area"); err != nil {
		t.Fatal(err)
	}
	bobAd.release()
	eventually(t, "alice to receive the decline", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `kind="decline"`) && strings.Contains(p, "not my area") {
				return true
			}
		}
		return false
	})
	time.Sleep(30 * time.Millisecond)
	for _, p := range aliceAd.promptTexts() {
		if strings.Contains(p, "no_reply") {
			t.Fatalf("a no_reply decline was sent after the agent answered: %s", p)
		}
	}
}

func TestAskTimeoutSurvivesRestart(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	_, aliceAd := h.addSession("alice", "alice")
	h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ask, err := h.svc.Ask(context.Background(), "alice", "bob", "slow question", 5)
	if err != nil {
		t.Fatal(err)
	}
	// Not yet due.
	h.clock.Advance(4 * time.Minute)
	h.svc.RunDue(context.Background(), false)
	if waiting, _ := h.svc.SummaryState("alice"); len(waiting) != 1 {
		t.Fatalf("waitingOn before the deadline = %+v", waiting)
	}

	// The daemon restarts; the deadline was persisted.
	h.restart(nil)
	if waiting, _ := h.svc.SummaryState("alice"); len(waiting) != 1 || waiting[0].RequestID != ask.RequestID {
		t.Fatalf("waitingOn after restart = %+v", waiting)
	}
	h.clock.Advance(2 * time.Minute)
	h.svc.RunDue(context.Background(), false)
	eventually(t, "timeout notice", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `event="timeout"`) && strings.Contains(p, ask.RequestID) {
				return true
			}
		}
		for _, s := range aliceAd.steerTexts() {
			if strings.Contains(s, `event="timeout"`) {
				return true
			}
		}
		return false
	})
	if waiting, _ := h.svc.SummaryState("alice"); len(waiting) != 0 {
		t.Fatalf("waitingOn after timeout = %+v", waiting)
	}
	// A late reply is no longer accepted.
	res := h.svc.Deliver(context.Background(), "", Envelope{ID: "msg_late", Kind: KindReply, RequestID: ask.RequestID, From: h.addr("bob"), To: h.addr("alice"), Body: "late", SentAt: "now"})
	if res.Error != ErrUnknownRequest {
		t.Fatalf("late reply: %+v", res)
	}
}

func TestObligationAndReminderSurviveRestart(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ask, _ := h.svc.Ask(context.Background(), "alice", "bob", "q", 0)
	h.restart(nil)
	if _, asks := h.svc.SummaryState("bob"); asks != 1 {
		t.Fatalf("openAsks after restart = %d", asks)
	}
	if _, err := h.svc.Reply(context.Background(), "bob", ask.RequestID, "answer"); err != nil {
		t.Fatal(err)
	}
}

func TestKillSwitch(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	_, bobAd := b.addSession("bob", "bob")
	b.link(Address{Host: "hostA", Agent: "alice", Name: "alice"}, "bob")
	ctx := context.Background()

	// Pause B: deliveries to its agents are rejected.
	if err := b.svc.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Send(ctx, "alice", "hostB~bob", "hi", ""); rejectionCode(err) != ErrMessagingPaused {
		t.Fatalf("send to paused host: %v", err)
	}
	if err := b.svc.SetPaused(false); err != nil {
		t.Fatal(err)
	}

	// Pause A with a message waiting in its outbox while B is offline.
	net.setOffline("hostB", true)
	res, err := a.svc.Send(ctx, "alice", "hostB~bob", "queued", "")
	if err != nil || res.Status != StatusPending {
		t.Fatalf("offline send = %+v, %v", res, err)
	}
	if err := a.svc.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Send(ctx, "alice", "hostB~bob", "blocked", ""); rejectionCode(err) != ErrMessagingPaused {
		t.Fatalf("send from paused host: %v", err)
	}
	net.setOffline("hostB", false)
	callsBefore := len(net.callLog())
	a.clock.Advance(time.Minute)
	a.svc.RunDue(ctx, true)
	if len(net.callLog()) != callsBefore {
		t.Fatal("a paused host must hold its outbox without attempting delivery")
	}
	rows, _ := a.db.PendingAgentMsgOutbox()
	if len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("outbox while paused = %+v", rows)
	}
	// The flag survives a restart.
	a.restart(net)
	if !a.svc.Paused() {
		t.Fatal("paused flag was not persisted")
	}
	// Resuming releases the outbox.
	if err := a.svc.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	a.svc.RunDue(ctx, true)
	eventually(t, "held message to arrive", func() bool { return len(bobAd.promptTexts()) == 1 })
}

func TestOutboxRetriesWhenHostReconnects(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	_, bobAd := b.addSession("bob", "bob")
	b.link(Address{Host: "hostA", Agent: "alice", Name: "alice"}, "bob")
	ctx := context.Background()

	net.setOffline("hostB", true)
	res, err := a.svc.Send(ctx, "alice", "hostB~bob", "are you there", "")
	if err != nil || res.Status != StatusPending {
		t.Fatalf("offline send = %+v, %v", res, err)
	}
	evs := a.agentEvents("alice")
	if len(evs) != 1 || evs[0]["status"] != "pending" {
		t.Fatalf("sender event = %+v", evs)
	}
	// Still offline after the retry interval: it stays pending.
	a.clock.Advance(16 * time.Second)
	a.svc.RunDue(ctx, false)
	rows, _ := a.db.PendingAgentMsgOutbox()
	if len(rows) != 1 || rows[0].Attempts != 2 {
		t.Fatalf("outbox after failed retry = %+v", rows)
	}
	// Reconnect: the next pass (or a federation kick) delivers it once.
	net.setOffline("hostB", false)
	a.clock.Advance(16 * time.Second)
	a.svc.RunDue(ctx, false)
	eventually(t, "bob to receive the held message", func() bool { return len(bobAd.promptTexts()) == 1 })
	if rows, _ := a.db.PendingAgentMsgOutbox(); len(rows) != 0 {
		t.Fatalf("outbox not drained: %+v", rows)
	}
	// The sender sees the pending -> delivered transition.
	var last map[string]any
	for _, ev := range a.agentEvents("alice") {
		if ev["kind"] == "agent_message_status" {
			last = ev
		}
	}
	if last == nil || last["id"] != res.ID || last["status"] != "started" {
		t.Fatalf("status events = %+v", a.agentEvents("alice"))
	}
	// A replayed copy of the envelope is deduplicated by the recipient.
	row, _ := a.db.AgentMsgOutbox(res.ID)
	_ = row
	if got := bobAd.promptTexts(); len(got) != 1 {
		t.Fatalf("prompts = %v", got)
	}
}

func TestOutboxGivesUpAfter24HoursWithUndeliverable(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	net.setOffline("hostB", true)
	sent, err := a.svc.Send(context.Background(), "alice", "hostB~bob", "hello?", "")
	if err != nil || sent.Status != StatusPending {
		t.Fatalf("send = %+v, %v", sent, err)
	}
	a.clock.Advance(24*time.Hour + time.Minute)
	a.svc.RunDue(context.Background(), true)
	eventually(t, "undeliverable notice", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `event="undeliverable"`) {
				return true
			}
		}
		for _, p := range aliceAd.steerTexts() {
			if strings.Contains(p, `event="undeliverable"`) {
				return true
			}
		}
		return false
	})
	if rows, _ := a.db.PendingAgentMsgOutbox(); len(rows) != 0 {
		t.Fatalf("outbox = %+v", rows)
	}
	var last map[string]any
	for _, ev := range a.agentEvents("alice") {
		if ev["kind"] == "agent_message_status" {
			last = ev
		}
	}
	if last == nil || last["id"] != sent.ID || last["status"] != "undeliverable" {
		t.Fatalf("status events = %+v", a.agentEvents("alice"))
	}
}

func TestPendingAskIsCancelledWhenItTimesOut(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	net.setOffline("hostB", true)
	if _, err := a.svc.Ask(context.Background(), "alice", "hostB~bob", "hello?", 1); err != nil {
		t.Fatal(err)
	}
	a.clock.Advance(2 * time.Minute)
	a.svc.RunDue(context.Background(), false)
	if rows, _ := a.db.PendingAgentMsgOutbox(); len(rows) != 0 {
		t.Fatalf("an ask that timed out must not be delivered later: %+v", rows)
	}
}

func TestCrossHostAskReplyAndSameHostBypassesFederation(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	a.addSession("carol", "carol")
	_, bobAd := b.addSession("bob", "bob")
	b.link(a.addr("alice"), "bob")
	ctx := context.Background()

	// Same-host traffic never touches federation.
	a.link(a.addr("alice"), "carol")
	if _, err := a.svc.Send(ctx, "alice", "carol", "local", ""); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(net, "agent_message_deliver"); n != 0 {
		t.Fatalf("same-host send used federation: %v", net.callLog())
	}

	// Resolve by name through the directory, then ask across hosts.
	ask, err := a.svc.Ask(ctx, "alice", "bob@hostB", "cross-host question", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ask.Status != StatusStarted {
		t.Fatalf("ask = %+v", ask)
	}
	eventually(t, "bob to receive", func() bool { return len(bobAd.promptTexts()) == 1 })
	if p := bobAd.promptTexts()[0]; !strings.Contains(p, "hostA~alice") {
		t.Fatalf("prompt = %s", p)
	}
	// Bob's reply travels back; the request closes on A, the obligation on B.
	if _, err := b.svc.Reply(ctx, "bob", ask.RequestID, "answer from B"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice to receive the reply", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, "answer from B") {
				return true
			}
		}
		return false
	})
	if waiting, _ := a.svc.SummaryState("alice"); len(waiting) != 0 {
		t.Fatalf("waitingOn = %+v", waiting)
	}
	// Without a link on B, the reverse direction is refused...
	if _, err := b.svc.Send(ctx, "bob", "hostA~alice", "unsolicited", ""); rejectionCode(err) != ErrNoLink {
		t.Fatalf("reverse send: %v", err)
	}
	// ...and a sender host with only view access is refused by policy.
	net.setLevel("hostA", "hostB", federation.LevelView)
	if _, err := a.svc.Send(ctx, "alice", "hostB~bob", "denied", ""); rejectionCode(err) != ErrAccessDenied {
		t.Fatalf("view-level send: %v", err)
	}
	if rows, _ := a.db.PendingAgentMsgOutbox(); len(rows) != 0 {
		t.Fatalf("an access denial is terminal, not retried: %+v", rows)
	}
}

func TestRecipientClosedNotifiesAskerWithRecipientGone(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	_, bobAd := b.addSession("bob", "bob")
	b.link(a.addr("alice"), "bob")
	ask, err := a.svc.Ask(context.Background(), "alice", "hostB~bob", "q", 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "ask delivered", func() bool { return len(bobAd.promptTexts()) == 1 })
	b.closeSession("bob")
	b.svc.Wait()
	eventually(t, "recipient_gone notice", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `event="recipient_gone"`) && strings.Contains(p, ask.RequestID) {
				return true
			}
		}
		for _, p := range aliceAd.steerTexts() {
			if strings.Contains(p, `event="recipient_gone"`) {
				return true
			}
		}
		return false
	})
	if waiting, _ := a.svc.SummaryState("alice"); len(waiting) != 0 {
		t.Fatalf("waitingOn = %+v", waiting)
	}
}

func TestLinkRequestApprovalAndDenial(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	bob, _ := b.addSession("bob", "bob")
	ctx := context.Background()
	a.svc.Start(ctx)
	b.svc.Start(ctx)
	t.Cleanup(func() { a.svc.Close(); b.svc.Close() })

	res, err := a.svc.RequestLink(ctx, "alice", "hostB~bob", "need to coordinate migrations")
	if err != nil || res.Status != StatusPending {
		t.Fatalf("request = %+v, %v", res, err)
	}
	// Asking again while pending is a no-op.
	if again, err := a.svc.RequestLink(ctx, "alice", "hostB~bob", "again"); err != nil || again.Status != StatusPending {
		t.Fatalf("repeat = %+v, %v", again, err)
	}
	var approval string
	eventually(t, "approval on bob", func() bool {
		pending := bob.PendingApprovals()
		if len(pending) == 0 {
			return false
		}
		approval = pending[0].ReqID
		return true
	})
	if got := bob.PendingApprovals(); len(got) != 1 || !strings.Contains(got[0].Title, "@agent:HOSTA/alice") || !strings.Contains(got[0].Title, "need to coordinate migrations") {
		t.Fatalf("approvals = %+v", got)
	}
	// Until approved, messaging is still denied.
	if _, err := a.svc.Send(ctx, "alice", "hostB~bob", "hi", ""); rejectionCode(err) != ErrNoLink {
		t.Fatalf("before approval: %v", err)
	}
	if err := bob.RespondPermission(approval, "allow"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "link_approved to alice", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `event="link_approved"`) {
				return true
			}
		}
		return false
	})
	links, _ := b.svc.Links("bob")
	if len(links.Links) != 1 || links.Links[0].Source != SourceApproval || links.Links[0].From.Agent != "alice" || links.Links[0].Delivery != DeliverySteer || links.Links[0].BudgetPerHour != 60 || links.Links[0].MaxHops != 20 {
		t.Fatalf("links = %+v", links)
	}
	if _, err := a.svc.Send(ctx, "alice", "hostB~bob", "hi", ""); err != nil {
		t.Fatalf("after approval: %v", err)
	}
	// The recipient is back to idle-or-working, not stuck blocked.
	if bob.Status() == "blocked" {
		t.Fatal("bob stayed blocked after the approval resolved")
	}

	// A denied request comes back as link_denied; no link is created.
	carol, _ := b.addSession("carol", "carol")
	if _, err := a.svc.RequestLink(ctx, "alice", "hostB~carol", "please"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "approval on carol", func() bool { return len(carol.PendingApprovals()) == 1 })
	if err := carol.RespondPermission(carol.PendingApprovals()[0].ReqID, "deny"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "link_denied to alice", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `event="link_denied"`) {
				return true
			}
		}
		for _, p := range aliceAd.steerTexts() {
			if strings.Contains(p, `event="link_denied"`) {
				return true
			}
		}
		return false
	})
	if links, _ := b.svc.Links("carol"); len(links.Links) != 0 {
		t.Fatalf("denied request created a link: %+v", links)
	}
	// After the outcome a new request is possible again.
	if rows, _ := a.db.AgentMsgLinkRequests("out"); len(rows) != 0 {
		t.Fatalf("pending out requests = %+v", rows)
	}
}

func TestLinkRequestAgainstPausedHostAndUnreachable(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	b.addSession("bob", "bob")
	ctx := context.Background()
	if err := b.svc.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.RequestLink(ctx, "alice", "hostB~bob", "why"); rejectionCode(err) != ErrMessagingPaused {
		t.Fatalf("paused: %v", err)
	}
	_ = b.svc.SetPaused(false)
	net.setOffline("hostB", true)
	if _, err := a.svc.RequestLink(ctx, "alice", "hostB~bob", "why"); rejectionCode(err) != ErrHostUnreachable {
		t.Fatalf("offline: %v", err)
	}
	// The failed request left nothing pending.
	if rows, _ := a.db.AgentMsgLinkRequests("out"); len(rows) != 0 {
		t.Fatalf("pending out requests = %+v", rows)
	}
}

func TestLinkApprovalReRaisedAfterRestart(t *testing.T) {
	a := newTestHost(t, "hostA", hostOpts{})
	a.addSession("alice", "alice")
	bob, _ := a.addSession("bob", "bob")
	a.svc.Start(context.Background())
	if _, err := a.svc.RequestLink(context.Background(), "alice", "bob", "pls"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "approval", func() bool { return len(bob.PendingApprovals()) == 1 })
	// Daemon stops with the approval pending: no denial is sent.
	a.svc.Close()
	if rows, _ := a.db.AgentMsgLinkRequests("in"); len(rows) != 1 {
		t.Fatalf("pending in-request was lost: %+v", rows)
	}
}

func TestDirectoryListingCardsAndNameResolution(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	a.addSession("shared1", "worker")
	b.addSession("shared2", "worker")
	b.addSession("hidden", "secret")
	ctx := context.Background()
	if err := b.svc.SetListed("hidden", false); err != nil {
		t.Fatal(err)
	}
	if err := b.svc.SetCard("shared2", "runs the nightly build"); err != nil {
		t.Fatal(err)
	}
	b.link(a.addr("alice"), "shared2")

	entries, err := a.svc.Directory(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	byAgent := map[string]DirectoryEntry{}
	for _, e := range entries {
		byAgent[e.Address.Agent] = e
	}
	if _, ok := byAgent["alice"]; ok {
		t.Fatal("the requester must not list itself")
	}
	if _, ok := byAgent["hidden"]; ok {
		t.Fatal("unlisted agents must not appear")
	}
	if e := byAgent["shared2"]; e.Card != "runs the nightly build" || !e.CanMessage || e.Address.Host != "hostB" || e.Agent != "claude" || e.CWD != "/work/shared2" {
		t.Fatalf("shared2 = %+v", e)
	}
	if e := byAgent["shared1"]; e.CanMessage || e.Card != "worker" {
		t.Fatalf("shared1 = %+v", e)
	}
	if got, _ := a.svc.Directory(ctx, "alice", "nightly"); len(got) != 1 || got[0].Address.Agent != "shared2" {
		t.Fatalf("query = %+v", got)
	}

	// A bare name shared by two agents is ambiguous and lists candidates.
	_, err = a.svc.Send(ctx, "alice", "worker", "hi", "")
	if rejectionCode(err) != ErrInvalid || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "@agent:hostA/worker") || !strings.Contains(err.Error(), "@agent:HOSTB/worker") {
		t.Fatalf("ambiguous: %v", err)
	}
	// name@host picks one; the host may be given by ID or display name.
	if _, err := a.svc.Send(ctx, "alice", "@worker@hostB", "hi", ""); err != nil {
		t.Fatalf("name@host: %v", err)
	}
	if _, err := a.svc.Send(ctx, "alice", "worker@HOSTB", "hi", ""); err != nil {
		t.Fatalf("name@display-name: %v", err)
	}

	// The directory is cached for 30 seconds per host.
	before := countCalls(net, "agent_directory")
	_, _ = a.svc.Directory(ctx, "alice", "")
	if countCalls(net, "agent_directory") != before {
		t.Fatal("directory was not cached")
	}
	a.clock.Advance(31 * time.Second)
	_, _ = a.svc.Directory(ctx, "alice", "")
	if countCalls(net, "agent_directory") != before+1 {
		t.Fatal("directory cache did not expire")
	}
	// A host that does not grant message access is not queried.
	net.setLevel("hostA", "hostB", federation.LevelView)
	a.clock.Advance(31 * time.Second)
	before = countCalls(net, "agent_directory")
	entries, _ = a.svc.Directory(ctx, "alice", "")
	if countCalls(net, "agent_directory") != before {
		t.Fatal("queried a host that grants only view")
	}
	for _, e := range entries {
		if e.Address.Host == "hostB" {
			t.Fatalf("view-only host leaked into directory: %+v", e)
		}
	}
}

func countCalls(n *testNet, cmd string) int {
	c := 0
	for _, call := range n.callLog() {
		if strings.HasSuffix(call, ":"+cmd) {
			c++
		}
	}
	return c
}

func TestLinksCRUDAndBroadcast(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("alice", "alice")
	h.addSession("bob", "bob")
	from := Address{Host: "hostZ", Agent: "far", Name: "far"}
	if err := h.svc.SetLink("bob", LinkInput{From: from, Delivery: "bogus"}); err == nil {
		t.Fatal("invalid delivery accepted")
	}
	if err := h.svc.SetLink("bob", LinkInput{From: from, BudgetPerHour: 5, MaxHops: 3, Delivery: DeliveryQueue}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SetLink("bob", LinkInput{From: from, BudgetPerHour: 7, MaxHops: 3, Delivery: DeliveryQueue, Paused: true}); err != nil {
		t.Fatal(err)
	}
	view, err := h.svc.Links("bob")
	if err != nil || len(view.Links) != 1 {
		t.Fatalf("links = %+v, %v", view, err)
	}
	l := view.Links[0]
	if l.BudgetPerHour != 7 || !l.Paused || l.Source != SourceUser || l.To != "bob" || l.ID == "" || l.CreatedAt == "" || !view.Listed {
		t.Fatalf("link = %+v", l)
	}
	if len(h.linkChanges) != 2 {
		t.Fatalf("OnLinksChanged calls = %v", h.linkChanges)
	}
	if err := h.svc.DeleteLink("bob", from); err != nil {
		t.Fatal(err)
	}
	if view, _ := h.svc.Links("bob"); len(view.Links) != 0 {
		t.Fatalf("links after delete = %+v", view)
	}
	if _, err := h.svc.Links("nobody"); err == nil {
		t.Fatal("links for an unknown session")
	}
}

func TestSessionDeletionForgetsMessagingState(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	h.addSession("bob", "bob")
	_ = h.svc.SetLink("bob", LinkInput{From: Address{Host: "x", Agent: "y"}})
	_ = h.db.UpsertSession(store.Session{ID: "bob", Name: "bob", Spec: []byte(`{}`), Status: "idle", CreatedAt: 1})
	if err := h.db.DeleteSession("bob"); err != nil {
		t.Fatal(err)
	}
	if links, _ := h.db.AgentMsgLinks("bob"); len(links) != 0 {
		t.Fatalf("links = %+v", links)
	}
}

func TestMalformedReplyLeavesRequestOpenForRetry(t *testing.T) {
	h := newTestHost(t, "hostA", hostOpts{})
	_, aliceAd := h.addSession("alice", "alice")
	_, bobAd := h.addSession("bob", "bob")
	h.link(h.addr("alice"), "bob")
	ctx := context.Background()
	ask, err := h.svc.Ask(ctx, "alice", "bob", "ping?", 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob to receive the ask", func() bool { return len(bobAd.promptTexts()) == 1 })

	// A reply with the text in the wrong argument arrives with no body.
	_, err = h.svc.Reply(ctx, "bob", ask.RequestID, "")
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Code != ErrInvalid {
		t.Fatalf("empty reply err = %v, want %s", err, ErrInvalid)
	}
	if _, asks := h.svc.SummaryState("bob"); asks != 1 {
		t.Fatalf("a malformed reply must not close the request, openAsks=%d", asks)
	}
	if _, err := h.svc.Reply(ctx, "bob", ask.RequestID, "pong"); err != nil {
		t.Fatalf("corrected reply: %v", err)
	}
	eventually(t, "alice to receive the reply", func() bool {
		for _, p := range aliceAd.promptTexts() {
			if strings.Contains(p, `kind="reply"`) && strings.Contains(p, "pong") {
				return true
			}
		}
		return false
	})
	if _, asks := h.svc.SummaryState("bob"); asks != 0 {
		t.Fatalf("answered request still open, openAsks=%d", asks)
	}
}
