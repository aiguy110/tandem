package acpadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/eventlog"
)

// AssetResolver is the narrow asset-store surface needed to turn durable image
// references into ACP's inline base64 prompt blocks.
type AssetResolver interface {
	Get(agentID, assetID string) (assets.Stored, error)
}

type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     []acp.EnvVariable `json:"env,omitempty"`
}

type AdapterConfig struct {
	Transport       acp.Config
	AgentID         string
	Cwd             string
	ResumeSessionID string
	CaptureReplay   bool
	MCPServers      []MCPServer
	Assets          AssetResolver
	Logger          *log.Logger
}

type Capabilities struct {
	Structured  bool
	LoadSession bool
	Image       bool
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

	mu          sync.RWMutex
	caps        Capabilities
	sessionID   string
	state       SessionState
	replaying   bool
	permissions map[string]json.RawMessage
	liveTools   map[string]struct{}
	fatal       error
	closeOnce   sync.Once
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
	a := &Adapter{cfg: cfg, ctx: childCtx, cancel: cancel, tr: tr, events: make(chan eventlog.Event, 256), errs: make(chan error, 32), done: make(chan struct{}), permissions: make(map[string]json.RawMessage), liveTools: make(map[string]struct{})}
	a.promptGate = make(chan struct{}, 1)
	a.promptGate <- struct{}{}
	go a.readLoop()

	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := tr.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
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
	a.caps = Capabilities{Structured: true, LoadSession: init.AgentCapabilities.LoadSession, Image: init.AgentCapabilities.PromptCapabilities.Image}
	a.mu.Unlock()
	a.emit(map[string]any{"kind": "prompt_capabilities", "image": init.AgentCapabilities.PromptCapabilities.Image})

	if cfg.ResumeSessionID != "" && init.AgentCapabilities.LoadSession {
		a.mu.Lock()
		a.replaying = !cfg.CaptureReplay
		a.mu.Unlock()
		err = a.load(ctx, cfg.ResumeSessionID)
		a.mu.Lock()
		a.replaying = false
		a.mu.Unlock()
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
	defer func() { a.mu.Lock(); a.replaying = false; a.mu.Unlock() }()
	if err := a.load(ctx, sessionID); err != nil {
		return err
	}
	a.emitConfigIfPresent()
	return nil
}

func (a *Adapter) Prompt(ctx context.Context, blocks []PromptBlock) (string, error) {
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
	wire, err := a.resolvePrompt(blocks)
	if err != nil {
		return "", err
	}
	a.emit(map[string]any{"kind": "status", "status": "working"})
	var response struct {
		StopReason string `json:"stopReason"`
	}
	if err := a.tr.Call(ctx, "session/prompt", map[string]any{"sessionId": a.SessionID(), "prompt": wire}, &response); err != nil {
		return "", fmt.Errorf("acp session/prompt: %w", err)
	}
	a.emit(map[string]any{"kind": "status", "status": "idle"})
	if response.StopReason == "" {
		return "end_turn", nil
	}
	return response.StopReason, nil
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

func (a *Adapter) RespondPermission(reqID, optionID string) error {
	a.mu.Lock()
	id, ok := a.permissions[reqID]
	if ok {
		delete(a.permissions, reqID)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown permission request %s", reqID)
	}
	if err := a.tr.Respond(id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}); err != nil {
		return err
	}
	a.emit(map[string]any{"kind": "status", "status": "working"})
	return nil
}

func (a *Adapter) Interrupt() error {
	a.mu.Lock()
	permissions := a.permissions
	a.permissions = make(map[string]json.RawMessage)
	tools := make([]string, 0, len(a.liveTools))
	for id := range a.liveTools {
		tools = append(tools, id)
	}
	a.liveTools = make(map[string]struct{})
	sessionID := a.sessionID
	a.mu.Unlock()
	for _, id := range permissions {
		if err := a.tr.Respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}); err != nil {
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
	a.closeOnce.Do(func() { a.cancel(); err = a.tr.Close() })
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
			if note.Method != "session/update" {
				a.diagnostic(&OptionalUpdateError{Variant: note.Method})
				continue
			}
			if err := a.handleUpdate(note.Params); err != nil {
				a.fail(err)
				return
			}
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

func (a *Adapter) handleRequest(req acp.Request) {
	if req.Method != "session/request_permission" {
		_ = a.tr.RespondError(req.ID, -32601, "client does not support method: "+req.Method, nil)
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
	a.mu.Lock()
	a.permissions[reqID] = cloneRaw(req.ID)
	a.mu.Unlock()
	options := make([]map[string]string, len(p.Options))
	for i, option := range p.Options {
		options[i] = map[string]string{"optionId": option.OptionID, "name": option.Name}
	}
	title := p.ToolCall.Title
	if title == "" {
		title = "(command)"
	}
	a.emit(map[string]any{"kind": "permission_request", "reqId": reqID, "toolCallId": p.ToolCall.ToolCallID, "title": title, "options": options})
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
		a.push(map[string]any{"kind": kind, "text": text})
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
		a.mu.Lock()
		if status == "pending" || status == "running" {
			a.liveTools[id] = struct{}{}
		} else {
			delete(a.liveTools, id)
		}
		a.mu.Unlock()
		ev := map[string]any{"kind": "tool_call", "id": id, "title": title, "status": status}
		copyJSONField(ev, "content", u["content"])
		copyJSONField(ev, "rawInput", u["rawInput"])
		a.push(ev)
	case "tool_call_update":
		id, err := requiredString(u, "toolCallId")
		if err != nil {
			return err
		}
		ev := map[string]any{"kind": "tool_call_update", "id": id}
		if statusWire := rawString(u["status"]); statusWire != "" {
			status := toolStatus(statusWire)
			ev["status"] = status
			if status != "pending" && status != "running" {
				a.mu.Lock()
				delete(a.liveTools, id)
				a.mu.Unlock()
			}
		}
		copyJSONField(ev, "content", u["content"])
		a.push(ev)
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
		a.push(map[string]any{"kind": "plan", "entries": norm})
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
		a.push(map[string]any{"kind": "available_commands", "commands": norm})
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
		a.push(ev)
	case "user_message_chunk", "plan_removed", "session_info_update":
		// Known optional updates have no normalized Tandem event yet.
	default:
		a.diagnostic(&OptionalUpdateError{Variant: header.Variant})
	}
	return nil
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
