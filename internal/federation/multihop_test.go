package federation

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/store"
)

// routedTestLocal models the tiny but important piece of an intermediate
// daemon's loopback WebSocket: a command bearing hostId is forwarded to its
// own child federation service after that routing field is consumed.  The
// production wsserver does this for all browser commands; keeping this test
// local focused makes the three real tunnel hops inexpensive to exercise.
type routedTestLocal struct {
	child  *Service
	events chan json.RawMessage
}

func (l *routedTestLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"t":"agents","agents":[]}`), nil
}

func (l *routedTestLocal) Events() <-chan json.RawMessage { return l.events }

func (l *routedTestLocal) Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	var hostID string
	if raw := envelope["hostId"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &hostID); err != nil {
			return nil, err
		}
		delete(envelope, "hostId")
	}
	if hostID == "" || l.child == nil {
		return nil, &routeTestError{message: "intermediate did not receive a descendant route"}
	}
	clean, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return l.child.Call(ctx, hostID, clean)
}

type routeTestError struct{ message string }

func (e *routeTestError) Error() string { return e.message }

type leafTestLocal struct {
	mu      sync.Mutex
	payload json.RawMessage
}

func (l *leafTestLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"t":"agents","agents":[{"id":"leaf-agent","name":"Leaf agent"}]}`), nil
}

func (l *leafTestLocal) Execute(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
	l.mu.Lock()
	l.payload = append(json.RawMessage(nil), payload...)
	l.mu.Unlock()
	return json.RawMessage(`{"t":"ack","owner":"leaf"}`), nil
}

func runTestSlave(t *testing.T, service *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.RunSlave(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("slave stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("slave did not stop")
		}
	})
}

func acceptTestRegistration(t *testing.T, service *Service, center *notifications.Center) string {
	t.Helper()
	var pending notifications.Notification
	eventuallyTest(t, "registration request", func() bool {
		items := center.List()
		if len(items) == 0 {
			return false
		}
		pending = items[0]
		return strings.HasPrefix(pending.ID, "federation-registration-")
	})
	if _, handled, err := service.HandleNotificationAction(context.Background(), pending.ID, "accept"); err != nil || !handled {
		t.Fatalf("accept registration: handled=%v err=%v", handled, err)
	}
	return strings.TrimPrefix(pending.ID, "federation-registration-")
}

func eventuallyTest(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// This is deliberately a real root -> intermediate -> leaf tunnel chain, not
// a mocked Host list.  It verifies the two promises that make multilayer
// federation useful: the root learns a flattened leaf entry with a route, and
// a command sent to that entry reaches the leaf through the intermediate.
func TestThreeNodeFederationDiscoversAndRoutesToLeaf(t *testing.T) {
	rootStore := openStore(t)
	rootCenter := notifications.New()
	root, err := New(Options{Store: rootStore, Notifications: rootCenter, Name: "root", PollInterval: 10 * time.Millisecond, BuildVersion: "root-version"})
	if err != nil {
		t.Fatal(err)
	}
	rootServer := httptest.NewServer(root)
	t.Cleanup(rootServer.Close)

	middleStore := openStore(t)
	middleCenter := notifications.New()
	middleRelay := &routedTestLocal{events: make(chan json.RawMessage, 1)}
	middle, err := New(Options{Store: middleStore, Notifications: middleCenter, MasterURL: rootServer.URL, Name: "middle", Local: middleRelay, PollInterval: 10 * time.Millisecond, BuildVersion: "middle-version"})
	if err != nil {
		t.Fatal(err)
	}
	middleServer := httptest.NewServer(middle)
	t.Cleanup(middleServer.Close)
	runTestSlave(t, middle)
	middleID := acceptTestRegistration(t, root, rootCenter)
	eventuallyTest(t, "middle tunnel", func() bool {
		peer, _ := rootStore.FederationSlave(middleID)
		return peer != nil && peer.Status == "connected"
	})

	leafStore := openStore(t)
	leafLocal := &leafTestLocal{}
	leaf, err := New(Options{Store: leafStore, MasterURL: middleServer.URL, Name: "leaf", Local: leafLocal, PollInterval: 10 * time.Millisecond, BuildVersion: "leaf-version"})
	if err != nil {
		t.Fatal(err)
	}
	// The intermediate's browser handler calls its own federation service;
	// that service owns the direct tunnel to leaf.  It does not call the leaf
	// service object (which is the outbound side of that tunnel).
	middleRelay.child = middle
	runTestSlave(t, leaf)
	leafID := acceptTestRegistration(t, middle, middleCenter)
	// In a real intermediate, wsserver turns the child's host-change event
	// into an event on its private loopback socket.  The relay above models
	// command forwarding rather than a full browser server, so provide that
	// same edge explicitly and verify the upstream tunnel republishes topology.
	middleRelay.events <- json.RawMessage(`{"t":"federation_hosts_changed"}`)

	var descendant Host
	eventuallyTest(t, "connected leaf topology at root", func() bool {
		for _, host := range root.Hosts() {
			if host.Name == "leaf" && host.Depth == 2 && host.Status == "connected" && host.BuildVersion == "leaf-version" {
				descendant = host
				return true
			}
		}
		return false
	})
	if descendant.ParentID != middleID || len(descendant.Route) != 2 || descendant.Route[0] != middleID || descendant.NodeID != leafID || descendant.BuildVersion != "leaf-version" {
		t.Fatalf("leaf topology = %#v", descendant)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := root.Call(ctx, descendant.ID, json.RawMessage(`{"t":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(reply) != `{"t":"ack","owner":"leaf"}` {
		t.Fatalf("leaf reply = %s", reply)
	}
	leafLocal.mu.Lock()
	received := append(json.RawMessage(nil), leafLocal.payload...)
	leafLocal.mu.Unlock()
	if string(received) != `{"t":"ping"}` {
		t.Fatalf("leaf received routing metadata: %s", received)
	}
}

func TestTopologyCycleGuardRejectsOurUpstreamIdentityAndExcessDepth(t *testing.T) {
	s := openStore(t)
	if err := s.SaveFederationMaster(store.FederationMaster{URL: "http://upstream.example", HostID: "this-node", Credential: "credential"}); err != nil {
		t.Fatal(err)
	}
	service, err := New(Options{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	if !service.topologyCycles([]Host{{ID: "route-to-self", NodeID: "this-node", Depth: 2}}) {
		t.Fatal("topology containing this node's upstream identity was accepted")
	}
	if !service.topologyCycles([]Host{{ID: "too-deep", NodeID: "other", Depth: maxFederationDepth + 1}}) {
		t.Fatal("topology over the maximum depth was accepted")
	}
	if service.topologyCycles([]Host{{ID: "normal", NodeID: "other", Depth: 2}}) {
		t.Fatal("ordinary descendant topology was rejected")
	}
}
