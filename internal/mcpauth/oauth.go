package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxMetadataBytes = 1 << 20

// challenge is what an MCP server's 401 says about how to authorize.
type challenge struct {
	ResourceMetadata string
	Scope            string
}

// discovery is the resolved OAuth configuration for one MCP server: the
// protected-resource identity it should be requested for, and the
// authorization server endpoints that issue its tokens.
type discovery struct {
	Resource              string   `json:"resource"`
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorizationEndpoint"`
	TokenEndpoint         string   `json:"tokenEndpoint"`
	RegistrationEndpoint  string   `json:"registrationEndpoint,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	AuthMethods           []string `json:"authMethods,omitempty"`
}

type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	TokenType    string          `json:"token_type"`
	RefreshToken string          `json:"refresh_token"`
	Scope        string          `json:"scope"`
	ExpiresIn    json.RawMessage `json:"expires_in"`
	Error        string          `json:"error"`
	Description  string          `json:"error_description"`
}

// parseChallenge reads the Bearer parameters of a WWW-Authenticate header
// (RFC 6750 §3, RFC 9728 §5.1). Only the parameters MCP clients act on are
// kept.
func parseChallenge(header string) challenge {
	var out challenge
	lower := strings.ToLower(header)
	idx := strings.Index(lower, "bearer")
	if idx < 0 {
		return out
	}
	rest := header[idx+len("bearer"):]
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,\t")
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = rest[eq+1:]
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := 1
			var b strings.Builder
			for end < len(rest) && rest[end] != '"' {
				if rest[end] == '\\' && end+1 < len(rest) {
					end++
				}
				b.WriteByte(rest[end])
				end++
			}
			value = b.String()
			if end < len(rest) {
				end++
			}
			rest = rest[end:]
		} else {
			end := strings.IndexAny(rest, ", \t")
			if end < 0 {
				end = len(rest)
			}
			value, rest = rest[:end], rest[end:]
		}
		switch key {
		case "resource_metadata":
			out.ResourceMetadata = value
		case "scope":
			out.Scope = value
		}
	}
	return out
}

// discover resolves the authorization server for an MCP server following the
// MCP authorization spec: protected resource metadata (RFC 9728) names the
// authorization server, whose RFC 8414 / OIDC metadata names the endpoints.
// Servers predating protected resource metadata are treated as their own
// authorization server, with the spec's default endpoint paths as a fallback.
func (s *Service) discover(ctx context.Context, serverURL string, ch challenge) (discovery, error) {
	target, err := url.Parse(serverURL)
	if err != nil {
		return discovery{}, err
	}
	origin := target.Scheme + "://" + target.Host
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	candidates := []string{}
	if ch.ResourceMetadata != "" {
		candidates = append(candidates, ch.ResourceMetadata)
	}
	if path := strings.TrimSuffix(target.Path, "/"); path != "" {
		candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+path)
	}
	candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")
	foundPRM := false
	for _, candidate := range candidates {
		if s.getJSON(ctx, candidate, &prm) == nil && len(prm.AuthorizationServers) > 0 {
			foundPRM = true
			break
		}
	}
	d := discovery{Resource: serverURL}
	issuer := origin
	if foundPRM {
		issuer = prm.AuthorizationServers[0]
		if prm.Resource != "" && resourceCovers(prm.Resource, serverURL) {
			d.Resource = prm.Resource
		}
		d.Scopes = prm.ScopesSupported
	}
	if ch.Scope != "" {
		d.Scopes = strings.Fields(ch.Scope)
	}
	var meta struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RegistrationEndpoint  string   `json:"registration_endpoint"`
		ScopesSupported       []string `json:"scopes_supported"`
		AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
	}
	foundMeta := false
	for _, candidate := range metadataURLs(issuer) {
		if s.getJSON(ctx, candidate, &meta) == nil && meta.AuthorizationEndpoint != "" && meta.TokenEndpoint != "" {
			foundMeta = true
			break
		}
	}
	if !foundMeta {
		if foundPRM {
			return discovery{}, fmt.Errorf("authorization server %s publishes no OAuth metadata", issuer)
		}
		base, _ := url.Parse(issuer)
		meta.Issuer = issuer
		meta.AuthorizationEndpoint = base.Scheme + "://" + base.Host + "/authorize"
		meta.TokenEndpoint = base.Scheme + "://" + base.Host + "/token"
		meta.RegistrationEndpoint = base.Scheme + "://" + base.Host + "/register"
	}
	d.Issuer = issuer
	d.AuthorizationEndpoint = meta.AuthorizationEndpoint
	d.TokenEndpoint = meta.TokenEndpoint
	d.RegistrationEndpoint = meta.RegistrationEndpoint
	d.AuthMethods = meta.AuthMethods
	if len(d.Scopes) == 0 {
		d.Scopes = meta.ScopesSupported
	}
	return d, nil
}

// metadataURLs lists the RFC 8414 and OIDC discovery locations for an issuer,
// in the order the MCP authorization spec prescribes.
func metadataURLs(issuer string) []string {
	parsed, err := url.Parse(issuer)
	if err != nil {
		return nil
	}
	origin := parsed.Scheme + "://" + parsed.Host
	path := strings.TrimSuffix(parsed.Path, "/")
	if path == "" {
		return []string{origin + "/.well-known/oauth-authorization-server", origin + "/.well-known/openid-configuration"}
	}
	return []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + "/.well-known/openid-configuration" + path,
		origin + path + "/.well-known/openid-configuration",
	}
}

// resourceCovers reports whether a protected-resource identifier advertised
// by metadata may stand in for the configured server URL: same origin, and a
// path prefix of it. Anything else would let metadata redirect the token's
// audience to an unrelated resource.
func resourceCovers(resource, serverURL string) bool {
	r, err1 := url.Parse(resource)
	t, err2 := url.Parse(serverURL)
	if err1 != nil || err2 != nil {
		return false
	}
	if !strings.EqualFold(r.Scheme, t.Scheme) || !strings.EqualFold(r.Host, t.Host) {
		return false
	}
	prefix := strings.TrimSuffix(r.Path, "/")
	return prefix == "" || t.Path == prefix || strings.HasPrefix(t.Path, prefix+"/")
}

func (s *Service) getJSON(ctx context.Context, target string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", target, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(dst)
}

// register performs RFC 7591 dynamic client registration as a public,
// PKCE-only native client.
func (s *Service) register(ctx context.Context, d discovery, redirectURI string) (clientRecord, error) {
	if d.RegistrationEndpoint == "" {
		return clientRecord{}, errors.New("the authorization server does not support dynamic client registration; set mcpServers.<name>.oauth.clientId in config.yml")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Tandem",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      strings.Join(d.Scopes, " "),
	})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return clientRecord{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return clientRecord{}, fmt.Errorf("register OAuth client: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return clientRecord{}, fmt.Errorf("register OAuth client: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ClientID == "" {
		return clientRecord{}, errors.New("register OAuth client: response carried no client_id")
	}
	return clientRecord{ClientID: out.ClientID, ClientSecret: out.ClientSecret, Issuer: d.Issuer}, nil
}

// exchange posts a token request and normalizes the result.
func (s *Service) exchange(ctx context.Context, tokenEndpoint string, form url.Values, client clientRecord) (tokenRecord, error) {
	form.Set("client_id", client.ClientID)
	if client.ClientSecret != "" {
		form.Set("client_secret", client.ClientSecret)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenRecord{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return tokenRecord{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	var out tokenResponse
	decodeErr := json.Unmarshal(raw, &out)
	if decodeErr != nil && strings.Contains(resp.Header.Get("Content-Type"), "x-www-form-urlencoded") {
		values, _ := url.ParseQuery(string(raw))
		out = tokenResponse{AccessToken: values.Get("access_token"), TokenType: values.Get("token_type"), RefreshToken: values.Get("refresh_token"), Scope: values.Get("scope"), ExpiresIn: json.RawMessage(values.Get("expires_in")), Error: values.Get("error"), Description: values.Get("error_description")}
		decodeErr = nil
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		reason := out.Error
		if out.Description != "" {
			reason += ": " + out.Description
		}
		if reason == "" {
			reason = truncate(string(raw), 300)
		}
		return tokenRecord{}, &tokenError{Status: resp.StatusCode, Code: out.Error, Reason: reason}
	}
	if decodeErr != nil {
		return tokenRecord{}, fmt.Errorf("decode token response: %w", decodeErr)
	}
	token := tokenRecord{AccessToken: out.AccessToken, TokenType: out.TokenType, RefreshToken: out.RefreshToken, Scope: out.Scope}
	if seconds := expiresIn(out.ExpiresIn); seconds > 0 {
		token.ExpiresAt = s.opts.Now().Add(time.Duration(seconds) * time.Second)
	}
	return token, nil
}

type tokenError struct {
	Status int
	Code   string
	Reason string
}

func (e *tokenError) Error() string {
	return fmt.Sprintf("token endpoint rejected the request (%d): %s", e.Status, e.Reason)
}

// invalidGrant reports a refresh failure that will not succeed on retry: the
// refresh token was revoked or expired, so the user must authorize again.
func invalidGrant(err error) bool {
	var te *tokenError
	if !errors.As(err, &te) {
		return false
	}
	return te.Code == "invalid_grant" || te.Code == "invalid_client" || te.Code == "unauthorized_client" || te.Status == http.StatusUnauthorized
}

func expiresIn(raw json.RawMessage) int64 {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" {
		return 0
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return int64(value)
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
