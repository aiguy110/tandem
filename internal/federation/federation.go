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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/gorilla/websocket"
)

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

type Options struct {
	Store         *store.Store
	Notifications *notifications.Center
	MasterURL     string
	Name          string
	Endpoint      string
	Local         Local
	HTTPClient    *http.Client
	PollInterval  time.Duration
}

type Service struct {
	store         *store.Store
	notifications *notifications.Center
	masterURL     string
	name          string
	endpoint      string
	local         Local
	client        *http.Client
	poll          time.Duration

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
	HostID   string `json:"hostId"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint,omitempty"`
}
type registerResponse struct {
	Status     string `json:"status"`
	Credential string `json:"credential,omitempty"`
	Error      string `json:"error,omitempty"`
}
type heartbeat struct {
	HostID   string          `json:"hostId"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	Results  []result        `json:"results,omitempty"`
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
}
type tunnel struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("federation: store is required")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
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
	return &Service{store: opts.Store, notifications: opts.Notifications, masterURL: master, name: opts.Name, endpoint: opts.Endpoint, local: opts.Local, client: opts.HTTPClient, poll: opts.PollInterval, snapshots: map[string]json.RawMessage{}, queues: map[string][]command{}, waiters: map[string]chan result{}, tunnels: map[string]*tunnel{}, subs: map[int]func(string, json.RawMessage){}}, nil
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
		h := Host{ID: p.ID, Name: p.Name, Endpoint: p.Endpoint, Status: p.Status, LastSeen: p.LastSeenAt}
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
	if tunnel := s.tunnels[hostID]; tunnel != nil {
		if err := tunnel.send(tunnelMessage{T: "command", ID: id, Payload: append(json.RawMessage(nil), payload...)}); err != nil {
			delete(s.waiters, id)
			s.mu.Unlock()
			return nil, err
		}
	} else {
		s.queues[hostID] = append(s.queues[hostID], command{ID: id, Payload: append(json.RawMessage(nil), payload...)})
	}
	s.mu.Unlock()
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
	peer, err := s.store.FederationSlave(req.HostID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if peer != nil && peer.Status == "accepted" {
		// A host should heartbeat after initial acceptance. Do not reveal its
		// existing credential to an unauthenticated registration request.
		writeJSON(w, http.StatusUnauthorized, registerResponse{Status: "rejected", Error: "host is already registered; use its stored credential"})
		return
	}
	now := time.Now().UnixMilli()
	if err := s.store.UpsertFederationSlave(store.FederationSlave{ID: req.HostID, Name: req.Name, Endpoint: req.Endpoint, Status: "pending", RequestedAt: now}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.notifyPending(req.HostID, req.Name, req.Endpoint)
	writeJSON(w, http.StatusAccepted, registerResponse{Status: "pending"})
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
	resp := registerResponse{Status: peer.Status}
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
	if peer == nil || peer.Status != "accepted" || !secretMatches(federationToken(r), peer.Credential) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	now := time.Now().UnixMilli()
	peer.Status = "connected"
	peer.LastSeenAt = now
	if err := s.store.UpsertFederationSlave(*peer); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
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
	_ = conn.SetReadDeadline(time.Time{})
	peer, err := s.store.FederationSlave(hello.HostID)
	if err != nil || peer == nil || peer.Status != "accepted" || !secretMatches(federationToken(r), peer.Credential) {
		_ = conn.WriteJSON(tunnelMessage{T: "error", Error: "unauthorized"})
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
	_ = s.store.UpsertFederationSlave(*peer)
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
		p, _ := s.store.FederationSlave(hello.HostID)
		if p != nil && p.Status == "connected" {
			p.Status = "offline"
			_ = s.store.UpsertFederationSlave(*p)
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

// HandleNotificationAction accepts a master UI notification action. It
// returns handled=false for unrelated notification IDs so daemon code can
// chain this with updater actions.
func (s *Service) HandleNotificationAction(_ context.Context, id, action string) (agentID string, handled bool, err error) {
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
	case "accept":
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
		s.removeNotification(hostID)
		return hostID, true, nil
	case "reject":
		peer.Status = "rejected"
		if e := s.store.UpsertFederationSlave(*peer); e != nil {
			return "", true, e
		}
		s.removeNotification(hostID)
		return hostID, true, nil
	default:
		return "", true, fmt.Errorf("unknown federation registration action %q", action)
	}
}

func (s *Service) notifyPending(id, name, endpoint string) {
	if s.notifications == nil {
		return
	}
	label := name
	if label == "" {
		label = id
	}
	message := "A Tandem agent host requests registration."
	if endpoint != "" {
		message += " Endpoint: " + endpoint
	}
	s.notifications.Upsert(notifications.Notification{ID: "federation-registration-" + id, Severity: "attention", Title: "Register agent host " + label, Message: message, Actions: []notifications.Action{{ID: "accept", Label: "Accept", Primary: true}, {ID: "reject", Label: "Reject"}}})
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
	if master != nil {
		hostID, credential = master.HostID, master.Credential
	}
	if hostID == "" {
		hostID, err = randomID()
		if err != nil {
			return err
		}
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if credential == "" {
			status, e := s.registrationWithMaster(ctx, hostID)
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
		_ = s.runTunnel(ctx, hostID, credential)
		if !sleepContext(ctx, s.poll) {
			return nil
		}
	}
}

func (s *Service) registrationWithMaster(ctx context.Context, id string) (registerResponse, error) {
	var out registerResponse
	code, err := s.request(ctx, http.MethodGet, StatusPath+"?hostId="+url.QueryEscape(id), "", nil, &out)
	if err == nil {
		return out, nil
	}
	if code != http.StatusNotFound {
		return out, err
	}
	return s.registerWithMaster(ctx, id)
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
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), head)
	if err != nil {
		return err
	}
	defer conn.Close()
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
	if err = t.send(tunnelMessage{T: "hello", HostID: hostID}); err != nil {
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
	if events != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-events:
					if !ok {
						return
					}
					if err := t.send(tunnelMessage{T: "event", Payload: event}); err != nil {
						select {
						case writeErr <- err:
						default:
						}
						return
					}
				}
			}
		}()
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
		var msg tunnelMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return err
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
				data, execErr = s.local.Execute(ctx, msg.Payload)
			}
			reply := tunnelMessage{T: "response", ID: msg.ID, Payload: data}
			if execErr != nil {
				reply.Error = execErr.Error()
			}
			if err := t.send(reply); err != nil {
				select {
				case writeErr <- err:
				default:
				}
			}
		}(msg)
		select {
		case err := <-writeErr:
			return err
		default:
		}
	}
}

func (s *Service) registerWithMaster(ctx context.Context, id string) (registerResponse, error) {
	var out registerResponse
	code, err := s.request(ctx, http.MethodPost, RegisterPath, "", registerRequest{HostID: id, Name: s.name, Endpoint: s.endpoint}, &out)
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
	_, err := s.request(ctx, http.MethodPost, HeartbeatPath, credential, heartbeat{HostID: id, Snapshot: snapshot, Results: results}, &out)
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
