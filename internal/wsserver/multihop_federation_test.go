package wsserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aiguy110/tandem/internal/federation"
)

// topologyFederation is deliberately route-aware: its descendant has an
// opaque advertised ID, while the hierarchy information explains how it is
// reached.  This lets the browser protocol test exercise the same contract as
// a real three-node federation without needing three complete agent daemons.
type topologyFederation struct {
	hosts []federation.Host

	mu         sync.Mutex
	calls      []topologyFederationCall
	subscriber func(string, json.RawMessage)
}

type topologyFederationCall struct {
	hostID  string
	payload json.RawMessage
}

func (f *topologyFederation) Hosts() []federation.Host { return f.hosts }

func (f *topologyFederation) Subscribe(fn func(string, json.RawMessage)) func() {
	f.mu.Lock()
	f.subscriber = fn
	f.mu.Unlock()
	return func() {}
}

func (f *topologyFederation) Call(_ context.Context, hostID string, payload json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, topologyFederationCall{hostID: hostID, payload: append(json.RawMessage(nil), payload...)})
	subscriber := f.subscriber
	f.mu.Unlock()

	var command struct {
		T string `json:"t"`
	}
	_ = json.Unmarshal(payload, &command)
	switch command.T {
	case "spawn_agent":
		return json.RawMessage(`{"t":"ack","agentId":"leaf-agent"}`), nil
	case "subscribe":
		if subscriber != nil {
			subscriber(hostID, json.RawMessage(`{"t":"snapshot","agentId":"leaf-agent","seq":0,"transcript":[],"status":"idle","pendingApprovals":[]}`))
		}
		return json.RawMessage(`{"t":"ack","agentId":"leaf-agent"}`), nil
	default:
		return json.RawMessage(`{"t":"ack"}`), nil
	}
}

func (f *topologyFederation) emit(hostID string, payload json.RawMessage) {
	f.mu.Lock()
	subscriber := f.subscriber
	f.mu.Unlock()
	if subscriber != nil {
		subscriber(hostID, payload)
	}
}

// A descendant is addressed by its advertised opaque route ID, never by the
// direct parent.  That matters for controls as much as discovery: once the
// root has shown a leaf's session, spawn and subscribe requests must take the
// same route and events must retain the leaf identity on the way back.
func TestFederationRoutesDescendantHostCommandsAndEvents(t *testing.T) {
	db, backend, _, _, _ := setupWS(t, 0)
	const middleID = "middle-1"
	const leafID = "route@WyJtaWRkbGUtMSIsImxlYWYtMSJd"
	fed := &topologyFederation{hosts: []federation.Host{
		{ID: middleID, NodeID: middleID, Name: "middle", Status: "connected", Depth: 1, Route: []string{middleID}},
		{
			ID: leafID, NodeID: "leaf-1", ParentID: middleID, Name: "leaf", Status: "connected", Depth: 2,
			Route:    []string{middleID, "leaf-1"},
			Snapshot: json.RawMessage(`{"t":"agents","agents":[{"id":"leaf-agent","name":"Leaf agent","adapter":"acp","status":"idle","controlMode":"transcript"}]}`),
		},
	}}
	handler := New(Options{Token: "secret", Registry: backend, Automation: db, Federation: fed})
	t.Cleanup(handler.Close)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := dial(t, "ws"+strings.TrimPrefix(server.URL, "http"))
	t.Cleanup(func() { _ = c.Close() })

	send(t, c, map[string]any{"t": "list_agents", "corrId": "agents"})
	got := recv(t, c)
	agents := got["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents = %#v", got)
	}
	remote := agents[1].(map[string]any)
	remoteID := remoteSessionID(leafID, "leaf-agent")
	if remote["id"] != remoteID || remote["hostId"] != leafID || remote["hostName"] != "leaf" {
		t.Fatalf("descendant summary = %#v", remote)
	}

	send(t, c, map[string]any{"t": "spawn_agent", "spec": map[string]any{
		"hostId": leafID, "adapter": "acp", "workspace": map[string]any{"kind": "existing", "cwd": "/repo"},
	}, "corrId": "spawn"})
	got = recv(t, c)
	if got["t"] != "ack" || got["agentId"] != remoteID || got["hostId"] != leafID {
		t.Fatalf("descendant spawn ack = %#v", got)
	}
	fed.mu.Lock()
	if len(fed.calls) != 1 || fed.calls[0].hostID != leafID {
		calls := append([]topologyFederationCall(nil), fed.calls...)
		fed.mu.Unlock()
		t.Fatalf("descendant spawn route = %#v", calls)
	}
	spawnPayload := append(json.RawMessage(nil), fed.calls[0].payload...)
	fed.mu.Unlock()
	if strings.Contains(string(spawnPayload), leafID) || strings.Contains(string(spawnPayload), middleID) {
		t.Fatalf("route metadata leaked into forwarded command: %s", spawnPayload)
	}

	send(t, c, map[string]any{"t": "subscribe", "agentId": remoteID, "channels": []string{"transcript"}, "corrId": "sub"})
	got = recv(t, c)
	if got["t"] != "snapshot" || got["agentId"] != remoteID || got["hostId"] != leafID {
		t.Fatalf("descendant snapshot = %#v", got)
	}
	got = recv(t, c)
	if got["t"] != "ack" || got["agentId"] != remoteID || got["corrId"] != "sub" {
		t.Fatalf("descendant subscribe ack = %#v", got)
	}

	fed.emit(leafID, json.RawMessage(`{"t":"transcript","sessionId":"leaf-agent","seq":1,"role":"assistant","text":"through two hops"}`))
	got = recv(t, c)
	if got["t"] != "transcript" || got["sessionId"] != remoteID || got["agentId"] != remoteID || got["hostId"] != leafID {
		t.Fatalf("descendant event = %#v", got)
	}
}
