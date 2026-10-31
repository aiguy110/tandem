// Package artifactsmcp implements the internal MCP bridge (tandem-artifacts)
// that lets ACP agents publish rendered artifacts (docs/artifacts.md). It is a
// thin stdio JSON-RPC server: artifact_publish reads a file argument from the
// agent's workspace and POSTs the content, with the agent's session ID, to the
// daemon's /internal/artifacts/publish; artifact_docs is answered locally.
package artifactsmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Tool names exposed to agents.
const (
	ToolPublish = "artifact_publish"
	ToolDocs    = "artifact_docs"
)

// MaxBytes mirrors the daemon's asset limit so oversized files fail before
// they are read into memory and sent.
const MaxBytes = 10 * 1024 * 1024

// Config identifies the calling agent and the daemon to call.
type Config struct {
	ControlURL   string
	Token        string
	SessionID    string
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
	// An inline artifact arrives as one JSON-RPC line.
	scan.Buffer(make([]byte, 64<<10), 4*MaxBytes)
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
		s.result(req.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "tandem-artifacts", "version": "0.1.0"}})
	case "ping":
		s.result(req.ID, map[string]any{})
	case "tools/list":
		s.result(req.ID, map[string]any{"tools": toolDeclarations()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &p) != nil || (p.Name != ToolPublish && p.Name != ToolDocs) {
			s.rpcError(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		if p.Name == ToolDocs {
			s.result(req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": Docs}}})
			return
		}
		var args publishArgs
		if len(p.Arguments) > 0 && json.Unmarshal(p.Arguments, &args) != nil {
			s.rpcError(req.ID, -32602, "tool arguments must be an object")
			return
		}
		value, err := s.publish(ctx, args)
		if err != nil {
			slog.Warn("artifact_publish failed", "session_id", s.cfg.SessionID, "error", err)
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

type publishArgs struct {
	Path     string `json:"path"`
	HTML     string `json:"html"`
	Title    string `json:"title"`
	Filename string `json:"filename"`
}

func (s *server) publish(ctx context.Context, args publishArgs) (any, error) {
	if (args.Path == "") == (args.HTML == "") {
		return nil, errors.New("provide exactly one of `path` (an HTML file) or `html` (the document as a string)")
	}
	content, source := args.HTML, ""
	if args.Path != "" {
		file := args.Path
		if !filepath.IsAbs(file) {
			if s.cfg.WorkspaceCWD == "" {
				return nil, errors.New("relative path given but the workspace directory is unknown; pass an absolute path")
			}
			file = filepath.Join(s.cfg.WorkspaceCWD, file)
		}
		info, err := os.Stat(file)
		if err != nil {
			return nil, fmt.Errorf("read artifact file: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a directory; pass an HTML file", file)
		}
		if info.Size() > MaxBytes {
			return nil, fmt.Errorf("%s is %d bytes; artifacts are limited to %d", file, info.Size(), MaxBytes)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read artifact file: %w", err)
		}
		content, source = string(data), file
	}
	return s.post(ctx, map[string]any{
		"sessionId": s.cfg.SessionID, "type": "html", "title": args.Title, "filename": args.Filename,
		"content": content, "sourcePath": source,
	})
}

// post sends a publish request to the daemon. A non-2xx response carries
// {"error":"<detail>"}.
func (s *server) post(ctx context.Context, body map[string]any) (any, error) {
	if s.cfg.ControlURL == "" {
		return nil, errors.New("TANDEM_CONTROL_URL not set")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(s.cfg.ControlURL, "/") + "/internal/artifacts/publish"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifacts request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read artifacts response: %w", err)
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
	var value map[string]any
	if err := json.Unmarshal(responseBody, &value); err != nil {
		return nil, errors.New("artifacts service returned invalid JSON")
	}
	delete(value, "kind")
	value["status"] = "published"
	value["note"] = "The user now sees a preview card in the chat and can open it full-page or download it. Do not paste the HTML into your reply."
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

func toolDeclarations() []any {
	return []any{
		map[string]any{
			"name": ToolPublish,
			"description": "Publish a rendered HTML artifact to the user. It appears in the chat as a scaled-down live preview the user can open full-page or download as an .html file. " +
				"Pass exactly one of `path` (an HTML file, absolute or relative to your workspace) or `html` (the complete document as a string). " +
				"Each call publishes a new artifact; to revise one, publish again. Call artifact_docs first if you have not used artifacts in this session.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"path":     str("HTML file to publish, absolute or relative to your workspace. Prefer this for large documents."),
				"html":     str("A complete, self-contained HTML document."),
				"title":    str("Short human-readable title shown on the preview card (defaults to the file name)."),
				"filename": str("Download file name (optional; .html is appended if missing)."),
			}},
		},
		map[string]any{
			"name":        ToolDocs,
			"description": "Instructions and best practices for Tandem artifacts: how they render, sandbox limits, and how to make them look good in the preview and full-page views.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
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
