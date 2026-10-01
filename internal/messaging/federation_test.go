package messaging

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/federation"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
)

// fedNode is one daemon in a real federation tree: a federation.Service over
// real tunnels, a messaging service, and a Local that executes browser
// commands the way wsserver does (relaying commands that name another hostId,
// delivering the rest to messaging with the command's origin).
type fedNode struct {
	t        *testing.T
	name     string
	fed      *federation.Service
	svc      *Service
	sessions *testSessions
	adapters map[string]*fakeAdapter
	db       *store.Store
}

func (n *fedNode) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"t":"agents","agents":[]}`), nil
}

func (n *fedNode) Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var cmd map[string]json.RawMessage
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	origin := federation.OriginFrom(ctx)
	if raw, ok := cmd[federation.OriginField]; ok {
		_ = json.Unmarshal(raw, &origin)
	}
	var hostID string
	_ = json.Unmarshal(cmd["hostId"], &hostID)
	if hostID != "" {
		delete(cmd, "hostId")
		delete(cmd, federation.OriginField)
		relayed, _ := json.Marshal(cmd)
		return n.fed.Call(federation.WithOrigin(ctx, origin), hostID, relayed)
	}
	var typ string
	_ = json.Unmarshal(cmd["t"], &typ)
	switch typ {
	case "agent_message_deliver":
		var env Envelope
		_ = json.Unmarshal(cmd["envelope"], &env)
		res := n.svc.Deliver(ctx, origin, env)
		reply := map[string]any{"t": "agent_message_result", "id": res.ID, "status": res.Status}
		if res.Error != "" {
			reply["error"], reply["message"] = res.Error, res.Message
		}
		return json.Marshal(reply)
	case "agent_link_request":
		var from, to Address
		var reason string
		_ = json.Unmarshal(cmd["from"], &from)
		_ = json.Unmarshal(cmd["to"], &to)
		_ = json.Unmarshal(cmd["reason"], &reason)
		res := n.svc.HandleLinkRequest(ctx, origin, from, to, reason)
		return json.Marshal(map[string]any{"t": "agent_link_request_result", "status": res.Status, "error": res.Error})
	case "agent_message_pull":
		envs, err := n.svc.HandlePull(origin)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"t": "agent_message_pull_result", "envelopes": envs})
	case "agent_message_pull_ack":
		var ids []string
		var results []PullAck
		_ = json.Unmarshal(cmd["ids"], &ids)
		_ = json.Unmarshal(cmd["results"], &results)
		applied, err := n.svc.HandlePullAck(origin, ids, results)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"t": "agent_message_pull_ack_result", "applied": applied})
	case "agent_directory":
		var requester *Address
		_ = json.Unmarshal(cmd["requester"], &requester)
		return json.Marshal(map[string]any{"t": "agent_directory", "entries": n.svc.LocalDirectory(requester, "")})
	}
	return json.RawMessage(`{"t":"ack","error":"unsupported"}`), nil
}

func (n *fedNode) addSession(id, name string) *fakeAdapter {
	n.t.Helper()
	log, err := eventlog.New(n.name+"-"+id, n.db, 64)
	if err != nil {
		n.t.Fatal(err)
	}
	a := newFakeAdapter()
	s, err := session.New(id, name, agentadapter.Spec{Agent: "claude"}, a, log)
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { _ = s.Dispose(context.Background()) })
	n.sessions.mu.Lock()
	n.sessions.m[id] = s
	n.sessions.mu.Unlock()
	n.adapters[id] = a
	n.svc.Watch(s)
	return a
}

func (n *fedNode) addr(id string) Address {
	return Address{Host: n.fed.SelfID(), Agent: id, Name: n.sessions.Get(id).DisplayName()}
}

func newFedNode(t *testing.T, name string, opts federation.Options, local bool) *fedNode {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	n := &fedNode{t: t, name: name, sessions: &testSessions{m: map[string]*session.Session{}}, adapters: map[string]*fakeAdapter{}, db: db}
	opts.Store, opts.Name, opts.PollInterval = db, name, 10*time.Millisecond
	if local {
		opts.Local = n
	}
	fed, err := federation.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	n.fed = fed
	n.svc, err = New(Options{Store: db, Sessions: n.sessions, Federation: fed, LocalName: name, Token: "tok", CallTimeout: 5 * time.Second, TickInterval: 20 * time.Millisecond, PullInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// startSiblings builds root with two children, alpha and beta, whose policies
// are the given ones. The root only relays.
func startSiblings(t *testing.T, alphaPolicy, betaPolicy federation.Policy) (root, alpha, beta *fedNode) {
	t.Helper()
	center := notifications.New()
	var rootNode *fedNode
	rootNode = newFedNode(t, "root", federation.Options{
		Notifications: center,
		NewLocal:      func() (federation.Local, error) { return rootNode, nil },
	}, false)
	server := httptest.NewServer(rootNode.fed)
	t.Cleanup(server.Close)

	start := func(name string, policy federation.Policy) *fedNode {
		node := newFedNode(t, name, federation.Options{ParentURL: server.URL, Policy: policy}, true)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = node.fed.RunParentLink(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		})
		return node
	}
	alpha = start("alpha", alphaPolicy)
	beta = start("beta", betaPolicy)

	// Accept both registrations.
	accepted := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(accepted) < 2 {
		for _, item := range center.List() {
			if !strings.HasPrefix(item.ID, "federation-registration-") || accepted[item.ID] {
				continue
			}
			if _, _, err := rootNode.fed.HandleNotificationAction(context.Background(), item.ID, "accept"); err != nil {
				t.Fatal(err)
			}
			accepted[item.ID] = true
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(accepted) != 2 {
		t.Fatal("registrations were not accepted")
	}
	eventually(t, "both children connected and visible to each other", func() bool {
		connected := 0
		for _, h := range rootNode.fed.Hosts() {
			if h.Status == "connected" {
				connected++
			}
		}
		if connected != 2 {
			return false
		}
		for _, h := range alpha.fed.Hosts() {
			if h.Upstream && h.NodeID == beta.fed.SelfID() && h.Status == "connected" {
				return true
			}
		}
		return false
	})
	for _, n := range []*fedNode{rootNode, alpha, beta} {
		n.svc.Start(context.Background())
		node := n
		t.Cleanup(node.svc.Close)
	}
	return rootNode, alpha, beta
}

func TestSiblingsMessageThroughRealFederation(t *testing.T) {
	message := federation.Policy{{From: "*", Level: federation.LevelMessage}}
	_, alpha, beta := startSiblings(t, message, message)
	alphaAd := alpha.addSession("alice", "alice")
	betaAd := beta.addSession("bob", "bob")
	_ = betaAd
	ctx := context.Background()

	// The envelope's sender host is the real federation node ID.
	if alpha.fed.SelfID() == "" || beta.fed.SelfID() == "" || alpha.fed.SelfID() == beta.fed.SelfID() {
		t.Fatalf("host IDs: %q %q", alpha.fed.SelfID(), beta.fed.SelfID())
	}
	// Default-deny: no link yet.
	if _, err := alpha.svc.Send(ctx, "alice", beta.addr("bob").String(), "hi", ""); rejectionCode(err) != ErrNoLink {
		t.Fatalf("before link: %v", err)
	}
	// Discovery goes through the relay and finds bob; canMessage is false.
	entries, err := alpha.svc.Directory(ctx, "alice", "bob")
	if err != nil || len(entries) != 1 || entries[0].Address.Agent != "bob" || entries[0].CanMessage || entries[0].Address.Host != beta.fed.SelfID() {
		t.Fatalf("directory = %+v, %v", entries, err)
	}
	// The human grants the link on bob's host; discovery reflects it after the cache expires.
	if err := beta.svc.SetLink("bob", LinkInput{From: alpha.addr("alice")}); err != nil {
		t.Fatal(err)
	}
	ask, err := alpha.svc.Ask(ctx, "alice", "bob", "across the relay?", 0)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob to receive", func() bool { return len(betaAd.promptTexts()) == 1 })
	if p := betaAd.promptTexts()[0]; !strings.Contains(p, alpha.addr("alice").String()) {
		t.Fatalf("prompt = %s", p)
	}
	// The reply needs no link on alpha: it answers an open request.
	if _, err := beta.svc.Reply(ctx, "bob", ask.RequestID, "yes, via the relay"); err != nil {
		t.Fatal(err)
	}
	// Steered into the turn the undeliverable notice started, or queued as a
	// prompt, depending on timing.
	eventually(t, "alice to receive the reply", func() bool { return sawText(alphaAd, "yes, via the relay") })
	if waiting, _ := alpha.svc.SummaryState("alice"); len(waiting) != 0 {
		t.Fatalf("waitingOn = %+v", waiting)
	}
}

func TestViewOnlyHostIsDeniedAcrossFederation(t *testing.T) {
	message := federation.Policy{{From: "*", Level: federation.LevelMessage}}
	view := federation.Policy{{From: "*", Level: federation.LevelView}}
	_, alpha, beta := startSiblings(t, message, view)
	alpha.addSession("alice", "alice")
	beta.addSession("bob", "bob")
	ctx := context.Background()
	if err := beta.svc.SetLink("bob", LinkInput{From: alpha.addr("alice")}); err != nil {
		t.Fatal(err)
	}
	// Beta grants alpha only view: even a linked sender is stopped by policy,
	// and that is a terminal denial, not an outbox retry.
	_, err := alpha.svc.Send(ctx, "alice", beta.addr("bob").String(), "hi", "")
	if rejectionCode(err) != ErrAccessDenied {
		t.Fatalf("send to a view-only host: %v", err)
	}
	if rows, _ := alpha.db.PendingAgentMsgOutbox(); len(rows) != 0 {
		t.Fatalf("outbox = %+v", rows)
	}
	// Directory discovery skips hosts that do not grant message access.
	entries, _ := alpha.svc.Directory(ctx, "alice", "")
	for _, e := range entries {
		if e.Address.Agent == "bob" {
			t.Fatalf("view-only host was listed: %+v", e)
		}
	}
	if _, err := alpha.svc.RequestLink(ctx, "alice", beta.addr("bob").String(), "pls"); rejectionCode(err) != ErrAccessDenied {
		t.Fatalf("link request to a view-only host: %v", err)
	}
}
