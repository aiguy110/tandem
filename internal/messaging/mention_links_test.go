package messaging

import (
	"context"
	"testing"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/federation"
)

func promptAndFinish(t *testing.T, h *testHost, sessionID, text string) {
	t.Helper()
	h.adapters[sessionID].release()
	if _, err := h.sessions.Get(sessionID).Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: text}}); err != nil {
		t.Fatal(err)
	}
}

func hasLinkFrom(h *testHost, to string, from Address, source string) bool {
	view, err := h.svc.Links(to)
	if err != nil {
		return false
	}
	for _, l := range view.Links {
		if l.From.Same(from) && (source == "" || l.Source == source) {
			return true
		}
	}
	return false
}

func TestMentionLinksLocalAgentsBothWays(t *testing.T) {
	a := newTestHost(t, "hostA", hostOpts{})
	a.addSession("alice", "alice")
	a.addSession("bob", "bob")
	ctx := context.Background()
	a.svc.Start(ctx)
	t.Cleanup(a.svc.Close)

	promptAndFinish(t, a, "alice", "please ask @agent:HOSTA/bob about the schema.")
	eventually(t, "links both ways", func() bool {
		return hasLinkFrom(a, "bob", a.addr("alice"), SourceMention) && hasLinkFrom(a, "alice", a.addr("bob"), SourceMention)
	})
	if _, err := a.svc.Send(ctx, "alice", "@agent:HOSTA/bob", "hi", ""); err != nil {
		t.Fatalf("alice -> bob: %v", err)
	}
	if _, err := a.svc.Send(ctx, "bob", "@agent:HOSTA/alice", "hi", ""); err != nil {
		t.Fatalf("bob -> alice: %v", err)
	}
}

func TestMentionKeepsExistingLinkSettings(t *testing.T) {
	a := newTestHost(t, "hostA", hostOpts{})
	a.addSession("alice", "alice")
	a.addSession("bob", "bob")
	a.svc.Start(context.Background())
	t.Cleanup(a.svc.Close)
	if err := a.svc.SetLink("bob", LinkInput{From: a.addr("alice"), Paused: true}); err != nil {
		t.Fatal(err)
	}
	promptAndFinish(t, a, "alice", "@agent:HOSTA/bob")
	eventually(t, "reverse link", func() bool { return hasLinkFrom(a, "alice", a.addr("bob"), SourceMention) })
	view, _ := a.svc.Links("bob")
	if len(view.Links) != 1 || !view.Links[0].Paused || view.Links[0].Source != SourceUser {
		t.Fatalf("existing link changed: %+v", view.Links)
	}
}

func TestMentionLinksRemoteAgentWithOperateAccess(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	net.setLevel("hostA", "hostB", federation.LevelOperate)
	a.addSession("alice", "alice")
	b.addSession("bob", "bob")
	ctx := context.Background()
	a.svc.Start(ctx)
	b.svc.Start(ctx)
	t.Cleanup(func() { a.svc.Close(); b.svc.Close() })

	promptAndFinish(t, a, "alice", "check with @agent:HOSTB/bob")
	eventually(t, "links both ways", func() bool {
		return hasLinkFrom(b, "bob", a.addr("alice"), SourceUser) && hasLinkFrom(a, "alice", b.addr("bob"), SourceMention)
	})
	if len(b.sessions.Get("bob").PendingApprovals()) != 0 {
		t.Fatal("direct link should not raise an approval")
	}
	if _, err := a.svc.Send(ctx, "alice", "hostB~bob", "hi", ""); err != nil {
		t.Fatalf("alice -> bob: %v", err)
	}
}

func TestMentionFallsBackToLinkRequestWithoutOperateAccess(t *testing.T) {
	net := newNet()
	a := newTestHost(t, "hostA", hostOpts{net: net})
	b := newTestHost(t, "hostB", hostOpts{net: net, clock: a.clock})
	a.addSession("alice", "alice")
	bob, _ := b.addSession("bob", "bob")
	ctx := context.Background()
	a.svc.Start(ctx)
	b.svc.Start(ctx)
	t.Cleanup(func() { a.svc.Close(); b.svc.Close() })

	promptAndFinish(t, a, "alice", "check with @agent:HOSTB/bob")
	eventually(t, "approval on bob", func() bool { return len(bob.PendingApprovals()) == 1 })
	if !hasLinkFrom(a, "alice", b.addr("bob"), SourceMention) {
		t.Fatal("local link from bob missing")
	}
}
