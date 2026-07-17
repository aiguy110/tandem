// Package session owns one live adapter and its durable normalized event stream.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/eventlog"
	proc "github.com/aiguy110/tandem/internal/process"
)

type Status string

const (
	Idle    Status = "idle"
	Working Status = "working"
	Blocked Status = "blocked"
	Error   Status = "error"
)

type Session struct {
	ID, Name string
	Spec     agentadapter.Spec
	Log      *eventlog.Log
	adapter  agentadapter.Adapter

	mu           sync.RWMutex
	status       Status
	approvals    map[string]agentadapter.Approval
	listeners    map[uint64]func(eventlog.LoggedEvent)
	nextListener uint64
	turnMu       sync.Mutex
	active       int
	controlMode  string
	adapterEpoch uint64
	disposeOnce  sync.Once
	disposeErr   error
	done         chan struct{}

	// User escape-hatch shell (docs/terminal.md: the Terminal tab). Independent
	// of the agent adapter, so it survives ACP↔CLI control swaps and runs
	// concurrently with the agent. Lazily spawned on first OpenUserShell.
	shellMu      sync.Mutex
	shell        *proc.PTY
	shellCancel  context.CancelFunc
	shellDone    chan struct{}
	shellRunning bool
}

func New(id, name string, spec agentadapter.Spec, adapter agentadapter.Adapter, log *eventlog.Log) (*Session, error) {
	return NewWithStatus(id, name, spec, adapter, log, Idle)
}

func NewWithStatus(id, name string, spec agentadapter.Spec, adapter agentadapter.Adapter, log *eventlog.Log, status Status) (*Session, error) {
	if adapter == nil || log == nil {
		return nil, errors.New("session: adapter and event log are required")
	}
	s := &Session{ID: id, Name: name, Spec: spec, Log: log, adapter: adapter, status: status, controlMode: "transcript", approvals: map[string]agentadapter.Approval{}, listeners: map[uint64]func(eventlog.LoggedEvent){}, done: make(chan struct{})}
	if binder, ok := adapter.(agentadapter.EventBinder); ok {
		binder.BindEventSink(s.append)
	}
	s.adapterEpoch = 1
	go s.pump(adapter, 1, nil)
	return s, nil
}

func (s *Session) pump(adapter agentadapter.Adapter, epoch uint64, onExit func()) {
	for ev := range adapter.Events() {
		s.mu.RLock()
		current := epoch == s.adapterEpoch
		s.mu.RUnlock()
		if current {
			s.emit(ev)
		}
	}
	s.mu.RLock()
	current := epoch == s.adapterEpoch
	s.mu.RUnlock()
	if current && onExit != nil {
		onExit()
	}
}

func (s *Session) emit(ev eventlog.Event) {
	_, _ = s.append(ev)
}

func (s *Session) append(ev eventlog.Event) (eventlog.LoggedEvent, error) {
	if ev.Kind != "raw_pty" {
		var wire struct {
			Kind, Status, ReqID, ToolCallID, Title string
			Options                                []agentadapter.ApprovalOption
		}
		if json.Unmarshal(ev.Payload, &wire) == nil {
			s.mu.Lock()
			switch wire.Kind {
			case "status":
				s.status = Status(wire.Status)
			case "permission_request":
				s.status = Blocked
				s.approvals[wire.ReqID] = agentadapter.Approval{ReqID: wire.ReqID, ToolCallID: wire.ToolCallID, Title: wire.Title, Options: wire.Options}
			}
			s.mu.Unlock()
		}
	}
	le, err := s.Log.Append(ev)
	if err != nil {
		s.mu.Lock()
		s.status = Error
		s.mu.Unlock()
		return eventlog.LoggedEvent{}, err
	}
	s.mu.RLock()
	callbacks := make([]func(eventlog.LoggedEvent), 0, len(s.listeners))
	for _, cb := range s.listeners {
		callbacks = append(callbacks, cb)
	}
	s.mu.RUnlock()
	for _, cb := range callbacks {
		cb(le)
	}
	return le, nil
}

func (s *Session) Status() Status     { s.mu.RLock(); defer s.mu.RUnlock(); return s.status }
func (s *Session) SetStatus(v Status) { s.mu.Lock(); s.status = v; s.mu.Unlock() }

// PushEvent lets daemon-owned auxiliary services (notably browser takeover)
// enter the same durable event stream as adapter updates.
func (s *Session) PushEvent(ev eventlog.Event) { s.emit(ev) }
func (s *Session) SessionID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adapter.SessionID()
}
func (s *Session) PID() int { s.mu.RLock(); defer s.mu.RUnlock(); return s.adapter.PID() }
func (s *Session) Capabilities() agentadapter.Capabilities {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adapter.Capabilities()
}
func (s *Session) ControlMode() string { s.mu.RLock(); defer s.mu.RUnlock(); return s.controlMode }
func (s *Session) ActiveTurn() bool    { s.mu.RLock(); defer s.mu.RUnlock(); return s.active > 0 }
func (s *Session) PendingApprovals() []agentadapter.Approval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]agentadapter.Approval, 0, len(s.approvals))
	for _, a := range s.approvals {
		out = append(out, a)
	}
	return out
}

// ValidatePrompt performs the synchronous prompt checks required by the wire
// protocol. In particular, image failures must be reported before a user
// message is persisted or an ACP turn starts.
func (s *Session) ValidatePrompt(blocks []agentadapter.PromptBlock) error {
	if len(blocks) == 0 {
		return errors.New("prompt must contain at least one block")
	}
	images := 0
	for _, block := range blocks {
		switch block.Type {
		case "text":
		case "image":
			images++
			if !s.adapter.Capabilities().Image {
				return errors.New("this agent does not support image prompts")
			}
		default:
			return errors.New("invalid prompt block type " + block.Type)
		}
	}
	if images > assets.MaxPromptImages {
		return fmt.Errorf("prompt may contain at most %d images", assets.MaxPromptImages)
	}
	if validator, ok := s.adapter.(interface {
		ValidatePrompt([]agentadapter.PromptBlock) error
	}); ok {
		return validator.ValidatePrompt(blocks)
	}
	return nil
}

func (s *Session) Prompt(ctx context.Context, blocks []agentadapter.PromptBlock) (string, error) {
	if s.ControlMode() != "transcript" {
		return "", errors.New("agent session is controlled by the terminal")
	}
	if err := s.ValidatePrompt(blocks); err != nil {
		return "", err
	}
	// A turn holds this gate through completion: concurrent callers are ordered,
	// never rejected or allowed to overlap an adapter's session/prompt call.
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.mu.Lock()
	s.active++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	text := ""
	for _, b := range blocks {
		if b.Type == "text" {
			text += b.Text
		}
	}
	payload, _ := json.Marshal(map[string]any{"kind": "user_message", "text": text, "blocks": blocks})
	s.emit(eventlog.Event{Kind: "user_message", Payload: payload})
	return s.adapter.Prompt(ctx, blocks)
}
func (s *Session) SendInput(b []byte) error       { return s.adapter.SendInput(b) }
func (s *Session) Resize(cols, rows uint16) error { return s.adapter.Resize(cols, rows) }
func (s *Session) RespondPermission(reqID, optionID string) error {
	if err := s.adapter.RespondPermission(reqID, optionID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.approvals, reqID)
	s.mu.Unlock()
	return nil
}
func (s *Session) Interrupt() error {
	s.mu.RLock()
	a := s.adapter
	s.mu.RUnlock()
	if err := a.Interrupt(); err != nil {
		return err
	}
	s.mu.Lock()
	s.approvals = map[string]agentadapter.Approval{}
	s.mu.Unlock()
	return nil
}

// SetMode and SetConfigOption expose ACP's live session configuration without
// making those structured-only operations part of every adapter implementation.
func (s *Session) SetMode(ctx context.Context, modeID string) error {
	s.mu.RLock()
	a := s.adapter
	s.mu.RUnlock()
	configurable, ok := a.(interface {
		SetMode(context.Context, string) error
	})
	if !ok {
		return errors.New("agent does not support session modes")
	}
	return configurable.SetMode(ctx, modeID)
}

func (s *Session) SetConfigOption(ctx context.Context, configID string, value any) error {
	s.mu.RLock()
	a := s.adapter
	s.mu.RUnlock()
	configurable, ok := a.(interface {
		SetConfigOption(context.Context, string, any) error
	})
	if !ok {
		return errors.New("agent does not support session config options")
	}
	return configurable.SetConfigOption(ctx, configID, value)
}

func (s *Session) SetPersistedSessionConfig(config json.RawMessage) {
	s.mu.Lock()
	s.Spec.SessionConfig = append(json.RawMessage(nil), config...)
	s.mu.Unlock()
}

// InterruptAndWait requests ACP cancellation and gives the active prompt a
// bounded window to resolve before its process is replaced during handoff.
func (s *Session) InterruptAndWait(ctx context.Context) error {
	if err := s.Interrupt(); err != nil {
		return err
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for s.ActiveTurn() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}

func (s *Session) SetControlMode(mode string) {
	s.mu.Lock()
	s.controlMode = mode
	s.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"kind": "control_state", "mode": mode})
	s.emit(eventlog.Event{Kind: "control_state", Payload: payload})
}

// SwapAdapter disposes the current process, starts its replacement, and keeps
// the Session/event log/listeners intact. The start callback is deliberately
// invoked after disposal so ACP and its resumable CLI never own one session at
// the same time. A caller can invoke SwapAdapter again to recover a failed start.
func (s *Session) SwapAdapter(ctx context.Context, start func() (agentadapter.Adapter, error), mode string, onExit func()) error {
	s.mu.Lock()
	s.adapterEpoch++
	old := s.adapter
	s.mu.Unlock()
	if err := old.Close(ctx); err != nil {
		return err
	}
	a, err := start()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.adapter = a
	s.adapterEpoch++
	epoch := s.adapterEpoch
	s.mu.Unlock()
	if binder, ok := a.(agentadapter.EventBinder); ok {
		binder.BindEventSink(s.append)
	}
	go s.pump(a, epoch, onExit)
	s.SetControlMode(mode)
	return nil
}
func (s *Session) OnEvent(cb func(eventlog.LoggedEvent)) func() {
	s.mu.Lock()
	id := s.nextListener
	s.nextListener++
	s.listeners[id] = cb
	s.mu.Unlock()
	return func() { s.mu.Lock(); delete(s.listeners, id); s.mu.Unlock() }
}
func (s *Session) Dispose(ctx context.Context) error {
	s.disposeOnce.Do(func() {
		s.CloseUserShell()
		s.mu.Lock()
		s.adapterEpoch++
		a := s.adapter
		s.mu.Unlock()
		s.disposeErr = a.Close(ctx)
		close(s.done)
	})
	return s.disposeErr
}
func (s *Session) Done() <-chan struct{} { return s.done }
