// Package browsermcp implements tandem mcp-browser, the agent-facing browser
// MCP server. It re-exports Playwright MCP's tools with a required browserId
// argument and routes each call to a Playwright MCP child connected to that
// browser instance, plus tools to list saved browser profiles and to list,
// request (human-approved) and close browser instances.
package browsermcp

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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/toolregistry"
)

const (
	ListSavedProfilesTool = "browser_list_saved_profiles"
	ListInstancesTool     = "browser_list_instances"
	RequestInstanceTool   = "browser_request_instance"
	CloseInstanceTool     = "browser_close_instance"
)

// Child is one Playwright MCP connection to one browser instance.
type Child interface {
	ToolDeclarations() []toolregistry.Declaration
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error)
	Availability(context.Context) error
	Close() error
}

type Config struct {
	ControlURL, Token, SessionID string
	// NodeRuntime and PlaywrightCLI launch Playwright MCP; CDPBase is the
	// broker's endpoint prefix (see browser.Broker.CDPBase).
	NodeRuntime, PlaywrightCLI, CDPBase, OutputDir string
	HTTPClient                                     *http.Client
	PollInterval                                   time.Duration
	Stderr                                         io.Writer
	// StartChild overrides how a Playwright MCP child is launched (tests).
	StartChild func(ctx context.Context, browserID, endpoint string) (Child, error)
}

type childSlot struct {
	callMu sync.Mutex
	child  Child
}

type server struct {
	cfg     Config
	ctx     context.Context
	out     io.Writer
	writeMu sync.Mutex

	startMu  sync.Mutex
	mu       sync.Mutex
	children map[string]*childSlot
	tools    []toolregistry.Declaration
	toolSet  map[string]bool
}

// Run serves newline-delimited MCP JSON-RPC until input closes or ctx ends.
func Run(ctx context.Context, in io.Reader, out io.Writer, cfg Config) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{cfg: cfg, ctx: ctx, out: out, children: make(map[string]*childSlot)}
	if s.cfg.StartChild == nil {
		s.cfg.StartChild = s.startPlaywright
	}
	defer s.closeAll()
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 64<<10), 16<<20)
	var calls sync.WaitGroup
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		calls.Add(1)
		go func() { defer calls.Done(); s.handle(line) }()
	}
	err := scan.Err()
	cancel()
	calls.Wait()
	return err
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *server) handle(line []byte) {
	var req request
	if json.Unmarshal(line, &req) != nil {
		return
	}
	switch req.Method {
	case "notifications/initialized":
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		s.result(req.ID, map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "tandem-browser", "version": "0.1.0"}})
	case "ping":
		s.result(req.ID, map[string]any{})
	case "tools/list":
		tools := routerDeclarations()
		for _, tool := range s.playwrightTools() {
			tools = append(tools, tool)
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
		result, err := s.call(p.Name, p.Arguments)
		if err != nil {
			// Tool failures are reported as tool results so the model sees them.
			s.result(req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": err.Error()}}, "isError": true})
			return
		}
		s.result(req.ID, result)
	default:
		if len(req.ID) > 0 {
			s.rpcError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *server) call(name string, raw json.RawMessage) (any, error) {
	ctx := s.ctx
	switch name {
	case ListSavedProfilesTool:
		var out struct {
			SavedProfiles []browser.SavedProfileInfo `json:"savedProfiles"`
		}
		if err := s.control(ctx, http.MethodGet, browser.InstanceRequestsPath+"/saved-profiles", nil, &out); err != nil {
			return nil, err
		}
		return toolJSON(map[string]any{"savedProfiles": out.SavedProfiles, "note": "Opening a browser from a saved profile requires human approval via " + RequestInstanceTool + "."}), nil
	case ListInstancesTool:
		instances, err := s.instances(ctx)
		if err != nil {
			return nil, err
		}
		return toolJSON(map[string]any{"instances": instances}), nil
	case RequestInstanceTool:
		return s.requestInstance(ctx, raw)
	case CloseInstanceTool:
		var args struct {
			BrowserID string `json:"browserId"`
		}
		if json.Unmarshal(raw, &args) != nil || args.BrowserID == "" {
			return nil, errors.New("browserId is required")
		}
		if err := s.control(ctx, http.MethodPost, browser.InstanceRequestsPath+"/close", map[string]string{"sessionId": s.cfg.SessionID, "browserId": args.BrowserID}, nil); err != nil {
			return nil, err
		}
		s.dropChild(args.BrowserID)
		return toolText("Closed browser " + args.BrowserID + "."), nil
	}
	s.mu.Lock()
	known := s.toolSet[name]
	s.mu.Unlock()
	if !known {
		// Tools are listed before any call, but a client may call first.
		s.playwrightTools()
		s.mu.Lock()
		known = s.toolSet[name]
		s.mu.Unlock()
		if !known {
			return nil, fmt.Errorf("unknown tool: %s", name)
		}
	}
	return s.callPlaywright(ctx, name, raw)
}

func (s *server) requestInstance(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Source         string `json:"source"`
		SavedProfileID string `json:"savedProfileId"`
		Reason         string `json:"reason"`
		Name           string `json:"name"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("invalid arguments")
	}
	var registered struct {
		RequestID string `json:"requestId"`
	}
	body := browser.InstanceRequest{SessionID: s.cfg.SessionID, Source: browser.InstanceSource{Kind: args.Source, ProfileID: args.SavedProfileID}, Reason: args.Reason, Name: args.Name}
	if err := s.control(ctx, http.MethodPost, browser.InstanceRequestsPath+"/request", body, &registered); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		var status struct {
			Done     bool              `json:"done"`
			Denied   bool              `json:"denied"`
			Instance *browser.Instance `json:"instance"`
		}
		query := "?sessionId=" + url.QueryEscape(s.cfg.SessionID) + "&requestId=" + url.QueryEscape(registered.RequestID)
		if err := s.control(ctx, http.MethodGet, browser.InstanceRequestsPath+"/status"+query, nil, &status); err != nil {
			if strings.Contains(err.Error(), "no such browser request") {
				return nil, errors.New("the browser request was abandoned (the daemon restarted); ask again if still needed")
			}
			continue
		}
		if !status.Done {
			continue
		}
		if status.Denied || status.Instance == nil {
			return nil, errors.New("the human denied the browser request")
		}
		instance := *status.Instance
		source := "a fresh profile"
		if instance.SourceProfileName != "" {
			source = "saved profile \"" + instance.SourceProfileName + "\""
		}
		return toolText(fmt.Sprintf("Approved. Opened browser %q (browserId %q) from %s. Pass browserId %q to browser tools to act in it; your other browsers are unchanged.", instance.Name, instance.ID, source, instance.ID)), nil
	}
}

func (s *server) instances(ctx context.Context) ([]browser.Instance, error) {
	var out struct {
		Instances []browser.Instance `json:"instances"`
	}
	err := s.control(ctx, http.MethodGet, browser.InstanceRequestsPath+"?sessionId="+url.QueryEscape(s.cfg.SessionID), nil, &out)
	return out.Instances, err
}

func (s *server) callPlaywright(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	args := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("invalid arguments")
	}
	browserID, _ := args["browserId"].(string)
	if browserID == "" {
		return nil, fmt.Errorf("browserId is required; use %q for this session's original browser or call %s", browser.DefaultBrowserID, ListInstancesTool)
	}
	delete(args, "browserId")
	instances, err := s.instances(ctx)
	if err != nil {
		return nil, fmt.Errorf("list browsers: %w", err)
	}
	found := false
	for _, instance := range instances {
		found = found || instance.ID == browserID
	}
	if !found {
		s.dropChild(browserID)
		return nil, fmt.Errorf("no browser %q is open for this session; call %s", browserID, ListInstancesTool)
	}
	if err := unscopeRefs(args, browserID); err != nil {
		return nil, err
	}
	slot, err := s.child(browserID)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(args)
	// Playwright MCP keeps mutable per-page state (selected tab, element
	// refs); calls to one browser run one at a time.
	slot.callMu.Lock()
	result, err := slot.child.CallTool(ctx, name, encoded)
	slot.callMu.Unlock()
	if err != nil {
		slog.Warn("browser tool call failed", "session_id", s.cfg.SessionID, "browser_id", browserID, "tool", name, "error", err)
		return nil, err
	}
	if browserID != browser.DefaultBrowserID {
		result = scopeRefs(result, browserID)
		s.scopeSnapshotFiles(result, browserID)
	}
	return result, nil
}

// snapshotLinkPattern matches the page-snapshot files Playwright MCP links
// from tool results instead of inlining large snapshots.
var snapshotLinkPattern = regexp.MustCompile(`\]\(([^)\s]+\.(?:yml|yaml|md))\)`)

// scopeSnapshotFiles applies scopeRefs' prefixing to snapshot files linked
// from a result, so references an agent reads from disk are scoped the same
// way as inline ones. Only files inside this browser's output directory are
// touched.
func (s *server) scopeSnapshotFiles(result json.RawMessage, browserID string) {
	if s.cfg.OutputDir == "" {
		return
	}
	root, err := filepath.Abs(filepath.Join(s.cfg.OutputDir, browserID))
	if err != nil {
		return
	}
	for _, match := range snapshotLinkPattern.FindAllStringSubmatch(string(result), -1) {
		path, err := filepath.Abs(match[1])
		if err != nil || !strings.HasPrefix(path, root+string(filepath.Separator)) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scoped := refPattern.ReplaceAll(data, []byte("[ref="+browserID+":$1]"))
		if err := os.WriteFile(path, scoped, 0o644); err != nil {
			slog.Warn("scope snapshot file refs failed", "browser_id", browserID, "path", path, "error", err)
		}
	}
}

// refPattern matches element references in Playwright's page snapshots.
var refPattern = regexp.MustCompile(`\[ref=([A-Za-z0-9_]+)\]`)

// scopeRefs prefixes element references produced by an additional browser
// with its ID, so a reference can never silently resolve against another
// browser.
func scopeRefs(result json.RawMessage, browserID string) json.RawMessage {
	var decoded map[string]any
	if json.Unmarshal(result, &decoded) != nil {
		return result
	}
	content, _ := decoded["content"].([]any)
	for _, item := range content {
		block, _ := item.(map[string]any)
		if block == nil || block["type"] != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			block["text"] = refPattern.ReplaceAllString(text, "[ref="+browserID+":$1]")
		}
	}
	out, err := json.Marshal(decoded)
	if err != nil {
		return result
	}
	return out
}

// elementRefPattern matches a bare Playwright element reference such as
// "e12" or "f1e3" (an element inside a frame).
var elementRefPattern = regexp.MustCompile(`^(?:f\d+)?e\d+$`)

// isTargetKey reports whether an argument names an element: older Playwright
// MCP uses ref/startRef/endRef, newer target/startTarget/endTarget (which may
// also hold a CSS/text selector).
func isTargetKey(key string) bool {
	return key == "ref" || key == "target" || strings.HasSuffix(key, "Ref") || strings.HasSuffix(key, "Target")
}

// unscopeRefs validates and strips browser-scoped references in arguments:
// "b2:e5" must target b2, and an additional browser never accepts a bare
// reference (which came from the default browser). Selectors pass through.
func unscopeRefs(value any, browserID string) error {
	switch v := value.(type) {
	case map[string]any:
		for key, field := range v {
			if ref, ok := field.(string); ok && isTargetKey(key) {
				owner, local, scoped := strings.Cut(ref, ":")
				switch {
				case scoped && browser.ValidBrowserID(owner) && elementRefPattern.MatchString(local):
					if owner != browserID {
						return fmt.Errorf("element reference %q belongs to browser %q, not %q; take a new browser_snapshot of browser %q", ref, owner, browserID, browserID)
					}
					v[key] = local
				case elementRefPattern.MatchString(ref) && browserID != browser.DefaultBrowserID:
					return fmt.Errorf("element reference %q is not from browser %q; take a new browser_snapshot of browser %q", ref, browserID, browserID)
				}
				continue
			}
			if err := unscopeRefs(field, browserID); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := unscopeRefs(item, browserID); err != nil {
				return err
			}
		}
	}
	return nil
}

// playwrightTools lists Playwright MCP's tools (learned from the default
// browser's child) with the routing argument added.
func (s *server) playwrightTools() []map[string]any {
	s.mu.Lock()
	tools := s.tools
	s.mu.Unlock()
	if tools == nil {
		slot, err := s.child(browser.DefaultBrowserID)
		if err != nil {
			slog.Warn("playwright MCP unavailable; browser tools omitted", "session_id", s.cfg.SessionID, "error", err)
			return nil
		}
		tools = slot.child.ToolDeclarations()
		set := make(map[string]bool, len(tools))
		for _, tool := range tools {
			set[tool.Name] = true
		}
		s.mu.Lock()
		s.tools, s.toolSet = tools, set
		s.mu.Unlock()
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if isRouterTool(tool.Name) {
			continue
		}
		out = append(out, routedDeclaration(tool))
	}
	return out
}

func isRouterTool(name string) bool {
	switch name {
	case ListSavedProfilesTool, ListInstancesTool, RequestInstanceTool, CloseInstanceTool:
		return true
	}
	return false
}

func routedDeclaration(tool toolregistry.Declaration) map[string]any {
	schema := map[string]any{}
	_ = json.Unmarshal(tool.InputSchema, &schema)
	if schema["type"] == nil {
		schema["type"] = "object"
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
	}
	properties["browserId"] = map[string]any{"type": "string", "description": "Browser instance to act in: \"default\" for this session's original browser, or an ID from browser_list_instances."}
	schema["properties"] = properties
	required, _ := schema["required"].([]any)
	schema["required"] = append([]any{"browserId"}, required...)
	declaration := map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": schema}
	if len(tool.OutputSchema) > 0 {
		declaration["outputSchema"] = tool.OutputSchema
	}
	return declaration
}

func routerDeclarations() []any {
	return []any{
		map[string]any{
			"name":        ListSavedProfilesTool,
			"description": "List saved browser profiles: reusable starting states (cookies, logins, site data) the human has captured. Listing grants no access; use browser_request_instance to ask for a browser started from one.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name":        ListInstancesTool,
			"description": "List this session's open browser instances. Each has its own profile, tabs and element references; every browser tool takes the browserId to act in. \"default\" is the session's original browser.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name":        RequestInstanceTool,
			"description": "Ask the human to open an additional, independent browser instance for this session. Blocks until the human approves or denies; on approval returns the new browserId. Existing browsers keep running unchanged. Use source \"user_choice\" when you do not know which saved profile (if any) is appropriate — the human picks.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"source":         map[string]any{"type": "string", "enum": []string{browser.SourceFresh, browser.SourceSavedProfile, browser.SourceUserChoice}, "description": "fresh: an empty profile. saved_profile: start from savedProfileId. user_choice: let the human choose a saved profile or fresh."},
				"savedProfileId": map[string]any{"type": "string", "description": "Required when source is saved_profile; an ID from browser_list_saved_profiles."},
				"reason":         map[string]any{"type": "string", "description": "Why you need this browser; shown to the human."},
				"name":           map[string]any{"type": "string", "description": "Optional short label for the browser, e.g. the task it is for."},
			}, "required": []string{"source", "reason"}},
		},
		map[string]any{
			"name":        CloseInstanceTool,
			"description": "Close an additional browser instance and discard its working copy. The default browser cannot be closed.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"browserId": map[string]any{"type": "string"},
			}, "required": []string{"browserId"}},
		},
	}
}

func (s *server) child(browserID string) (*childSlot, error) {
	s.mu.Lock()
	slot := s.children[browserID]
	s.mu.Unlock()
	if slot != nil && slot.child.Availability(s.ctx) == nil {
		return slot, nil
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	slot = s.children[browserID]
	s.mu.Unlock()
	if slot != nil && slot.child.Availability(s.ctx) == nil {
		return slot, nil
	}
	if slot != nil {
		_ = slot.child.Close()
	}
	endpoint := s.cfg.CDPBase + url.PathEscape(browser.InstanceKey(s.cfg.SessionID, browserID))
	child, err := s.cfg.StartChild(s.ctx, browserID, endpoint)
	if err != nil {
		return nil, fmt.Errorf("start Playwright MCP for browser %s: %w", browserID, err)
	}
	slog.Info("playwright MCP started", "session_id", s.cfg.SessionID, "browser_id", browserID)
	slot = &childSlot{child: child}
	s.mu.Lock()
	s.children[browserID] = slot
	s.mu.Unlock()
	return slot, nil
}

func (s *server) dropChild(browserID string) {
	s.mu.Lock()
	slot := s.children[browserID]
	delete(s.children, browserID)
	s.mu.Unlock()
	if slot != nil {
		_ = slot.child.Close()
	}
}

func (s *server) closeAll() {
	s.mu.Lock()
	slots := s.children
	s.children = map[string]*childSlot{}
	s.mu.Unlock()
	for _, slot := range slots {
		_ = slot.child.Close()
	}
}

func (s *server) startPlaywright(ctx context.Context, browserID, endpoint string) (Child, error) {
	if s.cfg.NodeRuntime == "" || s.cfg.PlaywrightCLI == "" {
		return nil, errors.New("Playwright MCP is not installed")
	}
	outputDir := filepath.Join(s.cfg.OutputDir, browserID)
	_ = os.MkdirAll(outputDir, 0o755)
	server := browser.MCPServer{
		Name: "playwright-" + browserID, Command: s.cfg.NodeRuntime,
		Args: []string{s.cfg.PlaywrightCLI, "--cdp-endpoint", endpoint, "--output-dir", outputDir},
	}
	return toolregistry.StartMCP(ctx, server, "", s.cfg.Stderr)
}

func (s *server) control(ctx context.Context, method, path string, body, out any) error {
	if s.cfg.ControlURL == "" {
		return errors.New("TANDEM_CONTROL_URL not set")
	}
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.ControlURL, "/")+path, reader)
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
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func toolText(value string) map[string]any {
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": value}}}
}

func toolJSON(value any) map[string]any {
	encoded, _ := json.MarshalIndent(value, "", "  ")
	return toolText(string(encoded))
}

func (s *server) result(id json.RawMessage, value any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": idValue(id), "result": value})
}

func (s *server) rpcError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": idValue(id), "error": map[string]any{"code": code, "message": message}})
}

func idValue(raw json.RawMessage) any { var v any; _ = json.Unmarshal(raw, &v); return v }

func (s *server) write(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = json.NewEncoder(s.out).Encode(v)
}
