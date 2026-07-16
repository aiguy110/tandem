// Package session owns one live adapter and its durable normalized event stream.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
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
	disposeOnce  sync.Once
	disposeErr   error
	done         chan struct{}
}

func New(id, name string, spec agentadapter.Spec, adapter agentadapter.Adapter, log *eventlog.Log) (*Session, error) {
	return NewWithStatus(id, name, spec, adapter, log, Idle)
}

func NewWithStatus(id, name string, spec agentadapter.Spec, adapter agentadapter.Adapter, log *eventlog.Log, status Status) (*Session, error) {
	if adapter == nil || log == nil {
		return nil, errors.New("session: adapter and event log are required")
	}
	s := &Session{ID: id, Name: name, Spec: spec, Log: log, adapter: adapter, status: status, approvals: map[string]agentadapter.Approval{}, listeners: map[uint64]func(eventlog.LoggedEvent){}, done: make(chan struct{})}
	if binder, ok := adapter.(agentadapter.EventBinder); ok {
		binder.BindEventSink(s.append)
	}
	go s.pump()
	return s, nil
}

func (s *Session) pump() {
	defer close(s.done)
	for ev := range s.adapter.Events() {
		s.emit(ev)
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
func (s *Session) PushEvent(ev eventlog.Event)             { s.emit(ev) }
func (s *Session) SessionID() string                       { return s.adapter.SessionID() }
func (s *Session) PID() int                                { return s.adapter.PID() }
func (s *Session) Capabilities() agentadapter.Capabilities { return s.adapter.Capabilities() }
func (s *Session) ActiveTurn() bool                        { s.mu.RLock(); defer s.mu.RUnlock(); return s.active > 0 }
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
	for _, block := range blocks {
		switch block.Type {
		case "text":
		case "image":
			if !s.adapter.Capabilities().Image {
				return errors.New("this agent does not support image prompts")
			}
		default:
			return errors.New("invalid prompt block type " + block.Type)
		}
	}
	if validator, ok := s.adapter.(interface {
		ValidatePrompt([]agentadapter.PromptBlock) error
	}); ok {
		return validator.ValidatePrompt(blocks)
	}
	return nil
}

func (s *Session) Prompt(ctx context.Context, blocks []agentadapter.PromptBlock) (string, error) {
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
	if err := s.adapter.Interrupt(); err != nil {
		return err
	}
	s.mu.Lock()
	s.approvals = map[string]agentadapter.Approval{}
	s.mu.Unlock()
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
	s.disposeOnce.Do(func() { s.disposeErr = s.adapter.Close(ctx) })
	return s.disposeErr
}
func (s *Session) Done() <-chan struct{} { return s.done }
