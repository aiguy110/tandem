package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/gorilla/websocket"
)

// Adoption is a link the parent dials. It exists for a child the parent can
// reach but that cannot reach its parent -- a Tandem in a container, say,
// whose published port the host can dial. Only the connection's direction
// differs: once the socket is open the child still says hello and the parent
// still welcomes it, exactly as on a link the child dialed.
//
// Trust runs the other way too. On a dialed-in link the parent approves the
// child; here the child is about to hand admin access to whoever dialed it,
// so the child decides. The parent generates a credential per child URL and
// presents it on every dial. The child accepts a new one when the dial also
// carries the child's join token, or when its operator accepts the request;
// after that the credential alone is enough. The parent in turn trusts the
// child because its operator configured that URL, so a link that leaves the
// machine should use https or a private network.

const adoptionNotificationPrefix = "federation-adoption-"

// validLinkURL reports whether raw is an http(s) origin a link can dial.
func validLinkURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// linkWebSocketURL turns a host's HTTP origin into the websocket URL of path.
func linkWebSocketURL(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = path
	return u.String(), nil
}

// adoptionKey names a credential in notification IDs and logs without
// revealing it.
func adoptionKey(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:6])
}

// serveAdoption is the child's end of a parent's dial. It authorizes the
// parent before upgrading, so the parent learns why it was turned away from
// the HTTP status: 202 pending approval here, 403 rejected, 409 this host
// already has a parent.
func (s *Service) serveAdoption(w http.ResponseWriter, r *http.Request) {
	remote := r.RemoteAddr
	if s.parentURL != "" {
		slog.Warn("federation adoption refused: this host dials its own parent", "remote", remote, "parent_url", s.parentURL)
		http.Error(w, "this host dials its own parent", http.StatusConflict)
		return
	}
	credential := federationToken(r)
	if len(credential) < 32 {
		http.Error(w, "an adoption credential is required", http.StatusUnauthorized)
		return
	}
	hostID, status, reason := s.admitParent(r, credential)
	if hostID == "" {
		http.Error(w, reason, status)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("federation adoption upgrade failed", "remote", remote, "error", err)
		return
	}
	defer conn.Close()
	// One parent session at a time: a reconnecting parent replaces its stale
	// session, and waits for it to wind down before starting its own.
	s.mu.Lock()
	previous := s.upstream
	s.mu.Unlock()
	if previous != nil {
		slog.Info("federation adoption replaces the current parent session", "host_id", hostID, "remote", remote)
		_ = previous.conn.Close()
	}
	s.adoptMu.Lock()
	defer s.adoptMu.Unlock()
	slog.Info("federation parent connected by adoption", "host_id", hostID, "remote", remote, "parent_id", r.Header.Get(parentIDHeader))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = s.serveParent(ctx, conn, hostID)
	slog.Warn("federation adopted parent session ended", "host_id", hostID, "remote", remote, "error", err)
}

// admitParent decides an adoption request. It returns the host ID this
// daemon goes by under the parent, or "" with the HTTP status and reason to
// refuse with.
func (s *Service) admitParent(r *http.Request, credential string) (hostID string, status int, reason string) {
	stored, err := s.store.FederationParent()
	if err != nil {
		return "", http.StatusInternalServerError, err.Error()
	}
	if stored != nil && stored.Adopted && secretMatches(credential, stored.Credential) {
		return stored.HostID, 0, ""
	}
	key := adoptionKey(credential)
	parentID := strings.TrimSpace(r.Header.Get(parentIDHeader))
	if !ValidHostID(parentID) {
		parentID = ""
	}
	name := strings.TrimSpace(r.Header.Get(parentNameHeader))
	if len(name) > 80 {
		name = name[:80]
	}
	if s.joinToken != "" && secretMatches(r.Header.Get(joinTokenHeader), s.joinToken) {
		s.mu.Lock()
		connected := s.upstream != nil
		s.mu.Unlock()
		if connected {
			// The token admits a parent, not a takeover: while one parent is
			// connected, another holder of the token has to wait.
			slog.Warn("federation adoption refused: another parent is connected", "key", key, "parent_id", parentID, "remote", r.RemoteAddr)
			return "", http.StatusConflict, "this host is already adopted by a connected parent"
		}
		hostID, err := s.adopt(credential)
		if err != nil {
			return "", http.StatusInternalServerError, err.Error()
		}
		slog.Info("federation adoption accepted with join token", "host_id", hostID, "key", key, "parent_id", parentID, "remote", r.RemoteAddr)
		return hostID, 0, ""
	}
	if r.Header.Get(joinTokenHeader) != "" {
		slog.Warn("federation adoption presented a wrong join token", "key", key, "parent_id", parentID, "remote", r.RemoteAddr, "join_token_configured", s.joinToken != "")
	}
	s.mu.Lock()
	rejected := s.rejectedAdoptions[key]
	_, known := s.pendingAdoptions[key]
	if !rejected {
		s.pendingAdoptions[key] = pendingAdoption{credential: credential, parentID: parentID, name: name, remote: r.RemoteAddr}
	}
	s.mu.Unlock()
	if rejected {
		return "", http.StatusForbidden, "adoption was rejected by this host's operator"
	}
	if !known {
		slog.Info("federation adoption awaiting approval", "key", key, "parent_id", parentID, "name", name, "remote", r.RemoteAddr)
	}
	s.notifyAdoption(key)
	return "", http.StatusAccepted, "adoption is waiting for approval on this host"
}

// adopt makes credential the one parent this daemon trusts. A daemon keeps
// the ID it already had -- its own children know it by that ID -- so being
// adopted does not rename it.
func (s *Service) adopt(credential string) (string, error) {
	stored, err := s.store.FederationParent()
	if err != nil {
		return "", err
	}
	hostID := ""
	if stored != nil && stored.Adopted {
		hostID = stored.HostID
	}
	if hostID == "" {
		hostID = s.selfID()
	}
	if hostID == "" {
		if hostID, err = newHostID(s.name); err != nil {
			return "", err
		}
	}
	if err := s.store.SaveFederationParent(store.FederationParent{HostID: hostID, Credential: credential, Adopted: true}); err != nil {
		return "", err
	}
	return hostID, nil
}

func (s *Service) notifyAdoption(key string) {
	if s.notifications == nil {
		return
	}
	s.mu.Lock()
	request, ok := s.pendingAdoptions[key]
	s.mu.Unlock()
	if !ok {
		return
	}
	label := request.name
	if label == "" {
		label = request.parentID
	}
	if label == "" {
		label = "A Tandem host"
	}
	message := label + " wants to adopt this Tandem as its child. Accepting gives it the access your federation policy grants ancestors (admin by default)."
	if request.parentID != "" && request.parentID != label {
		message += " Host ID: " + request.parentID + "."
	}
	if request.remote != "" {
		message += " Connecting from " + request.remote + "."
	}
	s.notifications.Upsert(notifications.Notification{
		ID: adoptionNotificationPrefix + key, Severity: "attention",
		Title:   "Adopt this host under " + label + "?",
		Message: message,
		Actions: []notifications.Action{{ID: "accept", Label: "Accept", Primary: true}, {ID: "reject", Label: "Reject"}},
	})
}

// handleAdoptionAction answers an adoption notification.
func (s *Service) handleAdoptionAction(key, action string) error {
	s.mu.Lock()
	request, ok := s.pendingAdoptions[key]
	delete(s.pendingAdoptions, key)
	if ok && action == "reject" {
		s.rejectedAdoptions[key] = true
	}
	upstream := s.upstream
	s.mu.Unlock()
	if s.notifications != nil {
		s.notifications.Remove(adoptionNotificationPrefix + key)
	}
	if !ok {
		return errors.New("federation adoption request is no longer pending")
	}
	switch action {
	case "accept":
		hostID, err := s.adopt(request.credential)
		if err != nil {
			return err
		}
		// Accepting is the operator's explicit choice of parent, so a parent
		// connected under the old credential makes way.
		if upstream != nil {
			_ = upstream.conn.Close()
		}
		slog.Info("federation adoption accepted by operator", "host_id", hostID, "key", key, "parent_id", request.parentID)
		return nil
	case "reject":
		slog.Info("federation adoption rejected by operator", "key", key, "parent_id", request.parentID)
		return nil
	}
	return fmt.Errorf("unknown federation adoption action %q", action)
}

// RunChildLinks dials and adopts each configured child, reconnecting until
// ctx ends.
func (s *Service) RunChildLinks(ctx context.Context) {
	var wg sync.WaitGroup
	for _, child := range s.childLinks {
		wg.Add(1)
		go func(child ChildLink) {
			defer wg.Done()
			s.runAdoption(ctx, child)
		}(child)
	}
	wg.Wait()
}

// errAdoptionPending means the child is waiting for its operator to accept.
var errAdoptionPending = errors.New("waiting for the child's operator to accept the adoption (or configure a matching join token)")

func (s *Service) runAdoption(ctx context.Context, child ChildLink) {
	const maxBackoff = 30 * time.Second
	backoff := s.poll
	lastErr := ""
	for ctx.Err() == nil {
		connected, err := s.adoptOnce(ctx, child)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = s.poll
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		// A child that stays unreachable or pending would otherwise log the
		// same line every retry.
		if msg != lastErr {
			if err != nil {
				slog.Warn("federation child adoption failed; retrying", "url", child.URL, "error", err, "retry_in", backoff)
			} else {
				slog.Warn("federation adopted child disconnected; reconnecting", "url", child.URL)
			}
		} else {
			slog.Debug("federation child adoption retry", "url", child.URL, "error", msg)
		}
		lastErr = msg
		if !sleepContext(ctx, backoff) {
			return
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// adoptOnce dials child and, once it is admitted, serves the link until it
// ends. connected reports whether the child got as far as a session.
func (s *Service) adoptOnce(ctx context.Context, child ChildLink) (connected bool, err error) {
	adoption, err := s.store.FederationAdoption(child.URL)
	if err != nil {
		return false, err
	}
	if adoption == nil {
		credential, err := randomID()
		if err != nil {
			return false, err
		}
		adoption = &store.FederationAdoption{URL: child.URL, Credential: credential}
		if err := s.store.SaveFederationAdoption(*adoption); err != nil {
			return false, err
		}
		slog.Info("federation adoption credential generated", "url", child.URL, "key", adoptionKey(credential))
	}
	target, err := linkWebSocketURL(child.URL, AdoptPath)
	if err != nil {
		return false, err
	}
	head := http.Header{}
	head.Set("Authorization", "Bearer "+adoption.Credential)
	if child.JoinToken != "" {
		head.Set(joinTokenHeader, child.JoinToken)
	}
	if self := s.selfID(); self != "" {
		head.Set(parentIDHeader, self)
	}
	if s.name != "" {
		head.Set(parentNameHeader, s.name)
	}
	conn, resp, err := child.dialer.DialContext(ctx, target, head)
	if err != nil {
		if resp == nil {
			return false, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		reason := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusAccepted:
			return false, errAdoptionPending
		case http.StatusNotFound:
			return false, errors.New("the child does not accept adoption; its Tandem may predate v0.19")
		}
		return false, fmt.Errorf("child refused adoption (%s): %s", resp.Status, reason)
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	hello, ok := readHello(conn)
	if !ok {
		return false, errors.New("the child did not open the link with a hello")
	}
	peer, err := s.claimAdoptedChild(adoption, hello, child)
	if err != nil {
		_ = conn.WriteJSON(tunnelMessage{T: "error", Error: err.Error()})
		return false, err
	}
	slog.Info("federation child adopted", "url", child.URL, "host_id", hello.HostID)
	s.serveChild(conn, hello, peer)
	return true, nil
}

// claimAdoptedChild records the child an adoption reached under the ID it
// reported, and refuses an ID that already belongs to a different child. The
// record carries the adoption's credential, so every other path (Hosts,
// routing, forget) treats it like any accepted child.
func (s *Service) claimAdoptedChild(adoption *store.FederationAdoption, hello tunnelMessage, child ChildLink) (*store.FederationChild, error) {
	if !ValidHostID(hello.HostID) {
		return nil, fmt.Errorf("the child reported an invalid host ID %q", hello.HostID)
	}
	existing, err := s.store.FederationChild(hello.HostID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Credential != adoption.Credential && adoption.HostID != hello.HostID {
		return nil, fmt.Errorf("the child reports host ID %q, which already belongs to another child", hello.HostID)
	}
	if adoption.HostID != "" && adoption.HostID != hello.HostID {
		slog.Info("federation adopted child changed its host ID", "url", child.URL, "previous_host_id", adoption.HostID, "host_id", hello.HostID)
		s.forget(adoption.HostID)
	}
	name := child.Name
	if name == "" {
		name = hello.Name
	}
	now := time.Now().UnixMilli()
	peer := store.FederationChild{ID: hello.HostID, Name: name, Endpoint: child.URL, Credential: adoption.Credential, Status: "accepted", RequestedAt: now, AcceptedAt: now}
	if existing != nil {
		peer.RequestedAt, peer.AcceptedAt, peer.LastSeenAt = existing.RequestedAt, existing.AcceptedAt, existing.LastSeenAt
		if peer.AcceptedAt == 0 {
			peer.AcceptedAt = now
		}
	}
	if err := s.store.UpsertFederationChild(peer); err != nil {
		return nil, err
	}
	if adoption.HostID != hello.HostID {
		adoption.HostID = hello.HostID
		if err := s.store.SaveFederationAdoption(*adoption); err != nil {
			return nil, err
		}
	}
	return &peer, nil
}
