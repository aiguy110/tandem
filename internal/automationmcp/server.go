// Package automationmcp implements the internal MCP bridge used by ACP agents
// to invoke Tandem's repository automation service.
package automationmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	ToolRun        = "scripts_run"
	ToolEvaluate   = "scripts_evaluate"
	ToolPreapprove = "scripts_preapprove"
)

// Config identifies the calling agent and workspace. The daemon derives the
// repository (and therefore approval scope) from WorkspaceCWD; it must not
// trust a repository identifier supplied by an agent.
type Config struct {
	ControlURL   string
	Token        string
	AgentID      string
	WorkspaceCWD string
	HTTPClient   *http.Client
}

type server struct {
	cfg Config
	out io.Writer
	mu  sync.Mutex
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Run serves newline-delimited MCP JSON-RPC until input closes or ctx ends.
func Run(ctx context.Context, in io.Reader, out io.Writer, cfg Config) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
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
	if err := scan.Err(); err != nil {
		return err
	}
	calls.Wait()
	return nil
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
		s.result(req.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "tandem-scripts", "version": "0.1.0"}})
	case "ping":
		s.result(req.ID, map[string]any{})
	case "tools/list":
		s.result(req.ID, map[string]any{"tools": toolDeclarations()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || !knownTool(p.Name) {
			s.rpcError(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage(`{}`)
		}
		var args map[string]any
		if json.Unmarshal(p.Arguments, &args) != nil {
			s.rpcError(req.ID, -32602, "tool arguments must be an object")
			return
		}
		value, err := s.call(ctx, p.Name, args)
		if err != nil {
			s.result(req.ID, toolError(err.Error()))
			return
		}
		s.result(req.ID, toolResult(value))
	default:
		if len(req.ID) > 0 {
			s.rpcError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func knownTool(name string) bool {
	return name == ToolRun || name == ToolEvaluate || name == ToolPreapprove
}

func endpointFor(name string) string {
	switch name {
	case ToolRun:
		return "run"
	case ToolEvaluate:
		return "evaluate"
	default:
		return "preapprove"
	}
}

// call defines the daemon contract: POST /internal/automation/{run,evaluate,
// preapprove}, bearer-authenticated, with agentId and workspaceCwd added to the
// MCP arguments. A successful response is any JSON value. A non-2xx response
// may return {"error":"..."} or plain text.
func (s *server) call(ctx context.Context, tool string, args map[string]any) (any, error) {
	if s.cfg.ControlURL == "" {
		return nil, errors.New("TANDEM_CONTROL_URL not set")
	}
	body := make(map[string]any, len(args)+2)
	for k, v := range args {
		body[k] = v
	}
	body["agentId"] = s.cfg.AgentID
	body["workspaceCwd"] = s.cfg.WorkspaceCWD
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(s.cfg.ControlURL, "/") + "/internal/automation/" + endpointFor(tool)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("automation request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read automation response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		var problem struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(responseBody, &problem) == nil && problem.Error != "" {
			message = problem.Error
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("automation request failed (%d): %s", resp.StatusCode, message)
	}
	if len(bytes.TrimSpace(responseBody)) == 0 {
		return map[string]any{"ok": true}, nil
	}
	var value any
	if err := json.Unmarshal(responseBody, &value); err != nil {
		return nil, errors.New("automation service returned invalid JSON")
	}
	return value, nil
}

func toolResult(value any) map[string]any {
	data, _ := json.Marshal(value)
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": string(data)}}, "structuredContent": value}
}

func toolError(message string) map[string]any {
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": message}}, "structuredContent": map[string]any{"error": message}, "isError": true}
}

func toolDeclarations() []any {
	return []any{
		map[string]any{"name": ToolRun, "description": "Run a TypeScript automation script from this repository's .tandem/scripts directory.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "description": "Repository-relative script path."}, "args": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}}, "required": []string{"path"}}},
		map[string]any{"name": ToolEvaluate, "description": "Evaluate ephemeral TypeScript in this repository workspace, optionally requesting approved MCP tools.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"source": map[string]any{"type": "string"}, "args": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}, "requestedTools": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"}}, "required": []string{"name", "reason"}}}}, "required": []string{"source"}}},
		map[string]any{"name": ToolPreapprove, "description": "Ask the user to approve a saved script's requested MCP tools and optionally register its declared schedule. This never grants approval by itself.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "registerSchedule": map[string]any{"type": "boolean", "default": false}}, "required": []string{"path"}}},
	}
}

func (s *server) result(id json.RawMessage, value any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": idValue(id), "result": value})
}
func (s *server) rpcError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": idValue(id), "error": map[string]any{"code": code, "message": message}})
}
func idValue(raw json.RawMessage) any { var v any; _ = json.Unmarshal(raw, &v); return v }
func (s *server) write(v any)         { s.mu.Lock(); defer s.mu.Unlock(); _ = json.NewEncoder(s.out).Encode(v) }
