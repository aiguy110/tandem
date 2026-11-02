// Package artifactsmcp implements the internal MCP bridge (tandem-artifacts)
// that lets ACP agents publish, find, read, and revise rendered artifacts
// (docs/artifacts.md) on their own host. It is a thin stdio JSON-RPC server:
// file arguments are read and written in the agent's workspace, and each tool
// POSTs to the daemon's /internal/artifacts/<op> with the agent's session ID;
// artifact_docs is answered locally.
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
	ToolPublish   = "artifact_publish"
	ToolList      = "artifact_list"
	ToolRead      = "artifact_read"
	ToolRevisions = "artifact_revisions"
	ToolDocs      = "artifact_docs"
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
		if json.Unmarshal(req.Params, &p) != nil {
			s.rpcError(req.ID, -32602, "invalid tools/call params")
			return
		}
		if p.Name == ToolDocs {
			var args struct {
				Guide string `json:"guide"`
			}
			_ = json.Unmarshal(p.Arguments, &args)
			text := Docs
			if args.Guide != "" {
				guide, ok := Guides[strings.TrimSpace(args.Guide)]
				if !ok {
					s.result(req.ID, toolError("unknown guide "+args.Guide+"; see the Guides section of artifact_docs"))
					return
				}
				text = guide
			}
			s.result(req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}})
			return
		}
		var args toolArgs
		if len(p.Arguments) > 0 && json.Unmarshal(p.Arguments, &args) != nil {
			s.rpcError(req.ID, -32602, "tool arguments must be an object")
			return
		}
		var (
			value any
			err   error
		)
		switch p.Name {
		case ToolPublish:
			value, err = s.publish(ctx, args)
		case ToolList:
			value, err = s.call(ctx, "list", map[string]any{"query": args.Query, "limit": args.Limit})
		case ToolRevisions:
			if args.ArtifactID == "" {
				err = errors.New("artifactId is required")
			} else {
				value, err = s.call(ctx, "revisions", map[string]any{"artifactId": args.ArtifactID})
			}
		case ToolRead:
			value, err = s.read(ctx, args)
		default:
			s.rpcError(req.ID, -32602, "unknown tool: "+p.Name)
			return
		}
		if err != nil {
			slog.Warn("artifacts tool failed", "tool", p.Name, "session_id", s.cfg.SessionID, "artifact_id", args.ArtifactID, "error", err)
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

type toolArgs struct {
	Path         string `json:"path"`
	HTML         string `json:"html"`
	Title        string `json:"title"`
	Filename     string `json:"filename"`
	ArtifactID   string `json:"artifactId"`
	BaseRevision string `json:"baseRevision"`
	Revision     string `json:"revision"`
	Query        string `json:"query"`
	Limit        int    `json:"limit"`
}

// workspacePath resolves a path argument against the agent's workspace.
func (s *server) workspacePath(p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	if s.cfg.WorkspaceCWD == "" {
		return "", errors.New("relative path given but the workspace directory is unknown; pass an absolute path")
	}
	return filepath.Join(s.cfg.WorkspaceCWD, p), nil
}

func (s *server) publish(ctx context.Context, args toolArgs) (any, error) {
	if (args.Path == "") == (args.HTML == "") {
		return nil, errors.New("provide exactly one of `path` (an HTML file) or `html` (the document as a string)")
	}
	content, source := args.HTML, ""
	if args.Path != "" {
		file, err := s.workspacePath(args.Path)
		if err != nil {
			return nil, err
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
	value, err := s.call(ctx, "publish", map[string]any{
		"type": "html", "title": args.Title, "filename": args.Filename, "content": content, "sourcePath": source,
		"artifactId": args.ArtifactID, "baseRevision": args.BaseRevision,
	})
	if err != nil {
		return nil, err
	}
	delete(value, "kind")
	value["status"] = "published"
	note := "The user now sees a preview card in the chat and can open it full-page or download it. Do not paste the HTML into your reply."
	if args.ArtifactID != "" {
		note = "Published as a new revision. The card for it appears in this chat; earlier cards elsewhere keep showing the revisions they announced, and the gallery shows this one as the latest. Do not paste the HTML into your reply."
	}
	value["note"] = note
	return value, nil
}

// read returns a revision's document inline, or with `path` writes it to a
// workspace file (better for editing) and returns only the metadata.
func (s *server) read(ctx context.Context, args toolArgs) (any, error) {
	if args.ArtifactID == "" {
		return nil, errors.New("artifactId is required; use artifact_list to find it")
	}
	value, err := s.call(ctx, "read", map[string]any{"artifactId": args.ArtifactID, "revision": args.Revision})
	if err != nil {
		return nil, err
	}
	if args.Path == "" {
		return value, nil
	}
	file, err := s.workspacePath(args.Path)
	if err != nil {
		return nil, err
	}
	content, _ := value["content"].(string)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, fmt.Errorf("write artifact file: %w", err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write artifact file: %w", err)
	}
	delete(value, "content")
	value["path"] = file
	value["note"] = "Edit the file, then publish it with artifact_publish({path, artifactId, baseRevision}) using this revision as baseRevision."
	return value, nil
}

// call POSTs one operation to the daemon's bridge. A non-2xx response carries
// {"error":"<detail>"}.
func (s *server) call(ctx context.Context, op string, body map[string]any) (map[string]any, error) {
	if s.cfg.ControlURL == "" {
		return nil, errors.New("TANDEM_CONTROL_URL not set")
	}
	body["sessionId"] = s.cfg.SessionID
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(s.cfg.ControlURL, "/") + "/internal/artifacts/" + op
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
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*MaxBytes))
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

func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func toolDeclarations() []any {
	return []any{
		map[string]any{
			"name": ToolPublish,
			"description": "Publish a rendered HTML artifact to the user, or a new revision of an existing one. It appears in this chat as a scaled-down live preview the user can open full-page or download as an .html file. " +
				"Pass exactly one of `path` (an HTML file, absolute or relative to your workspace) or `html` (the complete document as a string). " +
				"Without `artifactId` this creates a new artifact; with `artifactId` (from artifact_list or an earlier publish) it adds a revision to that artifact, keeping its history. " +
				"Call artifact_docs first if you have not used artifacts in this session.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"path":         str("HTML file to publish, absolute or relative to your workspace. Prefer this for large documents."),
				"html":         str("A complete, self-contained HTML document."),
				"title":        str("Short human-readable title shown on the preview card (defaults to the file name, or the artifact's current title when updating)."),
				"filename":     str("Download file name (optional; .html is appended if missing; kept from the previous revision when updating)."),
				"artifactId":   str("Update this existing artifact with a new revision instead of creating a new artifact."),
				"baseRevision": str("Optional with artifactId: the revision your change is based on. The update is refused if someone published a newer revision meanwhile, instead of overwriting it."),
			}},
		},
		map[string]any{
			"name":        ToolList,
			"description": "Find artifacts on this host (published by any session): each artifact's latest revision with artifactId, revision, revisionNumber, title, filename, publishing session, and time, newest first.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query": str("Optional case-insensitive filter on title, artifactId, file name, or session name."),
				"limit": num("Optional maximum number of artifacts."),
			}},
		},
		map[string]any{
			"name":        ToolRevisions,
			"description": "List one artifact's revisions, newest first, with who published each and when.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"artifactId"}, "properties": map[string]any{
				"artifactId": str("The artifact to list."),
			}},
		},
		map[string]any{
			"name":        ToolRead,
			"description": "Read an artifact's HTML (latest revision unless `revision` is given). With `path`, writes the document to that workspace file instead of returning it inline; edit the file, then publish it back with artifact_publish({path, artifactId, baseRevision}).",
			"inputSchema": map[string]any{"type": "object", "required": []string{"artifactId"}, "properties": map[string]any{
				"artifactId": str("The artifact to read."),
				"revision":   str("Optional revision hash; defaults to the latest."),
				"path":       str("Optional file (absolute or relative to your workspace) to write the document to instead of returning it."),
			}},
		},
		map[string]any{
			"name": ToolDocs,
			"description": "Instructions and best practices for Tandem artifacts: how they render, sandbox limits, revisions, and how to make them look good in the preview and full-page views. " +
				"Pass `guide` for a task-specific playbook, e.g. \"paper-explainer\" for converting or summarizing a white paper, spec, or plan as a scroll-driven explainer with a pinned, animated figure.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"guide": str("Optional playbook name: paper-explainer. Omit for the general docs, which list all guides."),
			}},
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
