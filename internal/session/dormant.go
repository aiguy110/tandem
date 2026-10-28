package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
)

// Waker starts the real adapter for a dormant session. It receives the
// session's event log so the replacement writes into the same durable stream.
type Waker func(ctx context.Context, log *eventlog.Log) (agentadapter.Adapter, error)

var errDormant = errors.New("agent is dormant; send it a prompt to wake it")

// dormantAdapter stands in for an agent whose process is not running: one
// restored lazily after a daemon restart, or hibernated after sitting idle.
// It holds just enough identity to answer read-only queries; anything that
// needs the agent wakes the session first (see ensureAwake).
type dormantAdapter struct {
	externalSessionID string
	caps              agentadapter.Capabilities
	events            chan eventlog.Event
	done              chan struct{}
	once              sync.Once
}

func newDormantAdapter(externalSessionID string, caps agentadapter.Capabilities) *dormantAdapter {
	return &dormantAdapter{externalSessionID: externalSessionID, caps: caps, events: make(chan eventlog.Event), done: make(chan struct{})}
}

func (d *dormantAdapter) Capabilities() agentadapter.Capabilities { return d.caps }
func (d *dormantAdapter) Events() <-chan eventlog.Event           { return d.events }
func (d *dormantAdapter) Done() <-chan struct{}                   { return d.done }
func (d *dormantAdapter) Prompt(context.Context, []agentadapter.PromptBlock) (string, error) {
	return "", errDormant
}
func (d *dormantAdapter) SendInput([]byte) error      { return errDormant }
func (d *dormantAdapter) Resize(uint16, uint16) error { return nil }
func (d *dormantAdapter) RespondPermission(string, string) error {
	return errors.New("no such pending approval: the agent is dormant")
}

// Interrupt is a no-op: a dormant agent has no turn to cancel.
func (d *dormantAdapter) Interrupt() error { return nil }
func (d *dormantAdapter) Close(context.Context) error {
	d.once.Do(func() { close(d.events); close(d.done) })
	return nil
}
func (d *dormantAdapter) ExternalSessionID() string { return d.externalSessionID }
func (d *dormantAdapter) PID() int                  { return 0 }

// optimisticCapabilities is what a dormant session reports before it has ever
// run in this daemon. Checks made against it are re-made by the real adapter
// once it wakes, so erring permissive only defers the rejection.
var optimisticCapabilities = agentadapter.Capabilities{Structured: true, LoadSession: true, ForkSession: true, Image: true, Steering: true}

// NewDormant builds a session whose agent process is not started until it is
// needed. wake is called (once per wake) to launch the real adapter.
func NewDormant(id, name string, spec agentadapter.Spec, log *eventlog.Log, status Status, externalSessionID string, wake Waker) (*Session, error) {
	if wake == nil {
		return nil, errors.New("session: a dormant session needs a waker")
	}
	s, err := NewWithStatus(id, name, spec, newDormantAdapter(externalSessionID, optimisticCapabilities), log, status)
	if err != nil {
		return nil, err
	}
	s.SetWaker(wake)
	return s, nil
}

// SetWaker lets the session hibernate when idle, relaunching through wake.
func (s *Session) SetWaker(wake Waker) {
	s.mu.Lock()
	s.waker = wake
	s.mu.Unlock()
}

// Dormant reports whether the session's agent process is currently stopped.
func (s *Session) Dormant() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, dormant := s.adapter.(*dormantAdapter)
	return dormant
}

// IdleFor reports how long the session has gone without an event or turn.
// It is zero while a turn is running.
func (s *Session) IdleFor() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active > 0 {
		return 0
	}
	return time.Since(s.lastActivity)
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// Wake starts the agent's process now if the session is dormant.
func (s *Session) Wake(ctx context.Context) error { return s.ensureAwake(ctx) }

// ensureAwake starts the real adapter if the session is dormant. Concurrent
// callers share one wake. The adapter's lifetime is the session's, not the
// caller's, so the caller's cancellation is detached from the launch.
func (s *Session) ensureAwake(ctx context.Context) error {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	s.mu.RLock()
	dormant, isDormant := s.adapter.(*dormantAdapter)
	disposed := s.disposed
	wake := s.waker
	s.mu.RUnlock()
	if !isDormant {
		return nil
	}
	if disposed {
		return errors.New("session: disposed")
	}
	if wake == nil {
		return errors.New("session: dormant agent has no waker")
	}
	started := time.Now()
	slog.Info("waking dormant agent", "session_id", s.ID, "agent", s.Spec.Agent)
	a, err := wake(context.WithoutCancel(ctx), s.Log)
	if err != nil {
		slog.Warn("failed to wake dormant agent", "session_id", s.ID, "agent", s.Spec.Agent, "elapsed_ms", time.Since(started).Milliseconds(), "error", err)
		return fmt.Errorf("wake agent: %w", err)
	}
	s.mu.Lock()
	if s.disposed || s.adapter != agentadapter.Adapter(dormant) {
		// Disposed, or a handoff/restart replaced the placeholder while the
		// process was starting; the replacement owns the session now.
		s.mu.Unlock()
		_ = a.Close(context.Background())
		slog.Info("discarded woken agent superseded during wake", "session_id", s.ID)
		return nil
	}
	s.adapter = a
	s.adapterEpoch++
	epoch := s.adapterEpoch
	s.lastActivity = time.Now()
	s.mu.Unlock()
	_ = dormant.Close(context.Background())
	if binder, ok := a.(agentadapter.EventBinder); ok {
		binder.BindEventSink(s.append)
	}
	go s.pump(a, epoch, nil)
	slog.Info("woke dormant agent", "session_id", s.ID, "agent", s.Spec.Agent, "pid", a.PID(), "elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

// Hibernate stops an idle agent's process, leaving the session dormant until
// it is next needed. It refuses (returning false) while a turn is running or
// queued, an approval is pending, a hand-off is in progress, or the session is
// not in transcript control, since any of those need the live process.
func (s *Session) Hibernate(ctx context.Context) (bool, error) {
	s.mu.RLock()
	canWake := s.waker != nil
	s.mu.RUnlock()
	if !canWake {
		return false, nil
	}
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	// Holding promptMu across the swap closes prompt admission: a prompt
	// accepted after this point sees the dormant adapter and wakes it.
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	if s.promptRunning || len(s.promptQueue) > 0 || s.handoffSnapshotting {
		return false, nil
	}
	s.mu.Lock()
	if _, dormant := s.adapter.(*dormantAdapter); dormant || s.disposed || s.active > 0 || len(s.approvals) > 0 || s.controlMode != "transcript" {
		s.mu.Unlock()
		return false, nil
	}
	old := s.adapter
	s.adapter = newDormantAdapter(old.ExternalSessionID(), old.Capabilities())
	s.adapterEpoch++
	idle := time.Since(s.lastActivity)
	s.mu.Unlock()
	if err := old.Close(ctx); err != nil {
		slog.Warn("error stopping hibernated agent", "session_id", s.ID, "error", err)
		return true, err
	}
	slog.Info("hibernated idle agent", "session_id", s.ID, "agent", s.Spec.Agent, "idle_ms", idle.Milliseconds())
	return true, nil
}

func (s *Session) emitStatus(status Status) {
	payload, _ := json.Marshal(map[string]any{"kind": "status", "status": string(status)})
	s.emit(eventlog.Event{Kind: "status", Payload: payload})
}
