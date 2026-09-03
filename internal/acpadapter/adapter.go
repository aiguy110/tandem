package acpadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/terminalhost"
	"github.com/aiguy110/tandem/internal/workspacefs"
)

// AssetStore is the narrow asset-store surface needed to resolve prompt images
// and capture image-bearing tool results before they enter the event log.
type AssetStore interface {
	Get(agentID, assetID string) (assets.Stored, error)
	Put(agentID string, data []byte, declaredMIME string) (assets.Stored, error)
}

type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     []acp.EnvVariable `json:"env"`
}

type AdapterConfig struct {
	Transport       acp.Config
	AgentID         string
	Cwd             string
	ResumeSessionID string
	CaptureReplay   bool
	MCPServers      []MCPServer
	Assets          AssetStore
	WorkspaceFS     *workspacefs.FS
	Terminals       *terminalhost.Host
	Logger          *log.Logger
	// ParentToolCallIDPath is a dotted path into a session/update's `_meta`
	// whose value is the parent tool call's id (see config.ACPMeta). When set,
	// updates carrying it are annotated with a normalized `parentId`. Empty
	// disables the lookup — the common case for agents without subagent meta.
	ParentToolCallIDPath string
}

type Capabilities struct {
	Structured  bool
	LoadSession bool
	ForkSession bool
	Image       bool
	Steering    bool
}

type PromptBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	AssetID  string `json:"assetId,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Name     string `json:"name,omitempty"`
}

type SessionState struct {
	Modes         json.RawMessage   `json:"modes,omitempty"`
	ConfigOptions []json.RawMessage `json:"configOptions,omitempty"`
}

// Approval is the registry-facing view of an outstanding ACP permission
// request. The wire request ID remains private to the adapter.
type Approval struct {
	ReqID      string           `json:"reqId"`
	ToolCallID string           `json:"toolCallId"`
	Title      string           `json:"title"`
	Options    []ApprovalOption `json:"options"`
}

type ApprovalOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
}

type pendingPermission struct {
	wireID   json.RawMessage
	approval Approval
}

// OptionalUpdateError is diagnostic only. ACP can add session/update variants;
// clients must ignore those they do not understand.
type OptionalUpdateError struct{ Variant string }

func (e *OptionalUpdateError) Error() string {
	return "acp: ignored optional session update " + e.Variant
}

type Adapter struct {
	cfg    AdapterConfig
	ctx    context.Context
	cancel context.CancelFunc
	tr     *acp.Transport

	events     chan eventlog.Event
	errs       chan error
	done       chan struct{}
	promptGate chan struct{}
	// replayBarrier flushes replayed session/load notifications through readLoop
	// before the replaying gate is cleared (see endReplay).
	replayBarrier chan chan struct{}
	asideBarrier  chan chan struct{}

	mu             sync.RWMutex
	caps           Capabilities
	sessionID      string
	state          SessionState
	replaying      bool
	permissions    map[string]pendingPermission
	liveTools      map[string]struct{}
	toolFiles      map[string]string
	serviceCtx     context.Context
	serviceStop    context.CancelFunc
	serviceWG      sync.WaitGroup
	serviceDone    bool
	turnSessionID  string
	asideID        string
	asideSessionID string
	fatal          error
	closeOnce      sync.Once
	// parentPath is cfg.ParentToolCallIDPath pre-split on "." (nil when unset),
	// used to pull a normalized parentId out of each update's `_meta`.
	parentPath []string
}

var permissionCounter atomic.Uint64

// StartAdapter launches an ACP process and completes initialize plus either
// session/new or capability-gated session/load before returning.
func StartAdapter(ctx context.Context, cfg AdapterConfig) (*Adapter, error) {
	if cfg.AgentID == "" {
		return nil, errors.New("acp adapter: agent ID is required")
	}
	childCtx, cancel := context.WithCancel(ctx)
	transportCfg := cfg.Transport
	if transportCfg.Dir == "" {
		transportCfg.Dir = cfg.Cwd
	}
	tr, err := acp.Start(childCtx, transportCfg)
	if err != nil {
		cancel()
		return nil, err
	}
	serviceCtx, serviceStop := context.WithCancel(childCtx)
	a := &Adapter{cfg: cfg, ctx: childCtx, cancel: cancel, tr: tr, events: make(chan eventlog.Event, 256), errs: make(chan error, 32), done: make(chan struct{}), permissions: make(map[string]pendingPermission), liveTools: make(map[string]struct{}), toolFiles: make(map[string]string), serviceCtx: serviceCtx, serviceStop: serviceStop}
	if cfg.ParentToolCallIDPath != "" {
		a.parentPath = strings.Split(cfg.ParentToolCallIDPath, ".")
	}
	a.promptGate = make(chan struct{}, 1)
	a.promptGate <- struct{}{}
	a.replayBarrier = make(chan chan struct{})
	a.asideBarrier = make(chan chan struct{})
	go a.readLoop()

	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Fork *struct{} `json:"fork"`
			} `json:"sessionCapabilities"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
		Meta struct {
			Steering struct {
				Supported bool `json:"supported"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	if err := tr.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": a.clientCapabilities(),
		"clientInfo":         map[string]string{"name": "tandem", "version": "go"},
	}, &init); err != nil {
		a.Close()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	if init.ProtocolVersion != 1 {
		a.Close()
		return nil, fmt.Errorf("acp initialize: unsupported or missing protocolVersion %d", init.ProtocolVersion)
	}
	a.mu.Lock()
	a.caps = Capabilities{Structured: true, LoadSession: init.AgentCapabilities.LoadSession, ForkSession: init.AgentCapabilities.SessionCapabilities.Fork != nil, Image: init.AgentCapabilities.PromptCapabilities.Image, Steering: init.Meta.Steering.Supported}
	a.mu.Unlock()
	a.emit(map[string]any{"kind": "prompt_capabilities", "image": init.AgentCapabilities.PromptCapabilities.Image})
	a.emit(map[string]any{"kind": "aside_capabilities", "fork": init.AgentCapabilities.SessionCapabilities.Fork != nil})
	a.emit(map[string]any{"kind": "steering_capabilities", "supported": init.Meta.Steering.Supported})

	if cfg.ResumeSessionID != "" && init.AgentCapabilities.LoadSession {
		a.mu.Lock()
		a.replaying = !cfg.CaptureReplay
		a.mu.Unlock()
		err = a.load(ctx, cfg.ResumeSessionID)
		a.endReplay()
	} else {
		err = a.newSession(ctx)
	}
	if err != nil {
		a.Close()
		return nil, err
	}
	a.emitConfigIfPresent()
	a.emit(map[string]any{"kind": "status", "status": "idle"})
	return a, nil
}

func (a *Adapter) clientCapabilities() map[string]any {
	caps := make(map[string]any)
	if a.cfg.WorkspaceFS != nil {
		caps["fs"] = map[string]bool{"readTextFile": true, "writeTextFile": true}
	}
	if a.cfg.Terminals != nil {
		caps["terminal"] = true
	}
	return caps
}

func (a *Adapter) newSession(ctx context.Context) error {
	var raw struct {
		SessionID     string            `json:"sessionId"`
		Modes         json.RawMessage   `json:"modes"`
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if err := a.tr.Call(ctx, "session/new", map[string]any{"cwd": a.cwd(), "mcpServers": a.cfg.MCPServers}, &raw); err != nil {
		return fmt.Errorf("acp session/new: %w", err)
	}
	if raw.SessionID == "" {
		return errors.New("acp session/new: response is missing sessionId")
	}
	a.mu.Lock()
	a.sessionID, a.state = raw.SessionID, SessionState{Modes: cloneRaw(raw.Modes), ConfigOptions: normalizeConfigOptions(raw.ConfigOptions)}
	a.mu.Unlock()
	return nil
}

func (a *Adapter) load(ctx context.Context, sessionID string) error {
	var raw struct {
		Modes         json.RawMessage   `json:"modes"`
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	// Re-declare MCP servers on resume. ACP runtimes may recreate their backing
	// agent process during session/load and cannot reliably recover subprocess
	// configuration that was supplied only to the original session/new call.
	if err := a.tr.Call(ctx, "session/load", map[string]any{"sessionId": sessionID, "cwd": a.cwd(), "mcpServers": a.cfg.MCPServers}, &raw); err != nil {
		return fmt.Errorf("acp session/load: %w", err)
	}
	a.mu.Lock()
	a.sessionID, a.state = sessionID, SessionState{Modes: cloneRaw(raw.Modes), ConfigOptions: normalizeConfigOptions(raw.ConfigOptions)}
	a.mu.Unlock()
	return nil
}

func (a *Adapter) cwd() string {
	if a.cfg.Cwd != "" {
		return a.cfg.Cwd
	}
	return a.cfg.Transport.Dir
}

func (a *Adapter) Capabilities() Capabilities { a.mu.RLock(); defer a.mu.RUnlock(); return a.caps }
func (a *Adapter) SessionID() string          { a.mu.RLock(); defer a.mu.RUnlock(); return a.sessionID }
func (a *Adapter) SessionState() SessionState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return SessionState{Modes: cloneRaw(a.state.Modes), ConfigOptions: cloneRaws(a.state.ConfigOptions)}
}
func (a *Adapter) PID() int                      { return a.tr.PID() }
func (a *Adapter) Events() <-chan eventlog.Event { return a.events }
func (a *Adapter) Errors() <-chan error          { return a.errs }
func (a *Adapter) Done() <-chan struct{}         { return a.done }

func (a *Adapter) LoadSession(ctx context.Context, sessionID string, captureReplay bool) error {
	if !a.Capabilities().LoadSession {
		return errors.New("agent does not advertise loadSession")
	}
	a.mu.Lock()
	a.replaying = !captureReplay
	a.mu.Unlock()
	err := a.load(ctx, sessionID)
	// Clear replaying only after readLoop has flushed the replayed history, so
	// the tail of it cannot be re-logged, and so the resumed session_config
	// below is emitted with the gate already down.
	a.endReplay()
	if err != nil {
		return err
	}
	a.emitConfigIfPresent()
	return nil
}

// endReplay drains every session/load replay notification still queued in
// readLoop and only then lowers the replaying gate. The ACP resume contract
// re-streams the whole prior conversation as session/update notifications
// before answering session/load; the transport enqueues all of them before it
// delivers the response that unblocks load(). Clearing the gate directly here
// would race readLoop's consumption of that queue and re-log its tail (the
// agent's last message). Instead we route a barrier through readLoop itself,
// which flushes the queue under the gate before dropping it.
func (a *Adapter) endReplay() {
	done := make(chan struct{})
	select {
	case a.replayBarrier <- done:
		select {
		case <-done:
			return
		case <-a.done:
		case <-a.ctx.Done():
		}
	case <-a.done:
	case <-a.ctx.Done():
	}
	// readLoop has exited (or is exiting); no further notifications will be
	// processed, so clearing the gate directly is safe.
	a.mu.Lock()
	a.replaying = false
	a.mu.Unlock()
}

func (a *Adapter) Prompt(ctx context.Context, blocks []PromptBlock) (string, error) {
	return a.promptSession(ctx, a.SessionID(), blocks, true)
}

// Aside forks the current session, runs one isolated prompt, and leaves the
// original session untouched. Updates from the fork are wrapped as aside_event
// records so Tandem can persist and render them inline without treating them as
// parent conversation history.
func (a *Adapter) Aside(ctx context.Context, asideID string, blocks []PromptBlock) (string, error) {
	if !a.Capabilities().ForkSession {
		return "", errors.New("this agent does not support context-isolated asides")
	}
	if asideID == "" {
		return "", errors.New("aside id is required")
	}
	var fork struct {
		SessionID string `json:"sessionId"`
	}
	if err := a.tr.Call(ctx, "session/fork", map[string]any{
		"sessionId": a.SessionID(), "cwd": a.cwd(), "mcpServers": a.cfg.MCPServers,
	}, &fork); err != nil {
		return "", fmt.Errorf("acp session/fork: %w", err)
	}
	if fork.SessionID == "" {
		return "", errors.New("acp session/fork: response is missing sessionId")
	}
	a.mu.Lock()
	a.asideID, a.asideSessionID = asideID, fork.SessionID
	a.mu.Unlock()
	defer func() {
		a.flushAsideUpdates()
		a.mu.Lock()
		a.asideID, a.asideSessionID = "", ""
		a.mu.Unlock()
		// session/close is stable in newer agents but may be absent in runtimes
		// that only implemented the fork draft. Best-effort cleanup must not turn
		// a successfully answered aside into an error.
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.tr.Call(closeCtx, "session/close", map[string]any{"sessionId": fork.SessionID}, nil)
	}()
	return a.promptSession(ctx, fork.SessionID, blocks, true)
}

func (a *Adapter) flushAsideUpdates() {
	done := make(chan struct{})
	select {
	case a.asideBarrier <- done:
		select {
		case <-done:
		case <-a.done:
		case <-a.ctx.Done():
		}
	case <-a.done:
	case <-a.ctx.Done():
	}
}

func (a *Adapter) promptSession(ctx context.Context, sessionID string, blocks []PromptBlock, emitStatus bool) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.ctx.Done():
		return "", errors.New("acp adapter is closed")
	case <-a.promptGate:
	}
	defer func() { a.promptGate <- struct{}{} }()
	if err := a.ctx.Err(); err != nil {
		return "", errors.New("acp adapter is closed")
	}
	if len(blocks) == 0 {
		return "", errors.New("prompt must contain at least one block")
	}
	stopServices := a.resetServiceContext(ctx)
	defer stopServices()
	a.mu.Lock()
	a.turnSessionID = sessionID
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.turnSessionID = ""; a.mu.Unlock() }()
	wire, err := a.resolvePrompt(blocks)
	if err != nil {
		return "", err
	}
	if emitStatus {
		a.emit(map[string]any{"kind": "status", "status": "working"})
	}
	// Always balance the working transition. Transport failures and cancelled
	// prompt contexts return before a normal ACP response, but the adapter
	// process can remain healthy and accept another turn.
	if emitStatus {
		defer a.emit(map[string]any{"kind": "status", "status": "idle"})
	}
	var response struct {
		StopReason string `json:"stopReason"`
	}
	if err := a.tr.Call(ctx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": wire}, &response); err != nil {
		return "", fmt.Errorf("acp session/prompt: %w", err)
	}
	if response.StopReason == "" {
		return "end_turn", nil
	}
	return response.StopReason, nil
}

// Steer injects content into the active ACP turn using the negotiated steering
// extension. Asking for promptRequired on an idle race avoids an unowned,
// detached turn; the caller can leave the draft intact for an explicit send.
func (a *Adapter) Steer(ctx context.Context, blocks []PromptBlock) error {
	if !a.Capabilities().Steering {
		return errors.New("this agent does not support steering")
	}
	wire, err := a.resolvePrompt(blocks)
	if err != nil {
		return err
	}
	a.mu.RLock()
	sessionID := a.sessionID
	a.mu.RUnlock()
	var response struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := a.tr.Call(ctx, "_session/steering", map[string]any{
		"sessionId": sessionID,
		"prompt":    wire,
		"_meta":     map[string]any{"steering": map[string]any{"idleBehavior": "promptRequired"}},
	}, &response); err != nil {
		return fmt.Errorf("acp _session/steering: %w", err)
	}
	if response.Outcome == "injected" || response.Outcome == "startedNewTurn" {
		return nil
	}
	if response.Outcome == "promptRequired" {
		return errors.New("the turn finished before it could be steered; send or queue the prompt instead")
	}
	return fmt.Errorf("acp _session/steering: unknown outcome %q", response.Outcome)
}

func (a *Adapter) resetServiceContext(promptCtx context.Context) context.CancelFunc {
	a.mu.Lock()
	a.serviceStop()
	a.serviceCtx, a.serviceStop = context.WithCancel(a.ctx)
	cancel := a.serviceStop
	a.mu.Unlock()
	stopLink := context.AfterFunc(promptCtx, cancel)
	return func() {
		stopLink()
		cancel()
	}
}

func (a *Adapter) resolvePrompt(blocks []PromptBlock) ([]map[string]any, error) {
	images, total := 0, int64(0)
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": block.Text})
		case "image":
			if !a.Capabilities().Image {
				return nil, errors.New("this agent does not support image prompts")
			}
			if a.cfg.Assets == nil {
				return nil, errors.New("image asset storage is unavailable")
			}
			images++
			if images > assets.MaxPromptImages {
				return nil, fmt.Errorf("prompt may contain at most %d images", assets.MaxPromptImages)
			}
			asset, err := a.cfg.Assets.Get(a.cfg.AgentID, block.AssetID)
			if err != nil {
				return nil, fmt.Errorf("resolve image %s: %w", block.AssetID, err)
			}
			if block.MIMEType != asset.MIMEType {
				return nil, fmt.Errorf("image MIME type does not match uploaded asset: %s", block.AssetID)
			}
			total += asset.Size
			if total > assets.MaxPromptImageBytes {
				return nil, fmt.Errorf("prompt images exceed %d byte limit", assets.MaxPromptImageBytes)
			}
			out = append(out, map[string]any{"type": "image", "mimeType": asset.MIMEType, "data": base64.StdEncoding.EncodeToString(asset.Data)})
		default:
			return nil, fmt.Errorf("invalid prompt block type %q", block.Type)
		}
	}
	return out, nil
}

// ValidatePrompt resolves all referenced assets without starting a turn. The
// WebSocket boundary uses this to reject invalid image prompts synchronously.
func (a *Adapter) ValidatePrompt(blocks []PromptBlock) error {
	_, err := a.resolvePrompt(blocks)
	return err
}

func (a *Adapter) RespondPermission(reqID, optionID string) error {
	a.mu.Lock()
	pending, ok := a.permissions[reqID]
	if ok {
		delete(a.permissions, reqID)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown permission request %s", reqID)
	}
	if err := a.tr.Respond(pending.wireID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}); err != nil {
		return err
	}
	a.emit(map[string]any{"kind": "status", "status": "working"})
	return nil
}

func (a *Adapter) PendingApprovals() []Approval {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Approval, 0, len(a.permissions))
	for _, pending := range a.permissions {
		approval := pending.approval
		approval.Options = append([]ApprovalOption(nil), approval.Options...)
		out = append(out, approval)
	}
	return out
}

func (a *Adapter) Interrupt() error {
	a.mu.Lock()
	permissions := a.permissions
	a.permissions = make(map[string]pendingPermission)
	tools := make([]string, 0, len(a.liveTools))
	for id := range a.liveTools {
		tools = append(tools, id)
	}
	a.liveTools = make(map[string]struct{})
	// An aside runs its prompt against the fork returned by session/fork, not
	// the durable parent session. Cancel whichever session currently owns the
	// turn so interrupting /btw reaches the in-flight prompt.
	sessionID := a.turnSessionID
	if sessionID == "" {
		sessionID = a.sessionID
	}
	// Keep the cancelled context installed until the next Prompt resets it.
	// Service requests already emitted by the agent can arrive after the
	// interrupt and must be rejected as part of the interrupted turn.
	a.serviceStop()
	a.mu.Unlock()
	for _, pending := range permissions {
		if err := a.tr.Respond(pending.wireID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}); err != nil {
			return err
		}
	}
	for _, id := range tools {
		a.emit(map[string]any{"kind": "tool_call_update", "id": id, "status": "cancelled"})
	}
	if sessionID == "" {
		return nil
	}
	return a.tr.Notify("session/cancel", map[string]any{"sessionId": sessionID})
}

func (a *Adapter) SetMode(ctx context.Context, modeID string) error {
	return a.tr.Call(ctx, "session/set_mode", map[string]any{"sessionId": a.SessionID(), "modeId": modeID}, nil)
}

func (a *Adapter) SetConfigOption(ctx context.Context, configID string, value any) error {
	params := map[string]any{"sessionId": a.SessionID(), "configId": configID, "value": value}
	if _, ok := value.(bool); ok {
		params["type"] = "boolean"
	}
	var response struct {
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if err := a.tr.Call(ctx, "session/set_config_option", params, &response); err != nil {
		return err
	}
	if response.ConfigOptions != nil {
		a.mu.Lock()
		a.state.ConfigOptions = normalizeConfigOptions(response.ConfigOptions)
		a.mu.Unlock()
	}
	a.emitConfigIfPresent()
	return nil
}

func (a *Adapter) Close() error {
	var err error
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.serviceDone = true
		a.serviceStop()
		a.mu.Unlock()
		a.cancel()
		// Transport.Close terminates the owned child, so an exit status caused by
		// that termination is the expected disposal path rather than a close
		// failure to surface through close_agent.
		_ = a.tr.Close()
		a.serviceWG.Wait()
	})
	return err
}

func (a *Adapter) readLoop() {
	defer close(a.done)
	requests, notifications, transportErrors := a.tr.Requests(), a.tr.Notifications(), a.tr.Errors()
	for requests != nil || notifications != nil || transportErrors != nil {
		select {
		case req, ok := <-requests:
			if !ok {
				requests = nil
				continue
			}
			a.handleRequest(req)
		case note, ok := <-notifications:
			if !ok {
				notifications = nil
				continue
			}
			if err := a.dispatchNote(note); err != nil {
				a.fail(err)
				return
			}
		case done := <-a.replayBarrier:
			// Flush every replay notification already queued (the transport
			// enqueued them all before the session/load response unblocked
			// endReplay) while the gate is still up, then lower it. A default
			// case bounds the drain to what is currently buffered so a racing
			// post-replay live notification is left for the main loop.
			for draining := true; draining; {
				select {
				case note, ok := <-notifications:
					if !ok {
						notifications = nil
						draining = false
						break
					}
					if err := a.dispatchNote(note); err != nil {
						a.fail(err)
						close(done)
						return
					}
				default:
					draining = false
				}
			}
			a.mu.Lock()
			a.replaying = false
			a.mu.Unlock()
			close(done)
		case done := <-a.asideBarrier:
			for draining := true; draining; {
				select {
				case note, ok := <-notifications:
					if !ok {
						notifications = nil
						draining = false
						break
					}
					if err := a.dispatchNote(note); err != nil {
						a.fail(err)
						close(done)
						return
					}
				default:
					draining = false
				}
			}
			close(done)
		case err, ok := <-transportErrors:
			if !ok {
				transportErrors = nil
				continue
			}
			a.diagnostic(err)
		}
	}
	if err := a.tr.Wait(); err != nil && a.ctx.Err() == nil {
		a.diagnostic(err)
		a.emit(map[string]any{"kind": "status", "status": "error"})
	} else if a.ctx.Err() == nil {
		a.emit(map[string]any{"kind": "status", "status": "idle"})
	}
}

func (a *Adapter) dispatchNote(note acp.Notification) error {
	if note.Method != "session/update" {
		a.diagnostic(&OptionalUpdateError{Variant: note.Method})
		return nil
	}
	return a.handleUpdate(note.Params)
}

func (a *Adapter) handleRequest(req acp.Request) {
	if req.Method != "session/request_permission" {
		a.mu.Lock()
		if a.serviceDone {
			a.mu.Unlock()
			return
		}
		ctx := a.serviceCtx
		a.serviceWG.Add(1)
		a.mu.Unlock()
		go func() {
			defer a.serviceWG.Done()
			a.handleServiceRequest(ctx, req)
		}()
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
		} `json:"options"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.SessionID == "" || p.ToolCall.ToolCallID == "" || len(p.Options) == 0 {
		_ = a.tr.RespondError(req.ID, -32602, "malformed session/request_permission", nil)
		a.fail(errors.New("acp: malformed required session/request_permission"))
		return
	}
	reqID := fmt.Sprintf("perm_%d", permissionCounter.Add(1))
	options := make([]map[string]string, len(p.Options))
	approvalOptions := make([]ApprovalOption, len(p.Options))
	for i, option := range p.Options {
		options[i] = map[string]string{"optionId": option.OptionID, "name": option.Name}
		approvalOptions[i] = ApprovalOption{OptionID: option.OptionID, Name: option.Name}
	}
	title := p.ToolCall.Title
	if title == "" {
		title = "(command)"
	}
	a.mu.Lock()
	a.permissions[reqID] = pendingPermission{wireID: cloneRaw(req.ID), approval: Approval{ReqID: reqID, ToolCallID: p.ToolCall.ToolCallID, Title: title, Options: approvalOptions}}
	a.mu.Unlock()
	a.emit(map[string]any{"kind": "permission_request", "reqId": reqID, "toolCallId": p.ToolCall.ToolCallID, "title": title, "options": options})
}

const (
	methodNotFound = -32601
	invalidParams  = -32602
	internalError  = -32603
)

type rpcCodedError interface{ JSONRPCCode() int }

func (a *Adapter) handleServiceRequest(ctx context.Context, req acp.Request) {
	result, err := a.dispatchService(ctx, req.Method, req.Params)
	if err == nil {
		if respondErr := a.tr.Respond(req.ID, result); respondErr != nil && a.ctx.Err() == nil {
			a.diagnostic(respondErr)
		}
		return
	}
	code := internalError
	var coded rpcCodedError
	if errors.As(err, &coded) {
		code = coded.JSONRPCCode()
	}
	if respondErr := a.tr.RespondError(req.ID, code, err.Error(), nil); respondErr != nil && a.ctx.Err() == nil {
		a.diagnostic(respondErr)
	}
}

type invalidServiceParams struct{ message string }

func (e *invalidServiceParams) Error() string    { return e.message }
func (e *invalidServiceParams) JSONRPCCode() int { return invalidParams }

type unsupportedServiceMethod struct{ method string }

func (e *unsupportedServiceMethod) Error() string {
	return "client does not support method: " + e.method
}
func (e *unsupportedServiceMethod) JSONRPCCode() int { return methodNotFound }

func decodeServiceParams(raw json.RawMessage, method string, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return &invalidServiceParams{message: "malformed " + method + ": params are required"}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &invalidServiceParams{message: "malformed " + method + ": " + err.Error()}
	}
	return nil
}

func (a *Adapter) validateServiceSession(method, sessionID string) error {
	if sessionID == "" {
		return &invalidServiceParams{message: "malformed " + method + ": sessionId is required"}
	}
	a.mu.RLock()
	active := a.turnSessionID
	a.mu.RUnlock()
	if active == "" {
		active = a.SessionID()
	}
	if sessionID != active {
		return &invalidServiceParams{message: "malformed " + method + ": sessionId does not match the active session"}
	}
	return nil
}

func (a *Adapter) dispatchService(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "fs/read_text_file":
		if a.cfg.WorkspaceFS == nil {
			return nil, errors.New("client filesystem service unavailable")
		}
		var p struct {
			SessionID string `json:"sessionId"`
			Path      string `json:"path"`
			Line      *int   `json:"line"`
			Limit     *int   `json:"limit"`
		}
		if err := decodeServiceParams(raw, method, &p); err != nil {
			return nil, err
		}
		if err := a.validateServiceSession(method, p.SessionID); err != nil {
			return nil, err
		}
		if p.Path == "" {
			return nil, &invalidServiceParams{message: "malformed " + method + ": path is required"}
		}
		if p.Line != nil && *p.Line < 1 {
			return nil, &invalidServiceParams{message: "malformed " + method + ": line must be at least 1"}
		}
		if p.Limit != nil && *p.Limit < 0 {
			return nil, &invalidServiceParams{message: "malformed " + method + ": limit cannot be negative"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		content, err := a.cfg.WorkspaceFS.ReadTextFile(p.Path, p.Line, p.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": content}, nil

	case "fs/write_text_file":
		if a.cfg.WorkspaceFS == nil {
			return nil, errors.New("client filesystem service unavailable")
		}
		var p struct {
			SessionID string  `json:"sessionId"`
			Path      string  `json:"path"`
			Content   *string `json:"content"`
		}
		if err := decodeServiceParams(raw, method, &p); err != nil {
			return nil, err
		}
		if err := a.validateServiceSession(method, p.SessionID); err != nil {
			return nil, err
		}
		if p.Path == "" || p.Content == nil {
			return nil, &invalidServiceParams{message: "malformed " + method + ": path and content are required"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if err := a.cfg.WorkspaceFS.WriteTextFile(p.Path, *p.Content); err != nil {
			return nil, err
		}
		return map[string]any{}, nil

	case "terminal/create":
		if a.cfg.Terminals == nil {
			return nil, errors.New("client terminal service unavailable")
		}
		var p struct {
			SessionID       string                     `json:"sessionId"`
			Command         string                     `json:"command"`
			Args            []string                   `json:"args"`
			Cwd             string                     `json:"cwd"`
			Env             []terminalhost.EnvVariable `json:"env"`
			OutputByteLimit *int                       `json:"outputByteLimit"`
		}
		if err := decodeServiceParams(raw, method, &p); err != nil {
			return nil, err
		}
		if err := a.validateServiceSession(method, p.SessionID); err != nil {
			return nil, err
		}
		if p.Command == "" {
			return nil, &invalidServiceParams{message: "malformed " + method + ": command is required"}
		}
		if p.OutputByteLimit != nil && *p.OutputByteLimit < 0 {
			return nil, &invalidServiceParams{message: "malformed " + method + ": outputByteLimit cannot be negative"}
		}
		terminalID, err := a.cfg.Terminals.Create(ctx, terminalhost.CreateOptions{Command: p.Command, Args: p.Args, Cwd: p.Cwd, Env: p.Env, OutputByteLimit: p.OutputByteLimit})
		if err != nil {
			return nil, err
		}
		return map[string]any{"terminalId": terminalID}, nil

	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		if a.cfg.Terminals == nil {
			return nil, errors.New("client terminal service unavailable")
		}
		var p struct {
			SessionID  string `json:"sessionId"`
			TerminalID string `json:"terminalId"`
		}
		if err := decodeServiceParams(raw, method, &p); err != nil {
			return nil, err
		}
		if err := a.validateServiceSession(method, p.SessionID); err != nil {
			return nil, err
		}
		if p.TerminalID == "" {
			return nil, &invalidServiceParams{message: "malformed " + method + ": terminalId is required"}
		}
		switch method {
		case "terminal/output":
			output, err := a.cfg.Terminals.Output(p.TerminalID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"output": output.Output, "truncated": output.Truncated, "exitStatus": output.ExitStatus}, nil
		case "terminal/wait_for_exit":
			exit, err := a.cfg.Terminals.WaitForExit(ctx, p.TerminalID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"exitCode": exit.ExitCode, "signal": exit.Signal}, nil
		case "terminal/kill":
			if err := a.cfg.Terminals.Kill(p.TerminalID); err != nil {
				return nil, err
			}
			return map[string]any{}, nil
		default:
			if err := a.cfg.Terminals.Release(p.TerminalID); err != nil {
				return nil, err
			}
			return map[string]any{}, nil
		}
	default:
		return nil, &unsupportedServiceMethod{method: method}
	}
}

// parentToolCallID resolves the configured `_meta` dotted path (parentPath)
// against a session/update's raw `_meta` object, returning the parent tool
// call's id when present. Returns "" when unconfigured, when `_meta` is absent
// or malformed, or when any path segment is missing — every miss degrades to
// flat (unattributed) rendering rather than erroring. The path may terminate on
// a string or (defensively) a number, matching how vendors stamp ids.
func (a *Adapter) parentToolCallID(meta json.RawMessage) string {
	if len(a.parentPath) == 0 || len(meta) == 0 {
		return ""
	}
	var node any
	if err := json.Unmarshal(meta, &node); err != nil {
		return ""
	}
	for _, seg := range a.parentPath {
		obj, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		node, ok = obj[seg]
		if !ok {
			return ""
		}
	}
	switch v := node.(type) {
	case string:
		return v
	case float64:
		// Numeric ids arrive as float64 from encoding/json; render without a
		// spurious decimal so it matches the string toolCallId it points at.
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// attachParentID adds the normalized parent attribution to an emitted event
// when one was resolved. Kept separate so every emitting variant opts in with a
// single line and an empty id never writes a noisy `"parentId":""`.
func attachParentID(ev map[string]any, parentID string) {
	if parentID != "" {
		ev["parentId"] = parentID
	}
}

func (a *Adapter) handleUpdate(params json.RawMessage) error {
	var note struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &note); err != nil || note.SessionID == "" || len(note.Update) == 0 {
		return errors.New("acp: malformed required session/update notification")
	}
	var header struct {
		Variant string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(note.Update, &header); err != nil || header.Variant == "" {
		return errors.New("acp: malformed required session/update payload")
	}
	var u map[string]json.RawMessage
	if err := json.Unmarshal(note.Update, &u); err != nil {
		return fmt.Errorf("acp: malformed %s update: %w", header.Variant, err)
	}
	parentID := a.parentToolCallID(u["_meta"])
	switch header.Variant {
	case "agent_message_chunk", "agent_thought_chunk":
		text, err := textContent(u["content"])
		if err != nil {
			return fmt.Errorf("acp: malformed %s update: %w", header.Variant, err)
		}
		kind := "message_chunk"
		if header.Variant == "agent_thought_chunk" {
			kind = "thought_chunk"
		}
		ev := map[string]any{"kind": kind, "text": text}
		attachParentID(ev, parentID)
		a.pushUpdate(note.SessionID, ev)
	case "tool_call":
		id, err := requiredString(u, "toolCallId")
		if err != nil {
			return err
		}
		title, err := requiredString(u, "title")
		if err != nil {
			return err
		}
		status := toolStatus(rawString(u["status"]))
		fileCandidate := playwrightScreenshotFile(title, u["rawInput"])
		a.mu.Lock()
		if fileCandidate != "" {
			a.toolFiles[id] = fileCandidate
		}
		if status == "pending" || status == "running" {
			a.liveTools[id] = struct{}{}
		} else {
			delete(a.liveTools, id)
			delete(a.toolFiles, id)
		}
		a.mu.Unlock()
		ev := map[string]any{"kind": "tool_call", "id": id, "title": title, "status": status}
		if content, ok := a.normalizeToolContent(u["content"], finalToolFile(status, fileCandidate)); ok {
			ev["content"] = content
		}
		copyJSONField(ev, "rawInput", u["rawInput"])
		copyJSONField(ev, "toolKind", u["kind"])
		attachParentID(ev, parentID)
		a.pushUpdate(note.SessionID, ev)
	case "tool_call_update":
		id, err := requiredString(u, "toolCallId")
		if err != nil {
			return err
		}
		ev := map[string]any{"kind": "tool_call_update", "id": id}
		status := ""
		if statusWire := rawString(u["status"]); statusWire != "" {
			status = toolStatus(statusWire)
			ev["status"] = status
		}
		// title/rawInput/kind are optional on an update, but agents that stream
		// tool input incrementally (e.g. Claude refining a Bash command as it
		// parses) send the real values here, after an initial tool_call whose
		// input wasn't fully known yet. Forward them so the transcript picks up
		// the refined command instead of getting stuck on the placeholder.
		if titleWire := rawString(u["title"]); titleWire != "" {
			ev["title"] = titleWire
		}
		copyJSONField(ev, "rawInput", u["rawInput"])
		copyJSONField(ev, "toolKind", u["kind"])
		a.mu.Lock()
		fileCandidate := a.toolFiles[id]
		if status != "" && status != "pending" && status != "running" {
			delete(a.liveTools, id)
			delete(a.toolFiles, id)
		}
		a.mu.Unlock()
		if content, ok := a.normalizeToolContent(u["content"], finalToolFile(status, fileCandidate)); ok {
			ev["content"] = content
		}
		attachParentID(ev, parentID)
		a.pushUpdate(note.SessionID, ev)
	case "plan", "plan_update":
		var entries []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		}
		if raw, ok := u["entries"]; !ok || json.Unmarshal(raw, &entries) != nil {
			return fmt.Errorf("acp: malformed %s update: entries are required", header.Variant)
		}
		norm := make([]map[string]string, len(entries))
		for i, e := range entries {
			if e.Content == "" {
				return fmt.Errorf("acp: malformed %s update: entry content is required", header.Variant)
			}
			norm[i] = map[string]string{"label": e.Content, "status": planStatus(e.Status)}
		}
		a.pushUpdate(note.SessionID, map[string]any{"kind": "plan", "entries": norm})
	case "current_mode_update":
		mode, err := requiredString(u, "currentModeId")
		if err != nil {
			return err
		}
		a.updateCurrentMode(mode)
		a.pushConfig()
	case "config_option_update":
		var options []json.RawMessage
		if raw, ok := u["configOptions"]; !ok || json.Unmarshal(raw, &options) != nil {
			return errors.New("acp: malformed config_option_update")
		}
		a.mu.Lock()
		a.state.ConfigOptions = normalizeConfigOptions(options)
		a.mu.Unlock()
		a.pushConfig()
	case "available_commands_update":
		var commands []map[string]any
		if raw, ok := u["availableCommands"]; !ok || json.Unmarshal(raw, &commands) != nil {
			return errors.New("acp: malformed available_commands_update")
		}
		norm := make([]map[string]any, 0, len(commands))
		for _, c := range commands {
			name, _ := c["name"].(string)
			if name == "" {
				return errors.New("acp: malformed available_commands_update command")
			}
			out := map[string]any{"name": name}
			if v, ok := c["description"].(string); ok {
				out["description"] = v
			}
			if input, ok := c["input"].(map[string]any); ok {
				if hint, ok := input["hint"].(string); ok {
					out["input"] = hint
				}
			}
			norm = append(norm, out)
		}
		a.pushUpdate(note.SessionID, map[string]any{"kind": "available_commands", "commands": norm})
	case "usage_update":
		var usage struct {
			Used float64         `json:"used"`
			Size float64         `json:"size"`
			Cost json.RawMessage `json:"cost"`
		}
		if err := json.Unmarshal(note.Update, &usage); err != nil || usage.Used < 0 || usage.Size <= 0 {
			return errors.New("acp: malformed usage_update")
		}
		ev := map[string]any{"kind": "usage", "used": usage.Used, "size": usage.Size}
		if string(usage.Cost) == "null" {
			ev["cost"] = nil
		} else if len(usage.Cost) != 0 {
			var cost struct {
				Amount   float64 `json:"amount"`
				Currency string  `json:"currency"`
			}
			if json.Unmarshal(usage.Cost, &cost) == nil && cost.Currency != "" {
				ev["cost"] = map[string]any{"amount": cost.Amount, "currency": cost.Currency}
			}
		}
		a.pushUpdate(note.SessionID, ev)
	case "user_message_chunk", "plan_removed", "session_info_update":
		// Known optional updates have no normalized Tandem event yet.
	default:
		a.diagnostic(&OptionalUpdateError{Variant: header.Variant})
	}
	return nil
}

func finalToolFile(status, candidate string) string {
	if status == "done" && candidate != "" {
		return candidate
	}
	return ""
}

func playwrightScreenshotFile(title string, raw json.RawMessage) string {
	if !strings.Contains(title, "browser_take_screenshot") || len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil {
		return ""
	}
	if arguments, ok := input["arguments"].(map[string]any); ok {
		input = arguments
	}
	filename, _ := input["filename"].(string)
	return filename
}

// normalizeToolContent moves image bytes out of durable event JSON and into
// Tandem's content-addressed asset store. Unknown ACP blocks are retained so a
// newer agent cannot lose data merely because this client cannot render it yet.
func (a *Adapter) normalizeToolContent(raw json.RawMessage, extraFile string) (any, bool) {
	if len(raw) == 0 && extraFile == "" {
		return nil, false
	}
	var blocks []any
	if len(raw) > 0 && json.Unmarshal(raw, &blocks) != nil {
		var value any
		if json.Unmarshal(raw, &value) == nil {
			return value, true
		}
		return nil, false
	}
	captured := false
	for _, value := range blocks {
		block, ok := value.(map[string]any)
		if !ok || block["type"] != "content" {
			continue
		}
		content, ok := block["content"].(map[string]any)
		if !ok {
			continue
		}
		var stored assets.Stored
		var err error
		switch content["type"] {
		case "image":
			data, _ := content["data"].(string)
			mime, _ := content["mimeType"].(string)
			if data == "" || len(data) > base64.StdEncoding.EncodedLen(assets.MaxAssetBytes) {
				block["content"] = map[string]any{"type": "text", "text": "Image unavailable: invalid or oversized image data"}
				continue
			}
			var decoded []byte
			decoded, err = base64.StdEncoding.DecodeString(data)
			if err == nil && a.cfg.Assets != nil {
				stored, err = a.cfg.Assets.Put(a.cfg.AgentID, decoded, mime)
			}
		case "resource_link":
			uri, _ := content["uri"].(string)
			stored, err = a.captureToolFile(uri)
		default:
			continue
		}
		if err != nil || stored.AssetID == "" {
			if content["type"] == "image" {
				block["content"] = map[string]any{"type": "text", "text": "Image unavailable: tool image could not be stored"}
			}
			continue
		}
		name, _ := content["name"].(string)
		block["content"] = toolImageAsset(stored, name)
		captured = true
	}
	if extraFile != "" && !captured {
		if stored, err := a.captureToolFile(extraFile); err == nil {
			blocks = append(blocks, map[string]any{"type": "content", "content": toolImageAsset(stored, filepath.Base(extraFile))})
		}
	}
	return blocks, true
}

func toolImageAsset(stored assets.Stored, name string) map[string]any {
	result := map[string]any{"type": "image", "assetId": stored.AssetID, "mimeType": stored.MIMEType}
	if name != "" {
		result["name"] = name
	}
	return result
}

func (a *Adapter) captureToolFile(raw string) (assets.Stored, error) {
	if raw == "" || a.cfg.Assets == nil {
		return assets.Stored{}, assets.ErrNotFound
	}
	path := raw
	if parsed, err := url.Parse(raw); err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "file" || parsed.Host != "" && parsed.Host != "localhost" {
			return assets.Stored{}, assets.ErrNotFound
		}
		path = parsed.Path
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.cfg.Cwd, path)
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return assets.Stored{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, assets.MaxAssetBytes+1))
	if err != nil {
		return assets.Stored{}, err
	}
	return a.cfg.Assets.Put(a.cfg.AgentID, data, "")
}

func (a *Adapter) updateCurrentMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var current map[string]any
	if len(a.state.Modes) != 0 {
		_ = json.Unmarshal(a.state.Modes, &current)
	}
	if current == nil {
		current = map[string]any{"availableModes": []any{}}
	}
	current["currentModeId"] = mode
	a.state.Modes, _ = json.Marshal(current)
}

func (a *Adapter) emitConfigIfPresent() {
	state := a.SessionState()
	if len(state.Modes) != 0 || len(state.ConfigOptions) != 0 {
		a.pushConfig()
	}
}
func (a *Adapter) pushConfig() {
	state := a.SessionState()
	var modes any = nil
	if len(state.Modes) != 0 {
		_ = json.Unmarshal(state.Modes, &modes)
	}
	options := make([]any, 0, len(state.ConfigOptions))
	for _, raw := range state.ConfigOptions {
		var option any
		_ = json.Unmarshal(raw, &option)
		options = append(options, option)
	}
	a.push(map[string]any{"kind": "session_config", "modes": modes, "configOptions": options})
}

func (a *Adapter) push(value map[string]any) {
	a.mu.RLock()
	replaying := a.replaying
	a.mu.RUnlock()
	if !replaying {
		a.emit(value)
	}
}

func (a *Adapter) pushUpdate(sessionID string, value map[string]any) {
	a.mu.RLock()
	asideID, asideSessionID := a.asideID, a.asideSessionID
	a.mu.RUnlock()
	if asideID != "" && sessionID == asideSessionID {
		a.push(map[string]any{"kind": "aside_event", "asideId": asideID, "event": value})
		return
	}
	a.push(value)
}
func (a *Adapter) emit(value map[string]any) {
	b, err := json.Marshal(value)
	if err != nil {
		a.fail(err)
		return
	}
	event, err := eventlog.ParseNormalized(b)
	if err != nil {
		a.fail(err)
		return
	}
	select {
	case a.events <- event:
	case <-a.ctx.Done():
	}
}
func (a *Adapter) diagnostic(err error) {
	if a.cfg.Logger != nil {
		a.cfg.Logger.Print(err)
	}
	select {
	case a.errs <- err:
	default:
	}
}
func (a *Adapter) fail(err error) {
	a.mu.Lock()
	if a.fatal == nil {
		a.fatal = err
	}
	a.mu.Unlock()
	a.diagnostic(err)
	a.emit(map[string]any{"kind": "error", "message": err.Error()})
	a.emit(map[string]any{"kind": "status", "status": "error"})
	a.cancel()
}

func textContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("content is required")
	}
	var one struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if raw[0] == '[' {
		var many []struct{ Type, Text string }
		if err := json.Unmarshal(raw, &many); err != nil {
			return "", err
		}
		out := ""
		for _, block := range many {
			if block.Type == "text" {
				out += block.Text
			}
		}
		return out, nil
	}
	if err := json.Unmarshal(raw, &one); err != nil {
		return "", err
	}
	if one.Type != "text" {
		return "", nil
	}
	return one.Text, nil
}
func requiredString(m map[string]json.RawMessage, key string) (string, error) {
	value := rawString(m[key])
	if value == "" {
		return "", fmt.Errorf("acp: malformed update: %s is required", key)
	}
	return value, nil
}
func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
func toolStatus(status string) string {
	switch status {
	case "in_progress":
		return "running"
	case "completed":
		return "done"
	case "failed":
		return "error"
	default:
		return "pending"
	}
}
func planStatus(status string) string {
	if status == "completed" {
		return "done"
	}
	if status == "in_progress" {
		return "in_progress"
	}
	return "pending"
}
func copyJSONField(dst map[string]any, key string, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		dst[key] = value
	}
}
func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }
func cloneRaws(raw []json.RawMessage) []json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make([]json.RawMessage, len(raw))
	for i := range raw {
		out[i] = cloneRaw(raw[i])
	}
	return out
}

func normalizeConfigOptions(raws []json.RawMessage) []json.RawMessage {
	if raws == nil {
		return nil
	}
	out := make([]json.RawMessage, 0, len(raws))
	for _, raw := range raws {
		var input map[string]any
		if json.Unmarshal(raw, &input) != nil {
			continue
		}
		id, idOK := input["id"].(string)
		name, nameOK := input["name"].(string)
		if !idOK || id == "" || !nameOK || name == "" {
			continue
		}
		normalized := map[string]any{"id": id, "name": name}
		for _, key := range []string{"description", "category"} {
			if value, ok := input[key].(string); ok && value != "" {
				normalized[key] = value
			}
		}
		if current, ok := input["currentValue"].(bool); ok || input["type"] == "boolean" {
			normalized["type"] = "boolean"
			normalized["currentValue"] = current
		} else {
			normalized["type"] = "select"
			normalized["currentValue"], _ = input["currentValue"].(string)
			flat := make([]any, 0)
			if options, ok := input["options"].([]any); ok {
				for _, item := range options {
					option, ok := item.(map[string]any)
					if !ok {
						continue
					}
					if grouped, ok := option["options"].([]any); ok {
						flat = append(flat, grouped...)
					} else {
						flat = append(flat, option)
					}
				}
			}
			normalized["options"] = flat
		}
		encoded, _ := json.Marshal(normalized)
		out = append(out, encoded)
	}
	return out
}

var _ io.Closer = (*Adapter)(nil)
