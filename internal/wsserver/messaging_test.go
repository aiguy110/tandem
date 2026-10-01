package wsserver

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/messaging"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/gorilla/websocket"
)

// messagingSessions adapts the test backend to messaging.Sessions.
type messagingSessions struct{ b *testBackend }

func (m messagingSessions) Get(id string) *session.Session { return m.b.Get(id) }
func (m messagingSessions) List() []*session.Session {
	m.b.mu.RLock()
	defer m.b.mu.RUnlock()
	out := []*session.Session{}
	for _, s := range m.b.sessions {
		out = append(out, s)
	}
	return out
}
func (messagingSessions) CWD(string) string { return "/repo" }

func setupMessagingWS(t *testing.T) (*messaging.Service, *websocket.Conn, *Handler) {
	t.Helper()
	db, backend, _, _, _ := setupWS(t, 0)
	var handler *Handler
	svc, err := messaging.New(messaging.Options{
		Store: db, Sessions: messagingSessions{backend}, SelfID: "hostA", LocalName: "A",
		OnLinksChanged:  func(id string) { handler.BroadcastAgentLinks(id) },
		OnStateChanged:  func() { handler.BroadcastMessagingState() },
		OnSummaryChange: func() {},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler = New(Options{Token: "secret", Registry: backend, Automation: db, Messaging: svc})
	t.Cleanup(handler.Close)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return svc, dial(t, "ws"+strings.TrimPrefix(srv.URL, "http")), handler
}

func TestMessagingLinkCommandsAckAndBroadcast(t *testing.T) {
	_, c, _ := setupMessagingWS(t)
	send(t, c, map[string]any{"t": "subscribe", "sessionId": "a", "channels": []string{"status"}, "corrId": "sub"})
	for {
		if got := recv(t, c); got["t"] == "ack" && got["corrId"] == "sub" {
			break
		}
	}
	link := map[string]any{"from": map[string]any{"host": "hostZ", "agent": "far", "name": "far"}, "delivery": "queue", "budgetPerHour": 5, "maxHops": 4, "paused": false}
	send(t, c, map[string]any{"t": "set_agent_link", "sessionId": "a", "link": link, "corrId": "set"})
	sawAck, sawLinks := false, false
	for !(sawAck && sawLinks) {
		got := recv(t, c)
		switch {
		case got["t"] == "ack" && got["corrId"] == "set":
			if got["error"] != nil {
				t.Fatalf("set_agent_link ack = %#v", got)
			}
			sawAck = true
		case got["t"] == "agent_links" && got["sessionId"] == "a":
			links := got["links"].([]any)
			if len(links) != 1 || links[0].(map[string]any)["delivery"] != "queue" || links[0].(map[string]any)["budgetPerHour"] != float64(5) {
				t.Fatalf("broadcast links = %#v", got)
			}
			sawLinks = true
		}
	}

	send(t, c, map[string]any{"t": "list_agent_links", "sessionId": "a", "corrId": "list"})
	var listed map[string]any
	for {
		if listed = recv(t, c); listed["corrId"] == "list" {
			break
		}
	}
	if listed["t"] != "agent_links" || listed["listed"] != true || listed["card"] != "" || len(listed["links"].([]any)) != 1 {
		t.Fatalf("list_agent_links = %#v", listed)
	}
	l := listed["links"].([]any)[0].(map[string]any)
	for _, key := range []string{"id", "from", "to", "delivery", "budgetPerHour", "maxHops", "paused", "source", "createdAt", "usedLastHour"} {
		if _, ok := l[key]; !ok {
			t.Errorf("link is missing %q: %#v", key, l)
		}
	}

	send(t, c, map[string]any{"t": "set_agent_listed", "sessionId": "a", "listed": false, "corrId": "unlist"})
	for {
		if got := recv(t, c); got["corrId"] == "unlist" {
			if got["t"] != "ack" || got["error"] != nil {
				t.Fatalf("set_agent_listed ack = %#v", got)
			}
			break
		}
	}
	send(t, c, map[string]any{"t": "delete_agent_link", "sessionId": "a", "from": link["from"], "corrId": "del"})
	for {
		if got := recv(t, c); got["corrId"] == "del" {
			if got["t"] != "ack" || got["error"] != nil {
				t.Fatalf("delete_agent_link ack = %#v", got)
			}
			break
		}
	}
	send(t, c, map[string]any{"t": "list_agent_links", "sessionId": "a", "corrId": "list2"})
	for {
		if got := recv(t, c); got["corrId"] == "list2" {
			if got["listed"] != false || len(got["links"].([]any)) != 0 {
				t.Fatalf("after delete = %#v", got)
			}
			break
		}
	}
}

func TestMessagingStateAndKillSwitchBroadcast(t *testing.T) {
	svc, c, _ := setupMessagingWS(t)
	send(t, c, map[string]any{"t": "get_messaging_state", "corrId": "state"})
	got := recv(t, c)
	if got["t"] != "messaging_state" || got["paused"] != false || got["corrId"] != "state" {
		t.Fatalf("get_messaging_state = %#v", got)
	}
	send(t, c, map[string]any{"t": "set_messaging_paused", "paused": true, "corrId": "pause"})
	sawAck, sawState := false, false
	for !(sawAck && sawState) {
		got := recv(t, c)
		switch {
		case got["t"] == "ack" && got["corrId"] == "pause":
			if got["error"] != nil {
				t.Fatalf("ack = %#v", got)
			}
			sawAck = true
		case got["t"] == "messaging_state":
			if got["paused"] != true {
				t.Fatalf("broadcast = %#v", got)
			}
			sawState = true
		}
	}
	if !svc.Paused() {
		t.Fatal("kill switch was not set")
	}
}

func TestMessagingDeliverChecksSenderAgainstFederationOrigin(t *testing.T) {
	svc, c, _ := setupMessagingWS(t)
	env := func(fromHost string) map[string]any {
		return map[string]any{"id": "msg_" + fromHost, "threadId": "thr_1", "kind": "send", "hop": 0, "sentAt": "now", "body": "hi",
			"from": map[string]any{"host": fromHost, "agent": "x"}, "to": map[string]any{"host": "hostA", "agent": "a"}}
	}
	result := func(corr string) map[string]any {
		for {
			if got := recv(t, c); got["corrId"] == corr {
				return got
			}
		}
	}
	// A relayed delivery: the executing host trusts the federation origin,
	// not what the envelope says.
	send(t, c, map[string]any{"t": "agent_message_deliver", "envelope": env("hostB"), "federationOrigin": "hostC", "corrId": "forged"})
	if got := result("forged"); got["t"] != "agent_message_result" || got["error"] != messaging.ErrAccessDenied {
		t.Fatalf("forged origin = %#v", got)
	}
	// Matching origin gets past that check and meets default-deny.
	send(t, c, map[string]any{"t": "agent_message_deliver", "envelope": env("hostB"), "federationOrigin": "hostB", "corrId": "genuine"})
	if got := result("genuine"); got["error"] != messaging.ErrNoLink || got["id"] != "msg_hostB" {
		t.Fatalf("genuine origin = %#v", got)
	}
	// With no origin (this host's own browser) the sender must be this host.
	send(t, c, map[string]any{"t": "agent_message_deliver", "envelope": env("hostB"), "corrId": "local-forged"})
	if got := result("local-forged"); got["error"] != messaging.ErrAccessDenied {
		t.Fatalf("local forged = %#v", got)
	}
	// Grant a link and a genuine message is accepted.
	if err := svc.SetLink("a", messaging.LinkInput{From: messaging.Address{Host: "hostB", Agent: "x"}}); err != nil {
		t.Fatal(err)
	}
	send(t, c, map[string]any{"t": "agent_message_deliver", "envelope": env("hostB"), "federationOrigin": "hostB", "corrId": "linked"})
	if got := result("linked"); got["error"] != nil || (got["status"] != "started" && got["status"] != "queued") {
		t.Fatalf("linked = %#v", got)
	}
}

func TestMessagingDirectoryAndLinkRequestCommands(t *testing.T) {
	svc, c, _ := setupMessagingWS(t)
	send(t, c, map[string]any{"t": "agent_directory", "requester": map[string]any{"host": "hostB", "agent": "x"}, "corrId": "dir"})
	got := recv(t, c)
	entries, _ := got["entries"].([]any)
	if got["t"] != "agent_directory" || len(entries) != 1 {
		t.Fatalf("agent_directory = %#v", got)
	}
	e := entries[0].(map[string]any)
	addr := e["address"].(map[string]any)
	if addr["host"] != "hostA" || addr["agent"] != "a" || e["canMessage"] != false {
		t.Fatalf("entry = %#v", e)
	}
	_ = svc
	// A link request with a forged sender is refused; a genuine one is pending.
	req := func(origin, corr string) map[string]any {
		m := map[string]any{"t": "agent_link_request", "from": map[string]any{"host": "hostB", "agent": "x", "name": "x"}, "to": map[string]any{"host": "hostA", "agent": "a"}, "reason": "why", "corrId": corr}
		if origin != "" {
			m["federationOrigin"] = origin
		}
		send(t, c, m)
		for {
			if got := recv(t, c); got["corrId"] == corr {
				return got
			}
		}
	}
	if got := req("hostC", "r1"); got["t"] != "agent_link_request_result" || got["error"] != messaging.ErrAccessDenied {
		t.Fatalf("forged link request = %#v", got)
	}
	if got := req("hostB", "r2"); got["t"] != "agent_link_request_result" || got["status"] != "pending" || got["error"] != nil {
		t.Fatalf("link request = %#v", got)
	}
}
