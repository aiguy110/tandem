// Package controlmcp implements Tandem's internal browser-control MCP server.
package controlmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	ToolName              = "browser_request_takeover"
	SecretRequestTool     = "secret_request"
	SecretHTTPTool        = "secret_http_request"
	SecretFileRequestTool = "secret_file_request"
	SecretFileReadTool    = "secret_file_read"
	SecretFileWriteTool   = "secret_file_write"
)

type Config struct {
	ControlURL, Token, SessionID string
	BrowserEnabled               bool
	HTTPClient                   *http.Client
	PollInterval                 time.Duration
}

type server struct {
	cfg Config
	out io.Writer
	mu  sync.Mutex
}

// Run serves newline-delimited MCP JSON-RPC until input closes or ctx ends.
func Run(ctx context.Context, in io.Reader, out io.Writer, cfg Config) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	s := &server{cfg: cfg, out: out}
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	var calls sync.WaitGroup
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		calls.Add(1)
		go func() { defer calls.Done(); s.handle(ctx, line) }()
	}
	err := scan.Err()
	if err != nil {
		return err
	}
	calls.Wait()
	return nil
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *server) handle(ctx context.Context, line []byte) {
	var req request
	if json.Unmarshal(line, &req) != nil {
		return
	}
	switch req.Method {
	case "notifications/initialized":
		return
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		s.result(req.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "tandem-control", "version": "0.1.0"}})
	case "ping":
		s.result(req.ID, map[string]any{})
	case "tools/list":
		tools := []any{secretRequestDeclaration(), secretHTTPDeclaration(), secretFileRequestDeclaration(), secretFileReadDeclaration(), secretFileWriteDeclaration()}
		if s.cfg.BrowserEnabled {
			tools = append([]any{toolDeclaration()}, tools...)
		}
		s.result(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			s.rpcError(req.ID, -32602, "invalid tool call")
			return
		}
		switch p.Name {
		case ToolName:
			var args struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(p.Arguments, &args)
			if args.Reason == "" {
				args.Reason = "the agent needs you"
			}
			if err := s.takeover(ctx, args.Reason); err != nil {
				s.rpcError(req.ID, -32603, err.Error())
				return
			}
			s.result(req.ID, toolText("The human took the wheel, completed the step, and handed control back. You may continue."))
		case SecretRequestTool:
			var args secretRequestArgs
			if json.Unmarshal(p.Arguments, &args) != nil {
				s.rpcError(req.ID, -32602, "invalid secret request")
				return
			}
			grantID, err := s.requestSecret(ctx, args)
			if err != nil {
				s.rpcError(req.ID, -32603, err.Error())
				return
			}
			s.result(req.ID, toolText("Secret approved. Use secret_http_request with grantId "+grantID+". The secret value was not disclosed."))
		case SecretHTTPTool:
			var args secretHTTPArgs
			if json.Unmarshal(p.Arguments, &args) != nil {
				s.rpcError(req.ID, -32602, "invalid secret HTTP request")
				return
			}
			result, err := s.secretHTTP(ctx, args)
			if err != nil {
				s.rpcError(req.ID, -32603, err.Error())
				return
			}
			encoded, _ := json.Marshal(result)
			s.result(req.ID, toolText(string(encoded)))
		case SecretFileRequestTool:
			var args secretFileRequestArgs
			if json.Unmarshal(p.Arguments, &args) != nil {
				s.rpcError(req.ID, -32602, "invalid secret file request")
				return
			}
			grantID, err := s.requestFileSecret(ctx, args)
			if err != nil {
				s.rpcError(req.ID, -32603, err.Error())
				return
			}
			s.result(req.ID, toolText("Secret approved for "+args.Path+". Use marker {{TANDEM_SECRET:"+grantID+"}} with secret_file_write. The value was not disclosed."))
		case SecretFileReadTool:
			var args secretFileReadArgs
			if json.Unmarshal(p.Arguments, &args) != nil {
				s.rpcError(req.ID, -32602, "invalid secret file read")
				return
			}
			result, err := s.secretFileRead(ctx, args)
			if err != nil {
				s.rpcError(req.ID, -32603, err.Error())
				return
			}
			encoded, _ := json.Marshal(result)
			s.result(req.ID, toolText(string(encoded)))
		case SecretFileWriteTool:
			var args secretFileWriteArgs
			if json.Unmarshal(p.Arguments, &args) != nil {
				s.rpcError(req.ID, -32602, "invalid secret file write")
				return
			}
			if err := s.secretFileWrite(ctx, args); err != nil {
				s.rpcError(req.ID, -32603, err.Error())
				return
			}
			s.result(req.ID, toolText("Secret file written. Its materialized contents were not returned."))
		default:
			s.rpcError(req.ID, -32602, "unknown tool: "+p.Name)
		}
	default:
		if len(req.ID) > 0 {
			s.rpcError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func toolText(value string) map[string]any {
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": value}}}
}

func toolDeclaration() map[string]any {
	return map[string]any{
		"name":        ToolName,
		"description": "Ask the human to take control of the shared browser to complete a step you cannot. Blocks until the human hands control back.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]string{"type": "string", "description": "Short reason shown to the human."}}, "required": []string{"reason"}},
	}
}

func secretRequestDeclaration() map[string]any {
	return map[string]any{
		"name":        SecretRequestTool,
		"description": "Ask the human for an API credential without exposing its value to the model. Blocks until approved or denied, then returns only a scoped grant ID.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"service":    map[string]string{"type": "string", "description": "Short service name shown in trusted Tandem UI."},
			"reason":     map[string]string{"type": "string", "description": "Why the credential is needed; shown as untrusted agent text."},
			"origin":     map[string]string{"type": "string", "description": "Exact HTTPS origin where Tandem may inject the credential, for example https://api.example.com."},
			"headerName": map[string]string{"type": "string", "description": "Credential header; defaults to Authorization."},
			"prefix":     map[string]string{"type": "string", "description": "Optional header prefix, commonly 'Bearer '."},
		}, "required": []string{"service", "reason", "origin"}},
	}
}

func secretHTTPDeclaration() map[string]any {
	return map[string]any{
		"name":        SecretHTTPTool,
		"description": "Make an HTTP request using a secret grant. Tandem injects the credential only for its approved HTTPS origin; the credential is never returned.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"grantId": map[string]string{"type": "string"}, "method": map[string]string{"type": "string"}, "url": map[string]string{"type": "string"},
			"headers": map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}}, "body": map[string]string{"type": "string"},
		}, "required": []string{"grantId", "url"}},
	}
}

func secretFileRequestDeclaration() map[string]any {
	return map[string]any{"name": SecretFileRequestTool, "description": "Ask the human for a secret scoped to one workspace file without exposing its value to the model. Returns an opaque grant marker.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
		"service": map[string]string{"type": "string"}, "reason": map[string]string{"type": "string"}, "path": map[string]string{"type": "string", "description": "Workspace-relative file path, for example .env."},
	}, "required": []string{"service", "reason", "path"}}}
}
func secretFileReadDeclaration() map[string]any {
	return map[string]any{"name": SecretFileReadTool, "description": "Read a secret-bearing workspace file through Tandem. Active approved secret values are replaced with opaque {{TANDEM_SECRET:...}} markers.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}}, "required": []string{"path"}}}
}
func secretFileWriteDeclaration() map[string]any {
	return map[string]any{"name": SecretFileWriteTool, "description": "Atomically write a redacted document returned by secret_file_read. Tandem materializes approved secret markers and returns no file content.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
		"path": map[string]string{"type": "string"}, "revision": map[string]string{"type": "string", "description": "Revision from secret_file_read; use missing for a new file."}, "content": map[string]string{"type": "string", "description": "Complete redacted file content containing opaque secret markers."},
	}, "required": []string{"path", "revision", "content"}}}
}

type secretRequestArgs struct {
	Service    string `json:"service"`
	Reason     string `json:"reason"`
	Origin     string `json:"origin"`
	HeaderName string `json:"headerName"`
	Prefix     string `json:"prefix"`
}
type secretHTTPArgs struct {
	GrantID string            `json:"grantId"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}
type secretFileRequestArgs struct {
	Service string `json:"service"`
	Reason  string `json:"reason"`
	Path    string `json:"path"`
}
type secretFileReadArgs struct {
	Path string `json:"path"`
}
type secretFileWriteArgs struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Content  string `json:"content"`
}

func (s *server) requestSecret(ctx context.Context, args secretRequestArgs) (string, error) {
	var registered struct {
		RequestID string `json:"requestId"`
	}
	if err := s.secretJSON(ctx, http.MethodPost, "/internal/secrets/request", "", map[string]string{
		"sessionId": s.cfg.SessionID, "service": args.Service, "reason": args.Reason, "origin": args.Origin, "headerName": args.HeaderName, "prefix": args.Prefix,
	}, &registered); err != nil {
		return "", err
	}
	if registered.RequestID == "" {
		return "", errors.New("secret request returned no requestId")
	}
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			var status struct {
				Done, Denied bool
				GrantID      string `json:"grantId"`
			}
			query := "?sessionId=" + url.QueryEscape(s.cfg.SessionID) + "&requestId=" + url.QueryEscape(registered.RequestID)
			if err := s.secretJSON(ctx, http.MethodGet, "/internal/secrets/status", query, nil, &status); err != nil {
				continue
			}
			if status.Done {
				if status.Denied {
					return "", errors.New("human denied the secret request")
				}
				return status.GrantID, nil
			}
		}
	}
}

func (s *server) requestFileSecret(ctx context.Context, args secretFileRequestArgs) (string, error) {
	var registered struct {
		RequestID string `json:"requestId"`
	}
	if err := s.secretJSON(ctx, http.MethodPost, "/internal/secrets/request", "", map[string]string{
		"sessionId": s.cfg.SessionID, "service": args.Service, "reason": args.Reason, "usage": "file", "path": args.Path,
	}, &registered); err != nil {
		return "", err
	}
	return s.waitForSecret(ctx, registered.RequestID)
}

func (s *server) waitForSecret(ctx context.Context, requestID string) (string, error) {
	if requestID == "" {
		return "", errors.New("secret request returned no requestId")
	}
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			var status struct {
				Done, Denied bool
				GrantID      string `json:"grantId"`
			}
			query := "?sessionId=" + url.QueryEscape(s.cfg.SessionID) + "&requestId=" + url.QueryEscape(requestID)
			if err := s.secretJSON(ctx, http.MethodGet, "/internal/secrets/status", query, nil, &status); err != nil {
				continue
			}
			if status.Done {
				if status.Denied {
					return "", errors.New("human denied the secret request")
				}
				return status.GrantID, nil
			}
		}
	}
}

func (s *server) secretFileRead(ctx context.Context, args secretFileReadArgs) (map[string]any, error) {
	var result map[string]any
	err := s.secretJSON(ctx, http.MethodPost, "/internal/secrets/file/read", "", map[string]string{"sessionId": s.cfg.SessionID, "path": args.Path}, &result)
	return result, err
}
func (s *server) secretFileWrite(ctx context.Context, args secretFileWriteArgs) error {
	var result map[string]any
	return s.secretJSON(ctx, http.MethodPost, "/internal/secrets/file/write", "", map[string]string{"sessionId": s.cfg.SessionID, "path": args.Path, "revision": args.Revision, "content": args.Content}, &result)
}

func (s *server) secretHTTP(ctx context.Context, args secretHTTPArgs) (map[string]any, error) {
	var result map[string]any
	err := s.secretJSON(ctx, http.MethodPost, "/internal/secrets/proxy", "", map[string]any{"sessionId": s.cfg.SessionID, "grantId": args.GrantID, "method": args.Method, "url": args.URL, "headers": args.Headers, "body": args.Body}, &result)
	return result, err
}

func (s *server) secretJSON(ctx context.Context, method, path, query string, body any, out any) error {
	if s.cfg.ControlURL == "" {
		return errors.New("TANDEM_CONTROL_URL not set")
	}
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.ControlURL, "/")+path+query, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *server) takeover(ctx context.Context, reason string) error {
	if s.cfg.ControlURL == "" {
		return errors.New("TANDEM_CONTROL_URL not set")
	}
	base := strings.TrimRight(s.cfg.ControlURL, "/") + "/internal/browser/takeover"
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("sessionId", s.cfg.SessionID)
	u.RawQuery = q.Encode()
	body, _ := json.Marshal(map[string]string{"reason": reason})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("takeover register failed (%d)", resp.StatusCode)
	}
	var registered struct {
		ReqID string `json:"reqId"`
	}
	if json.NewDecoder(resp.Body).Decode(&registered) != nil || registered.ReqID == "" {
		return errors.New("takeover register returned no reqId")
	}

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resolved, err := s.poll(ctx, base, registered.ReqID)
			if err == nil && resolved {
				return nil
			}
		}
	}
}

func (s *server) poll(ctx context.Context, base, reqID string) (bool, error) {
	u, err := url.Parse(base)
	if err != nil {
		return false, err
	}
	q := u.Query()
	q.Set("reqId", reqID)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("takeover status failed (%d)", resp.StatusCode)
	}
	var body struct {
		Resolved bool `json:"resolved"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	return body.Resolved, err
}

func (s *server) result(id json.RawMessage, value any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": idValue(id), "result": value})
}
func (s *server) rpcError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": idValue(id), "error": map[string]any{"code": code, "message": message}})
}
func idValue(raw json.RawMessage) any { var v any; _ = json.Unmarshal(raw, &v); return v }
func (s *server) write(v any)         { s.mu.Lock(); defer s.mu.Unlock(); _ = json.NewEncoder(s.out).Encode(v) }
