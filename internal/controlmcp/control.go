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

const ToolName = "browser_request_takeover"

type Config struct {
	ControlURL, Token, SessionID string
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
		s.result(req.ID, map[string]any{"tools": []any{toolDeclaration()}})
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Reason string `json:"reason"`
			} `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.Name != ToolName {
			s.rpcError(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		if p.Arguments.Reason == "" {
			p.Arguments.Reason = "the agent needs you"
		}
		if err := s.takeover(ctx, p.Arguments.Reason); err != nil {
			s.rpcError(req.ID, -32603, err.Error())
			return
		}
		s.result(req.ID, map[string]any{
			"content": []any{map[string]string{
				"type": "text",
				"text": "The human took the wheel, completed the step, and handed control back. You may continue.",
			}},
		})
	default:
		if len(req.ID) > 0 {
			s.rpcError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func toolDeclaration() map[string]any {
	return map[string]any{
		"name":        ToolName,
		"description": "Ask the human to take control of the shared browser to complete a step you cannot. Blocks until the human hands control back.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]string{"type": "string", "description": "Short reason shown to the human."}}, "required": []string{"reason"}},
	}
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
