package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
)

func TestPolicyLevels(t *testing.T) {
	policy := Policy{
		{From: "laptop-*", Level: LevelOperate},
		{From: AncestorsSubject, Level: LevelView},
		{From: "*", Level: LevelNone},
	}
	cases := []struct {
		origin   string
		ancestor bool
		want     Level
	}{
		{"laptop-1a2b3c", false, LevelOperate},
		{"laptop-1a2b3c", true, LevelOperate},
		{"root-1a2b3c", true, LevelView},
		{"", true, LevelView},
		{"builder-1a2b3c", false, LevelNone},
	}
	for _, c := range cases {
		if got := policy.LevelFor(c.origin, c.ancestor); got != c.want {
			t.Errorf("LevelFor(%q, %v) = %s, want %s", c.origin, c.ancestor, got, c.want)
		}
	}
	var defaults Policy
	if defaults.LevelFor("root", true) != LevelAdmin || defaults.LevelFor("sibling", false) != LevelNone {
		t.Fatal("default policy must be ancestors: admin, others: none")
	}
	if err := (Policy{{From: "[", Level: LevelView}}).Validate(); err == nil {
		t.Fatal("invalid glob accepted")
	}
	if CommandLevel("subscribe") != LevelView || CommandLevel("prompt") != LevelOperate || CommandLevel("spawn_agent") != LevelAdmin || CommandLevel("something_new") != LevelAdmin {
		t.Fatal("command classification")
	}
}

// routingLocal stands in for a daemon's private browser socket: a command
// naming a hostId is routed through that daemon's federation service (the
// production wsserver does the same), anything else runs "here".
type routingLocal struct {
	owner    string
	snapshot string
	service  func() *Service
	events   chan json.RawMessage
	mu       sync.Mutex
	received []string
}

func newRoutingLocal(owner, snapshot string, service func() *Service) *routingLocal {
	return &routingLocal{owner: owner, snapshot: snapshot, service: service, events: make(chan json.RawMessage, 16)}
}

func (l *routingLocal) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(l.snapshot), nil
}

func (l *routingLocal) Events() <-chan json.RawMessage { return l.events }

func (l *routingLocal) Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	var hostID, origin, commandType string
	_ = json.Unmarshal(envelope["hostId"], &hostID)
	_ = json.Unmarshal(envelope[OriginField], &origin)
	_ = json.Unmarshal(envelope["t"], &commandType)
	delete(envelope, "hostId")
	delete(envelope, OriginField)
	if hostID != "" {
		clean, _ := json.Marshal(envelope)
		return l.service().Call(WithOrigin(ctx, origin), hostID, clean)
	}
	l.mu.Lock()
	l.received = append(l.received, commandType)
	l.mu.Unlock()
	return json.Marshal(map[string]string{"t": "ack", "owner": l.owner, "type": commandType})
}

type upstreamFleet struct {
	root, alpha, beta       *Service
	rootLinks               chan *routingLocal
	alphaID, betaID, rootID string
	alphaLocal, betaLocal   *routingLocal
	rootCenter              *notifications.Center
}

// startUpstreamFleet builds root with two children, alpha and beta, over real
// tunnels. Only root opens per-child bridges (NewLocal); each is reported on
// rootLinks so a test can inject the events that bridge would hear.
func startUpstreamFleet(t *testing.T, rootPolicy, alphaPolicy, betaPolicy Policy) *upstreamFleet {
	t.Helper()
	f := &upstreamFleet{rootLinks: make(chan *routingLocal, 8), rootCenter: notifications.New()}
	var err error
	f.root, err = New(Options{
		Store: openStore(t), Notifications: f.rootCenter, Name: "root", PollInterval: 10 * time.Millisecond, Policy: rootPolicy,
		NewLocal: func() (Local, error) {
			link := newRoutingLocal("root", `{"t":"agents","agents":[{"id":"root-agent"}]}`, func() *Service { return f.root })
			f.rootLinks <- link
			return link, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.root)
	t.Cleanup(server.Close)
	f.rootID = f.root.selfID()

	f.alphaLocal = newRoutingLocal("alpha", `{"t":"agents","agents":[{"id":"alpha-agent"}]}`, func() *Service { return f.alpha })
	f.alpha, err = New(Options{Store: openStore(t), ParentURL: server.URL, Name: "alpha", Local: f.alphaLocal, PollInterval: 10 * time.Millisecond, Policy: alphaPolicy})
	if err != nil {
		t.Fatal(err)
	}
	runTestChild(t, f.alpha)
	f.alphaID = acceptTestRegistration(t, f.root, f.rootCenter)

	f.betaLocal = newRoutingLocal("beta", `{"t":"agents","agents":[{"id":"beta-agent"}]}`, func() *Service { return f.beta })
	f.beta, err = New(Options{Store: openStore(t), ParentURL: server.URL, Name: "beta", Local: f.betaLocal, PollInterval: 10 * time.Millisecond, Policy: betaPolicy})
	if err != nil {
		t.Fatal(err)
	}
	runTestChild(t, f.beta)
	eventuallyTest(t, "beta registration", func() bool {
		for _, n := range f.rootCenter.List() {
			if strings.HasPrefix(n.ID, "federation-registration-beta") {
				return true
			}
		}
		return false
	})
	for _, n := range f.rootCenter.List() {
		if strings.HasPrefix(n.ID, "federation-registration-beta") {
			if _, _, err := f.root.HandleNotificationAction(context.Background(), n.ID, "accept"); err != nil {
				t.Fatal(err)
			}
			f.betaID = strings.TrimPrefix(n.ID, "federation-registration-")
		}
	}
	eventuallyTest(t, "both children connected", func() bool {
		connected := 0
		for _, h := range f.root.Hosts() {
			if h.Status == "connected" {
				connected++
			}
		}
		return connected == 2
	})
	return f
}

func upstreamHost(s *Service, name string) (Host, bool) {
	for _, h := range s.Hosts() {
		if h.Upstream && h.Name == name {
			return h, true
		}
	}
	return Host{}, false
}

func callAck(t *testing.T, s *Service, hostID, command string) (map[string]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := s.Call(ctx, hostID, json.RawMessage(`{"t":"`+command+`"}`))
	if err != nil {
		return nil, err
	}
	var ack map[string]string
	if err := json.Unmarshal(reply, &ack); err != nil {
		t.Fatalf("reply %s: %v", reply, err)
	}
	return ack, nil
}

func TestChildSeesNothingUpstreamByDefault(t *testing.T) {
	f := startUpstreamFleet(t, nil, nil, nil)
	// Give root time to send its (empty) view.
	time.Sleep(200 * time.Millisecond)
	for _, s := range []*Service{f.alpha, f.beta} {
		for _, h := range s.Hosts() {
			if h.Upstream {
				t.Fatalf("default policy exposed upstream host %#v", h)
			}
		}
		if s.LocalHost().ParentID != "" {
			t.Fatal("local host has a visible parent under the default policy")
		}
	}
	// Downward control is unchanged.
	ack, err := callAck(t, f.root, f.alphaID, "spawn_agent")
	if err != nil || ack["owner"] != "alpha" {
		t.Fatalf("root -> alpha spawn: %v %v", ack, err)
	}
}

func TestChildControlsParentAndSiblingWithinPolicy(t *testing.T) {
	f := startUpstreamFleet(t,
		Policy{{From: "alpha-*", Level: LevelOperate}},
		nil,
		Policy{{From: "alpha-*", Level: LevelView}},
	)
	var rootHost, betaHost Host
	eventuallyTest(t, "alpha's view of root and beta", func() bool {
		var okRoot, okBeta bool
		rootHost, okRoot = upstreamHost(f.alpha, "root")
		betaHost, okBeta = upstreamHost(f.alpha, "beta")
		return okRoot && okBeta && len(rootHost.Snapshot) != 0 && len(betaHost.Snapshot) != 0
	})
	if rootHost.Access != "operate" || betaHost.Access != "view" {
		t.Fatalf("access root=%q beta=%q", rootHost.Access, betaHost.Access)
	}
	if rootHost.NodeID != f.rootID || betaHost.ParentID != rootHost.ID || f.alpha.LocalHost().ParentID != rootHost.ID || f.alpha.LocalHost().Dialer != DialerChild || betaHost.Dialer != DialerChild {
		t.Fatalf("topology: root=%#v beta=%#v local parent=%q", rootHost, betaHost, f.alpha.LocalHost().ParentID)
	}
	if !strings.Contains(string(rootHost.Snapshot), "root-agent") || !strings.Contains(string(betaHost.Snapshot), "beta-agent") {
		t.Fatalf("snapshots root=%s beta=%s", rootHost.Snapshot, betaHost.Snapshot)
	}
	for _, h := range f.alpha.Hosts() {
		if h.Name == "alpha" {
			t.Fatalf("alpha's own subtree echoed back: %#v", h)
		}
	}
	// Root never re-learns itself through alpha's topology.
	if hosts := f.root.Hosts(); len(hosts) != 2 {
		t.Fatalf("root hosts = %#v", hosts)
	}

	if ack, err := callAck(t, f.alpha, rootHost.ID, "prompt"); err != nil || ack["owner"] != "root" {
		t.Fatalf("alpha -> root prompt: %v %v", ack, err)
	}
	if _, err := callAck(t, f.alpha, rootHost.ID, "spawn_agent"); err == nil || !strings.Contains(err.Error(), "needs admin access, has operate") {
		t.Fatalf("alpha -> root spawn should be denied, got %v", err)
	}
	if ack, err := callAck(t, f.alpha, betaHost.ID, "subscribe"); err != nil || ack["owner"] != "beta" {
		t.Fatalf("alpha -> beta subscribe: %v %v", ack, err)
	}
	if _, err := callAck(t, f.alpha, betaHost.ID, "prompt"); err == nil || !strings.Contains(err.Error(), "needs operate access, has view") {
		t.Fatalf("alpha -> beta prompt should be denied by beta, got %v", err)
	}
	f.betaLocal.mu.Lock()
	received := strings.Join(f.betaLocal.received, ",")
	f.betaLocal.mu.Unlock()
	if received != "subscribe" {
		t.Fatalf("beta executed %q", received)
	}

	// Beta's policy grants alpha nothing and root grants beta nothing, so
	// beta's own view stays empty.
	for _, h := range f.beta.Hosts() {
		if h.Upstream {
			t.Fatalf("beta sees %#v", h)
		}
	}
	// Root still administers both children.
	if ack, err := callAck(t, f.root, f.betaID, "spawn_agent"); err != nil || ack["owner"] != "beta" {
		t.Fatalf("root -> beta spawn: %v %v", ack, err)
	}
}

func TestParentEventsReachChildUnderItsAddress(t *testing.T) {
	f := startUpstreamFleet(t, Policy{{From: "alpha-*", Level: LevelView}}, nil, nil)
	var rootHost Host
	eventuallyTest(t, "alpha's view of root", func() bool {
		var ok bool
		rootHost, ok = upstreamHost(f.alpha, "root")
		return ok
	})
	got := make(chan string, 8)
	stop := f.alpha.Subscribe(func(hostID string, payload json.RawMessage) {
		if strings.Contains(string(payload), "transcript") {
			got <- hostID
		}
	})
	defer stop()
	// Find the bridge root opened for alpha (beta's has none to find: root
	// opens one per capable child, so take the first that relays).
	links := []*routingLocal{}
	for len(f.rootLinks) != 0 {
		links = append(links, <-f.rootLinks)
	}
	for _, link := range links {
		link.events <- json.RawMessage(`{"t":"transcript","sessionId":"root-agent"}`)
	}
	select {
	case hostID := <-got:
		if hostID != rootHost.ID {
			t.Fatalf("event host = %q, want %q", hostID, rootHost.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("root event did not reach alpha")
	}
}

func TestChildCannotClaimAnOriginOutsideItsSubtree(t *testing.T) {
	f := startUpstreamFleet(t, Policy{{From: "beta-*", Level: LevelAdmin}, {From: "alpha-*", Level: LevelView}}, nil, nil)
	if got := f.root.childOrigin(f.alphaID, f.betaID); got != f.alphaID {
		t.Fatalf("spoofed origin accepted: %q", got)
	}
	var rootHost Host
	eventuallyTest(t, "alpha's view of root", func() bool {
		var ok bool
		rootHost, ok = upstreamHost(f.alpha, "root")
		return ok
	})
	ctx, cancel := context.WithTimeout(WithOrigin(context.Background(), f.betaID), 5*time.Second)
	defer cancel()
	if _, err := f.alpha.Call(ctx, rootHost.ID, json.RawMessage(`{"t":"spawn_agent"}`)); err == nil || !strings.Contains(err.Error(), f.alphaID) {
		t.Fatalf("alpha impersonating beta should be denied as alpha, got %v", err)
	}
}

func TestMessageLevelSitsBetweenViewAndOperate(t *testing.T) {
	if !(LevelView < LevelMessage && LevelMessage < LevelOperate) {
		t.Fatal("message must rank between view and operate")
	}
	if LevelMessage.String() != "message" {
		t.Fatalf("String = %q", LevelMessage.String())
	}
	if got, err := ParseLevel("Message"); err != nil || got != LevelMessage {
		t.Fatalf("ParseLevel = %v, %v", got, err)
	}
	for _, command := range []string{"agent_directory", "agent_message_deliver", "agent_link_request"} {
		if CommandLevel(command) != LevelMessage {
			t.Errorf("%s needs %s, want message", command, CommandLevel(command))
		}
	}
	for command, want := range map[string]Level{
		"list_agent_links": LevelView, "get_messaging_state": LevelView,
		"set_agent_link": LevelOperate, "delete_agent_link": LevelOperate,
		"set_agent_listed": LevelOperate, "set_messaging_paused": LevelOperate,
	} {
		if got := CommandLevel(command); got != want {
			t.Errorf("CommandLevel(%s) = %s, want %s", command, got, want)
		}
	}
}

// The executing host applies its policy to the command's origin: a sibling
// granted `message` may deliver messages and ask for links but nothing that
// edits links or drives agents.
func TestAuthorizeEnforcesMessageLevel(t *testing.T) {
	svc, err := New(Options{Store: openStore(t), Policy: Policy{{From: "sibling-*", Level: LevelMessage}, {From: "viewer-*", Level: LevelView}}})
	if err != nil {
		t.Fatal(err)
	}
	command := func(typ string) json.RawMessage { return json.RawMessage(`{"t":"` + typ + `"}`) }
	for _, typ := range []string{"agent_directory", "agent_message_deliver", "agent_link_request", "list_agents", "list_agent_links"} {
		if err := svc.authorize("sibling-1", false, command(typ)); err != nil {
			t.Errorf("message level must allow %s: %v", typ, err)
		}
	}
	for _, typ := range []string{"set_agent_link", "delete_agent_link", "set_agent_listed", "set_messaging_paused", "prompt", "spawn_agent"} {
		err := svc.authorize("sibling-1", false, command(typ))
		if err == nil || !IsAccessDenied(err) {
			t.Errorf("message level must deny %s as an access denial, got %v", typ, err)
		}
	}
	for _, typ := range []string{"agent_directory", "agent_message_deliver", "agent_link_request"} {
		if err := svc.authorize("viewer-1", false, command(typ)); !IsAccessDenied(err) {
			t.Errorf("view level must deny %s, got %v", typ, err)
		}
		if err := svc.authorize("stranger", false, command(typ)); !IsAccessDenied(err) {
			t.Errorf("no access must deny %s, got %v", typ, err)
		}
		// Ancestors keep their default admin access, which includes message.
		if err := svc.authorize("root", true, command(typ)); err != nil {
			t.Errorf("ancestors must keep access to %s: %v", typ, err)
		}
	}
	if IsAccessDenied(errors.New("federation: route to \"x\" is offline")) {
		t.Fatal("a transport failure is not an access denial")
	}
}
