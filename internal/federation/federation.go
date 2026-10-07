// Package federation implements Tandem's routed federation tree protocol.
// Hosts form a rooted tree: each has at most one parent and any number of
// children. Either end of a link may open its connection -- a child dials its
// parent (--parent), or a parent dials and adopts a child it can reach but
// that cannot reach it, such as a Tandem in a container. Once connected, both
// kinds of link speak the same protocol.
package federation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// ProtocolVersion is this daemon's federation wire version. The transport
// carries opaque browser-protocol envelopes, so adding a command or a field
// costs nothing across versions and must NOT bump this. Bump it only for a
// genuinely breaking change: new or reinterpreted tunnel/register/heartbeat
// framing, changed credential handling, or an existing field whose meaning
// changes. A peer reporting a different version still connects -- see
// checkProtocol -- because most commands remain mutually intelligible.
//
// Version 3 added upstream control: a parent offers each capable child a
// "view" of the rest of the fleet and executes commands the child sends up,
// both subject to the controlled host's access Policy.
const ProtocolVersion = 3

// upstreamCapability is advertised in a child's hello when it understands
// "view" messages and routes commands upward. A parent opens the per-child
// loopback that serves them only for children that advertise it.
const upstreamCapability = "upstream"

// upHop is the route step meaning "this daemon's parent". Real host IDs never
// contain it (see ValidHostID), so a route cannot confuse the two.
const upHop = "^"

// maxFederationDepth is a defensive bound for delegated trust and prevents a
// malformed peer from advertising an unbounded/cyclic topology.
const maxFederationDepth = 8

const (
	RegisterPath  = "/internal/federation/register"
	StatusPath    = "/internal/federation/registration"
	HeartbeatPath = "/internal/federation/heartbeat"
	TunnelPath    = "/internal/federation/tunnel"
	// AdoptPath is served by a child: a parent dials it to adopt the child.
	AdoptPath = "/internal/federation/adopt"
)

// Headers an adopting parent sends with its dial. The credential itself
// travels in the ordinary Authorization header.
const (
	joinTokenHeader  = "X-Tandem-Join-Token"
	parentIDHeader   = "X-Tandem-Federation-Parent"
	parentNameHeader = "X-Tandem-Federation-Name"
)

// Host is the parent-safe view of a registered agent host. Snapshot is the
// host's latest opaque catalog/state envelope and excludes its credential.
type Host struct {
	ID    string `json:"id"`
	Local bool   `json:"local,omitempty"`
	// NodeID is the identity assigned by the node's direct parent. ID is the
	// route-scoped address used to reach it from this daemon; they differ only
	// for descendants.
	NodeID   string          `json:"nodeId,omitempty"`
	ParentID string          `json:"parentId,omitempty"`
	Route    []string        `json:"route,omitempty"`
	Depth    int             `json:"depth,omitempty"`
	Name     string          `json:"name,omitempty"`
	Endpoint string          `json:"endpoint,omitempty"`
	Status   string          `json:"status"`
	LastSeen int64           `json:"lastSeenAt,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	// ProtocolVersion/BuildVersion are what the host reported when it last
	// connected; both are absent for a host predating version reporting.
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
	BuildVersion    string `json:"buildVersion,omitempty"`
	// Upstream marks a host reached through this daemon's parent rather than
	// one of its own descendants: the parent itself, its other branches, and
	// anything above it. Such hosts are never advertised further upstream.
	Upstream bool `json:"upstream,omitempty"`
	// Access is what this daemon may do on the host under that host's
	// policy: "none", "view", "operate" or "admin".
	Access string `json:"access,omitempty"`
	// Dialer says which end of the link to ParentID opened the TCP
	// connection: DialerChild or DialerParent (adoption). Empty when unknown,
	// e.g. reported by a daemon predating it.
	Dialer string `json:"dialer,omitempty"`
	// Rules is the host's own access policy, carried so each relay can
	// decide which hosts (and snapshots) a requester may see.
	Rules  Policy `json:"rules,omitempty"`
	nextID string
}

// Link dialers reported in Host.Dialer.
const (
	DialerChild  = "child"
	DialerParent = "parent"
)

// parentDialer reports which end dials this daemon's link to its parent, or
// "" without one.
func (s *Service) parentDialer() string {
	m, err := s.store.FederationParent()
	if err != nil || m == nil {
		return ""
	}
	if m.Adopted {
		return DialerParent
	}
	return DialerChild
}

// LocalHost returns this daemon's browser-safe federation identity. It is
// intentionally separate from Hosts, whose contents are advertised upstream
// as this daemon's descendants.
func (s *Service) LocalHost() Host {
	h := Host{
		ID:              "local",
		Local:           true,
		NodeID:          s.selfID(),
		Name:            "This host",
		Status:          "connected",
		ProtocolVersion: ProtocolVersion,
		BuildVersion:    s.buildVersion,
		Access:          LevelAdmin.String(),
	}
	dialer := s.parentDialer()
	s.mu.Lock()
	if s.upstreamView != nil {
		h.ParentID = routedHostID([]string{upHop})
		h.Dialer = dialer
	}
	s.mu.Unlock()
	return h
}

// Local supplies a child's local operations. Commands and snapshots are
// opaque JSON intentionally: this keeps the federation transport in lockstep
// with the browser protocol without duplicating every agent/browser command.
type Local interface {
	Snapshot(context.Context) (json.RawMessage, error)
	Execute(context.Context, json.RawMessage) (json.RawMessage, error)
}

// EventSource is optionally implemented by Local to relay unsolicited
// browser-protocol envelopes such as transcript, PTY, approvals, browser
// state and screencast frames with no polling delay.
type EventSource interface{ Events() <-chan json.RawMessage }

type connectionResetter interface{ Reset() }

type Options struct {
	Store         *store.Store
	Notifications *notifications.Center
	ParentURL     string
	Name          string
	Endpoint      string
	Local         Local
	HTTPClient    *http.Client
	PollInterval  time.Duration
	// ProxyURL routes this daemon's outbound dials to its parent through a
	// proxy, e.g. "socks5://127.0.0.1:1080". Dials to adopted children use
	// their own ChildLink.ProxyURL, and the loopback transport never leaves
	// the process. SOCKS sits below TLS, so an https/wss parent still terminates
	// its own TLS end to end.
	ProxyURL string
	// BuildVersion is this daemon's release, reported to the peer alongside
	// ProtocolVersion so a skew notification can name something a human can
	// act on ("update builder to v0.5.0") rather than a bare number.
	BuildVersion string
	// Policy decides what other hosts may do here. Nil keeps the original
	// contract: ancestors administer this host, nobody else reaches it.
	Policy Policy
	// NewLocal opens a private command bridge for one child's upstream
	// commands, so each child's subscriptions and events stay separate. Nil
	// disables upstream control on this daemon.
	NewLocal func() (Local, error)
	// Children are hosts this daemon dials and adopts; see RunChildLinks.
	Children []ChildLink
	// JoinToken, when set, lets a parent that presents it adopt this daemon
	// without an operator accepting the request here.
	JoinToken string
}

// ChildLink is a child this daemon dials rather than waiting for the child to
// dial it: the way to reach a host that cannot reach its parent.
type ChildLink struct {
	URL string
	// Name labels the child in the UI; the child's own report is the fallback.
	Name string
	// JoinToken is the child's join token. Without it the child's operator
	// must accept the adoption there.
	JoinToken string
	// ProxyURL routes dials to this child through a proxy, as for a parent.
	ProxyURL string
	dialer   *websocket.Dialer
}

type Service struct {
	store         *store.Store
	notifications *notifications.Center
	parentURL     string
	name          string
	endpoint      string
	local         Local
	client        *http.Client
	dialer        *websocket.Dialer
	poll          time.Duration
	buildVersion  string
	policy        Policy
	newLocal      func() (Local, error)

	mu         sync.Mutex
	snapshots  map[string]json.RawMessage
	topologies map[string][]Host
	queues     map[string][]command
	waiters    map[string]chan result
	tunnels    map[string]*tunnel
	upstream   *tunnel
	subs       map[int]func(string, json.RawMessage)
	nextSub    int
	// identity caches selfID's answer for a daemon without a parent.
	identity string
	// ancestors is this daemon's chain of parents, nearest first, as the
	// parent reported it in its welcome. It decides the "ancestors" subject.
	ancestors []string
	// upstreamView is the latest fleet view the parent sent, in the parent's
	// own addressing. Nil means the parent offers none (it is older, or has
	// not sent one on the current tunnel).
	upstreamView []Host
	// rules holds each direct child's advertised policy, from its hello.
	rules map[string]Policy
	// children holds the per-child upstream-control bridge for each child
	// that advertised upstreamCapability.
	children map[string]*childLink
	// localSnapshot is this daemon's own agent list, as offered to children.
	localSnapshot json.RawMessage

	childLinks []ChildLink
	joinToken  string
	// adoptMu serializes adopted parent sessions: a parent that reconnects
	// replaces its old session only once that session has fully wound down.
	adoptMu sync.Mutex
	// identityMu serializes generating this daemon's durable identity, so two
	// first callers cannot each mint (and announce) a different one.
	identityMu sync.Mutex
	// pendingAdoptions are adoption requests awaiting this operator's answer,
	// keyed by adoptionKey. rejectedAdoptions are refused until restart.
	pendingAdoptions  map[string]pendingAdoption
	rejectedAdoptions map[string]bool
}

// pendingAdoption is a parent asking to adopt this daemon without a join token.
type pendingAdoption struct {
	credential string
	parentID   string
	name       string
	remote     string
}

// childLink serves one child's upstream commands through its own loopback,
// so that child's subscriptions (and the events they produce) are its own.
type childLink struct {
	local  Local
	tunnel *tunnel
	cancel context.CancelFunc
	// viewMu serializes building and sending this child's view, so a view
	// computed earlier cannot overwrite a newer one on the wire.
	viewMu sync.Mutex
}

func (l *childLink) close() {
	l.cancel()
	if closer, ok := l.local.(io.Closer); ok {
		_ = closer.Close()
	}
}

type command struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
	Origin  string          `json:"origin,omitempty"`
}
type result struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}
type registerRequest struct {
	HostID          string `json:"hostId"`
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint,omitempty"`
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
	BuildVersion    string `json:"buildVersion,omitempty"`
	// PreviousHostID names the record this host is replacing when it changes
	// its own ID. The request carries that record's credential in the
	// ordinary Authorization header, which is what proves the two IDs belong
	// to the same host; see rotateIdentity.
	PreviousHostID string `json:"previousHostId,omitempty"`
}
type registerResponse struct {
	Status     string `json:"status"`
	Credential string `json:"credential,omitempty"`
	Error      string `json:"error,omitempty"`
	// The parent's own versions, so a host can report skew locally too.
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
	BuildVersion    string `json:"buildVersion,omitempty"`
}
type heartbeat struct {
	HostID          string          `json:"hostId"`
	Snapshot        json.RawMessage `json:"snapshot,omitempty"`
	Results         []result        `json:"results,omitempty"`
	ProtocolVersion int             `json:"protocolVersion,omitempty"`
	BuildVersion    string          `json:"buildVersion,omitempty"`
}
type heartbeatResponse struct {
	Commands []command `json:"commands"`
	Error    string    `json:"error,omitempty"`
}
type tunnelMessage struct {
	T        string          `json:"t"`
	HostID   string          `json:"hostId,omitempty"`
	ID       string          `json:"id,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Error    string          `json:"error,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	Hosts    []Host          `json:"hosts,omitempty"`
	// Ancestors is a best-effort loop guard. Older peers ignore it.
	Ancestors []string `json:"ancestors,omitempty"`
	// Carried on "hello" (host to parent) and "welcome" (parent to host). The
	// hello is the authoritative report: a long-registered host reconnects
	// without ever registering again.
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
	BuildVersion    string `json:"buildVersion,omitempty"`
	// Origin is the host a "command" was issued by. The receiving daemon
	// checks it against what the sender could plausibly relay for, then its
	// policy decides whether to execute. Absent from older peers.
	Origin string `json:"origin,omitempty"`
	// Capabilities (on "hello") lists optional features the sender speaks.
	Capabilities []string `json:"capabilities,omitempty"`
	// Rules (on "hello") is the sender's own access policy.
	Rules Policy `json:"rules,omitempty"`
	// Name (on "hello") is the child's display name. A child that dials its
	// parent reports it when registering instead, so only an adopting parent
	// relies on this.
	Name string `json:"name,omitempty"`
}
type tunnel struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	// sent remembers the last state message of each kind, so an unchanged
	// snapshot, topology or view is not re-sent. Besides saving bandwidth,
	// this is what stops the parent's view and the child's topology from
	// echoing each other forever: each is re-sent on the other's arrival.
	sentMu sync.Mutex
	sent   map[string][]byte
}

// proxyDialers builds the outbound dial path for a child that reaches its
// parent through a proxy. gorilla's own Dialer.Proxy speaks HTTP CONNECT only,
// so the websocket tunnel needs an explicit net dialer; net/http understands
// socks5 proxy URLs directly. Hostnames are handed to the proxy unresolved
// (socks5h semantics), so a parent name that only resolves on the far side --
// the common case behind an ssh -D tunnel -- still connects.
func proxyDialers(raw string) (func(context.Context, string, string) (net.Conn, error), *http.Transport, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("federation: invalid proxy URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, nil, fmt.Errorf("federation: invalid proxy URL %q: missing host", raw)
	}
	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, nil, fmt.Errorf("federation: proxy %q: %w", raw, err)
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, nil, fmt.Errorf("federation: proxy scheme %q does not support cancellation", u.Scheme)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, nil, errors.New("federation: default HTTP transport is not proxyable")
	}
	transport = transport.Clone()
	transport.Proxy = http.ProxyURL(u)
	return ctxDialer.DialContext, transport, nil
}

func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("federation: store is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if err := opts.Policy.Validate(); err != nil {
		return nil, err
	}
	dialer := *websocket.DefaultDialer
	if proxyURL := strings.TrimSpace(opts.ProxyURL); proxyURL != "" {
		dial, transport, err := proxyDialers(proxyURL)
		if err != nil {
			return nil, err
		}
		dialer.NetDialContext = dial
		// NetDialContext already lands on the proxy; leaving the inherited
		// ProxyFromEnvironment in place would stack an HTTP CONNECT on top.
		dialer.Proxy = nil
		// An injected client is the caller's to configure; only a client we
		// construct ourselves gets the proxy transport.
		if opts.HTTPClient == nil {
			opts.HTTPClient = &http.Client{Timeout: 20 * time.Second, Transport: transport}
		}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	parent := strings.TrimRight(strings.TrimSpace(opts.ParentURL), "/")
	if parent != "" && !validLinkURL(parent) {
		return nil, fmt.Errorf("federation: invalid parent URL %q", opts.ParentURL)
	}
	childLinks := make([]ChildLink, 0, len(opts.Children))
	seen := map[string]bool{}
	for _, child := range opts.Children {
		child.URL = strings.TrimRight(strings.TrimSpace(child.URL), "/")
		if !validLinkURL(child.URL) {
			return nil, fmt.Errorf("federation: invalid child URL %q", child.URL)
		}
		if seen[child.URL] {
			return nil, fmt.Errorf("federation: child URL %q is listed twice", child.URL)
		}
		seen[child.URL] = true
		childDialer := *websocket.DefaultDialer
		if proxyURL := strings.TrimSpace(child.ProxyURL); proxyURL != "" {
			dial, _, err := proxyDialers(proxyURL)
			if err != nil {
				return nil, err
			}
			childDialer.NetDialContext, childDialer.Proxy = dial, nil
		}
		child.dialer = &childDialer
		childLinks = append(childLinks, child)
	}
	service := &Service{childLinks: childLinks, joinToken: strings.TrimSpace(opts.JoinToken), pendingAdoptions: map[string]pendingAdoption{}, rejectedAdoptions: map[string]bool{}, store: opts.Store, notifications: opts.Notifications, buildVersion: opts.BuildVersion, parentURL: parent, name: opts.Name, endpoint: opts.Endpoint, local: opts.Local, client: opts.HTTPClient, dialer: &dialer, poll: opts.PollInterval, snapshots: map[string]json.RawMessage{}, topologies: map[string][]Host{}, queues: map[string][]command{}, waiters: map[string]chan result{}, tunnels: map[string]*tunnel{}, subs: map[int]func(string, json.RawMessage){}, policy: opts.Policy, newLocal: opts.NewLocal, rules: map[string]Policy{}, children: map[string]*childLink{}}
	// A prior process may have stopped without updating its connected peers.
	// Until a new authenticated tunnel arrives, those durable records are
	// offline rather than connected.
	peers, err := opts.Store.FederationChildren()
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		if peer.Status == "pending" {
			service.notifyPending(peer)
		}
		if peer.Status == "connected" {
			peer.Status = "offline"
			if err := opts.Store.UpsertFederationChild(peer); err != nil {
				return nil, err
			}
		}
	}
	return service, nil
}

// trusted reports whether an approved child record may authenticate with its
// stored credential. Approval survives disconnects: the parent demotes a
// dropped tunnel to "offline", so requiring "accepted" here would make every
// reconnection after the first one fail as unauthorized.
func trusted(peer *store.FederationChild) bool {
	if peer == nil {
		return false
	}
	switch peer.Status {
	case "accepted", "connected", "offline":
		return true
	}
	return false
}

// HasParent reports whether this daemon has an upstream. It may still accept
// children: federation is a rooted tree, not a one-hop relationship.
func (s *Service) HasParent() bool {
	if s.parentURL != "" {
		return true
	}
	m, err := s.store.FederationParent()
	return err == nil && m != nil
}

// Hosts lists every host this daemon can address: its descendants, then
// (when its parent offers a view) the hosts reached through that parent.
func (s *Service) Hosts() []Host {
	self := s.selfID()
	peers, err := s.store.FederationChildren()
	if err != nil {
		return []Host{}
	}
	adopted, err := s.store.FederationAdoptedHostIDs()
	if err != nil {
		slog.Warn("federation adopted host lookup failed", "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Host, 0, len(peers))
	for _, p := range peers {
		h := Host{ID: p.ID, NodeID: p.ID, Route: []string{p.ID}, Depth: 1, Name: p.Name, Endpoint: p.Endpoint, Status: p.Status, LastSeen: p.LastSeenAt, ProtocolVersion: p.ProtocolVersion, BuildVersion: p.BuildVersion, nextID: p.ID}
		if b := s.snapshots[p.ID]; len(b) != 0 {
			h.Snapshot = append(json.RawMessage(nil), b...)
		}
		h.Rules = s.rules[p.ID]
		h.Dialer = DialerChild
		if adopted[p.ID] {
			h.Dialer = DialerParent
		}
		out = append(out, h)
		out = append(out, s.descendantsLocked(p.ID, h.ID, h.Route, 1, p.Status == "connected")...)
	}
	// This daemon is above every descendant, so each one's policy is read
	// with this daemon as an ancestor.
	for i := range out {
		out[i].Access = out[i].Rules.LevelFor(self, true).String()
	}
	return append(out, s.upstreamHostsLocked()...)
}

// descendantHosts is Hosts without the hosts reached through the parent:
// what this daemon advertises upstream as its subtree.
func (s *Service) descendantHosts() []Host {
	all := s.Hosts()
	out := all[:0]
	for _, h := range all {
		if !h.Upstream {
			out = append(out, h)
		}
	}
	return out
}

// upstreamHostsLocked translates the parent's view into this daemon's
// addressing: the parent is route [^], and a host the parent calls X is
// [^, X's route...]. Hosts deeper than maxFederationDepth are dropped.
func (s *Service) upstreamHostsLocked() []Host {
	if s.upstreamView == nil {
		return nil
	}
	parentID := routedHostID([]string{upHop})
	idMap := map[string]string{"": parentID}
	routes := map[string][]string{}
	for _, h := range s.upstreamView {
		route := []string{upHop}
		if !h.Local {
			route = append(route, h.Route...)
		}
		if len(route) > maxFederationDepth {
			continue
		}
		routes[h.ID] = route
		idMap[h.ID] = routedHostID(route)
	}
	connected := s.upstream != nil
	out := make([]Host, 0, len(s.upstreamView))
	for _, h := range s.upstreamView {
		route, ok := routes[h.ID]
		if !ok {
			continue
		}
		next := h.ID
		if h.Local {
			next = ""
		}
		parent := ""
		if h.Local {
			if h.ParentID != "" {
				parent = idMap[h.ParentID]
			}
		} else {
			parent = idMap[h.ParentID]
		}
		entry := h
		entry.ID, entry.ParentID, entry.Route, entry.Depth, entry.nextID = idMap[h.ID], parent, route, len(route), next
		entry.Local, entry.Upstream = false, true
		entry.Snapshot = append(json.RawMessage(nil), h.Snapshot...)
		if !connected {
			entry.Status = "offline"
		}
		out = append(out, entry)
	}
	return out
}

// descendantsLocked translates a child's public addresses into addresses
// meaningful to this daemon. ParentID makes the flattened result a tree.
func (s *Service) descendantsLocked(via, parentID string, prefix []string, depth int, reachable bool) []Host {
	children := s.topologies[via]
	if len(children) == 0 {
		return nil
	}
	idMap := make(map[string]string, len(children))
	for _, child := range children {
		if depth+1 > maxFederationDepth {
			continue
		}
		idMap[child.ID] = routedHostID(append(append([]string(nil), prefix...), child.ID))
	}
	out := make([]Host, 0, len(children))
	for _, child := range children {
		if depth+1 > maxFederationDepth || idMap[child.ID] == "" {
			continue
		}
		id := idMap[child.ID]
		parent := parentID
		if child.ParentID != "" && idMap[child.ParentID] != "" {
			parent = idMap[child.ParentID]
		}
		route := append(append([]string(nil), prefix...), child.ID)
		h := child
		h.ID, h.ParentID, h.Route, h.Depth, h.nextID = id, parent, route, depth+1, child.ID
		if !reachable {
			h.Status = "offline"
		}
		out = append(out, h)
	}
	return out
}

// HostNames returns operator-chosen display names keyed by host ID.
func (s *Service) HostNames() map[string]string {
	names, err := s.store.FederationHostNames()
	if err != nil {
		slog.Warn("federation host names lookup failed", "error", err)
		return nil
	}
	return names
}

// SetHostName sets (or, with "", clears) a host's display name as this
// daemon shows it. The name stays local; it is not advertised to peers.
func (s *Service) SetHostName(hostID, name string) error {
	name = strings.TrimSpace(name)
	if len(name) > 120 {
		return errors.New("host name is too long")
	}
	if err := s.store.SetFederationHostName(hostID, name); err != nil {
		return err
	}
	slog.Info("federation host display name changed", "host_id", hostID, "cleared", name == "")
	return nil
}

func routedHostID(route []string) string {
	b, _ := json.Marshal(route)
	return "route@" + base64.RawURLEncoding.EncodeToString(b)
}

// Call queues a protocol envelope for a connected host and waits for the
// reply sent in its next heartbeat. Cancellation does not cancel the remote
// operation; callers should use an explicit interrupt command for that.
func (s *Service) Call(ctx context.Context, hostID string, payload json.RawMessage) (json.RawMessage, error) {
	if hostID == "" || len(payload) == 0 {
		return nil, errors.New("federation: host ID and command are required")
	}
	// A direct host executes locally. A descendant is addressed to its direct
	// child at the next hop; that child's wsserver repeats the same operation.
	// Keeping this transformation here avoids making the browser understand a
	// transport route.
	target := Host{}
	for _, h := range s.Hosts() {
		if h.ID == hostID {
			target = h
			break
		}
	}
	if target.ID == "" {
		return nil, fmt.Errorf("federation: unknown host %q", hostID)
	}
	origin := OriginFrom(ctx)
	if origin == "" {
		origin = s.selfID()
	}
	if target.Upstream {
		return s.callUpstream(ctx, target, origin, payload)
	}
	directID := target.Route[0]
	if len(target.Route) > 1 {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, fmt.Errorf("federation: invalid routed command: %w", err)
		}
		next, _ := json.Marshal(target.nextID)
		envelope["hostId"] = next
		payload, _ = json.Marshal(envelope)
	}
	peer, err := s.store.FederationChild(directID)
	if err != nil {
		return nil, err
	}
	if peer == nil || peer.Status != "connected" {
		return nil, fmt.Errorf("federation: route to %q is offline at %q", hostID, directID)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	done := make(chan result, 1)
	slog.Info("sending federation command", "host_id", hostID, "command_id", id, "bytes", len(payload))
	s.mu.Lock()
	s.waiters[id] = done
	tunnel := s.tunnels[directID]
	if tunnel == nil {
		s.queues[directID] = append(s.queues[directID], command{ID: id, Payload: append(json.RawMessage(nil), payload...), Origin: origin})
	}
	s.mu.Unlock()
	if tunnel != nil {
		if err := tunnel.send(tunnelMessage{T: "command", ID: id, Payload: append(json.RawMessage(nil), payload...), Origin: origin}); err != nil {
			s.mu.Lock()
			delete(s.waiters, id)
			s.mu.Unlock()
			return nil, err
		}
	}
	defer func() { s.mu.Lock(); delete(s.waiters, id); s.mu.Unlock() }()
	select {
	case r := <-done:
		if r.Error != "" {
			slog.Warn("federation command failed remotely", "host_id", hostID, "command_id", id, "error", r.Error)
			return nil, errors.New(r.Error)
		}
		slog.Info("federation command completed", "host_id", hostID, "command_id", id, "bytes", len(r.Payload))
		return r.Payload, nil
	case <-ctx.Done():
		slog.Warn("federation command timed out", "host_id", hostID, "command_id", id, "error", ctx.Err())
		return nil, ctx.Err()
	}
}

// callUpstream sends a command to a host reached through this daemon's
// parent. The parent's address for the target travels as the envelope's
// hostId (absent for the parent itself), exactly as a browser on the parent
// would address it; the parent's own wsserver does any further routing.
func (s *Service) callUpstream(ctx context.Context, target Host, origin string, payload json.RawMessage) (json.RawMessage, error) {
	var next any
	if target.nextID != "" {
		next = target.nextID
	}
	payload, err := withEnvelopeField(payload, "hostId", next)
	if err != nil {
		return nil, fmt.Errorf("federation: invalid routed command: %w", err)
	}
	s.mu.Lock()
	t := s.upstream
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("federation: route to %q is offline: not connected to parent", target.ID)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	commandType, _ := envelopeRoute(payload)
	done := make(chan result, 1)
	s.mu.Lock()
	s.waiters[id] = done
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.waiters, id); s.mu.Unlock() }()
	slog.Info("sending federation command upstream", "host_id", target.ID, "host_name", target.Name, "type", commandType, "command_id", id, "origin", origin, "bytes", len(payload))
	if err := t.send(tunnelMessage{T: "command", ID: id, Payload: payload, Origin: origin}); err != nil {
		slog.Warn("federation upstream command write failed", "host_id", target.ID, "command_id", id, "error", err)
		return nil, err
	}
	select {
	case r := <-done:
		if r.Error != "" {
			slog.Warn("federation upstream command failed remotely", "host_id", target.ID, "type", commandType, "command_id", id, "error", r.Error)
			return nil, errors.New(r.Error)
		}
		slog.Info("federation upstream command completed", "host_id", target.ID, "type", commandType, "command_id", id, "bytes", len(r.Payload))
		return r.Payload, nil
	case <-ctx.Done():
		slog.Warn("federation upstream command timed out", "host_id", target.ID, "type", commandType, "command_id", id, "error", ctx.Err())
		return nil, ctx.Err()
	}
}

// SelfID is the stable host ID this daemon is known by to its peers; agent
// messaging addresses agents by it.
func (s *Service) SelfID() string { return s.selfID() }

// selfID is the host ID this daemon names itself by to its peers: the ID its
// parent accepted, or else a durable generated one (a root has no parent to
// accept an ID, but its children still need to tell it apart).
func (s *Service) selfID() string {
	if m, err := s.store.FederationParent(); err == nil && m != nil && m.HostID != "" {
		if s.parentURL != "" && !m.Adopted && strings.TrimRight(m.URL, "/") == s.parentURL {
			return m.HostID
		}
		if s.parentURL == "" && m.Adopted {
			return m.HostID
		}
	}
	s.mu.Lock()
	cached := s.identity
	s.mu.Unlock()
	if cached != "" {
		return cached
	}
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	id, err := s.store.FederationIdentity()
	if err != nil {
		slog.Warn("federation identity lookup failed", "error", err)
		return ""
	}
	if id == "" {
		if id, err = newHostID(s.name); err != nil {
			return ""
		}
		if err := s.store.SaveFederationIdentity(id); err != nil {
			slog.Warn("federation identity could not be saved", "error", err)
		} else {
			slog.Info("generated federation identity", "host_id", id)
		}
	}
	s.mu.Lock()
	s.identity = id
	s.mu.Unlock()
	return id
}

// isAncestor reports whether a command's origin, received over the upstream
// link, is one of this daemon's parents. An unattributed command can only
// come from an older parent, which predates any other kind of sender.
func (s *Service) isAncestor(origin string) bool {
	if origin == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ancestor := range s.ancestors {
		if ancestor == origin {
			return true
		}
	}
	return false
}

// AccessLevelFor is the level this daemon's own policy grants the host with
// stable node ID hostID, as authorize would apply it to a command that host
// sends here. Agent messaging uses it to re-apply the policy where a host's
// envelopes are fetched (pulled) rather than pushed through authorize.
func (s *Service) AccessLevelFor(hostID string) Level {
	if hostID == "" {
		return LevelNone
	}
	return s.policy.LevelFor(hostID, s.isAncestor(hostID))
}

// authorize applies this daemon's policy to a command it is about to execute
// locally. Commands that name a further hostId are relays: the host that
// finally executes them decides.
func (s *Service) authorize(origin string, ancestor bool, payload json.RawMessage) error {
	commandType, hostID := envelopeRoute(payload)
	if hostID != "" {
		return nil
	}
	have, need := s.policy.LevelFor(origin, ancestor), CommandLevel(commandType)
	if have >= need {
		return nil
	}
	slog.Warn("federation command denied by access policy", "origin", origin, "ancestor", ancestor, "type", commandType, "have", have.String(), "need", need.String())
	who := origin
	if who == "" {
		who = "the requesting host"
	}
	return fmt.Errorf("federation: %s "+accessDeniedMarker+"%s on this host (needs %s access, has %s)", who, commandType, need, have)
}

// ServeHTTP mounts the small, separately authenticated federation protocol.
// It intentionally does not accept Tandem's browser token.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case RegisterPath:
		s.register(w, r)
	case StatusPath:
		s.registrationStatus(w, r)
	case HeartbeatPath:
		s.heartbeat(w, r)
	case TunnelPath:
		s.serveTunnel(w, r)
	case AdoptPath:
		s.serveAdoption(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req registerRequest
	if err := decode(r, &req); err != nil || req.HostID == "" {
		writeJSON(w, http.StatusBadRequest, registerResponse{Error: "hostId is required"})
		return
	}
	if !ValidHostID(req.HostID) {
		writeJSON(w, http.StatusBadRequest, registerResponse{Error: "hostId must be at most 64 characters of letters, digits, '-', '_' or '.'"})
		return
	}
	peer, err := s.store.FederationChild(req.HostID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if trusted(peer) {
		// A host should reconnect with its credential after acceptance. Do not
		// reveal that credential to an unauthenticated registration request,
		// and do not reset an established host back to pending approval.
		writeJSON(w, http.StatusUnauthorized, registerResponse{Status: "rejected", Error: "host is already registered; use its stored credential"})
		return
	}
	rotated, err := s.rotateIdentity(req, federationToken(r))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rotated != nil {
		writeJSON(w, http.StatusOK, registerResponse{Status: "accepted", Credential: rotated.Credential, ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion})
		return
	}
	now := time.Now().UnixMilli()
	pending := store.FederationChild{ID: req.HostID, Name: req.Name, Endpoint: req.Endpoint, Status: "pending", RequestedAt: now, ProtocolVersion: req.ProtocolVersion, BuildVersion: req.BuildVersion}
	if err := s.store.UpsertFederationChild(pending); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.notifyPending(pending)
	writeJSON(w, http.StatusAccepted, registerResponse{Status: "pending", ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion})
}

func (s *Service) registrationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("hostId")
	peer, err := s.store.FederationChild(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if peer == nil {
		writeJSON(w, 404, registerResponse{Status: "rejected", Error: "registration not found"})
		return
	}
	resp := registerResponse{Status: peer.Status, ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion}
	if peer.Status == "accepted" {
		resp.Credential = peer.Credential
	}
	if peer.Status == "rejected" {
		resp.Error = "registration was rejected by the parent"
	}
	writeJSON(w, 200, resp)
}

func (s *Service) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var hb heartbeat
	if err := decode(r, &hb); err != nil || hb.HostID == "" {
		http.Error(w, "hostId is required", 400)
		return
	}
	peer, err := s.store.FederationChild(hb.HostID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if !trusted(peer) || !secretMatches(federationToken(r), peer.Credential) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	now := time.Now().UnixMilli()
	peer.Status = "connected"
	peer.LastSeenAt = now
	if hb.ProtocolVersion != 0 || hb.BuildVersion != "" {
		peer.ProtocolVersion, peer.BuildVersion = hb.ProtocolVersion, hb.BuildVersion
	}
	if err := s.store.UpsertFederationChild(*peer); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.checkProtocol(*peer)
	s.mu.Lock()
	if len(hb.Snapshot) > 0 {
		s.snapshots[hb.HostID] = append(json.RawMessage(nil), hb.Snapshot...)
	}
	for _, got := range hb.Results {
		if waiter := s.waiters[got.ID]; waiter != nil {
			select {
			case waiter <- got:
			default:
			}
		}
	}
	commands := s.queues[hb.HostID]
	delete(s.queues, hb.HostID)
	s.mu.Unlock()
	writeJSON(w, 200, heartbeatResponse{Commands: commands})
}

// Subscribe receives live child envelopes. wsserver owns routing and agent-ID
// rewriting; federation deliberately remains independent of UI protocol.
func (s *Service) Subscribe(fn func(hostID string, payload json.RawMessage)) func() {
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	s.mu.Unlock()
	return func() { s.mu.Lock(); delete(s.subs, id); s.mu.Unlock() }
}

func (s *Service) serveTunnel(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: false}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	hello, ok := readHello(conn)
	if !ok {
		return
	}
	peer, err := s.store.FederationChild(hello.HostID)
	if err != nil || !trusted(peer) || !secretMatches(federationToken(r), peer.Credential) {
		_ = conn.WriteJSON(tunnelMessage{T: "error", Error: unauthorizedTunnelError})
		return
	}
	s.serveChild(conn, hello, peer)
}

// readHello reads the message a child opens every link with.
func readHello(conn *websocket.Conn) (tunnelMessage, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello tunnelMessage
	if err := conn.ReadJSON(&hello); err != nil || hello.T != "hello" || hello.HostID == "" {
		return hello, false
	}
	return hello, true
}

// serveChild runs this daemon's end of a link to an authenticated child,
// whichever of the two dialed, until the connection ends.
func (s *Service) serveChild(conn *websocket.Conn, hello tunnelMessage, peer *store.FederationChild) {
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	if self, _ := s.store.FederationParent(); self != nil {
		for _, ancestor := range hello.Ancestors {
			if ancestor == self.HostID {
				_ = conn.WriteJSON(tunnelMessage{T: "error", Error: "federation cycle detected"})
				return
			}
		}
	}
	t := &tunnel{conn: conn}
	// The welcome carries the child's ancestor chain, which it needs to
	// authorize this daemon's commands. Send it before the tunnel is
	// registered so no concurrent Call can put a command ahead of it.
	// A host predating the welcome message ignores unknown tunnel types, so
	// this is safe to send unconditionally.
	_ = t.send(s.welcome())
	s.mu.Lock()
	old := s.tunnels[hello.HostID]
	s.tunnels[hello.HostID] = t
	queued := s.queues[hello.HostID]
	delete(s.queues, hello.HostID)
	s.rules[hello.HostID] = hello.Rules
	// Recorded under s.mu, together with the registration above, so a
	// superseded tunnel's cleanup (dropChild) cannot interleave between the
	// two and leave a live tunnel recorded offline.
	peer.Status = "connected"
	peer.LastSeenAt = time.Now().UnixMilli()
	peer.ProtocolVersion, peer.BuildVersion = hello.ProtocolVersion, hello.BuildVersion
	_ = s.store.UpsertFederationChild(*peer)
	s.mu.Unlock()
	if old != nil {
		slog.Info("federation tunnel superseded by reconnect", "host_id", hello.HostID)
		_ = old.conn.Close()
	}
	upstreamCapable := hasCapability(hello.Capabilities, upstreamCapability)
	slog.Info("federation host connected", "host_id", hello.HostID, "protocol_version", hello.ProtocolVersion, "build_version", hello.BuildVersion, "upstream_capable", upstreamCapable, "access_rules", len(hello.Rules))
	s.checkProtocol(*peer)
	if upstreamCapable {
		s.startChildLink(hello.HostID, t)
	}
	s.publish(hello.HostID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
	for _, cmd := range queued {
		if t.send(tunnelMessage{T: "command", ID: cmd.ID, Payload: cmd.Payload, Origin: cmd.Origin}) != nil {
			break
		}
	}
	defer s.dropChild(hello.HostID, t)
	for {
		var msg tunnelMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.T {
		case "response":
			s.deliver(result{ID: msg.ID, Payload: msg.Payload, Error: msg.Error})
		case "command":
			go s.executeFromChild(hello.HostID, t, msg)
		case "snapshot":
			s.mu.Lock()
			s.snapshots[hello.HostID] = append(json.RawMessage(nil), msg.Snapshot...)
			s.mu.Unlock()
			s.publish(hello.HostID, msg.Snapshot)
		case "topology":
			if s.topologyCycles(msg.Hosts) {
				_ = t.send(tunnelMessage{T: "error", Error: "federation cycle detected"})
				return
			}
			s.mu.Lock()
			s.topologies[hello.HostID] = cloneHosts(msg.Hosts)
			s.mu.Unlock()
			s.publish(hello.HostID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
		case "event":
			s.publish(s.relayHostID(hello.HostID, msg.HostID), msg.Payload)
		}
	}
}

// dropChild cleans up after a child's tunnel ends. A reconnecting child's new
// tunnel closes the old one, whose cleanup then runs concurrently with (or
// after) the new registration; only the current tunnel may mark the host
// offline. Otherwise the parent records a live child as offline, keeps
// showing the events it relays, and refuses every command routed to it.
func (s *Service) dropChild(hostID string, t *tunnel) {
	s.mu.Lock()
	current := s.tunnels[hostID] == t
	offline := false
	if current {
		delete(s.tunnels, hostID)
		if p, _ := s.store.FederationChild(hostID); p != nil && p.Status == "connected" {
			p.Status = "offline"
			_ = s.store.UpsertFederationChild(*p)
			offline = true
		}
	}
	s.mu.Unlock()
	s.stopChildLink(hostID, t)
	if !current {
		slog.Info("superseded federation tunnel closed", "host_id", hostID)
		return
	}
	s.removeProtocolNotification(hostID)
	if offline {
		s.publish(hostID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
	}
	slog.Warn("federation host disconnected", "host_id", hostID)
}

func cloneHosts(in []Host) []Host {
	out := make([]Host, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Route = append([]string(nil), in[i].Route...)
		out[i].Snapshot = append(json.RawMessage(nil), in[i].Snapshot...)
	}
	return out
}

// relayHostID converts the emitting child's public ID into this daemon's
// public ID. Empty means the directly connected child itself emitted it.
func (s *Service) relayHostID(via, childID string) string {
	if childID == "" || childID == via {
		return via
	}
	peers, err := s.store.FederationChildren()
	if err != nil {
		return via
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range peers {
		if p.ID != via {
			continue
		}
		for _, h := range s.descendantsLocked(via, via, []string{via}, 1, true) {
			if h.nextID == childID {
				return h.ID
			}
		}
	}
	return via
}
func (s *Service) deliver(r result) {
	s.mu.Lock()
	waiter := s.waiters[r.ID]
	s.mu.Unlock()
	if waiter != nil {
		select {
		case waiter <- r:
		default:
		}
	}
}
func (s *Service) publish(hostID string, payload json.RawMessage) {
	s.mu.Lock()
	var envelope struct {
		T string `json:"t"`
	}
	if json.Unmarshal(payload, &envelope) == nil && envelope.T == "agents" {
		s.snapshots[hostID] = append(json.RawMessage(nil), payload...)
	}
	callbacks := make([]func(string, json.RawMessage), 0, len(s.subs))
	for _, fn := range s.subs {
		callbacks = append(callbacks, fn)
	}
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn(hostID, append(json.RawMessage(nil), payload...))
	}
	if envelope.T == "federation_hosts_changed" || envelope.T == "agents" {
		s.sendTopologyUpstream()
		s.refreshChildViews()
	}
}

// publishLocal delivers an envelope to this daemon's own subscribers only.
// It is for state learned from the parent, which must neither be cached as a
// descendant's snapshot nor trigger re-advertisement upstream.
func (s *Service) publishLocal(hostID string, payload json.RawMessage) {
	s.mu.Lock()
	callbacks := make([]func(string, json.RawMessage), 0, len(s.subs))
	for _, fn := range s.subs {
		callbacks = append(callbacks, fn)
	}
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn(hostID, append(json.RawMessage(nil), payload...))
	}
}

// setAncestors records the parent's report of the hosts above this one. A
// changed chain is passed on to this daemon's own children (it is their
// chain too) and re-evaluates what the parent may see here.
func (s *Service) setAncestors(chain []string) {
	s.mu.Lock()
	changed := len(chain) != len(s.ancestors)
	for i := 0; !changed && i < len(chain); i++ {
		changed = chain[i] != s.ancestors[i]
	}
	if !changed {
		s.mu.Unlock()
		return
	}
	s.ancestors = append([]string(nil), chain...)
	upstream := s.upstream
	children := make([]*tunnel, 0, len(s.tunnels))
	for _, t := range s.tunnels {
		children = append(children, t)
	}
	s.mu.Unlock()
	slog.Info("federation ancestors updated", "ancestors", chain)
	welcome := s.welcome()
	for _, t := range children {
		_ = t.send(welcome)
	}
	if upstream != nil && s.local != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if snapshot, err := s.local.Snapshot(ctx); err == nil && len(snapshot) > 0 {
				_ = upstream.sendState("snapshot", tunnelMessage{T: "snapshot", Snapshot: s.upstreamSnapshot(snapshot)})
			}
		}()
	}
}

// setUpstreamView installs the parent's latest view of the rest of the
// fleet and tells this daemon's browsers (and children) when it changed.
func (s *Service) setUpstreamView(hosts []Host) {
	var next []Host
	if len(hosts) != 0 {
		next = cloneHosts(hosts)
	}
	s.mu.Lock()
	before, _ := json.Marshal(s.upstreamView)
	after, _ := json.Marshal(next)
	if bytes.Equal(before, after) {
		s.mu.Unlock()
		return
	}
	s.upstreamView = next
	s.mu.Unlock()
	slog.Info("federation upstream view updated", "hosts", len(next))
	parentID := routedHostID([]string{upHop})
	s.publishLocal(parentID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
	s.publishLocal(parentID, json.RawMessage(`{"t":"agents"}`))
	s.refreshChildViews()
}

// upstreamIDLocked maps the parent's address for a host (empty for the
// parent itself) to this daemon's address for it, or "" if unknown.
func (s *Service) upstreamIDLocked(parentRelative string) string {
	for _, h := range s.upstreamHostsLocked() {
		if h.nextID == parentRelative {
			return h.ID
		}
	}
	return ""
}

func (s *Service) isUpstreamHostID(hostID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.upstreamHostsLocked() {
		if h.ID == hostID {
			return true
		}
	}
	return false
}

// relayUpstreamEvent publishes an event the parent forwarded for one of the
// hosts reached through it, under this daemon's address for that host.
func (s *Service) relayUpstreamEvent(parentRelative string, payload json.RawMessage) {
	var meta struct {
		T string `json:"t"`
	}
	_ = json.Unmarshal(payload, &meta)
	if meta.T == "system_notifications" {
		return
	}
	s.mu.Lock()
	hostID := s.upstreamIDLocked(parentRelative)
	s.mu.Unlock()
	if hostID == "" {
		slog.Debug("dropping upstream event for unknown host", "parent_host_id", parentRelative, "type", meta.T)
		return
	}
	s.publishLocal(hostID, payload)
}

// startChildLink opens the private bridge that serves one child's upstream
// commands and sends it its first view of the fleet.
func (s *Service) startChildLink(childID string, t *tunnel) {
	if s.newLocal == nil {
		return
	}
	local, err := s.newLocal()
	if err != nil {
		slog.Warn("federation child link could not open a local bridge", "host_id", childID, "error", err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	link := &childLink{local: local, tunnel: t, cancel: cancel}
	s.mu.Lock()
	old := s.children[childID]
	s.children[childID] = link
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	slog.Info("federation child link opened", "host_id", childID, "access", s.policy.LevelFor(childID, false).String())
	if source, ok := local.(EventSource); ok {
		go s.pumpChildEvents(ctx, childID, link, source.Events())
	}
	go func() {
		snapshotCtx, cancelSnapshot := context.WithTimeout(ctx, 30*time.Second)
		defer cancelSnapshot()
		if snapshot, err := local.Snapshot(snapshotCtx); err == nil && len(snapshot) > 0 {
			s.setLocalSnapshot(snapshot)
		} else if err != nil && ctx.Err() == nil {
			slog.Warn("federation child link could not read local agents", "host_id", childID, "error", err)
		}
		s.sendView(childID)
	}()
}

func (s *Service) stopChildLink(childID string, t *tunnel) {
	s.mu.Lock()
	link := s.children[childID]
	if link != nil && link.tunnel == t {
		delete(s.children, childID)
	} else {
		link = nil
	}
	s.mu.Unlock()
	if link != nil {
		link.close()
		slog.Info("federation child link closed", "host_id", childID)
	}
}

func (s *Service) setLocalSnapshot(raw json.RawMessage) {
	snapshot := localOnlySnapshot(raw)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(snapshot, &envelope) == nil {
		// A correlated reply's corrId would make every refresh look new.
		delete(envelope, "corrId")
		if b, err := json.Marshal(envelope); err == nil {
			snapshot = b
		}
	}
	s.mu.Lock()
	s.localSnapshot = snapshot
	s.mu.Unlock()
}

// pumpChildEvents forwards what the child's bridge hears -- the events of
// the subscriptions that child made -- down its tunnel. State-change
// broadcasts become a fresh view instead, and nothing from a host the child
// may not view is passed on.
func (s *Service) pumpChildEvents(ctx context.Context, childID string, link *childLink, events <-chan json.RawMessage) {
	visible := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			var meta struct {
				T      string `json:"t"`
				HostID string `json:"hostId"`
			}
			_ = json.Unmarshal(event, &meta)
			switch meta.T {
			case "agents":
				if meta.HostID == "" {
					s.setLocalSnapshot(event)
				}
				visible = map[string]bool{}
				s.sendView(childID)
				continue
			case "hosts", "federation_hosts_changed":
				visible = map[string]bool{}
				s.sendView(childID)
				continue
			case "system_notifications":
				// A host's notifications carry actions (including approving
				// federation registrations) for its own operators only.
				continue
			}
			allowed, known := visible[meta.HostID]
			if !known {
				allowed = s.childMayView(childID, meta.HostID)
				visible[meta.HostID] = allowed
			}
			if !allowed {
				continue
			}
			if err := link.tunnel.send(tunnelMessage{T: "event", HostID: meta.HostID, Payload: event}); err != nil {
				slog.Warn("federation child event write failed", "host_id", childID, "type", meta.T, "error", err)
				return
			}
		}
	}
}

// childMayView reports whether the direct child may see state from hostID
// (this daemon's address; "" is this daemon itself). A child never gets its
// own subtree's state echoed back.
func (s *Service) childMayView(childID, hostID string) bool {
	if hostID == "" {
		return s.policy.LevelFor(childID, false) >= LevelView
	}
	for _, h := range s.Hosts() {
		if h.ID != hostID {
			continue
		}
		if !h.Upstream && len(h.Route) != 0 && h.Route[0] == childID {
			return false
		}
		return h.Rules.LevelFor(childID, false) >= LevelView
	}
	return false
}

func (s *Service) refreshChildViews() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.children))
	for id := range s.children {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.sendView(id)
	}
}

// sendView sends a child the hosts it may see through this daemon, unless
// the view is unchanged since the last one sent on that tunnel.
func (s *Service) sendView(childID string) {
	s.mu.Lock()
	link := s.children[childID]
	s.mu.Unlock()
	if link == nil {
		return
	}
	link.viewMu.Lock()
	defer link.viewMu.Unlock()
	hosts := s.viewFor(childID)
	if err := link.tunnel.sendState("view", tunnelMessage{T: "view", Hosts: hosts}); err != nil {
		slog.Warn("federation view write failed", "host_id", childID, "error", err)
	}
}

// viewFor is the part of the fleet a direct child may see through this
// daemon, in this daemon's addressing: this daemon (as the Local entry),
// its other branches, and whatever it sees above itself -- each included only
// when that host's own policy grants the child view. The child's own subtree
// is omitted; the child already reaches it directly.
func (s *Service) viewFor(childID string) []Host {
	out := []Host{}
	for _, h := range s.Hosts() {
		if !h.Upstream && len(h.Route) != 0 && h.Route[0] == childID {
			continue
		}
		level := h.Rules.LevelFor(childID, false)
		if level < LevelView {
			continue
		}
		h.Access = level.String()
		out = append(out, h)
	}
	level := s.policy.LevelFor(childID, false)
	if level < LevelView && len(out) == 0 {
		return out
	}
	self := Host{ID: "local", Local: true, NodeID: s.selfID(), Name: s.name, Endpoint: s.endpoint, Status: "connected", ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion, Access: level.String(), Rules: s.policy}
	dialer := s.parentDialer()
	s.mu.Lock()
	if s.upstreamView != nil {
		self.ParentID = routedHostID([]string{upHop})
		self.Dialer = dialer
	}
	if level >= LevelView {
		self.Snapshot = append(json.RawMessage(nil), s.localSnapshot...)
	}
	s.mu.Unlock()
	return append([]Host{self}, out...)
}

// childOrigin decides whom a child's command is from. A child may relay for
// itself and for hosts in the subtree it advertises -- it controls those
// anyway -- but any other claim is treated as the child's own request.
func (s *Service) childOrigin(childID, claimed string) string {
	if claimed == "" || claimed == childID {
		return childID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.topologies[childID] {
		if h.NodeID == claimed {
			return claimed
		}
	}
	slog.Warn("federation child claimed an origin outside its subtree", "host_id", childID, "claimed_origin", claimed)
	return childID
}

// executeFromChild runs, or relays onward, a command a child sent upstream.
func (s *Service) executeFromChild(childID string, t *tunnel, msg tunnelMessage) {
	origin := s.childOrigin(childID, msg.Origin)
	commandType, next := envelopeRoute(msg.Payload)
	s.mu.Lock()
	link := s.children[childID]
	s.mu.Unlock()
	reply := tunnelMessage{T: "response", ID: msg.ID}
	switch {
	case link == nil || link.tunnel != t:
		reply.Error = "federation: this host does not accept commands from its agent hosts"
	default:
		if err := s.authorize(origin, false, msg.Payload); err != nil {
			reply.Error = err.Error()
			break
		}
		payload := msg.Payload
		if next != "" {
			var err error
			if payload, err = withEnvelopeField(payload, OriginField, origin); err != nil {
				reply.Error = err.Error()
				break
			}
		}
		slog.Info("executing federation command from agent host", "host_id", childID, "origin", origin, "type", commandType, "relay_to", next, "command_id", msg.ID)
		// The requester applies its own deadline; this only bounds a
		// command whose reply never comes (spawns may install runtimes).
		// The origin also rides in the context: a Local that executes the
		// command itself (rather than relaying it) learns who asked from it.
		ctx, cancel := context.WithTimeout(WithOrigin(context.Background(), origin), 11*time.Minute)
		data, err := link.local.Execute(ctx, payload)
		cancel()
		reply.Payload = data
		if err != nil {
			slog.Warn("federation command from agent host failed", "host_id", childID, "origin", origin, "type", commandType, "command_id", msg.ID, "error", err)
			reply.Error = err.Error()
		}
	}
	if err := t.send(reply); err != nil {
		slog.Warn("federation response write to agent host failed", "host_id", childID, "command_id", msg.ID, "error", err)
	}
}

func (s *Service) sendTopologyUpstream() {
	s.mu.Lock()
	t := s.upstream
	s.mu.Unlock()
	if t != nil {
		_ = t.sendState("topology", tunnelMessage{T: "topology", Hosts: s.descendantHosts()})
	}
}

// welcome is what a parent tells a connecting child: its versions and the
// chain of hosts above that child, nearest first, which the child's policy
// treats as its ancestors.
func (s *Service) welcome() tunnelMessage {
	chain := []string{}
	if self := s.selfID(); self != "" {
		chain = append(chain, self)
	}
	s.mu.Lock()
	chain = append(chain, s.ancestors...)
	s.mu.Unlock()
	return tunnelMessage{T: "welcome", ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion, Ancestors: chain, Capabilities: []string{upstreamCapability}}
}

func hasCapability(capabilities []string, want string) bool {
	for _, c := range capabilities {
		if c == want {
			return true
		}
	}
	return false
}

// A cycle would eventually re-advertise this daemon's upstream identity as a
// descendant. IDs are scoped to their direct parent, so this is deliberately
// a conservative guard paired with maxFederationDepth rather than a claim of
// global node identity.
func (s *Service) topologyCycles(hosts []Host) bool {
	self, _ := s.store.FederationParent()
	if self == nil || self.HostID == "" {
		return false
	}
	for _, host := range hosts {
		if host.NodeID == self.HostID || host.Depth > maxFederationDepth {
			return true
		}
	}
	return false
}
func (t *tunnel) send(message tunnelMessage) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := t.conn.WriteJSON(message)
	_ = t.conn.SetWriteDeadline(time.Time{})
	return err
}

// sendState sends a state message unless it is byte-identical to the last
// one of the same kind on this tunnel.
func (t *tunnel) sendState(kind string, message tunnelMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	t.sentMu.Lock()
	defer t.sentMu.Unlock()
	if bytes.Equal(t.sent[kind], data) {
		return nil
	}
	t.writeMu.Lock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = t.conn.WriteMessage(websocket.TextMessage, data)
	_ = t.conn.SetWriteDeadline(time.Time{})
	t.writeMu.Unlock()
	if err != nil {
		return err
	}
	if t.sent == nil {
		t.sent = map[string][]byte{}
	}
	t.sent[kind] = data
	return nil
}

func (t *tunnel) ping() error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
}

// HandleNotificationAction accepts a parent UI notification action. It
// returns handled=false for unrelated notification IDs so daemon code can
// chain this with updater actions.
func (s *Service) HandleNotificationAction(_ context.Context, id, action string) (sessionID string, handled bool, err error) {
	if id == parentProtocolNotificationID {
		if action != "dismiss" {
			return "", true, fmt.Errorf("unknown federation protocol action %q", action)
		}
		if s.notifications != nil {
			s.notifications.Remove(parentProtocolNotificationID)
		}
		return "", true, nil
	}
	if strings.HasPrefix(id, protocolNotificationPrefix) {
		if action != "dismiss" {
			return "", true, fmt.Errorf("unknown federation protocol action %q", action)
		}
		s.removeProtocolNotification(strings.TrimPrefix(id, protocolNotificationPrefix))
		return "", true, nil
	}
	if strings.HasPrefix(id, adoptionNotificationPrefix) {
		return "", true, s.handleAdoptionAction(strings.TrimPrefix(id, adoptionNotificationPrefix), action)
	}
	const prefix = "federation-registration-"
	if !strings.HasPrefix(id, prefix) {
		return "", false, nil
	}
	hostID := strings.TrimPrefix(id, prefix)
	peer, err := s.store.FederationChild(hostID)
	if err != nil {
		return "", true, err
	}
	if peer == nil || peer.Status != "pending" {
		return "", true, errors.New("federation registration is no longer pending")
	}
	switch action {
	case "accept", "accept_replace":
		credential, e := randomID()
		if e != nil {
			return "", true, e
		}
		peer.Credential = credential
		peer.Status = "accepted"
		peer.AcceptedAt = time.Now().UnixMilli()
		if e = s.store.UpsertFederationChild(*peer); e != nil {
			return "", true, e
		}
		if action == "accept_replace" {
			for _, stale := range s.supersededBy(*peer) {
				s.forget(stale.ID)
			}
		}
		s.removeNotification(hostID)
		return "", true, nil
	case "reject":
		peer.Status = "rejected"
		if e := s.store.UpsertFederationChild(*peer); e != nil {
			return "", true, e
		}
		s.removeNotification(hostID)
		return "", true, nil
	default:
		return "", true, fmt.Errorf("unknown federation registration action %q", action)
	}
}

// rotateIdentity transfers an established host's trust to the new ID it just
// generated for itself, and retires the record it is replacing. A host that
// changes its ID -- the short-ID migration is the first instance, and any
// future identity change behaves the same -- would otherwise leave a row
// behind that nothing can ever reconnect to, and would cost a second
// approval for a host the operator already approved.
//
// The old record's credential, presented the same way every other
// authenticated federation call presents it, is what proves the two IDs are
// one host; an unproven request falls through to ordinary approval.
func (s *Service) rotateIdentity(req registerRequest, credential string) (*store.FederationChild, error) {
	if req.PreviousHostID == "" || req.PreviousHostID == req.HostID {
		return nil, nil
	}
	previous, err := s.store.FederationChild(req.PreviousHostID)
	if err != nil {
		return nil, err
	}
	if !trusted(previous) || !secretMatches(credential, previous.Credential) {
		return nil, nil
	}
	issued, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	rotated := store.FederationChild{
		ID: req.HostID, Name: req.Name, Endpoint: req.Endpoint, Credential: issued,
		Status: "accepted", RequestedAt: now, AcceptedAt: now,
		ProtocolVersion: req.ProtocolVersion, BuildVersion: req.BuildVersion,
	}
	if err := s.store.UpsertFederationChild(rotated); err != nil {
		return nil, err
	}
	s.forget(previous.ID)
	return &rotated, nil
}

// supersededBy reports the records that look like earlier identities of the
// host just accepted: same reported name, not currently connected. The parent
// cannot prove this on its own -- two machines may legitimately share a
// hostname -- so it is only ever offered to the operator as a choice, never
// applied automatically.
func (s *Service) supersededBy(accepted store.FederationChild) []store.FederationChild {
	if strings.TrimSpace(accepted.Name) == "" {
		return nil
	}
	peers, err := s.store.FederationChildren()
	if err != nil {
		return nil
	}
	var out []store.FederationChild
	for _, p := range peers {
		if p.ID == accepted.ID || p.Status == "connected" || !strings.EqualFold(p.Name, accepted.Name) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// forget drops a host record and every piece of live state keyed to it. A
// child still holding the deleted credential is refused at the tunnel and
// registers again, so forgetting a host cannot strand it.
func (s *Service) forget(hostID string) {
	_ = s.store.DeleteFederationChild(hostID)
	s.removeNotification(hostID)
	s.removeProtocolNotification(hostID)
	s.mu.Lock()
	tunnel := s.tunnels[hostID]
	delete(s.tunnels, hostID)
	delete(s.queues, hostID)
	delete(s.snapshots, hostID)
	s.mu.Unlock()
	if tunnel != nil {
		_ = tunnel.conn.Close()
	}
	s.publish(hostID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
}

func (s *Service) notifyPending(peer store.FederationChild) {
	if s.notifications == nil {
		return
	}
	label := peer.Name
	if label == "" {
		label = peer.ID
	}
	message := "A Tandem agent host requests registration."
	if peer.Endpoint != "" {
		message += " Endpoint: " + peer.Endpoint
	}
	accept := notifications.Action{ID: "accept", Label: "Accept", Primary: true}
	reject := notifications.Action{ID: "reject", Label: "Reject"}
	actions := []notifications.Action{accept, reject}
	// A host that registers under a new ID while a same-named record already
	// exists is usually that record's replacement -- a reinstalled host, or
	// one whose stored identity was lost. Accepting alone keeps both rows,
	// which is right when the two really are different machines; the replace
	// action is the way to say they are not.
	if stale := s.supersededBy(peer); len(stale) != 0 {
		ids := make([]string, 0, len(stale))
		for _, p := range stale {
			ids = append(ids, p.ID)
		}
		message += " This name already has a disconnected record (" + strings.Join(ids, ", ") + "); replace it if this host is its replacement."
		actions = []notifications.Action{accept, {ID: "accept_replace", Label: "Accept and replace"}, reject}
	}
	s.notifications.Upsert(notifications.Notification{ID: "federation-registration-" + peer.ID, Severity: "attention", Title: "Register agent host " + label, Message: message, Actions: actions})
}

const protocolNotificationPrefix = "federation-protocol-"
const parentProtocolNotificationID = "federation-parent-protocol"

// noteParentProtocol is the child-side half of skew reporting, so an operator
// looking at the host's own UI sees the same fact the parent's UI shows.
func (s *Service) noteParentProtocol(version int, build string) {
	if s.notifications == nil {
		return
	}
	if version == ProtocolVersion {
		s.notifications.Remove(parentProtocolNotificationID)
		return
	}
	remote := fmt.Sprintf("protocol %d", version)
	if version == 0 {
		remote = "a federation protocol predating version reporting"
	}
	if build != "" {
		remote += " (" + build + ")"
	}
	action := "Update this host's Tandem to match its parent."
	if version < ProtocolVersion {
		action = "Update the parent's Tandem to match this host."
	}
	s.notifications.Upsert(notifications.Notification{
		ID: parentProtocolNotificationID, Severity: "attention",
		Title:   "This host's parent is a different Tandem version",
		Message: fmt.Sprintf("The parent speaks %s; this Tandem speaks protocol %d. %s", remote, ProtocolVersion, action),
		Actions: []notifications.Action{{ID: "dismiss", Label: "Dismiss"}},
	})
}

// checkProtocol surfaces version skew as an ordinary notification instead of
// letting it appear as commands that mysteriously do nothing. It deliberately
// does not refuse the connection: the tunnel carries opaque browser envelopes,
// so a peer one version off still handles every command both sides share.
func (s *Service) checkProtocol(peer store.FederationChild) {
	if s.notifications == nil {
		return
	}
	if peer.ProtocolVersion == ProtocolVersion {
		s.removeProtocolNotification(peer.ID)
		return
	}
	label := peer.Name
	if label == "" {
		label = peer.ID
	}
	remote := fmt.Sprintf("protocol %d", peer.ProtocolVersion)
	stale := peer.ProtocolVersion < ProtocolVersion
	if peer.ProtocolVersion == 0 {
		remote = "a federation protocol predating version reporting"
	}
	if peer.BuildVersion != "" {
		remote += " (" + peer.BuildVersion + ")"
	}
	action := "Update that host's Tandem to match this one."
	if !stale {
		action = "Update this Tandem to match that host."
	}
	message := fmt.Sprintf("%s speaks %s; this Tandem speaks protocol %d. Commands both versions share still work, but newer ones may fail on the older side. %s",
		label, remote, ProtocolVersion, action)
	s.notifications.Upsert(notifications.Notification{
		ID: protocolNotificationPrefix + peer.ID, Severity: "attention",
		Title:   "Agent host " + label + " is a different Tandem version",
		Message: message,
		Actions: []notifications.Action{{ID: "dismiss", Label: "Dismiss"}},
	})
}

func (s *Service) removeProtocolNotification(hostID string) {
	if s.notifications != nil {
		s.notifications.Remove(protocolNotificationPrefix + hostID)
	}
}

func (s *Service) removeNotification(id string) {
	if s.notifications != nil {
		s.notifications.Remove("federation-registration-" + id)
	}
}

// RunParentLink reconnects forever until ctx ends. It registers once, durably
// saves the issued credential, then keeps one persistent outbound tunnel.
func (s *Service) RunParentLink(ctx context.Context) error {
	if s.parentURL == "" {
		return nil
	}
	parent, err := s.store.FederationParent()
	if err != nil {
		return err
	}
	hostID := ""
	credential := ""
	// The identity being replaced, sent only to the parent that issued it, so
	// that parent can retire the record itself instead of keeping a row no
	// host will ever reconnect to. See rotateIdentity.
	previousID, previousCredential := "", ""
	if parent != nil && strings.TrimRight(parent.URL, "/") == s.parentURL {
		hostID, credential = parent.HostID, parent.Credential
	}
	if legacyHostID(hostID) {
		// Upgrading past the long random host IDs: take a readable one, and
		// prove with the old credential that the new ID is the same host, so
		// the swap is invisible to the operator.
		previousID, previousCredential = hostID, credential
		hostID, credential = "", ""
	}
	if hostID == "" {
		hostID, err = newHostID(s.name)
		if err != nil {
			return err
		}
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if credential == "" {
			status, e := s.registrationWithParent(ctx, hostID, previousID, previousCredential)
			if e == nil {
				// The proof is good for one registration. A parent that
				// answered at all has already decided what the new ID is:
				// rotated, or pending the operator's approval.
				previousID, previousCredential = "", ""
			}
			if e == nil && status.Credential != "" {
				credential = status.Credential
				if e = s.store.SaveFederationParent(store.FederationParent{URL: s.parentURL, HostID: hostID, Credential: credential}); e != nil {
					return e
				}
			} else if e == nil && status.Status == "rejected" {
				return errors.New(status.Error)
			}
			if !sleepContext(ctx, s.poll) {
				return nil
			}
			continue
		}
		tunnelErr := s.runTunnel(ctx, hostID, credential)
		if tunnelErr != nil && !errors.Is(tunnelErr, context.Canceled) {
			slog.Warn("federation upstream tunnel ended; reconnecting", "host_id", hostID, "error", tunnelErr)
		}
		if errors.Is(tunnelErr, errCredentialRejected) {
			// The parent no longer knows this credential -- its record was
			// replaced or deleted. Retrying it forever would leave this host
			// permanently unreachable, so ask for registration again under
			// the same ID. The stale credential stays on disk until a new one
			// replaces it, which costs one refused dial after a restart and
			// keeps the host's ID stable across one.
			credential = ""
		}
		if !sleepContext(ctx, s.poll) {
			return nil
		}
	}
}

// errCredentialRejected reports that the parent refused this host's stored
// credential, as opposed to the ordinary transport failures the reconnect
// loop retries through. unauthorizedTunnelError is how that refusal travels:
// the parent authenticates the tunnel after the websocket upgrade.
var errCredentialRejected = errors.New("federation: parent rejected the stored credential")

const unauthorizedTunnelError = "unauthorized"

func (s *Service) registrationWithParent(ctx context.Context, id, previousID, previousCredential string) (registerResponse, error) {
	var out registerResponse
	if previousID != "" {
		// A rotation has something to prove, and only the register call
		// carries the proof: go straight to it even if a pending row for the
		// new ID already exists from an earlier attempt.
		return s.registerWithParent(ctx, id, previousID, previousCredential)
	}
	code, err := s.request(ctx, http.MethodGet, StatusPath+"?hostId="+url.QueryEscape(id), "", nil, &out)
	if err == nil {
		return out, nil
	}
	if code != http.StatusNotFound {
		return out, err
	}
	return s.registerWithParent(ctx, id, previousID, previousCredential)
}

func (s *Service) runTunnel(ctx context.Context, hostID, credential string) error {
	target, err := linkWebSocketURL(s.parentURL, TunnelPath)
	if err != nil {
		return err
	}
	head := http.Header{}
	head.Set("Authorization", "Bearer "+credential)
	conn, _, err := s.dialer.DialContext(ctx, target, head)
	if err != nil {
		return err
	}
	defer conn.Close()
	return s.serveParent(ctx, conn, hostID)
}

// serveParent runs this daemon's end of the link to its parent, whichever of
// the two dialed, until the connection or ctx ends.
func (s *Service) serveParent(ctx context.Context, conn *websocket.Conn, hostID string) error {
	var err error
	if resetter, ok := s.local.(connectionResetter); ok {
		defer resetter.Reset()
	}
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	// ReadJSON cannot observe ctx directly. Closing this per-attempt socket on
	// shutdown makes the reconnect loop and daemon teardown prompt.
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)
	t := &tunnel{conn: conn}
	if err = t.send(tunnelMessage{T: "hello", HostID: hostID, Name: s.name, ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion, Ancestors: []string{hostID}, Capabilities: []string{upstreamCapability}, Rules: s.policy}); err != nil {
		return err
	}
	s.mu.Lock()
	s.upstream = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.upstream == t {
			s.upstream = nil
		}
		hadView := s.upstreamView != nil
		s.upstreamView = nil
		s.mu.Unlock()
		if hadView {
			slog.Info("federation upstream view withdrawn", "host_id", hostID)
			s.publishLocal(routedHostID([]string{upHop}), json.RawMessage(`{"t":"federation_hosts_changed"}`))
			s.publishLocal(routedHostID([]string{upHop}), json.RawMessage(`{"t":"agents"}`))
		}
	}()
	if s.local != nil {
		if snapshot, e := s.local.Snapshot(ctx); e == nil && len(snapshot) > 0 {
			if err = t.sendState("snapshot", tunnelMessage{T: "snapshot", Snapshot: s.upstreamSnapshot(snapshot)}); err != nil {
				return err
			}
		}
	}
	if err = t.sendState("topology", tunnelMessage{T: "topology", Hosts: s.descendantHosts()}); err != nil {
		return err
	}
	var events <-chan json.RawMessage
	if src, ok := s.local.(EventSource); ok {
		events = src.Events()
	}
	writeErr := make(chan error, 1)
	reportWriteError := func(err error) {
		select {
		case writeErr <- err:
		default:
		}
		_ = conn.Close()
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-attemptCtx.Done():
				return
			case <-ticker.C:
				if err := t.ping(); err != nil {
					reportWriteError(err)
					return
				}
			}
		}
	}()
	if events != nil {
		go func() {
			for {
				select {
				case <-attemptCtx.Done():
					return
				case event, ok := <-events:
					if !ok {
						return
					}
					var meta struct {
						T      string `json:"t"`
						HostID string `json:"hostId"`
					}
					_ = json.Unmarshal(event, &meta)
					if meta.T == "agents" {
						// A loopback's agents event is an aggregate view. Send only
						// this daemon's local portion as its snapshot; descendants
						// travel in the accompanying topology.
						if err := t.sendState("snapshot", tunnelMessage{T: "snapshot", Snapshot: s.upstreamSnapshot(event)}); err != nil {
							reportWriteError(err)
							return
						}
						if err := t.sendState("topology", tunnelMessage{T: "topology", Hosts: s.descendantHosts()}); err != nil {
							reportWriteError(err)
							return
						}
						continue
					}
					if meta.T == "federation_hosts_changed" {
						if err := t.sendState("topology", tunnelMessage{T: "topology", Hosts: s.descendantHosts()}); err != nil {
							reportWriteError(err)
							return
						}
					}
					if meta.HostID != "" && s.isUpstreamHostID(meta.HostID) {
						// Hosts reached through the parent are the parent's to
						// report; echoing their events back up would loop.
						continue
					}
					if err := t.send(tunnelMessage{T: "event", HostID: meta.HostID, Payload: event}); err != nil {
						reportWriteError(err)
						return
					}
				}
			}
		}()
	}
	for {
		var msg tunnelMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.T == "welcome" {
			s.noteParentProtocol(msg.ProtocolVersion, msg.BuildVersion)
			s.setAncestors(msg.Ancestors)
			continue
		}
		// The parent authenticates after the upgrade, so a refused credential
		// arrives as an ordinary tunnel message rather than an HTTP status.
		if msg.T == "error" && msg.Error == unauthorizedTunnelError {
			return errCredentialRejected
		}
		switch msg.T {
		case "view":
			s.setUpstreamView(msg.Hosts)
			continue
		case "response":
			s.deliver(result{ID: msg.ID, Payload: msg.Payload, Error: msg.Error})
			continue
		case "event":
			s.relayUpstreamEvent(msg.HostID, msg.Payload)
			continue
		}
		if msg.T != "command" {
			continue
		}
		go func(msg tunnelMessage) {
			var data json.RawMessage
			var execErr error
			payload := msg.Payload
			if s.local == nil {
				execErr = errors.New("federation child has no command handler")
			} else if execErr = s.authorize(msg.Origin, s.isAncestor(msg.Origin), payload); execErr == nil {
				if _, next := envelopeRoute(payload); next != "" && msg.Origin != "" {
					payload, execErr = withEnvelopeField(payload, OriginField, msg.Origin)
				}
				if execErr == nil {
					data, execErr = s.local.Execute(WithOrigin(attemptCtx, msg.Origin), payload)
				}
			}
			reply := tunnelMessage{T: "response", ID: msg.ID, Payload: data}
			if execErr != nil {
				reply.Error = execErr.Error()
			}
			if err := t.send(reply); err != nil {
				reportWriteError(err)
			}
		}(msg)
		select {
		case err := <-writeErr:
			return err
		default:
		}
	}
}

// upstreamSnapshot is the agent list this daemon offers its parent: its own
// agents only, and none at all when its policy denies the parent view.
func (s *Service) upstreamSnapshot(raw json.RawMessage) json.RawMessage {
	s.mu.Lock()
	parent := ""
	if len(s.ancestors) != 0 {
		parent = s.ancestors[0]
	}
	s.mu.Unlock()
	if s.policy.LevelFor(parent, true) < LevelView {
		return json.RawMessage(`{"t":"agents","agents":[]}`)
	}
	return localOnlySnapshot(raw)
}

// A middle daemon's browser snapshot contains its own remotely visible
// sessions too. Those are advertised independently through topology, so only
// forward truly local sessions as this node's snapshot.
func localOnlySnapshot(raw json.RawMessage) json.RawMessage {
	var snapshot struct {
		Agents []map[string]any `json:"agents"`
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Agents == nil {
		return raw
	}
	local := snapshot.Agents[:0]
	for _, agent := range snapshot.Agents {
		id, _ := agent["id"].(string)
		if !strings.HasPrefix(id, "fed~") && !strings.HasPrefix(id, "federation~") {
			local = append(local, agent)
		}
	}
	snapshot.Agents = local
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return raw
	}
	b, err := json.Marshal(snapshot.Agents)
	if err != nil {
		return raw
	}
	envelope["agents"] = b
	// Older consumers accept either field and the loopback snapshot happens to
	// include both, so keep them in sync when present.
	if _, ok := envelope["sessions"]; ok {
		envelope["sessions"] = b
	}
	filtered, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return filtered
}

// registerWithParent asks for approval under id. previousID/previousCredential
// are sent only when this host is replacing an identity the same parent
// issued, which lets the parent retire that record without a second approval.
func (s *Service) registerWithParent(ctx context.Context, id, previousID, previousCredential string) (registerResponse, error) {
	var out registerResponse
	code, err := s.request(ctx, http.MethodPost, RegisterPath, previousCredential, registerRequest{HostID: id, Name: s.name, Endpoint: s.endpoint, ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion, PreviousHostID: previousID}, &out)
	if err != nil {
		return out, err
	}
	if code == http.StatusAccepted || code == http.StatusOK {
		if out.Status == "pending" {
			_, err = s.request(ctx, http.MethodGet, StatusPath+"?hostId="+url.QueryEscape(id), "", nil, &out)
		}
		return out, err
	}
	return out, fmt.Errorf("parent registration: %s", out.Error)
}
func (s *Service) sendHeartbeat(ctx context.Context, id, credential string, snapshot json.RawMessage, results []result) ([]command, error) {
	var out heartbeatResponse
	_, err := s.request(ctx, http.MethodPost, HeartbeatPath, credential, heartbeat{HostID: id, Snapshot: snapshot, Results: results, ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion}, &out)
	return out.Commands, err
}
func (s *Service) execute(ctx context.Context, commands []command) []result {
	out := make([]result, 0, len(commands))
	for _, cmd := range commands {
		if s.local == nil {
			out = append(out, result{ID: cmd.ID, Error: "federation child has no command handler"})
			continue
		}
		data, err := s.local.Execute(ctx, cmd.Payload)
		r := result{ID: cmd.ID, Payload: data}
		if err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	return out
}
func (s *Service) request(ctx context.Context, method, path, credential string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.parentURL+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("parent returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}
func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func federationToken(r *http.Request) string {
	const p = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, p) {
		return strings.TrimPrefix(h, p)
	}
	return r.Header.Get("X-Tandem-Federation")
}
func secretMatches(a, b string) bool {
	return a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ValidHostID reports whether id is usable as a federation host ID. Host IDs
// are embedded verbatim in the namespaced agent IDs the parent hands the
// browser, so they must stay short, printable, and free of the "~" separator
// those IDs split on.
func ValidHostID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// newHostID builds a host ID a human can read in the UI: the host's own name,
// slugged, plus enough randomness to keep two same-named hosts from colliding
// on one parent. The ID is not a secret -- the credential issued on
// acceptance is -- so it does not need to be unguessable.
func newHostID(name string) (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	slug := hostSlug(name)
	if slug == "" {
		slug = "host"
	}
	return slug + "-" + hex.EncodeToString(b), nil
}

func hostSlug(name string) string {
	var out []rune
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case len(out) > 0 && out[len(out)-1] != '-':
			out = append(out, '-')
		}
		if len(out) >= 24 {
			break
		}
	}
	return strings.Trim(string(out), "-")
}

// legacyHostID matches the original 24-random-byte hex host IDs, which made
// every federated agent ID in the UI unreadably long. A child carrying one
// re-registers under a short ID instead; see RunParentLink.
func legacyHostID(id string) bool {
	if len(id) != 48 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func randomID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
