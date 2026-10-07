// Package mcpauth lets the daemon, rather than each agent harness, hold the
// OAuth authorization for remote (HTTP) MCP servers.
//
// A server that answers an unauthenticated request with 401 is marked as
// needing sign-in and surfaced as a system notification. The user authorizes
// once from the Tandem UI (authorization code + PKCE, with dynamic client
// registration), and the daemon keeps the tokens in an owner-only file under
// TANDEM_HOME. Agents are then given a daemon-local proxy URL instead of the
// server's own: the proxy attaches a fresh access token to every request and
// refreshes it when it expires, so the token never enters an agent process,
// transcript, or MCP config file, and a long session outlives its token.
// See docs/mcp-auth.md.
package mcpauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/notifications"
)

const (
	// NotificationPrefix namespaces this service's system notifications.
	NotificationPrefix = "mcp-auth:"
	// CallbackPath receives the OAuth authorization response. It is reached
	// by the user's browser without the daemon bearer token, so it is
	// authenticated only by the single-use state parameter.
	CallbackPath = "/mcp/oauth/callback"
	// ProxyPrefix is where agents reach an authorized server.
	ProxyPrefix = "/mcp-proxy/"
	// SessionHeader carries the agent session a proxy capability was minted
	// for.
	SessionHeader = "X-Tandem-Session"

	protocolVersion = "2025-06-18"
	stateFileName   = "mcp-auth.json"

	probeTimeout    = 5 * time.Second
	probeFreshness  = 30 * time.Minute
	errorFreshness  = time.Minute
	pendingLifetime = 15 * time.Minute
	refreshSkew     = 60 * time.Second
	syncInterval    = 15 * time.Second
	maxProxyBody    = 16 << 20
)

// States reported for a tracked server.
const (
	StateChecking   = "checking"
	StateOpen       = "open"
	StateNeedsAuth  = "needs_auth"
	StateAuthorized = "authorized"
	StateStatic     = "static"
	StateError      = "error"
)

type Options struct {
	// Home is TANDEM_HOME; tokens persist in Home/mcp-auth.json.
	Home string
	// Token is the daemon bearer token; proxy capabilities derive from it.
	Token string
	// ProxyOrigin is the daemon origin agents use to reach the proxy.
	ProxyOrigin string
	// Servers returns the globally configured MCP servers. It is re-read
	// periodically so `tandem mcp add` surfaces a sign-in prompt without a
	// spawn or restart.
	Servers    func() (map[string]config.MCPServer, error)
	Center     *notifications.Center
	HTTPClient *http.Client
	Now        func() time.Time
	// OnChange is called after any change to the List snapshot.
	OnChange func()
}

// Status is one server row in the UI's MCP servers panel.
type Status struct {
	Key          string     `json:"key"`
	Name         string     `json:"name"`
	URL          string     `json:"url"`
	State        string     `json:"state"`
	Error        string     `json:"error,omitempty"`
	Scope        string     `json:"scope,omitempty"`
	AuthorizedAt *time.Time `json:"authorizedAt,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	Refreshable  bool       `json:"refreshable,omitempty"`
	// Configured is false for a server known only from a project's
	// .tandem/.config.yml, seen when an agent was started there.
	Configured bool `json:"configured"`
}

// Resolution is how one configured server should be handed to an agent.
type Resolution struct {
	URL     string
	Headers map[string]string
	// AuthRequired means the server needs sign-in and Tandem holds no token,
	// so it should be withheld from the agent rather than handed over to fail.
	AuthRequired bool
	Key          string
}

type server struct {
	key, name, url string
	cfg            config.MCPServer
	configured     bool
	static         bool
	state          string
	err            string
	challenge      challenge
	checkedAt      time.Time
	probing        chan struct{}
	dismissed      bool
}

type pendingAuth struct {
	key, redirectURI, verifier string
	discovery                  discovery
	client                     clientRecord
	created                    time.Time
}

type Service struct {
	opts      Options
	mu        sync.Mutex
	servers   map[string]*server
	state     fileState
	pending   map[string]*pendingAuth
	refreshMu sync.Mutex
}

func New(opts Options) (*Service, error) {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Service{opts: opts, servers: map[string]*server{}, pending: map[string]*pendingAuth{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Key identifies a server by its URL, not its name: a project override may
// reuse a name for a different server, and two names may share one server.
func Key(serverURL string) string {
	sum := sha256.Sum256([]byte(canonicalURL(serverURL)))
	return hex.EncodeToString(sum[:8])
}

func canonicalURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String()
}

// Start probes the configured servers and keeps watching the configuration
// for newly added ones until ctx ends.
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()
		for {
			s.Sync(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Sync reconciles tracked servers with the global configuration and probes
// any that are new or whose last check has gone stale.
func (s *Service) Sync(ctx context.Context) {
	if s.opts.Servers == nil {
		return
	}
	configured, err := s.opts.Servers()
	if err != nil {
		slog.Warn("mcp auth: read configured MCP servers failed", "err", err)
		return
	}
	seen := map[string]bool{}
	var stale []string
	changed := false
	s.mu.Lock()
	for name, cfg := range configured {
		if cfg.Type != "http" || strings.TrimSpace(cfg.URL) == "" {
			continue
		}
		srv, added := s.trackLocked(name, cfg)
		seen[srv.key] = true
		if !srv.configured {
			srv.configured, changed = true, true
		}
		changed = changed || added
		if s.needsProbeLocked(srv) {
			stale = append(stale, srv.key)
		}
	}
	for key, srv := range s.servers {
		if srv.configured && !seen[key] {
			slog.Info("mcp auth: server removed from configuration", "server", srv.name, "key", key)
			delete(s.servers, key)
			s.opts.Center.Remove(NotificationPrefix + key)
			changed = true
		}
	}
	s.mu.Unlock()
	if changed {
		s.changed()
	}
	for _, key := range stale {
		go s.ensureProbed(ctx, key)
	}
}

// trackLocked records name/config for a server and reports whether it was
// newly added.
func (s *Service) trackLocked(name string, cfg config.MCPServer) (*server, bool) {
	key := Key(cfg.URL)
	srv, ok := s.servers[key]
	if !ok {
		srv = &server{key: key, state: StateChecking}
		s.servers[key] = srv
	}
	srv.name, srv.url, srv.cfg = name, strings.TrimSpace(cfg.URL), cfg
	srv.static = hasAuthorization(cfg.Headers)
	if srv.static {
		srv.state = StateStatic
	}
	if record := s.state.Servers[key]; record != nil && record.Name != name {
		record.Name = name
	}
	return srv, !ok
}

func (s *Service) needsProbeLocked(srv *server) bool {
	if srv.static || srv.probing != nil {
		return false
	}
	if record := s.state.Servers[srv.key]; record != nil && record.Token != nil {
		return false
	}
	if srv.checkedAt.IsZero() {
		return true
	}
	freshness := probeFreshness
	if srv.state == StateError {
		freshness = errorFreshness
	}
	return s.opts.Now().Sub(srv.checkedAt) > freshness
}

// Resolve decides how an agent session should be given one configured MCP
// server. A server that needs no authorization, or carries its own static
// Authorization header, is passed through untouched; an authorized server is
// replaced with the daemon proxy; an unauthorized one is withheld.
func (s *Service) Resolve(ctx context.Context, name string, cfg config.MCPServer, sessionID string) Resolution {
	passthrough := Resolution{URL: cfg.URL, Headers: cfg.Headers}
	if cfg.Type != "http" || strings.TrimSpace(cfg.URL) == "" {
		return passthrough
	}
	s.mu.Lock()
	srv, added := s.trackLocked(name, cfg)
	key, static := srv.key, srv.static
	s.mu.Unlock()
	if added {
		s.changed()
	}
	passthrough.Key = key
	if static {
		return passthrough
	}
	if s.hasToken(key) {
		return s.proxied(key, cfg, sessionID)
	}
	state := s.ensureProbed(ctx, key)
	if s.hasToken(key) {
		return s.proxied(key, cfg, sessionID)
	}
	if state == StateNeedsAuth {
		slog.Info("mcp auth: withholding unauthorized MCP server from session", "session", sessionID, "server", name, "key", key)
		return Resolution{AuthRequired: true, Key: key}
	}
	return passthrough
}

func (s *Service) proxied(key string, cfg config.MCPServer, sessionID string) Resolution {
	headers := map[string]string{}
	for name, value := range cfg.Headers {
		headers[name] = value
	}
	headers["Authorization"] = "Bearer " + s.capability(sessionID)
	headers[SessionHeader] = sessionID
	return Resolution{URL: strings.TrimRight(s.opts.ProxyOrigin, "/") + ProxyPrefix + key, Headers: headers, Key: key}
}

func (s *Service) capability(sessionID string) string {
	mac := hmac.New(sha256.New, []byte(s.opts.Token))
	mac.Write([]byte("tandem-mcp-proxy\x00" + sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) hasToken(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.state.Servers[key]
	return record != nil && record.Token != nil
}

// ensureProbed returns the server's state, probing it first when it has not
// been checked recently. Concurrent callers share one probe.
func (s *Service) ensureProbed(ctx context.Context, key string) string {
	s.mu.Lock()
	srv := s.servers[key]
	if srv == nil {
		s.mu.Unlock()
		return ""
	}
	if wait := srv.probing; wait != nil {
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return srv.state
	}
	if !s.needsProbeLocked(srv) {
		state := srv.state
		s.mu.Unlock()
		return state
	}
	done := make(chan struct{})
	srv.probing = done
	cfg := srv.cfg
	s.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	state, ch, err := s.probe(probeCtx, cfg)
	cancel()

	s.mu.Lock()
	previous := srv.state
	srv.state, srv.challenge, srv.checkedAt, srv.probing = state, ch, s.opts.Now(), nil
	srv.err = ""
	if err != nil {
		srv.err = err.Error()
	}
	if record := s.state.Servers[key]; record != nil && record.Token != nil {
		srv.state = StateAuthorized
	}
	state = srv.state
	s.refreshNotificationLocked(srv)
	s.mu.Unlock()
	close(done)
	if previous != state {
		slog.Info("mcp auth: probed MCP server", "server", srv.name, "key", key, "url", srv.url, "state", state, "err", srv.err)
	}
	s.changed()
	return state
}

// probe sends an unauthenticated MCP initialize request to learn whether the
// server demands authorization.
func (s *Service) probe(ctx context.Context, cfg config.MCPServer) (string, challenge, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + protocolVersion + `","capabilities":{},"clientInfo":{"name":"tandem-auth-probe","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, strings.NewReader(body))
	if err != nil {
		return StateError, challenge{}, err
	}
	for name, value := range cfg.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return StateError, challenge{}, fmt.Errorf("reach MCP server: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	authenticate := resp.Header.Get("WWW-Authenticate")
	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(authenticate), "bearer"):
		return StateNeedsAuth, parseChallenge(authenticate), nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if sessionID := resp.Header.Get("Mcp-Session-Id"); sessionID != "" {
			s.closeProbeSession(cfg, sessionID)
		}
		return StateOpen, challenge{}, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500:
		return StateError, challenge{}, fmt.Errorf("MCP server answered %s", resp.Status)
	default:
		// Reachable and not asking for credentials (e.g. 400/405 to a probe
		// it did not like); let the agent's own client take it from here.
		return StateOpen, challenge{}, nil
	}
}

func (s *Service) closeProbeSession(cfg config.MCPServer, sessionID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.URL, nil)
		if err != nil {
			return
		}
		for name, value := range cfg.Headers {
			req.Header.Set(name, value)
		}
		req.Header.Set("Mcp-Session-Id", sessionID)
		if resp, err := s.opts.HTTPClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// refreshNotificationLocked raises or clears the sign-in notification for a
// server to match its current state.
func (s *Service) refreshNotificationLocked(srv *server) {
	if s.opts.Center == nil {
		return
	}
	id := NotificationPrefix + srv.key
	record := s.state.Servers[srv.key]
	authorized := record != nil && record.Token != nil
	if srv.state != StateNeedsAuth || authorized || srv.dismissed {
		// A success notice is left for the user to dismiss.
		if existing := s.findNotification(id); existing != nil && existing.Severity == "success" {
			return
		}
		s.opts.Center.Remove(id)
		return
	}
	title := fmt.Sprintf("MCP server %q needs sign-in", srv.name)
	message := fmt.Sprintf("%s requires authorization. Agents won't get its tools until you authorize Tandem.", srv.url)
	if record != nil && !record.AuthorizedAt.IsZero() {
		title = fmt.Sprintf("MCP server %q sign-in expired", srv.name)
		message = fmt.Sprintf("Tandem's authorization for %s is no longer accepted. Authorize again to restore its tools.", srv.url)
	}
	s.opts.Center.Upsert(notifications.Notification{
		ID: id, Severity: "attention", Title: title, Message: message,
		Actions: []notifications.Action{{ID: "authorize", Label: "Authorize", Primary: true}, {ID: "dismiss", Label: "Dismiss"}},
	})
}

func (s *Service) findNotification(id string) *notifications.Notification {
	for _, item := range s.opts.Center.List() {
		if item.ID == id {
			return &item
		}
	}
	return nil
}

// HandleAction implements the generic system-notification actions. The
// Authorize button is handled by the UI itself (it must open the sign-in page
// from the click), so only Dismiss arrives here.
func (s *Service) HandleAction(_ context.Context, id, action string) (string, error) {
	key := strings.TrimPrefix(id, NotificationPrefix)
	switch action {
	case "dismiss":
		s.mu.Lock()
		if srv := s.servers[key]; srv != nil {
			srv.dismissed = true
		}
		s.mu.Unlock()
		if s.opts.Center != nil {
			s.opts.Center.Remove(id)
		}
		return "", nil
	case "authorize":
		return "", errors.New("open the authorization from the Tandem UI")
	}
	return "", fmt.Errorf("unknown MCP auth action %q", action)
}

// List snapshots every tracked HTTP server for the UI.
func (s *Service) List() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.servers))
	for _, srv := range s.servers {
		status := Status{Key: srv.key, Name: srv.name, URL: srv.url, State: srv.state, Error: srv.err, Configured: srv.configured}
		if record := s.state.Servers[srv.key]; record != nil && record.Token != nil && !srv.static {
			status.State = StateAuthorized
			status.Error = ""
			status.Scope = record.Token.Scope
			status.Refreshable = record.Token.RefreshToken != ""
			if !record.AuthorizedAt.IsZero() {
				at := record.AuthorizedAt
				status.AuthorizedAt = &at
			}
			if !record.Token.ExpiresAt.IsZero() {
				at := record.Token.ExpiresAt
				status.ExpiresAt = &at
			}
		} else if record != nil && record.LastError != "" && srv.state == StateNeedsAuth {
			status.Error = record.LastError
		}
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Recheck forgets a server's cached probe and checks it again.
func (s *Service) Recheck(ctx context.Context, key string) error {
	s.mu.Lock()
	srv := s.servers[key]
	if srv == nil {
		s.mu.Unlock()
		return fmt.Errorf("unknown MCP server %q", key)
	}
	srv.checkedAt = time.Time{}
	srv.dismissed = false
	s.mu.Unlock()
	s.ensureProbed(ctx, key)
	return nil
}

// Begin starts an authorization for a server and returns the URL the user's
// browser should open. redirectOrigin is the origin the Tandem UI is served
// from; the authorization server sends the browser back to its callback.
func (s *Service) Begin(ctx context.Context, key, redirectOrigin string) (string, error) {
	redirectURI, err := callbackURL(redirectOrigin)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	srv := s.servers[key]
	if srv == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("unknown MCP server %q", key)
	}
	serverURL, ch, cfg, name := srv.url, srv.challenge, srv.cfg, srv.name
	s.mu.Unlock()
	if ch.ResourceMetadata == "" {
		// The challenge may be stale or missing (e.g. a token was revoked
		// while the daemon ran); ask the server again.
		if state, fresh, probeErr := s.probe(ctx, cfg); probeErr == nil && state == StateNeedsAuth {
			ch = fresh
		}
	}
	d, err := s.discover(ctx, serverURL, ch)
	if err != nil {
		s.recordError(key, err)
		return "", fmt.Errorf("discover OAuth configuration for %s: %w", name, err)
	}
	if cfg.OAuth != nil && len(cfg.OAuth.Scopes) > 0 {
		d.Scopes = cfg.OAuth.Scopes
	}
	client, err := s.clientFor(ctx, key, cfg, d, redirectURI)
	if err != nil {
		s.recordError(key, err)
		return "", err
	}
	verifier, state := randomToken(48), randomToken(24)
	authURL, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("invalid authorization endpoint: %w", err)
	}
	query := authURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", client.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("code_challenge", pkceChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	query.Set("resource", d.Resource)
	if len(d.Scopes) > 0 {
		query.Set("scope", strings.Join(d.Scopes, " "))
	}
	authURL.RawQuery = query.Encode()
	s.mu.Lock()
	now := s.opts.Now()
	for id, pending := range s.pending {
		if now.Sub(pending.created) > pendingLifetime {
			delete(s.pending, id)
		}
	}
	s.pending[state] = &pendingAuth{key: key, redirectURI: redirectURI, verifier: verifier, discovery: d, client: client, created: now}
	s.mu.Unlock()
	slog.Info("mcp auth: authorization started", "server", name, "key", key, "issuer", d.Issuer, "redirect_uri", redirectURI, "scopes", strings.Join(d.Scopes, " "))
	return authURL.String(), nil
}

func callbackURL(origin string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
		return "", fmt.Errorf("invalid UI origin %q", origin)
	}
	return parsed.Scheme + "://" + parsed.Host + CallbackPath, nil
}

// clientFor returns the OAuth client to authorize with: one pinned in config,
// one registered earlier for this redirect URI and issuer, or a new dynamic
// registration.
func (s *Service) clientFor(ctx context.Context, key string, cfg config.MCPServer, d discovery, redirectURI string) (clientRecord, error) {
	if cfg.OAuth != nil && cfg.OAuth.ClientID != "" {
		return clientRecord{ClientID: cfg.OAuth.ClientID, ClientSecret: cfg.OAuth.ClientSecret, Issuer: d.Issuer, RedirectURI: redirectURI}, nil
	}
	s.mu.Lock()
	if record := s.state.Servers[key]; record != nil {
		for _, client := range record.Clients {
			if client.RedirectURI == redirectURI && client.Issuer == d.Issuer {
				s.mu.Unlock()
				return client, nil
			}
		}
	}
	s.mu.Unlock()
	client, err := s.register(ctx, d, redirectURI)
	if err != nil {
		return clientRecord{}, err
	}
	client.RedirectURI = redirectURI
	s.mu.Lock()
	record := s.recordLocked(key)
	record.Clients = append(record.Clients, client)
	err = s.saveLocked()
	s.mu.Unlock()
	slog.Info("mcp auth: registered OAuth client", "key", key, "issuer", d.Issuer, "client_id", client.ClientID)
	return client, err
}

// ServeCallback completes an authorization: it validates the state, redeems
// the code, and stores the tokens.
func (s *Service) ServeCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	s.mu.Lock()
	pending := s.pending[query.Get("state")]
	if pending != nil {
		delete(s.pending, query.Get("state"))
	}
	name := ""
	if pending != nil {
		if srv := s.servers[pending.key]; srv != nil {
			name = srv.name
		}
	}
	s.mu.Unlock()
	if pending == nil || s.opts.Now().Sub(pending.created) > pendingLifetime {
		slog.Warn("mcp auth: callback with unknown or expired state")
		writePage(w, http.StatusBadRequest, "Authorization link expired", "This sign-in attempt is unknown or has expired. Start it again from Tandem.")
		return
	}
	if code := query.Get("error"); code != "" {
		reason := code
		if description := query.Get("error_description"); description != "" {
			reason += ": " + description
		}
		s.recordError(pending.key, errors.New(reason))
		slog.Warn("mcp auth: authorization denied", "server", name, "key", pending.key, "error", reason)
		writePage(w, http.StatusBadRequest, "Authorization was not granted", fmt.Sprintf("%s did not authorize Tandem: %s", name, reason))
		return
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", query.Get("code"))
	form.Set("redirect_uri", pending.redirectURI)
	form.Set("code_verifier", pending.verifier)
	form.Set("resource", pending.discovery.Resource)
	token, err := s.exchange(r.Context(), pending.discovery.TokenEndpoint, form, pending.client)
	if err != nil {
		s.recordError(pending.key, err)
		slog.Warn("mcp auth: code exchange failed", "server", name, "key", pending.key, "err", err)
		writePage(w, http.StatusBadGateway, "Authorization failed", err.Error())
		return
	}
	s.mu.Lock()
	record := s.recordLocked(pending.key)
	d, client := pending.discovery, pending.client
	record.Token, record.Discovery, record.TokenClient = &token, &d, &client
	record.AuthorizedAt, record.LastError = s.opts.Now(), ""
	saveErr := s.saveLocked()
	if srv := s.servers[pending.key]; srv != nil {
		srv.state, srv.err, srv.dismissed = StateAuthorized, "", false
	}
	if s.opts.Center != nil {
		s.opts.Center.Upsert(notifications.Notification{
			ID: NotificationPrefix + pending.key, Severity: "success",
			Title:   fmt.Sprintf("MCP server %q authorized", name),
			Message: "New agent sessions get its tools. Restart a running session's harness to give it access.",
			Actions: []notifications.Action{{ID: "dismiss", Label: "Dismiss"}},
		})
	}
	s.mu.Unlock()
	s.changed()
	if saveErr != nil {
		slog.Error("mcp auth: persist tokens failed", "key", pending.key, "err", saveErr)
		writePage(w, http.StatusInternalServerError, "Authorization could not be saved", saveErr.Error())
		return
	}
	slog.Info("mcp auth: authorized MCP server", "server", name, "key", pending.key, "scope", token.Scope, "expires_at", token.ExpiresAt, "refreshable", token.RefreshToken != "")
	writePage(w, http.StatusOK, "Tandem is authorized", fmt.Sprintf("Tandem can now use the %s MCP server. You can close this tab.", name))
}

// SignOut forgets a server's tokens. The registered client is kept so a later
// authorization does not register again.
func (s *Service) SignOut(key string) error {
	s.mu.Lock()
	record := s.state.Servers[key]
	if record == nil || record.Token == nil {
		s.mu.Unlock()
		return nil
	}
	record.Token, record.AuthorizedAt = nil, time.Time{}
	err := s.saveLocked()
	if srv := s.servers[key]; srv != nil {
		srv.state, srv.dismissed, srv.checkedAt = StateNeedsAuth, true, s.opts.Now()
		if s.opts.Center != nil {
			s.opts.Center.Remove(NotificationPrefix + key)
		}
	}
	s.mu.Unlock()
	slog.Info("mcp auth: signed out of MCP server", "key", key)
	s.changed()
	return err
}

func (s *Service) recordError(key string, err error) {
	s.mu.Lock()
	s.recordLocked(key).LastError = err.Error()
	_ = s.saveLocked()
	s.mu.Unlock()
	s.changed()
}

// accessToken returns a usable access token, refreshing first when the
// current one is about to expire.
func (s *Service) accessToken(ctx context.Context, key string) (string, error) {
	s.mu.Lock()
	record := s.state.Servers[key]
	if record == nil || record.Token == nil {
		s.mu.Unlock()
		return "", errNotAuthorized
	}
	token := *record.Token
	s.mu.Unlock()
	if !token.ExpiresAt.IsZero() && token.RefreshToken != "" && s.opts.Now().Add(refreshSkew).After(token.ExpiresAt) {
		return s.refresh(ctx, key, token.AccessToken)
	}
	return token.AccessToken, nil
}

var errNotAuthorized = errors.New("MCP server is not authorized")

// refresh exchanges the refresh token for a new access token unless another
// request already replaced stale. A refresh the authorization server rejects
// outright revokes the stored authorization.
func (s *Service) refresh(ctx context.Context, key, stale string) (string, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	record := s.state.Servers[key]
	if record == nil || record.Token == nil {
		s.mu.Unlock()
		return "", errNotAuthorized
	}
	if record.Token.AccessToken != stale {
		current := record.Token.AccessToken
		s.mu.Unlock()
		return current, nil
	}
	current, d, client := *record.Token, record.Discovery, record.TokenClient
	s.mu.Unlock()
	if current.RefreshToken == "" || d == nil || client == nil {
		s.revoke(key, "the access token expired and cannot be refreshed")
		return "", errNotAuthorized
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", current.RefreshToken)
	form.Set("resource", d.Resource)
	started := s.opts.Now()
	token, err := s.exchange(ctx, d.TokenEndpoint, form, *client)
	if err != nil {
		if invalidGrant(err) {
			s.revoke(key, err.Error())
			return "", errNotAuthorized
		}
		slog.Warn("mcp auth: token refresh failed", "key", key, "err", err)
		return "", err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = current.RefreshToken
	}
	s.mu.Lock()
	if record := s.state.Servers[key]; record != nil {
		record.Token, record.LastError = &token, ""
	}
	saveErr := s.saveLocked()
	s.mu.Unlock()
	if saveErr != nil {
		slog.Error("mcp auth: persist refreshed token failed", "key", key, "err", saveErr)
	}
	slog.Info("mcp auth: refreshed access token", "key", key, "expires_at", token.ExpiresAt, "duration_ms", s.opts.Now().Sub(started).Milliseconds())
	s.changed()
	return token.AccessToken, nil
}

// revoke drops a token the server no longer honors and asks the user to sign
// in again.
func (s *Service) revoke(key, reason string) {
	s.mu.Lock()
	if record := s.state.Servers[key]; record != nil {
		record.Token, record.LastError = nil, reason
	}
	_ = s.saveLocked()
	if srv := s.servers[key]; srv != nil {
		srv.state, srv.dismissed, srv.checkedAt = StateNeedsAuth, false, s.opts.Now()
		if s.opts.Center != nil {
			s.opts.Center.Remove(NotificationPrefix + key)
		}
		s.refreshNotificationLocked(srv)
	}
	s.mu.Unlock()
	slog.Warn("mcp auth: authorization revoked", "key", key, "reason", reason)
	s.changed()
}

// ServeProxy forwards an agent's MCP request to the authorized server with a
// current access token, refreshing and retrying once on a 401.
func (s *Service) ServeProxy(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, ProxyPrefix)
	sessionID := r.Header.Get(SessionHeader)
	want := "Bearer " + s.capability(sessionID)
	if sessionID == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.mu.Lock()
	record := s.state.Servers[key]
	upstream := ""
	if record != nil {
		upstream = record.URL
	}
	if srv := s.servers[key]; srv != nil {
		upstream = srv.url
	}
	s.mu.Unlock()
	if upstream == "" || strings.Contains(key, "/") {
		writeJSONError(w, http.StatusNotFound, "unknown MCP server")
		return
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBody))
		if err != nil {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
	}
	started := s.opts.Now()
	token, err := s.accessToken(r.Context(), key)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "Tandem is not authorized for this MCP server; authorize it from the Tandem UI")
		return
	}
	resp, err := s.forward(r, upstream, token, body)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		slog.Info("mcp auth: upstream rejected access token; refreshing", "key", key, "session", sessionID)
		token, err = s.refresh(r.Context(), key, token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "Tandem's authorization for this MCP server expired; authorize it again from the Tandem UI")
			return
		}
		resp, err = s.forward(r, upstream, token, body)
		if err == nil && resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			s.revoke(key, "the server rejected a freshly refreshed access token")
			writeJSONError(w, http.StatusUnauthorized, "the MCP server rejected Tandem's authorization; authorize it again from the Tandem UI")
			return
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			slog.Warn("mcp auth: proxy upstream request failed", "key", key, "session", sessionID, "method", r.Method, "err", err)
		}
		writeJSONError(w, http.StatusBadGateway, "MCP server unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn("mcp auth: proxy upstream error status", "key", key, "session", sessionID, "method", r.Method, "status", resp.StatusCode)
	} else {
		slog.Debug("mcp auth: proxied request", "key", key, "session", sessionID, "method", r.Method, "status", resp.StatusCode, "duration_ms", s.opts.Now().Sub(started).Milliseconds())
	}
	header := w.Header()
	for name, values := range resp.Header {
		if skipResponseHeader(name) {
			continue
		}
		header[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	controller := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

func (s *Service) forward(r *http.Request, upstream, token string, body []byte) (*http.Response, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	for name, values := range r.Header {
		if skipRequestHeader(name) {
			continue
		}
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return s.opts.HTTPClient.Do(req)
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

func skipRequestHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return hopHeaders[name] || name == "Authorization" || name == http.CanonicalHeaderKey(SessionHeader) ||
		name == "Host" || name == "Cookie" || name == "Content-Length" || name == "Accept-Encoding"
}

func skipResponseHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return hopHeaders[name] || name == "Www-Authenticate" || name == "Set-Cookie" || name == "Content-Length"
}

func hasAuthorization(headers map[string]string) bool {
	for name := range headers {
		if strings.EqualFold(name, "Authorization") {
			return true
		}
	}
	return false
}

func (s *Service) changed() {
	if s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writePage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	closeScript := ""
	if status == http.StatusOK {
		closeScript = `<script>setTimeout(function(){window.close()},1500)</script>`
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<style>body{font:15px/1.6 system-ui,sans-serif;max-width:36rem;margin:5rem auto;padding:0 1rem;color:#ddd;background:#14161a}h1{font-size:1.3rem}</style>
<h1>%s</h1><p>%s</p>%s`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message), closeScript)
}

// Persistence. Tokens live in an owner-only JSON file rather than the SQLite
// store so that copying or inspecting tandem.db never exposes credentials.

type fileState struct {
	Servers map[string]*record `json:"servers"`
}

type record struct {
	URL          string         `json:"url"`
	Name         string         `json:"name,omitempty"`
	Clients      []clientRecord `json:"clients,omitempty"`
	Token        *tokenRecord   `json:"token,omitempty"`
	TokenClient  *clientRecord  `json:"tokenClient,omitempty"`
	Discovery    *discovery     `json:"discovery,omitempty"`
	AuthorizedAt time.Time      `json:"authorizedAt,omitzero"`
	LastError    string         `json:"lastError,omitempty"`
}

type clientRecord struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Issuer       string `json:"issuer,omitempty"`
	RedirectURI  string `json:"redirectUri,omitempty"`
}

type tokenRecord struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	TokenType    string    `json:"tokenType,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt,omitzero"`
}

func (s *Service) path() string { return filepath.Join(s.opts.Home, stateFileName) }

func (s *Service) load() error {
	s.state = fileState{Servers: map[string]*record{}}
	if s.opts.Home == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", s.path(), err)
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return fmt.Errorf("parse %s: %w", s.path(), err)
	}
	if s.state.Servers == nil {
		s.state.Servers = map[string]*record{}
	}
	for key, record := range s.state.Servers {
		if record == nil || record.URL == "" {
			delete(s.state.Servers, key)
		}
	}
	return nil
}

func (s *Service) recordLocked(key string) *record {
	rec := s.state.Servers[key]
	if rec == nil {
		rec = &record{}
		s.state.Servers[key] = rec
	}
	if srv := s.servers[key]; srv != nil {
		rec.URL, rec.Name = srv.url, srv.name
	}
	return rec
}

func (s *Service) saveLocked() error {
	if s.opts.Home == "" {
		return nil
	}
	persisted := fileState{Servers: maps.Clone(s.state.Servers)}
	for key, rec := range persisted.Servers {
		if rec.URL == "" {
			delete(persisted.Servers, key)
		}
	}
	raw, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.opts.Home, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.opts.Home, stateFileName+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path())
}
