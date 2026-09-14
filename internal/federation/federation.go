// Package federation implements Tandem's single-hop master/agent-host
// protocol. A host dials its configured master; the master never needs to
// initiate a network connection back into a host.
package federation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
const ProtocolVersion = 1

const (
	RegisterPath  = "/internal/federation/register"
	StatusPath    = "/internal/federation/registration"
	HeartbeatPath = "/internal/federation/heartbeat"
	TunnelPath    = "/internal/federation/tunnel"
)

// Host is the master-safe view of a registered agent host. Snapshot is the
// host's latest opaque catalog/state envelope and excludes its credential.
type Host struct {
	ID       string          `json:"id"`
	Name     string          `json:"name,omitempty"`
	Endpoint string          `json:"endpoint,omitempty"`
	Status   string          `json:"status"`
	LastSeen int64           `json:"lastSeenAt,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	// ProtocolVersion/BuildVersion are what the host reported when it last
	// connected; both are absent for a host predating version reporting.
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
	BuildVersion    string `json:"buildVersion,omitempty"`
}

// Local supplies a slave's local operations. Commands and snapshots are
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
	MasterURL     string
	Name          string
	Endpoint      string
	Local         Local
	HTTPClient    *http.Client
	PollInterval  time.Duration
	// ProxyURL routes this daemon's outbound dials to its master through a
	// proxy, e.g. "socks5://127.0.0.1:1080". It affects the slave side only:
	// a master never dials out, and the loopback transport never leaves the
	// process. SOCKS sits below TLS, so an https/wss master still terminates
	// its own TLS end to end.
	ProxyURL string
	// BuildVersion is this daemon's release, reported to the peer alongside
	// ProtocolVersion so a skew notification can name something a human can
	// act on ("update builder to v0.5.0") rather than a bare number.
	BuildVersion string
}

type Service struct {
	store         *store.Store
	notifications *notifications.Center
	masterURL     string
	name          string
	endpoint      string
	local         Local
	client        *http.Client
	dialer        *websocket.Dialer
	poll          time.Duration
	buildVersion  string

	mu        sync.Mutex
	snapshots map[string]json.RawMessage
	queues    map[string][]command
	waiters   map[string]chan result
	tunnels   map[string]*tunnel
	subs      map[int]func(string, json.RawMessage)
	nextSub   int
}

type command struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
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
	// The master's own versions, so a host can report skew locally too.
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
	// Carried on "hello" (host to master) and "welcome" (master to host). The
	// hello is the authoritative report: a long-registered host reconnects
	// without ever registering again.
	ProtocolVersion int    `json:"protocolVersion,omitempty"`
	BuildVersion    string `json:"buildVersion,omitempty"`
}
type tunnel struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// proxyDialers builds the outbound dial path for a slave that reaches its
// master through a proxy. gorilla's own Dialer.Proxy speaks HTTP CONNECT only,
// so the websocket tunnel needs an explicit net dialer; net/http understands
// socks5 proxy URLs directly. Hostnames are handed to the proxy unresolved
// (socks5h semantics), so a master name that only resolves on the far side --
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
	master := strings.TrimRight(strings.TrimSpace(opts.MasterURL), "/")
	if master != "" {
		u, err := url.Parse(master)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("federation: invalid master URL %q", opts.MasterURL)
		}
	}
	service := &Service{store: opts.Store, notifications: opts.Notifications, buildVersion: opts.BuildVersion, masterURL: master, name: opts.Name, endpoint: opts.Endpoint, local: opts.Local, client: opts.HTTPClient, dialer: &dialer, poll: opts.PollInterval, snapshots: map[string]json.RawMessage{}, queues: map[string][]command{}, waiters: map[string]chan result{}, tunnels: map[string]*tunnel{}, subs: map[int]func(string, json.RawMessage){}}
	// A prior process may have stopped without updating its connected peers.
	// Until a new authenticated tunnel arrives, those durable records are
	// offline rather than connected.
	if master == "" {
		peers, err := opts.Store.FederationSlaves()
		if err != nil {
			return nil, err
		}
		for _, peer := range peers {
			if peer.Status == "pending" {
				service.notifyPending(peer)
			}
			if peer.Status == "connected" {
				peer.Status = "offline"
				if err := opts.Store.UpsertFederationSlave(peer); err != nil {
					return nil, err
				}
			}
		}
	}
	return service, nil
}

// trusted reports whether an approved slave record may authenticate with its
// stored credential. Approval survives disconnects: the master demotes a
// dropped tunnel to "offline", so requiring "accepted" here would make every
// reconnection after the first one fail as unauthorized.
func trusted(peer *store.FederationSlave) bool {
	if peer == nil {
		return false
	}
	switch peer.Status {
	case "accepted", "connected", "offline":
		return true
	}
	return false
}

// IsSlave is true once configured with or enrolled in an upstream. Such a
// daemon rejects all child registrations; federation has exactly one level.
func (s *Service) IsSlave() bool {
	if s.masterURL != "" {
		return true
	}
	m, err := s.store.FederationMaster()
	return err == nil && m != nil
}

func (s *Service) Hosts() []Host {
	peers, err := s.store.FederationSlaves()
	if err != nil {
		return []Host{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Host, 0, len(peers))
	for _, p := range peers {
		h := Host{ID: p.ID, Name: p.Name, Endpoint: p.Endpoint, Status: p.Status, LastSeen: p.LastSeenAt, ProtocolVersion: p.ProtocolVersion, BuildVersion: p.BuildVersion}
		if b := s.snapshots[p.ID]; len(b) != 0 {
			h.Snapshot = append(json.RawMessage(nil), b...)
		}
		out = append(out, h)
	}
	return out
}

// Call queues a protocol envelope for a connected host and waits for the
// reply sent in its next heartbeat. Cancellation does not cancel the remote
// operation; callers should use an explicit interrupt command for that.
func (s *Service) Call(ctx context.Context, hostID string, payload json.RawMessage) (json.RawMessage, error) {
	if hostID == "" || len(payload) == 0 {
		return nil, errors.New("federation: host ID and command are required")
	}
	peer, err := s.store.FederationSlave(hostID)
	if err != nil {
		return nil, err
	}
	if peer == nil || peer.Status != "connected" {
		return nil, fmt.Errorf("federation: host %q is offline", hostID)
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	done := make(chan result, 1)
	s.mu.Lock()
	s.waiters[id] = done
	tunnel := s.tunnels[hostID]
	if tunnel == nil {
		s.queues[hostID] = append(s.queues[hostID], command{ID: id, Payload: append(json.RawMessage(nil), payload...)})
	}
	s.mu.Unlock()
	if tunnel != nil {
		if err := tunnel.send(tunnelMessage{T: "command", ID: id, Payload: append(json.RawMessage(nil), payload...)}); err != nil {
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
			return nil, errors.New(r.Error)
		}
		return r.Payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.IsSlave() {
		writeJSON(w, http.StatusConflict, registerResponse{Status: "rejected", Error: "this Tandem instance is registered as a slave and cannot accept slave registrations"})
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
	peer, err := s.store.FederationSlave(req.HostID)
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
	pending := store.FederationSlave{ID: req.HostID, Name: req.Name, Endpoint: req.Endpoint, Status: "pending", RequestedAt: now, ProtocolVersion: req.ProtocolVersion, BuildVersion: req.BuildVersion}
	if err := s.store.UpsertFederationSlave(pending); err != nil {
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
	peer, err := s.store.FederationSlave(id)
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
		resp.Error = "registration was rejected by the master"
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
	peer, err := s.store.FederationSlave(hb.HostID)
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
	if err := s.store.UpsertFederationSlave(*peer); err != nil {
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

// Subscribe receives live slave envelopes. wsserver owns routing and agent-ID
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
	if s.IsSlave() {
		http.Error(w, "slave instances cannot accept federation tunnels", http.StatusConflict)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: false}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello tunnelMessage
	if err = conn.ReadJSON(&hello); err != nil || hello.T != "hello" || hello.HostID == "" {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	peer, err := s.store.FederationSlave(hello.HostID)
	if err != nil || !trusted(peer) || !secretMatches(federationToken(r), peer.Credential) {
		_ = conn.WriteJSON(tunnelMessage{T: "error", Error: unauthorizedTunnelError})
		return
	}
	t := &tunnel{conn: conn}
	s.mu.Lock()
	old := s.tunnels[hello.HostID]
	s.tunnels[hello.HostID] = t
	queued := s.queues[hello.HostID]
	delete(s.queues, hello.HostID)
	s.mu.Unlock()
	if old != nil {
		_ = old.conn.Close()
	}
	peer.Status = "connected"
	peer.LastSeenAt = time.Now().UnixMilli()
	peer.ProtocolVersion, peer.BuildVersion = hello.ProtocolVersion, hello.BuildVersion
	_ = s.store.UpsertFederationSlave(*peer)
	// A host predating the welcome message ignores unknown tunnel types, so
	// this is safe to send unconditionally.
	_ = t.send(tunnelMessage{T: "welcome", ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion})
	s.checkProtocol(*peer)
	s.publish(hello.HostID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
	for _, cmd := range queued {
		if t.send(tunnelMessage{T: "command", ID: cmd.ID, Payload: cmd.Payload}) != nil {
			break
		}
	}
	defer func() {
		s.mu.Lock()
		if s.tunnels[hello.HostID] == t {
			delete(s.tunnels, hello.HostID)
		}
		s.mu.Unlock()
		s.removeProtocolNotification(hello.HostID)
		p, _ := s.store.FederationSlave(hello.HostID)
		if p != nil && p.Status == "connected" {
			p.Status = "offline"
			_ = s.store.UpsertFederationSlave(*p)
			s.publish(hello.HostID, json.RawMessage(`{"t":"federation_hosts_changed"}`))
		}
	}()
	for {
		var msg tunnelMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.T {
		case "response":
			s.deliver(result{ID: msg.ID, Payload: msg.Payload, Error: msg.Error})
		case "snapshot":
			s.mu.Lock()
			s.snapshots[hello.HostID] = append(json.RawMessage(nil), msg.Snapshot...)
			s.mu.Unlock()
			s.publish(hello.HostID, msg.Snapshot)
		case "event":
			s.publish(hello.HostID, msg.Payload)
		}
	}
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
}
func (t *tunnel) send(message tunnelMessage) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := t.conn.WriteJSON(message)
	_ = t.conn.SetWriteDeadline(time.Time{})
	return err
}

func (t *tunnel) ping() error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
}

// HandleNotificationAction accepts a master UI notification action. It
// returns handled=false for unrelated notification IDs so daemon code can
// chain this with updater actions.
func (s *Service) HandleNotificationAction(_ context.Context, id, action string) (sessionID string, handled bool, err error) {
	if id == masterProtocolNotificationID {
		if action != "dismiss" {
			return "", true, fmt.Errorf("unknown federation protocol action %q", action)
		}
		if s.notifications != nil {
			s.notifications.Remove(masterProtocolNotificationID)
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
	const prefix = "federation-registration-"
	if !strings.HasPrefix(id, prefix) {
		return "", false, nil
	}
	hostID := strings.TrimPrefix(id, prefix)
	peer, err := s.store.FederationSlave(hostID)
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
		if e = s.store.UpsertFederationSlave(*peer); e != nil {
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
		if e := s.store.UpsertFederationSlave(*peer); e != nil {
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
func (s *Service) rotateIdentity(req registerRequest, credential string) (*store.FederationSlave, error) {
	if req.PreviousHostID == "" || req.PreviousHostID == req.HostID {
		return nil, nil
	}
	previous, err := s.store.FederationSlave(req.PreviousHostID)
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
	rotated := store.FederationSlave{
		ID: req.HostID, Name: req.Name, Endpoint: req.Endpoint, Credential: issued,
		Status: "accepted", RequestedAt: now, AcceptedAt: now,
		ProtocolVersion: req.ProtocolVersion, BuildVersion: req.BuildVersion,
	}
	if err := s.store.UpsertFederationSlave(rotated); err != nil {
		return nil, err
	}
	s.forget(previous.ID)
	return &rotated, nil
}

// supersededBy reports the records that look like earlier identities of the
// host just accepted: same reported name, not currently connected. The master
// cannot prove this on its own -- two machines may legitimately share a
// hostname -- so it is only ever offered to the operator as a choice, never
// applied automatically.
func (s *Service) supersededBy(accepted store.FederationSlave) []store.FederationSlave {
	if strings.TrimSpace(accepted.Name) == "" {
		return nil
	}
	peers, err := s.store.FederationSlaves()
	if err != nil {
		return nil
	}
	var out []store.FederationSlave
	for _, p := range peers {
		if p.ID == accepted.ID || p.Status == "connected" || !strings.EqualFold(p.Name, accepted.Name) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// forget drops a host record and every piece of live state keyed to it. A
// slave still holding the deleted credential is refused at the tunnel and
// registers again, so forgetting a host cannot strand it.
func (s *Service) forget(hostID string) {
	_ = s.store.DeleteFederationSlave(hostID)
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

func (s *Service) notifyPending(peer store.FederationSlave) {
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
const masterProtocolNotificationID = "federation-master-protocol"

// noteMasterProtocol is the slave-side half of skew reporting, so an operator
// looking at the host's own UI sees the same fact the master's UI shows.
func (s *Service) noteMasterProtocol(version int, build string) {
	if s.notifications == nil {
		return
	}
	if version == ProtocolVersion {
		s.notifications.Remove(masterProtocolNotificationID)
		return
	}
	remote := fmt.Sprintf("protocol %d", version)
	if version == 0 {
		remote = "a federation protocol predating version reporting"
	}
	if build != "" {
		remote += " (" + build + ")"
	}
	action := "Update this host's Tandem to match its master."
	if version < ProtocolVersion {
		action = "Update the master's Tandem to match this host."
	}
	s.notifications.Upsert(notifications.Notification{
		ID: masterProtocolNotificationID, Severity: "attention",
		Title:   "This host's master is a different Tandem version",
		Message: fmt.Sprintf("The master speaks %s; this Tandem speaks protocol %d. %s", remote, ProtocolVersion, action),
		Actions: []notifications.Action{{ID: "dismiss", Label: "Dismiss"}},
	})
}

// checkProtocol surfaces version skew as an ordinary notification instead of
// letting it appear as commands that mysteriously do nothing. It deliberately
// does not refuse the connection: the tunnel carries opaque browser envelopes,
// so a peer one version off still handles every command both sides share.
func (s *Service) checkProtocol(peer store.FederationSlave) {
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

// RunSlave reconnects forever until ctx ends. It registers once, durably
// saves the issued credential, then keeps one persistent outbound tunnel.
func (s *Service) RunSlave(ctx context.Context) error {
	if s.masterURL == "" {
		return nil
	}
	master, err := s.store.FederationMaster()
	if err != nil {
		return err
	}
	hostID := ""
	credential := ""
	// The identity being replaced, sent only to the master that issued it, so
	// that master can retire the record itself instead of keeping a row no
	// host will ever reconnect to. See rotateIdentity.
	previousID, previousCredential := "", ""
	if master != nil && strings.TrimRight(master.URL, "/") == s.masterURL {
		hostID, credential = master.HostID, master.Credential
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
			status, e := s.registrationWithMaster(ctx, hostID, previousID, previousCredential)
			if e == nil {
				// The proof is good for one registration. A master that
				// answered at all has already decided what the new ID is:
				// rotated, or pending the operator's approval.
				previousID, previousCredential = "", ""
			}
			if e == nil && status.Credential != "" {
				credential = status.Credential
				if e = s.store.SaveFederationMaster(store.FederationMaster{URL: s.masterURL, HostID: hostID, Credential: credential}); e != nil {
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
		if errors.Is(s.runTunnel(ctx, hostID, credential), errCredentialRejected) {
			// The master no longer knows this credential -- its record was
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

// errCredentialRejected reports that the master refused this host's stored
// credential, as opposed to the ordinary transport failures the reconnect
// loop retries through. unauthorizedTunnelError is how that refusal travels:
// the master authenticates the tunnel after the websocket upgrade.
var errCredentialRejected = errors.New("federation: master rejected the stored credential")

const unauthorizedTunnelError = "unauthorized"

func (s *Service) registrationWithMaster(ctx context.Context, id, previousID, previousCredential string) (registerResponse, error) {
	var out registerResponse
	if previousID != "" {
		// A rotation has something to prove, and only the register call
		// carries the proof: go straight to it even if a pending row for the
		// new ID already exists from an earlier attempt.
		return s.registerWithMaster(ctx, id, previousID, previousCredential)
	}
	code, err := s.request(ctx, http.MethodGet, StatusPath+"?hostId="+url.QueryEscape(id), "", nil, &out)
	if err == nil {
		return out, nil
	}
	if code != http.StatusNotFound {
		return out, err
	}
	return s.registerWithMaster(ctx, id, previousID, previousCredential)
}

func (s *Service) runTunnel(ctx context.Context, hostID, credential string) error {
	u, err := url.Parse(s.masterURL)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = TunnelPath
	head := http.Header{}
	head.Set("Authorization", "Bearer "+credential)
	conn, _, err := s.dialer.DialContext(ctx, u.String(), head)
	if err != nil {
		return err
	}
	defer conn.Close()
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
	if err = t.send(tunnelMessage{T: "hello", HostID: hostID, ProtocolVersion: ProtocolVersion, BuildVersion: s.buildVersion}); err != nil {
		return err
	}
	if s.local != nil {
		if snapshot, e := s.local.Snapshot(ctx); e == nil && len(snapshot) > 0 {
			if err = t.send(tunnelMessage{T: "snapshot", Snapshot: snapshot}); err != nil {
				return err
			}
		}
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
					if err := t.send(tunnelMessage{T: "event", Payload: event}); err != nil {
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
			s.noteMasterProtocol(msg.ProtocolVersion, msg.BuildVersion)
			continue
		}
		// The master authenticates after the upgrade, so a refused credential
		// arrives as an ordinary tunnel message rather than an HTTP status.
		if msg.T == "error" && msg.Error == unauthorizedTunnelError {
			return errCredentialRejected
		}
		if msg.T != "command" {
			continue
		}
		go func(msg tunnelMessage) {
			var data json.RawMessage
			var execErr error
			if s.local == nil {
				execErr = errors.New("federation slave has no command handler")
			} else {
				data, execErr = s.local.Execute(attemptCtx, msg.Payload)
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

// registerWithMaster asks for approval under id. previousID/previousCredential
// are sent only when this host is replacing an identity the same master
// issued, which lets the master retire that record without a second approval.
func (s *Service) registerWithMaster(ctx context.Context, id, previousID, previousCredential string) (registerResponse, error) {
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
	return out, fmt.Errorf("master registration: %s", out.Error)
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
			out = append(out, result{ID: cmd.ID, Error: "federation slave has no command handler"})
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
	req, err := http.NewRequestWithContext(ctx, method, s.masterURL+path, body)
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
		return resp.StatusCode, fmt.Errorf("master returned %s", resp.Status)
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
// are embedded verbatim in the namespaced agent IDs the master hands the
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
// on one master. The ID is not a secret -- the credential issued on
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
// every federated agent ID in the UI unreadably long. A slave carrying one
// re-registers under a short ID instead; see RunSlave.
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
