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
	// HandleDialogTool is Playwright MCP's dialog tool; the router answers
	// daemon-tracked dialogs itself (see handleDialog).
	HandleDialogTool = "browser_handle_dialog"

	// DefaultCallTimeout bounds one Playwright tool call, counting only time
	// the agent holds control (a human takeover legitimately holds calls).
	DefaultCallTimeout = 2 * time.Minute
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
	// JSRuntime (Bun) and PlaywrightCLI launch Playwright MCP; CDPBase is the
	// broker's endpoint prefix (see browser.Broker.CDPBase).
	JSRuntime, PlaywrightCLI, CDPBase, OutputDir string
	HTTPClient                                   *http.Client
	PollInterval                                 time.Duration
	// CallTimeout bounds a Playwright tool call (DefaultCallTimeout when
	// zero); a call that exceeds it restarts that browser's Playwright MCP.
	CallTimeout time.Duration
	// DialogPollInterval is how often a running call checks the daemon for a
	// newly opened dialog or a human takeover (500ms when zero).
	DialogPollInterval time.Duration
	Stderr             io.Writer
	// StartChild overrides how a Playwright MCP child is launched (tests).
	StartChild func(ctx context.Context, browserID, endpoint string) (Child, error)
}

// childSlot is one Playwright MCP child. Calls to it run one at a time;
// lock is a one-token channel so waiting for it can be bounded.
type childSlot struct {
	lock  chan struct{}
	child Child
}

func newChildSlot(child Child) *childSlot {
	return &childSlot{lock: make(chan struct{}, 1), child: child}
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
	// dialogSeen marks browsers where this router reported a dialog; once it
	// closes, Playwright's possibly stale modal state is cleared (see
	// clearStaleDialog).
	dialogSeen map[string]bool
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
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = DefaultCallTimeout
	}
	if cfg.DialogPollInterval <= 0 {
		cfg.DialogPollInterval = 500 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{cfg: cfg, ctx: ctx, out: out, children: make(map[string]*childSlot), dialogSeen: make(map[string]bool)}
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
	if name == HandleDialogTool {
		return s.handleDialog(ctx, browserID, args)
	}
	status, statusErr := s.dialogStatus(ctx, browserID)
	if statusErr == nil {
		if len(status.Dialogs) > 0 {
			s.setDialogSeen(browserID, true)
			return nil, errors.New(dialogBlockedMessage(browserID, status.Dialogs, "this tool call was not run"))
		}
		s.clearStaleDialog(ctx, browserID)
	}
	encoded, _ := json.Marshal(args)
	result, err := s.runPlaywright(ctx, browserID, name, encoded, true)
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

type callResult struct {
	result json.RawMessage
	err    error
}

// runPlaywright runs one tool call on a browser's Playwright MCP child,
// serialized with that browser's other calls because Playwright MCP keeps
// mutable per-page state (selected tab, element refs).
//
// The call is supervised rather than awaited blindly: a call (or the wait for
// the previous one) that runs past CallTimeout of agent-held time restarts
// the child, since nothing else can unblock it; with watchDialogs, a
// JavaScript dialog that opens meanwhile returns at once, because the page
// cannot make progress until someone answers it. In that case the call keeps
// running and holds the browser until it settles, which answering the dialog
// allows.
func (s *server) runPlaywright(ctx context.Context, browserID, name string, arguments json.RawMessage, watchDialogs bool) (json.RawMessage, error) {
	slot, err := s.child(browserID)
	if err != nil {
		return nil, err
	}
	clock := s.newCallClock(browserID)
	defer clock.stop()
	acquired := false
	for !acquired {
		select {
		case slot.lock <- struct{}{}:
			acquired = true
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clock.tick():
			status, statusErr := clock.advance(ctx)
			if watchDialogs && statusErr == nil && len(status.Dialogs) > 0 {
				s.setDialogSeen(browserID, true)
				return nil, errors.New(dialogBlockedMessage(browserID, status.Dialogs, "this tool call was not run"))
			}
			if clock.expired() {
				slog.Warn("browser tool call gave up waiting for the previous call", "session_id", s.cfg.SessionID, "browser_id", browserID, "tool", name, "timeout", s.cfg.CallTimeout)
				return nil, fmt.Errorf("browser %q is still busy with an earlier tool call after %s; try again shortly", browserID, s.cfg.CallTimeout)
			}
		}
	}
	callCtx, cancel := context.WithCancel(ctx)
	done := make(chan callResult, 1)
	go func() {
		result, err := slot.child.CallTool(callCtx, name, arguments)
		cancel()
		<-slot.lock
		done <- callResult{result, err}
	}()
	clock.stop()
	clock = s.newCallClock(browserID)
	defer clock.stop()
	for {
		select {
		case got := <-done:
			return got.result, got.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clock.tick():
			status, statusErr := clock.advance(ctx)
			if watchDialogs && statusErr == nil && len(status.Dialogs) > 0 {
				s.setDialogSeen(browserID, true)
				slog.Info("browser tool call interrupted by a dialog", "session_id", s.cfg.SessionID, "browser_id", browserID, "tool", name, "dialog_type", status.Dialogs[0].Type)
				go func() {
					got := <-done
					slog.Info("browser tool call settled after its dialog", "session_id", s.cfg.SessionID, "browser_id", browserID, "tool", name, "error", got.err)
				}()
				return nil, errors.New(dialogBlockedMessage(browserID, status.Dialogs, name+" opened it, or was running when it opened, and has not finished"))
			}
			if clock.expired() {
				slog.Warn("browser tool call timed out; restarting Playwright MCP", "session_id", s.cfg.SessionID, "browser_id", browserID, "tool", name, "timeout", s.cfg.CallTimeout)
				s.resetChild(browserID, slot)
				<-done
				return nil, fmt.Errorf("%s did not finish within %s, so Tandem restarted the Playwright connection for browser %q. The browser itself, its tabs and logins are unchanged; take a browser_snapshot to see where it is", name, s.cfg.CallTimeout, browserID)
			}
		}
	}
}

// callClock measures a call's agent-held time: while the human holds the
// browser, the broker deliberately queues the agent's CDP traffic, so that
// time does not count toward CallTimeout.
type callClock struct {
	s         *server
	browserID string
	ticker    *time.Ticker
	last      time.Time
	elapsed   time.Duration
}

func (s *server) newCallClock(browserID string) *callClock {
	return &callClock{s: s, browserID: browserID, ticker: time.NewTicker(s.cfg.DialogPollInterval), last: time.Now()}
}

func (c *callClock) tick() <-chan time.Time { return c.ticker.C }

func (c *callClock) advance(ctx context.Context) (browser.DialogStatus, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	status, err := c.s.dialogStatus(statusCtx, c.browserID)
	cancel()
	now := time.Now()
	if err != nil || status.ControlOwner != browser.ControlUser {
		c.elapsed += now.Sub(c.last)
	}
	c.last = now
	return status, err
}

func (c *callClock) expired() bool { return c.elapsed >= c.s.cfg.CallTimeout }

func (c *callClock) stop() { c.ticker.Stop() }

// resetChild closes a wedged Playwright MCP child; the next call starts a
// fresh one. Closing it ends its CDP connection, not the browser.
func (s *server) resetChild(browserID string, slot *childSlot) {
	s.mu.Lock()
	if s.children[browserID] == slot {
		delete(s.children, browserID)
	}
	s.mu.Unlock()
	_ = slot.child.Close()
}

func (s *server) dialogStatus(ctx context.Context, browserID string) (browser.DialogStatus, error) {
	var status browser.DialogStatus
	query := "?sessionId=" + url.QueryEscape(s.cfg.SessionID) + "&browserId=" + url.QueryEscape(browserID)
	err := s.control(ctx, http.MethodGet, browser.DialogsPath+query, nil, &status)
	return status, err
}

func (s *server) setDialogSeen(browserID string, seen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seen {
		s.dialogSeen[browserID] = true
	} else {
		delete(s.dialogSeen, browserID)
	}
}

// clearStaleDialog runs after a dialog this router reported has closed. If
// Playwright MCP also saw the dialog, it still records it as modal state and
// would refuse every tool until browser_handle_dialog; when the human (or the
// router) answered the dialog through the daemon, nobody would. Dismissing it
// through Playwright clears that state; if the dialog is truly gone the CDP
// call merely fails. Dismiss, not accept: should a dialog the daemon missed
// still be open, staying on the page loses nothing.
func (s *server) clearStaleDialog(ctx context.Context, browserID string) {
	s.mu.Lock()
	seen := s.dialogSeen[browserID]
	s.mu.Unlock()
	if !seen {
		return
	}
	s.setDialogSeen(browserID, false)
	cleanupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := s.runPlaywright(cleanupCtx, browserID, HandleDialogTool, json.RawMessage(`{"accept":false}`), false)
	slog.Debug("cleared Playwright dialog state", "session_id", s.cfg.SessionID, "browser_id", browserID, "error", err)
}

// handleDialog answers a dialog the daemon tracks, which works even when
// Playwright MCP missed the dialog or is itself stuck behind it. Without one
// it falls back to Playwright's own tool.
func (s *server) handleDialog(ctx context.Context, browserID string, args map[string]any) (any, error) {
	accept, _ := args["accept"].(bool)
	promptText, _ := args["promptText"].(string)
	status, err := s.dialogStatus(ctx, browserID)
	if err != nil || len(status.Dialogs) == 0 {
		s.setDialogSeen(browserID, false)
		encoded, _ := json.Marshal(args)
		return s.runPlaywright(ctx, browserID, HandleDialogTool, encoded, false)
	}
	dialog := status.Dialogs[0]
	var handled struct {
		Remaining []browser.PendingDialog `json:"remaining"`
	}
	body := map[string]any{"sessionId": s.cfg.SessionID, "browserId": browserID, "targetId": dialog.TargetID, "accept": accept, "promptText": promptText}
	if err := s.control(ctx, http.MethodPost, browser.DialogsPath+"/handle", body, &handled); err != nil {
		return nil, fmt.Errorf("answer the %q dialog: %w", dialog.Type, err)
	}
	s.setDialogSeen(browserID, true)
	verb := "Dismissed"
	if accept {
		verb = "Accepted"
	}
	text := fmt.Sprintf("%s the %q dialog on %s.", verb, dialog.Type, dialogPage(dialog))
	if len(handled.Remaining) > 0 {
		text += " " + dialogBlockedMessage(browserID, handled.Remaining, "another dialog is still open")
	}
	return toolText(text), nil
}

func dialogPage(dialog browser.PendingDialog) string {
	if dialog.URL != "" {
		return dialog.URL
	}
	return "page " + dialog.TargetID
}

// dialogBlockedMessage explains an open dialog to the agent, including how
// to answer it.
func dialogBlockedMessage(browserID string, dialogs []browser.PendingDialog, context string) string {
	dialog := dialogs[0]
	var b strings.Builder
	fmt.Fprintf(&b, "A JavaScript %q dialog is open in browser %q on %s", dialog.Type, browserID, dialogPage(dialog))
	if dialog.Message != "" {
		fmt.Fprintf(&b, " with message %q", dialog.Message)
	}
	fmt.Fprintf(&b, " (%s). The page cannot respond until the dialog is answered. Call %s with accept true or false; the human can also answer it from the Browser pane.", context, HandleDialogTool)
	if dialog.Type == "beforeunload" {
		b.WriteString(" For this beforeunload dialog, accept leaves the page and discards unsaved form input; dismiss stays on the page.")
	}
	if len(dialogs) > 1 {
		fmt.Fprintf(&b, " %d more dialog(s) are open on other tabs.", len(dialogs)-1)
	}
	return b.String()
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
	slot = newChildSlot(child)
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
	if s.cfg.JSRuntime == "" || s.cfg.PlaywrightCLI == "" {
		return nil, errors.New("Playwright MCP is not installed")
	}
	outputDir := filepath.Join(s.cfg.OutputDir, browserID)
	_ = os.MkdirAll(outputDir, 0o755)
	server := browser.MCPServer{
		Name: "playwright-" + browserID, Command: s.cfg.JSRuntime,
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
