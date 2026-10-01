package messaging

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/notifications"
)

// sawText reports whether an adapter was prompted or steered with text
// containing want.
func sawText(a *fakeAdapter, want string) bool {
	for _, p := range append(a.promptTexts(), a.steerTexts()...) {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}

// startParentChild builds a root daemon with one accepted child, both running
// agent messaging over real federation tunnels. The root's policy is the given
// one (default: children have no access on it).
func startParentChild(t *testing.T, parentPolicy federation.Policy) (parent, child *fedNode) {
	t.Helper()
	center := notifications.New()
	var p *fedNode
	p = newFedNode(t, "parent", federation.Options{
		Notifications: center, Policy: parentPolicy,
		NewLocal: func() (federation.Local, error) { return p, nil },
	}, false)
	server := httptest.NewServer(p.fed)
	t.Cleanup(server.Close)
	child = newFedNode(t, "child", federation.Options{ParentURL: server.URL}, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = child.fed.RunParentLink(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	accepted := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !accepted {
		for _, item := range center.List() {
			if !strings.HasPrefix(item.ID, "federation-registration-") {
				continue
			}
			if _, _, err := p.fed.HandleNotificationAction(context.Background(), item.ID, "accept"); err != nil {
				t.Fatal(err)
			}
			accepted = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !accepted {
		t.Fatal("registration was not accepted")
	}
	eventually(t, "child connected", func() bool {
		for _, h := range p.fed.Hosts() {
			if h.Status == "connected" {
				return true
			}
		}
		return false
	})
	for _, n := range []*fedNode{p, child} {
		n.svc.Start(context.Background())
		node := n
		t.Cleanup(node.svc.Close)
	}
	return p, child
}

// A parent that grants its child nothing is invisible to it: the child cannot
// push to the parent, so its answers must reach the parent by pull.
func TestChildResponsesReachParentByPull(t *testing.T) {
	parent, child := startParentChild(t, nil)
	aliceAd := parent.addSession("alice", "alice")
	bobAd := child.addSession("bob", "bob")
	_ = bobAd
	ctx := context.Background()

	if _, ok := child.svc.remoteHost(parent.fed.SelfID()); ok {
		t.Fatal("precondition: the child should not be able to see the parent")
	}

	// 1. Link request, approved on the child.
	if _, err := parent.svc.RequestLink(ctx, "alice", child.addr("bob").String(), "need to coordinate"); err != nil {
		t.Fatal(err)
	}
	bob := child.sessions.Get("bob")
	eventually(t, "approval on bob", func() bool { return len(bob.PendingApprovals()) == 1 })
	if err := bob.RespondPermission(bob.PendingApprovals()[0].ReqID, "allow"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "link_approved pulled to alice", func() bool { return sawText(aliceAd, `event="link_approved"`) })
	if rows, _ := parent.db.AgentMsgLinkRequests("out"); len(rows) != 0 {
		t.Fatalf("link request still pending on the requester: %+v", rows)
	}
	eventually(t, "child outbox settled", func() bool {
		rows, _ := child.db.PendingAgentMsgOutbox()
		return len(rows) == 0
	})

	// 2. An ask from the parent's agent; the child's agent replies.
	ask, err := parent.svc.Ask(ctx, "alice", child.addr("bob").String(), "what is the answer?", 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob to receive the ask", func() bool { return len(bobAd.promptTexts()) == 1 })
	reply, err := child.svc.Reply(ctx, "bob", requestID(t, bobAd.promptTexts()[0]), "forty-two")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Status != StatusPending {
		t.Fatalf("a reply the parent cannot be pushed should be held for pull, got %+v", reply)
	}
	eventually(t, "reply pulled to alice", func() bool { return sawText(aliceAd, "forty-two") })
	eventually(t, "ask closed", func() bool {
		waiting, _ := parent.svc.SummaryState("alice")
		return len(waiting) == 0
	})
	eventually(t, "reply acknowledged", func() bool {
		row, err := child.db.AgentMsgOutbox(reply.ID)
		return err == nil && row != nil && row.Status == "delivered"
	})
	_ = ask
}

// Responses are held for pull when a push is denied; a send or ask is not.
func TestAccessDeniedPushIsHeldForPullOnlyForResponses(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	_, bobAd := b.addSession("bob", "bob")
	ctx := context.Background()
	b.link(a.addr("alice"), "bob")
	a.link(b.addr("bob"), "alice")

	ask, err := a.svc.Ask(ctx, "alice", "hostB~bob", "q?", 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob prompted", func() bool { return len(bobAd.promptTexts()) == 1 })
	net.setLevel("hostB", "hostA", federation.LevelNone)

	// A send is a policy rejection: terminal, as before.
	if _, err := b.svc.Send(ctx, "bob", "hostA~alice", "hello", ""); rejectionCode(err) != ErrAccessDenied {
		t.Fatalf("send: %v", err)
	}
	if rows, _ := b.db.PendingAgentMsgOutbox(); len(rows) != 0 {
		t.Fatalf("a denied send must not stay pending: %+v", rows)
	}
	// A reply is held.
	reply, err := b.svc.Reply(ctx, "bob", ask.RequestID, "answer")
	if err != nil || reply.Status != StatusPending {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	rows, _ := b.db.PendingAgentMsgOutbox()
	if len(rows) != 1 || rows[0].ID != reply.ID {
		t.Fatalf("pending outbox = %+v", rows)
	}
	// It is still held after a retry pass.
	b.clock.Advance(outboxRetryInterval + time.Second)
	b.svc.RunDue(ctx, true)
	if rows, _ := b.db.PendingAgentMsgOutbox(); len(rows) != 1 {
		t.Fatalf("pending after retry = %+v", rows)
	}
	// A pull delivers it and the acknowledgement settles the row.
	// The background puller may race this explicit pass, and the prompt is
	// delivered asynchronously, so wait for the outcome rather than assert it.
	a.svc.pullOutstanding(ctx)
	eventually(t, "alice received the reply", func() bool { return sawText(aliceAd, "answer") })
	eventually(t, "reply acknowledged", func() bool {
		row, _ := b.db.AgentMsgOutbox(reply.ID)
		return row != nil && row.Status == "delivered"
	})
	// Acknowledging again is harmless, and a dedicated retry changes nothing.
	if n, err := b.svc.HandlePullAck("hostA", []string{reply.ID}, nil); err != nil || n != 0 {
		t.Fatalf("repeated ack = %d, %v", n, err)
	}
	// Once nothing is outstanding, nothing is polled.
	before := countCalls(net, "agent_message_pull")
	a.svc.pullOutstanding(ctx)
	if countCalls(net, "agent_message_pull") != before {
		t.Fatal("polled a host with nothing outstanding")
	}
}

func TestPulledSendNeedsMessageAccessOnThePuller(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	b.addSession("bob", "bob")
	ctx := context.Background()
	a.link(b.addr("bob"), "alice")
	b.link(a.addr("alice"), "bob")
	// A has an outstanding ask to B, so it polls B; B cannot reach A at all.
	if _, err := a.svc.Ask(ctx, "alice", "hostB~bob", "q?", 0); err != nil {
		t.Fatal(err)
	}
	net.setOffline("hostA", true)
	net.setLevel("hostB", "hostA", federation.LevelNone) // A's policy gives B no message access

	sent, err := b.svc.Send(ctx, "bob", "hostA~alice", "sneaky", "")
	if err != nil || sent.Status != StatusPending {
		t.Fatalf("send = %+v, %v", sent, err)
	}
	net.setOffline("hostA", false) // only so A's own calls are not confused; B's pushes are denied by level
	a.svc.pullOutstanding(ctx)
	if sawText(aliceAd, "sneaky") {
		t.Fatal("a pulled send from a host without message access was delivered")
	}
	row, _ := b.db.AgentMsgOutbox(sent.ID)
	if row == nil || row.Status != "failed" || row.Error != ErrAccessDenied {
		t.Fatalf("outbox row = %+v", row)
	}

	// With message access granted, the same kind of send is accepted.
	net.setLevel("hostB", "hostA", federation.LevelMessage)
	net.setOffline("hostA", true)
	sent2, err := b.svc.Send(ctx, "bob", "hostA~alice", "welcome", "")
	if err != nil || sent2.Status != StatusPending {
		t.Fatalf("send = %+v, %v", sent2, err)
	}
	net.setOffline("hostA", false)
	// Poll A->B works while B->A would also work now; make sure the push did
	// not already deliver it so the pull is what is exercised.
	a.svc.pullOutstanding(ctx)
	eventually(t, "alice receives the pulled send", func() bool { return sawText(aliceAd, "welcome") })
}

func TestPullFromOldHostBacksOff(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	b.addSession("bob", "bob")
	ctx := context.Background()
	b.link(a.addr("alice"), "bob")
	net.setNoPull("hostB", true)
	if _, err := a.svc.Ask(ctx, "alice", "hostB~bob", "q?", 0); err != nil {
		t.Fatal(err)
	}
	a.svc.pullOutstanding(ctx)
	if got := countCalls(net, "agent_message_pull"); got != 1 {
		t.Fatalf("first poll made %d calls", got)
	}
	// Backed off: polls inside the window make no calls.
	a.clock.Advance(pullBackoff - time.Minute)
	a.svc.pullOutstanding(ctx)
	if got := countCalls(net, "agent_message_pull"); got != 1 {
		t.Fatalf("polled during back-off: %d calls", got)
	}
	// After the window it tries again (and backs off again).
	a.clock.Advance(2 * time.Minute)
	a.svc.pullOutstanding(ctx)
	if got := countCalls(net, "agent_message_pull"); got != 2 {
		t.Fatalf("after back-off: %d calls", got)
	}
	// A reconnect clears the back-off, since the host may have been upgraded.
	net.setNoPull("hostB", false)
	a.svc.kickPull(true)
	a.svc.pullOutstanding(ctx)
	if got := countCalls(net, "agent_message_pull"); got != 3 {
		t.Fatalf("after reconnect: %d calls", got)
	}
}

func TestRequestLinkWaitReturnsTheDecision(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	bob, _ := b.addSession("bob", "bob")
	carol, _ := b.addSession("carol", "carol")
	ctx := context.Background()
	a.svc.Start(ctx)
	b.svc.Start(ctx)
	t.Cleanup(func() { a.svc.Close(); b.svc.Close() })

	type outcome struct {
		res LinkRequestResult
		err error
	}
	call := func(to string) chan outcome {
		ch := make(chan outcome, 1)
		go func() {
			res, err := a.svc.RequestLinkWait(ctx, "alice", to, "please")
			ch <- outcome{res, err}
		}()
		return ch
	}
	approved := call("hostB~bob")
	eventually(t, "approval on bob", func() bool { return len(bob.PendingApprovals()) == 1 })
	select {
	case o := <-approved:
		t.Fatalf("returned before the decision: %+v", o)
	case <-time.After(50 * time.Millisecond):
	}
	if err := bob.RespondPermission(bob.PendingApprovals()[0].ReqID, "allow"); err != nil {
		t.Fatal(err)
	}
	if o := <-approved; o.err != nil || o.res.Status != "approved" {
		t.Fatalf("approved = %+v", o)
	}
	// The agent already has the answer as the tool result: no system message
	// is injected, but the transcript records the outcome.
	time.Sleep(50 * time.Millisecond)
	if sawText(aliceAd, "link_approved") {
		t.Fatalf("the outcome was injected into the agent too: %v %v", aliceAd.promptTexts(), aliceAd.steerTexts())
	}
	seen := false
	for _, ev := range a.agentEvents("alice") {
		if env, ok := ev["envelope"].(map[string]any); ok && ev["direction"] == "in" {
			if sys, _ := env["system"].(map[string]any); sys["event"] == EventLinkApproved {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatalf("transcript has no inbound link_approved event: %v", a.agentEvents("alice"))
	}

	denied := call("hostB~carol")
	eventually(t, "approval on carol", func() bool { return len(carol.PendingApprovals()) == 1 })
	if err := carol.RespondPermission(carol.PendingApprovals()[0].ReqID, "deny"); err != nil {
		t.Fatal(err)
	}
	if o := <-denied; o.err != nil || o.res.Status != "denied" {
		t.Fatalf("denied = %+v", o)
	}
}

func TestRequestLinkWaitTimesOutPendingThenDeliversLater(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	_, aliceAd := a.addSession("alice", "alice")
	bob, _ := b.addSession("bob", "bob")
	ctx := context.Background()
	a.svc.opts.LinkRequestWait = 50 * time.Millisecond
	a.svc.Start(ctx)
	b.svc.Start(ctx)
	t.Cleanup(func() { a.svc.Close(); b.svc.Close() })

	res, err := a.svc.RequestLinkWait(ctx, "alice", "hostB~bob", "please")
	if err != nil || res.Status != StatusPending || res.Note == "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(a.svc.linkWaits) != 0 {
		t.Fatalf("waiter leaked: %v", a.svc.linkWaits)
	}
	// The human decides afterwards: the agent gets the usual system message.
	eventually(t, "approval on bob", func() bool { return len(bob.PendingApprovals()) == 1 })
	if err := bob.RespondPermission(bob.PendingApprovals()[0].ReqID, "allow"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "late link_approved", func() bool { return sawText(aliceAd, `event="link_approved"`) })

	// A cancelled call stops waiting and leaves nothing behind.
	carol, _ := b.addSession("carol", "carol")
	_ = carol
	a.svc.opts.LinkRequestWait = time.Minute
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := a.svc.RequestLinkWait(cctx, "alice", "hostB~carol", "please"); done <- err }()
	eventually(t, "carol approval", func() bool { return len(carol.PendingApprovals()) == 1 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled call returned no error")
	}
	if len(a.svc.linkWaits) != 0 {
		t.Fatalf("waiter leaked after cancel: %v", a.svc.linkWaits)
	}
}
