package mcpauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/notifications"
)

// fakeProvider is an MCP server that is its own OAuth authorization server,
// shaped like DeveloperHub's: 401 + resource_metadata, RFC 9728/8414
// metadata, dynamic registration, PKCE, and refresh tokens.
type fakeProvider struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	// valid is the access token /mcp currently accepts.
	valid         string
	issued        int
	challenges    map[string]string // code -> PKCE challenge
	registrations int
	lastAuthHdr   string
	lastResource  string
	refreshFails  bool
}

func newFakeProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{t: t, challenges: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.lastAuthHdr = r.Header.Get("Authorization")
		ok := p.valid != "" && p.lastAuthHdr == "Bearer "+p.valid
		p.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+p.server.URL+`/.well-known/oauth-protected-resource"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "upstream-session")
		_, _ = w.Write([]byte(`{"echo":` + string(body) + `,"session":"` + r.Header.Get("Mcp-Session-Id") + `"}`))
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": p.server.URL + "/", "authorization_servers": []string{p.server.URL + "/"}, "scopes_supported": []string{"editor"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.server.URL + "/", "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token",
			"registration_endpoint": p.server.URL + "/register", "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["token_endpoint_auth_method"] != "none" {
			t.Errorf("registration auth method = %v", body["token_endpoint_auth_method"])
		}
		p.mu.Lock()
		p.registrations++
		p.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "client-1"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.lastResource = r.Form.Get("resource")
		if r.Form.Get("client_id") != "client-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			want, ok := p.challenges[r.Form.Get("code")]
			if !ok || pkceChallenge(r.Form.Get("code_verifier")) != want {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if p.refreshFails || r.Form.Get("refresh_token") != "refresh-1" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
		}
		p.issued++
		p.valid = "access-" + string(rune('0'+p.issued))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": p.valid, "token_type": "Bearer", "refresh_token": "refresh-1", "expires_in": 3600, "scope": "editor"})
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

// authorize plays the user's browser at the authorization endpoint and
// returns the callback query the provider redirects to.
func (p *fakeProvider) authorize(authURL string) url.Values {
	parsed, err := url.Parse(authURL)
	if err != nil {
		p.t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "client-1" || q.Get("scope") != "editor" || q.Get("resource") != p.server.URL+"/" {
		p.t.Fatalf("unexpected authorize query: %v", q)
	}
	p.mu.Lock()
	p.challenges["code-1"] = q.Get("code_challenge")
	p.mu.Unlock()
	return url.Values{"code": {"code-1"}, "state": {q.Get("state")}}
}

func newService(t *testing.T, home string, servers map[string]config.MCPServer) (*Service, *notifications.Center) {
	t.Helper()
	center := notifications.New()
	s, err := New(Options{Home: home, Token: "daemon-token", ProxyOrigin: "http://127.0.0.1:7717", Center: center,
		Servers: func() (map[string]config.MCPServer, error) { return servers, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s, center
}

func callback(t *testing.T, s *Service, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeCallback(rec, httptest.NewRequest(http.MethodGet, CallbackPath+"?"+query.Encode(), nil))
	return rec
}

func TestAuthorizeProxyAndRefresh(t *testing.T) {
	provider := newFakeProvider(t)
	home := t.TempDir()
	cfg := config.MCPServer{Type: "http", URL: provider.server.URL + "/mcp"}
	s, center := newService(t, home, map[string]config.MCPServer{"developerhub": cfg})
	ctx := context.Background()

	s.Sync(ctx)
	key := Key(cfg.URL)
	if state := s.ensureProbed(ctx, key); state != StateNeedsAuth {
		t.Fatalf("probe state = %q, want needs_auth", state)
	}
	if items := center.List(); len(items) != 1 || items[0].ID != NotificationPrefix+key || items[0].Actions[0].ID != "authorize" {
		t.Fatalf("notifications = %+v", items)
	}
	if res := s.Resolve(ctx, "developerhub", cfg, "agent-1"); !res.AuthRequired {
		t.Fatalf("unauthorized server resolved to %+v", res)
	}

	authURL, err := s.Begin(ctx, key, "http://127.0.0.1:7717")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authURL, url.QueryEscape("http://127.0.0.1:7717"+CallbackPath)) {
		t.Fatalf("authorize URL lacks callback redirect: %s", authURL)
	}
	if rec := callback(t, s, provider.authorize(authURL)); rec.Code != http.StatusOK {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	if provider.lastResource != provider.server.URL+"/" {
		t.Fatalf("token resource = %q", provider.lastResource)
	}
	if items := center.List(); len(items) != 1 || items[0].Severity != "success" {
		t.Fatalf("expected success notification, got %+v", items)
	}
	info, err := os.Stat(filepath.Join(home, stateFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, %v", info, err)
	}
	if list := s.List(); len(list) != 1 || list[0].State != StateAuthorized || list[0].ExpiresAt == nil {
		t.Fatalf("list = %+v", list)
	}

	res := s.Resolve(ctx, "developerhub", cfg, "agent-1")
	if res.AuthRequired || res.URL != "http://127.0.0.1:7717"+ProxyPrefix+key || res.Headers[SessionHeader] != "agent-1" {
		t.Fatalf("authorized resolution = %+v", res)
	}
	if strings.Contains(res.Headers["Authorization"], "access-") {
		t.Fatal("upstream access token leaked into agent configuration")
	}

	proxy := httptest.NewServer(http.HandlerFunc(s.ServeProxy))
	defer proxy.Close()
	send := func(headers map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+ProxyPrefix+key, strings.NewReader(`{"id":1}`))
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		req.Header.Set("Mcp-Session-Id", "s-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.Header.Get("Mcp-Session-Id") == "" && resp.StatusCode == http.StatusOK {
			t.Error("proxy dropped Mcp-Session-Id response header")
		}
		return resp.StatusCode, string(body)
	}
	if code, body := send(res.Headers); code != http.StatusOK || !strings.Contains(body, `"session":"s-1"`) {
		t.Fatalf("proxied request = %d %s", code, body)
	}
	if provider.lastAuthHdr != "Bearer access-1" {
		t.Fatalf("upstream saw Authorization %q", provider.lastAuthHdr)
	}
	if code, _ := send(map[string]string{SessionHeader: "agent-1", "Authorization": "Bearer nope"}); code != http.StatusUnauthorized {
		t.Fatalf("forged capability got %d", code)
	}

	// The server stops accepting the token: the proxy refreshes and retries.
	provider.mu.Lock()
	provider.valid = "rotated"
	provider.mu.Unlock()
	if code, body := send(res.Headers); code != http.StatusOK {
		t.Fatalf("request after rotation = %d %s", code, body)
	}
	if provider.lastAuthHdr != "Bearer access-2" {
		t.Fatalf("upstream saw %q after refresh", provider.lastAuthHdr)
	}

	// A restarted daemon reloads the authorization.
	reloaded, _ := newService(t, home, map[string]config.MCPServer{"developerhub": cfg})
	if res := reloaded.Resolve(ctx, "developerhub", cfg, "agent-2"); res.AuthRequired || !strings.Contains(res.URL, ProxyPrefix) {
		t.Fatalf("reloaded resolution = %+v", res)
	}

	// A revoked refresh token asks the user to sign in again.
	provider.mu.Lock()
	provider.valid, provider.refreshFails = "rotated-again", true
	provider.mu.Unlock()
	if code, _ := send(res.Headers); code != http.StatusUnauthorized {
		t.Fatalf("request after revocation = %d", code)
	}
	if items := center.List(); len(items) != 1 || items[0].Severity != "attention" || !strings.Contains(items[0].Title, "expired") {
		t.Fatalf("expected re-auth notification, got %+v", items)
	}
	if res := s.Resolve(ctx, "developerhub", cfg, "agent-3"); !res.AuthRequired {
		t.Fatalf("revoked server resolved to %+v", res)
	}
}

func TestSecondAuthorizationReusesRegistrationAndSignOut(t *testing.T) {
	provider := newFakeProvider(t)
	cfg := config.MCPServer{Type: "http", URL: provider.server.URL + "/mcp"}
	s, center := newService(t, t.TempDir(), map[string]config.MCPServer{"dh": cfg})
	ctx := context.Background()
	s.Sync(ctx)
	key := Key(cfg.URL)
	s.ensureProbed(ctx, key)
	for i := 0; i < 2; i++ {
		authURL, err := s.Begin(ctx, key, "http://127.0.0.1:7717/")
		if err != nil {
			t.Fatal(err)
		}
		if rec := callback(t, s, provider.authorize(authURL)); rec.Code != http.StatusOK {
			t.Fatalf("callback %d = %d", i, rec.Code)
		}
	}
	if provider.registrations != 1 {
		t.Fatalf("registered %d clients, want 1", provider.registrations)
	}
	// Replaying a used state must fail.
	if rec := callback(t, s, url.Values{"code": {"code-1"}, "state": {"used"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown state = %d", rec.Code)
	}
	if _, err := s.HandleAction(ctx, NotificationPrefix+key, "dismiss"); err != nil {
		t.Fatal(err)
	}
	if err := s.SignOut(key); err != nil {
		t.Fatal(err)
	}
	if res := s.Resolve(ctx, "dh", cfg, "a"); !res.AuthRequired {
		t.Fatalf("signed-out server resolved to %+v", res)
	}
	if len(center.List()) != 0 {
		t.Fatalf("sign-out should not nag: %+v", center.List())
	}
}

func TestOpenAndStaticServersPassThrough(t *testing.T) {
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}
	}))
	defer open.Close()
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer static" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer locked.Close()
	openCfg := config.MCPServer{Type: "http", URL: open.URL + "/mcp"}
	staticCfg := config.MCPServer{Type: "http", URL: locked.URL + "/mcp", Headers: map[string]string{"authorization": "Bearer static"}}
	s, center := newService(t, t.TempDir(), nil)
	ctx := context.Background()
	if res := s.Resolve(ctx, "open", openCfg, "a"); res.AuthRequired || res.URL != openCfg.URL {
		t.Fatalf("open server resolved to %+v", res)
	}
	if res := s.Resolve(ctx, "static", staticCfg, "a"); res.AuthRequired || res.URL != staticCfg.URL || res.Headers["authorization"] != "Bearer static" {
		t.Fatalf("static server resolved to %+v", res)
	}
	if res := s.Resolve(ctx, "stdio", config.MCPServer{Command: "x"}, "a"); res.AuthRequired {
		t.Fatal("stdio server withheld")
	}
	if len(center.List()) != 0 {
		t.Fatalf("unexpected notifications %+v", center.List())
	}
	states := map[string]string{}
	for _, status := range s.List() {
		states[status.Name] = status.State
	}
	if states["open"] != StateOpen || states["static"] != StateStatic {
		t.Fatalf("states = %v", states)
	}
}

func TestBeginRejectsBadOrigins(t *testing.T) {
	for _, origin := range []string{"", "javascript:alert(1)", "http://", "http://h/path", "file:///x"} {
		if _, err := callbackURL(origin); err == nil {
			t.Errorf("origin %q accepted", origin)
		}
	}
	if got, err := callbackURL("https://tandem.example:8443"); err != nil || got != "https://tandem.example:8443"+CallbackPath {
		t.Fatalf("callbackURL = %q, %v", got, err)
	}
}

func TestParseChallenge(t *testing.T) {
	ch := parseChallenge(`Bearer error="invalid_token", resource_metadata="https://x/.well-known/oauth-protected-resource", scope="a b"`)
	if ch.ResourceMetadata != "https://x/.well-known/oauth-protected-resource" || ch.Scope != "a b" {
		t.Fatalf("challenge = %+v", ch)
	}
	if ch := parseChallenge(`Basic realm="x"`); ch.ResourceMetadata != "" {
		t.Fatalf("basic challenge parsed as %+v", ch)
	}
}

func TestResourceCovers(t *testing.T) {
	cases := map[[2]string]bool{
		{"https://a.io/", "https://a.io/mcp"}:       true,
		{"https://a.io/mcp", "https://a.io/mcp"}:    true,
		{"https://a.io/mc", "https://a.io/mcp"}:     false,
		{"https://evil.io/", "https://a.io/mcp"}:    false,
		{"http://a.io/", "https://a.io/mcp"}:        false,
		{"https://A.io/mcp/", "https://a.io/mcp/x"}: true,
	}
	for input, want := range cases {
		if got := resourceCovers(input[0], input[1]); got != want {
			t.Errorf("resourceCovers(%q, %q) = %v", input[0], input[1], got)
		}
	}
}

func TestExpiringTokenRefreshesBeforeUse(t *testing.T) {
	provider := newFakeProvider(t)
	cfg := config.MCPServer{Type: "http", URL: provider.server.URL + "/mcp"}
	now := time.Now()
	s, _ := newService(t, t.TempDir(), map[string]config.MCPServer{"dh": cfg})
	s.opts.Now = func() time.Time { return now }
	ctx := context.Background()
	s.Sync(ctx)
	key := Key(cfg.URL)
	s.ensureProbed(ctx, key)
	authURL, err := s.Begin(ctx, key, "http://127.0.0.1:7717")
	if err != nil {
		t.Fatal(err)
	}
	callback(t, s, provider.authorize(authURL))
	now = now.Add(time.Hour)
	token, err := s.accessToken(ctx, key)
	if err != nil || token != "access-2" {
		t.Fatalf("accessToken = %q, %v", token, err)
	}
}
