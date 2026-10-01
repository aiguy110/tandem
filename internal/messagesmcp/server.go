// Package messagesmcp implements the internal MCP bridge (tandem-messages)
// that lets ACP agents message other agents. It is a thin stdio JSON-RPC
// server: each tool call is POSTed to the daemon's /internal/messages/<tool>
// with the agent's session ID, and the daemon enforces links and budgets.
package messagesmcp

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

// Tool names exposed to agents.
const (
	ToolDirectory   = "messages_directory"
	ToolSend        = "messages_send"
	ToolAsk         = "messages_ask"
	ToolReply       = "messages_reply"
	ToolDecline     = "messages_decline"
	ToolRequestLink = "messages_request_link"
	ToolSetCard     = "messages_set_card"
)

// endpoints maps a tool to its daemon operation.
var endpoints = map[string]string{
	ToolDirectory: "directory", ToolSend: "send", ToolAsk: "ask", ToolReply: "reply",
	ToolDecline: "decline", ToolRequestLink: "request_link", ToolSetCard: "set_card",
}

// Config identifies the calling agent and the daemon to call.
type Config struct {
	ControlURL string
	Token      string
	SessionID  string
	HTTPClient *http.Client
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
		s.result(req.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "tandem-messages", "version": "0.1.0"}})
	case "ping":
		s.result(req.ID, map[string]any{})
	case "tools/list":
		s.result(req.ID, map[string]any{"tools": toolDeclarations()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || endpoints[p.Name] == "" {
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

// call POSTs the tool arguments, plus the caller's sessionId, to the daemon.
// A non-2xx response carries {"error":"<code>: <detail>"}.
func (s *server) call(ctx context.Context, tool string, args map[string]any) (any, error) {
	if s.cfg.ControlURL == "" {
		return nil, errors.New("TANDEM_CONTROL_URL not set")
	}
	body := make(map[string]any, len(args)+1)
	for k, v := range args {
		body[k] = v
	}
	body["sessionId"] = s.cfg.SessionID
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(s.cfg.ControlURL, "/") + "/internal/messages/" + endpoints[tool]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("messages request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read messages response: %w", err)
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
		return nil, errors.New(message)
	}
	if len(bytes.TrimSpace(responseBody)) == 0 {
		return map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal(responseBody, &value); err != nil {
		return nil, errors.New("messages service returned invalid JSON")
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

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{"name": name, "description": desc, "inputSchema": schema}
}

const toAddr = "Agent reference, e.g. @agent:bifrost/api-worker (from messages_directory)"

func toolDeclarations() []any {
	return []any{
		tool(ToolDirectory, "List agents you can message, on this host and on federated hosts. Each entry has a ref (e.g. @agent:bifrost/api-worker): pass it as `to` to the other tools. It also shows whether you already hold a link (canMessage). Optionally filter with a query.",
			map[string]any{"query": str("Case-insensitive text matched against name, purpose, repo, path and host.")}),
		tool(ToolSend, "Send a one-way message to another agent over a human-granted link. Delivered into the agent's turn (or queued). Fails with no_link if you have no link; use messages_request_link.",
			map[string]any{"to": str(toAddr), "body": str("Message text."), "threadId": str("Continue an existing thread (optional).")}, "to", "body"),
		tool(ToolAsk, "Ask another agent a question and return immediately. The answer arrives later as a message in your conversation; end your turn normally instead of waiting. Requires a link.",
			map[string]any{"to": str(toAddr), "body": str("The question."), "timeoutMinutes": map[string]any{"type": "integer", "description": "Give up after this many minutes (default 30, max 1440)."}}, "to", "body"),
		tool(ToolReply, "Answer an ask you received (a message with a request-id). Needs no link.",
			map[string]any{"requestId": str("The request-id of the ask."), "body": str("Your answer.")}, "requestId", "body"),
		tool(ToolDecline, "Decline an ask you received, with a reason. Needs no link.",
			map[string]any{"requestId": str("The request-id of the ask."), "reason": str("Why you are not answering.")}, "requestId", "reason"),
		tool(ToolRequestLink, "Ask a human to let you message another agent. The request appears in that agent's approvals. This call blocks until the human approves or denies (up to 10 minutes) and returns {status: \"approved\"|\"denied\"}; if there is still no decision it returns {status: \"pending\"} and the outcome later arrives as a message (link_approved or link_denied). Once approved you can use messages_send or messages_ask.",
			map[string]any{"to": str(toAddr), "reason": str("Why you need to message this agent.")}, "to", "reason"),
		tool(ToolSetCard, "Set the one-line description of your purpose shown to other agents in the directory.",
			map[string]any{"card": str("One line, about 200 characters at most.")}, "card"),
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
