// Package secretbroker keeps user-supplied credentials outside agent/model
// transports and applies them only at constrained network or file boundaries.
package secretbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxSecretBytes = 64 << 10
	maxBodyBytes   = 2 << 20
	grantLifetime  = 8 * time.Hour
)

var secretMarkerPattern = regexp.MustCompile(`\{\{TANDEM_SECRET:(sg_[0-9a-f]{32})\}\}`)

type Options struct {
	Token       string
	AgentExists func(string) bool
	Workspace   func(string) string
	OnRequest   func(Request)
	OnResolved  func(sessionID, requestID string, granted bool)
	HTTPClient  *http.Client
	Now         func() time.Time
}

type Request struct {
	RequestID  string `json:"requestId"`
	SessionID  string `json:"sessionId"`
	Service    string `json:"service"`
	Reason     string `json:"reason"`
	Origin     string `json:"origin"`
	HeaderName string `json:"headerName"`
	Prefix     string `json:"prefix"`
	Usage      string `json:"usage,omitempty"`
	Path       string `json:"path,omitempty"`
}

type requestState struct {
	Request
	created time.Time
	done    bool
	denied  bool
	grantID string
}

type grant struct {
	id, sessionID, usage, origin, path, headerName, prefix string
	secret                                                 []byte
	expires                                                time.Time
}

type Broker struct {
	mu       sync.Mutex
	opts     Options
	requests map[string]*requestState
	grants   map[string]*grant
}

func New(opts Options) *Broker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTPClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if privateIP(ip.IP) {
					continue
				}
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				err = dialErr
			}
			if err == nil {
				err = errors.New("origin resolved only to private or local addresses")
			}
			return nil, err
		}
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	return &Broker{opts: opts, requests: map[string]*requestState{}, grants: map[string]*grant{}}
}

func privateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func randomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("origin must be an https origin without credentials, path, query, or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("origin must not contain a path")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("local origins are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && privateIP(ip) {
		return "", errors.New("private network origins are not allowed")
	}
	u.Path, u.RawPath = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func normalizeFilePath(raw string) (string, error) {
	if raw == "" || filepath.IsAbs(raw) || strings.ContainsRune(raw, '\x00') {
		return "", errors.New("secret file path must be workspace-relative")
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("secret file path must stay inside the workspace")
	}
	return filepath.ToSlash(clean), nil
}

func pathInside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (b *Broker) fileTarget(sessionID, rawPath string) (string, string, error) {
	path, err := normalizeFilePath(rawPath)
	if err != nil {
		return "", "", err
	}
	if b.opts.Workspace == nil {
		return "", "", errors.New("agent workspace is unavailable")
	}
	root, err := filepath.EvalSymlinks(b.opts.Workspace(sessionID))
	if err != nil {
		return "", "", errors.New("agent workspace is unavailable")
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Dir(filepath.FromSlash(path))))
	if err != nil || !pathInside(root, parent) {
		return "", "", errors.New("secret file parent must exist inside the workspace")
	}
	target := filepath.Join(parent, filepath.Base(filepath.FromSlash(path)))
	if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("secret file must not be a symbolic link")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", "", statErr
	}
	return path, target, nil
}

type FileReadRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
}
type FileReadResponse struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Content  string `json:"content"`
}
type FileWriteRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Revision  string `json:"revision"`
	Content   string `json:"content"`
}

func fileRevision(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (b *Broker) FileRead(in FileReadRequest) (FileReadResponse, error) {
	path, target, err := b.fileTarget(in.SessionID, in.Path)
	if err != nil {
		return FileReadResponse{}, err
	}
	content, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		return FileReadResponse{Path: path, Revision: "missing"}, nil
	}
	if err != nil {
		return FileReadResponse{}, err
	}
	if len(content) > maxBodyBytes {
		return FileReadResponse{}, errors.New("secret file exceeds 2 MiB")
	}
	b.mu.Lock()
	redacted := append([]byte(nil), content...)
	count := 0
	for id, g := range b.grants {
		if g.sessionID == in.SessionID && g.usage == "file" && g.path == path && b.opts.Now().Before(g.expires) {
			marker := []byte("{{TANDEM_SECRET:" + id + "}}")
			matches := bytes.Count(redacted, g.secret)
			if matches > 0 {
				redacted = bytes.ReplaceAll(redacted, g.secret, marker)
				count += matches
			}
		}
	}
	b.mu.Unlock()
	slog.Info("secret file read with redaction", "session", in.SessionID, "path", path, "redactions", count, "bytes", len(content))
	return FileReadResponse{Path: path, Revision: fileRevision(content), Content: string(redacted)}, nil
}

func (b *Broker) FileWrite(in FileWriteRequest) error {
	path, target, err := b.fileTarget(in.SessionID, in.Path)
	if err != nil {
		return err
	}
	if len(in.Content) > maxBodyBytes {
		return errors.New("secret file exceeds 2 MiB")
	}
	current, readErr := os.ReadFile(target)
	actualRevision := "missing"
	if readErr == nil {
		actualRevision = fileRevision(current)
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if in.Revision != actualRevision {
		return errors.New("secret file changed; read it again before writing")
	}

	materialized := []byte(in.Content)
	matches := secretMarkerPattern.FindAllSubmatch(materialized, -1)
	markerCheck := secretMarkerPattern.ReplaceAll(materialized, nil)
	if bytes.Contains(markerCheck, []byte("{{TANDEM_SECRET:")) {
		return errors.New("invalid secret marker")
	}
	b.mu.Lock()
	used := 0
	for _, match := range matches {
		id := string(match[1])
		g := b.grants[id]
		if g == nil || g.sessionID != in.SessionID || g.usage != "file" || g.path != path || !b.opts.Now().Before(g.expires) {
			b.mu.Unlock()
			return errors.New("invalid, expired, or incorrectly scoped secret marker")
		}
		materialized = bytes.ReplaceAll(materialized, match[0], g.secret)
		used++
	}
	b.mu.Unlock()
	if bytes.Contains(materialized, []byte("{{TANDEM_SECRET:")) {
		return errors.New("invalid secret marker")
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tandem-secret-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(materialized)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, target)
	}
	for i := range materialized {
		materialized[i] = 0
	}
	if err != nil {
		return err
	}
	slog.Info("secret file written", "session", in.SessionID, "path", path, "markers", used, "bytes", len(in.Content))
	return nil
}

func (b *Broker) Register(req Request) (string, error) {
	if b.opts.AgentExists == nil || !b.opts.AgentExists(req.SessionID) {
		return "", errors.New("no such agent")
	}
	if len(req.Service) == 0 || len(req.Service) > 80 || len(req.Reason) > 500 {
		return "", errors.New("invalid service or reason")
	}
	if req.Usage == "" {
		req.Usage = "http"
	}
	var err error
	if req.Usage == "file" {
		req.Path, err = normalizeFilePath(req.Path)
		if err != nil || b.opts.Workspace == nil || b.opts.Workspace(req.SessionID) == "" {
			if err == nil {
				err = errors.New("agent workspace is unavailable")
			}
			return "", err
		}
	} else if req.Usage == "http" {
		req.Origin, err = normalizeOrigin(req.Origin)
		if err != nil {
			return "", err
		}
	} else {
		return "", errors.New("unsupported secret usage")
	}
	if req.HeaderName == "" {
		req.HeaderName = "Authorization"
	}
	canonical := ""
	if req.Usage == "http" {
		canonical = http.CanonicalHeaderKey(req.HeaderName)
		if canonical == "" || canonical == "Host" || canonical == "Cookie" || strings.ContainsAny(req.HeaderName, "\r\n:") {
			return "", errors.New("invalid credential header")
		}
	}
	if len(req.Prefix) > 80 || strings.ContainsAny(req.Prefix, "\r\n") {
		return "", errors.New("invalid credential prefix")
	}
	id, err := randomID("sr_")
	if err != nil {
		return "", err
	}
	req.RequestID, req.HeaderName = id, canonical
	b.mu.Lock()
	b.requests[id] = &requestState{Request: req, created: b.opts.Now()}
	b.mu.Unlock()
	slog.Info("secret requested", "session", req.SessionID, "request_id", id, "service", req.Service, "usage", req.Usage, "origin", req.Origin, "path", req.Path, "header", canonical)
	if b.opts.OnRequest != nil {
		b.opts.OnRequest(req)
	}
	return id, nil
}

func (b *Broker) Status(sessionID, requestID string) (done, denied bool, grantID string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.requests[requestID]
	if r == nil || r.SessionID != sessionID {
		return false, false, "", errors.New("no such secret request")
	}
	return r.done, r.denied, r.grantID, nil
}

func (b *Broker) Resolve(requestID string, secret []byte, deny bool) error {
	b.mu.Lock()
	r := b.requests[requestID]
	if r == nil || r.done {
		b.mu.Unlock()
		return errors.New("no such pending secret request")
	}
	if !deny && (len(secret) == 0 || len(secret) > maxSecretBytes) {
		b.mu.Unlock()
		return errors.New("secret must be between 1 byte and 64 KiB")
	}
	r.done, r.denied = true, deny
	if !deny {
		id, err := randomID("sg_")
		if err != nil {
			b.mu.Unlock()
			return err
		}
		r.grantID = id
		usage := r.Usage
		if usage == "" {
			usage = "http"
		}
		b.grants[id] = &grant{id: id, sessionID: r.SessionID, usage: usage, origin: r.Origin, path: r.Path, headerName: r.HeaderName, prefix: r.Prefix, secret: append([]byte(nil), secret...), expires: b.opts.Now().Add(grantLifetime)}
	}
	sessionID := r.SessionID
	b.mu.Unlock()
	for i := range secret {
		secret[i] = 0
	}
	slog.Info("secret request resolved", "session", sessionID, "request_id", requestID, "granted", !deny)
	if b.opts.OnResolved != nil {
		b.opts.OnResolved(sessionID, requestID, !deny)
	}
	return nil
}

type ProxyRequest struct {
	SessionID string            `json:"sessionId"`
	GrantID   string            `json:"grantId"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
}

type ProxyResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

func (b *Broker) Proxy(ctx context.Context, in ProxyRequest) (ProxyResponse, error) {
	b.mu.Lock()
	g := b.grants[in.GrantID]
	if g == nil || g.sessionID != in.SessionID || g.usage != "http" || !b.opts.Now().Before(g.expires) {
		b.mu.Unlock()
		return ProxyResponse{}, errors.New("invalid or expired secret grant")
	}
	secret := append([]byte(nil), g.secret...)
	origin, headerName, prefix := g.origin, g.headerName, g.prefix
	b.mu.Unlock()
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	u, err := url.Parse(in.URL)
	if err != nil || u.User != nil || strings.TrimRight((&url.URL{Scheme: u.Scheme, Host: u.Host}).String(), "/") != origin {
		return ProxyResponse{}, errors.New("request URL is outside the approved origin")
	}
	method := strings.ToUpper(in.Method)
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch && method != http.MethodDelete {
		return ProxyResponse{}, errors.New("unsupported HTTP method")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(in.Body))
	if err != nil {
		return ProxyResponse{}, err
	}
	for name, value := range in.Headers {
		name = http.CanonicalHeaderKey(name)
		if name == "" || name == headerName || name == "Host" || name == "Cookie" || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return ProxyResponse{}, fmt.Errorf("unsafe request header %q", name)
		}
		req.Header.Set(name, value)
	}
	req.Header.Set(headerName, prefix+string(secret))
	client := *b.opts.HTTPClient
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return ProxyResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return ProxyResponse{}, err
	}
	if len(body) > maxBodyBytes {
		return ProxyResponse{}, errors.New("response exceeds 2 MiB")
	}
	body = bytes.ReplaceAll(body, secret, []byte("[REDACTED SECRET]"))
	headers := map[string][]string{}
	for _, name := range []string{"Content-Type", "Location", "Retry-After", "X-Request-Id"} {
		if values := resp.Header.Values(name); len(values) > 0 {
			redacted := make([]string, len(values))
			for i, value := range values {
				redacted[i] = strings.ReplaceAll(value, string(secret), "[REDACTED SECRET]")
			}
			headers[name] = redacted
		}
	}
	slog.Info("secret grant used", "session", in.SessionID, "grant_id", in.GrantID, "method", method, "origin", origin, "status", resp.StatusCode)
	return ProxyResponse{Status: resp.StatusCode, Headers: headers, Body: string(body)}, nil
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch r.URL.Path {
	case "/internal/secrets/request":
		b.serveRequest(w, r)
	case "/internal/secrets/status":
		b.serveStatus(w, r)
	case "/internal/secrets/resolve":
		b.serveResolve(w, r)
	case "/internal/secrets/proxy":
		b.serveProxy(w, r)
	case "/internal/secrets/file/read":
		b.serveFileRead(w, r)
	case "/internal/secrets/file/write":
		b.serveFileWrite(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (b *Broker) serveRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body Request
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	id, err := b.Register(body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"requestId": id})
}
func (b *Broker) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	done, denied, grant, err := b.Status(r.URL.Query().Get("sessionId"), r.URL.Query().Get("requestId"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"done": done, "denied": denied, "grantId": grant})
}
func (b *Broker) serveResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		RequestID string `json:"requestId"`
		Secret    string `json:"secret"`
		Deny      bool   `json:"deny"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSecretBytes+4096)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	secret := []byte(body.Secret)
	if err := b.Resolve(body.RequestID, secret, body.Deny); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	body.Secret = ""
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (b *Broker) serveProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body ProxyRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes+64<<10)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	resp, err := b.Proxy(r.Context(), body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, resp)
}
func (b *Broker) serveFileRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body FileReadRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	resp, err := b.FileRead(body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, resp)
}
func (b *Broker) serveFileWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body FileWriteRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes+64<<10)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if err := b.FileWrite(body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (b *Broker) authorized(r *http.Request) bool {
	want, got := "Bearer "+b.opts.Token, r.Header.Get("Authorization")
	return b.opts.Token != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
