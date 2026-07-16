// Package ptyadapter runs terminal-only agents behind Tandem's normalized
// event vocabulary. It deliberately has no dependency on the structured ACP
// adapter; the registry supplies the common integration seam.
package ptyadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
	processx "github.com/aiguy110/tandem/internal/process"
)

// Capabilities describes the structured features a raw PTY can provide.
// Every field is false by design: a PTY is an opaque byte stream.
type Capabilities struct {
	Structured  bool
	Terminals   bool
	LoadSession bool
	FS          bool
	Image       bool
}

// SpawnOptions describes a terminal child. Command defaults to the platform
// shell. Env overlays the daemon environment, matching the Node adapter.
type SpawnOptions struct {
	Command         string
	Args            []string
	Dir             string
	Env             map[string]string
	Rows            uint16
	Cols            uint16
	ShutdownTimeout time.Duration
}

// Adapter owns one PTY child and its normalized event stream.
type Adapter struct {
	id string

	mu       sync.RWMutex
	terminal *processx.PTY
	spawned  bool
	closed   bool
	exit     processx.Exit
	spawnErr error

	events   chan eventlog.Event
	done     chan struct{}
	stopping chan struct{}
	cancel   context.CancelFunc

	disposeOnce sync.Once
	disposeErr  error
}

func New(id string) *Adapter {
	return &Adapter{
		id: id, events: make(chan eventlog.Event, 128),
		done: make(chan struct{}), stopping: make(chan struct{}),
	}
}

func (a *Adapter) ID() string                    { return a.id }
func (a *Adapter) Capabilities() Capabilities    { return Capabilities{} }
func (a *Adapter) Events() <-chan eventlog.Event { return a.events }
func (a *Adapter) Done() <-chan struct{}         { return a.done }

func (a *Adapter) PID() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.terminal == nil {
		return 0
	}
	return a.terminal.PID()
}

// Spawn starts the PTY and returns after the child has successfully launched.
// An adapter is single-use, including after a failed spawn attempt.
func (a *Adapter) Spawn(ctx context.Context, opts SpawnOptions) error {
	if ctx == nil {
		return errors.New("pty adapter: nil context")
	}
	a.mu.Lock()
	if a.spawned || a.closed {
		a.mu.Unlock()
		return errors.New("pty adapter: already spawned or disposed")
	}
	a.spawned = true

	command := opts.Command
	if command == "" {
		command = defaultShell()
	}
	rows, cols := opts.Rows, opts.Cols
	if rows == 0 {
		rows = 30
	}
	if cols == 0 {
		cols = 100
	}
	childCtx, cancel := context.WithCancel(ctx)
	terminal, err := processx.StartPTY(childCtx, processx.Spec{
		Argv: append([]string{command}, opts.Args...),
		Dir:  opts.Dir,
		Env:  mergedEnv(opts.Env),
	}, processx.Size{Rows: rows, Cols: cols}, processx.Options{ShutdownTimeout: opts.ShutdownTimeout})
	if err != nil {
		cancel()
		a.spawnErr = fmt.Errorf("pty adapter: spawn: %w", err)
		a.closed = true
		close(a.events)
		close(a.done)
		a.mu.Unlock()
		return a.spawnErr
	}
	a.terminal = terminal
	a.cancel = cancel
	a.mu.Unlock()
	a.emit(statusEvent("working"))
	go a.pump(terminal)
	return nil
}

func (a *Adapter) pump(terminal *processx.PTY) {
	readErr := stream(terminal, a.emit)
	exit, waitErr := terminal.Wait(context.Background())
	if waitErr != nil {
		a.emit(errorEvent(waitErr))
	}
	if readErr != nil {
		a.emit(errorEvent(fmt.Errorf("read PTY: %w", readErr)))
	}
	disposeErr := terminal.Dispose(context.Background())
	if disposeErr != nil {
		a.emit(errorEvent(fmt.Errorf("close PTY: %w", disposeErr)))
	}
	a.mu.Lock()
	a.exit = exit
	a.closed = true
	a.mu.Unlock()
	status := "idle"
	if waitErr != nil || readErr != nil || disposeErr != nil || exit.Code != 0 || exit.Signal != "" {
		status = "error"
	}
	a.emit(statusEvent(status))
	close(a.events)
	close(a.done)
}

// stream preserves read boundaries and bytes exactly. In particular, it never
// decodes UTF-8, so split code points and non-text bytes survive unchanged.
func stream(reader io.Reader, emit func(eventlog.Event) bool) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 && !emit(eventlog.RawPTY(buf[:n])) {
			return nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (a *Adapter) emit(event eventlog.Event) bool {
	a.mu.RLock()
	cancel := a.cancel
	a.mu.RUnlock()
	if cancel == nil {
		a.events <- event
		return true
	}
	// Cancellation is the disposal path. Abandon blocked event delivery so a
	// noisy child cannot prevent forced cleanup.
	select {
	case a.events <- event:
		return true
	case <-a.stopping:
		return false
	}
}

// SendInput writes bytes without converting them through UTF-8 strings.
func (a *Adapter) SendInput(data []byte) error {
	terminal, err := a.liveTerminal()
	if err != nil {
		return err
	}
	for len(data) > 0 {
		n, writeErr := terminal.Write(data)
		if writeErr != nil {
			return fmt.Errorf("pty adapter: input: %w", writeErr)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (a *Adapter) Resize(cols, rows uint16) error {
	terminal, err := a.liveTerminal()
	if err != nil {
		return err
	}
	if err := terminal.Resize(processx.Size{Rows: rows, Cols: cols}); err != nil {
		return fmt.Errorf("pty adapter: resize: %w", err)
	}
	return nil
}

// Interrupt sends the terminal's interrupt character, matching Ctrl-C typed by
// a user while leaving process-group ownership with the PTY primitive.
func (a *Adapter) Interrupt() error { return a.SendInput([]byte{3}) }

func (a *Adapter) Prompt(string) error {
	return errors.New("pty adapter: structured prompts are unsupported")
}

func (a *Adapter) RespondPermission(string, string) error {
	return errors.New("pty adapter: structured permissions are unsupported")
}

func (a *Adapter) LoadSession(string) error {
	return errors.New("pty adapter: session loading is unsupported")
}

// Wait returns the child's disposition after the event stream is complete.
func (a *Adapter) Wait(ctx context.Context) (processx.Exit, error) {
	if ctx == nil {
		return processx.Exit{}, errors.New("pty adapter: nil context")
	}
	a.mu.RLock()
	spawned := a.spawned
	a.mu.RUnlock()
	if !spawned {
		return processx.Exit{}, errors.New("pty adapter: not spawned")
	}
	select {
	case <-a.done:
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.spawnErr != nil {
			return processx.Exit{}, a.spawnErr
		}
		return a.exit, nil
	case <-ctx.Done():
		return processx.Exit{}, ctx.Err()
	}
}

// Dispose terminates the child session and closes the master. It is safe to
// race with natural exit and to call repeatedly.
func (a *Adapter) Dispose(ctx context.Context) error {
	if ctx == nil {
		return errors.New("pty adapter: nil context")
	}
	a.disposeOnce.Do(func() {
		close(a.stopping)
		a.mu.Lock()
		terminal, cancel := a.terminal, a.cancel
		if !a.spawned {
			a.closed = true
			close(a.events)
			close(a.done)
		}
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if terminal != nil {
			a.disposeErr = terminal.Dispose(ctx)
		}
	})
	return a.disposeErr
}

func (a *Adapter) liveTerminal() (*processx.PTY, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.terminal == nil {
		return nil, errors.New("pty adapter: not spawned")
	}
	select {
	case <-a.terminal.Done():
		return nil, errors.New("pty adapter: process has exited")
	default:
		return a.terminal, nil
	}
}

func mergedEnv(overlay map[string]string) []string {
	values := make(map[string]string, len(os.Environ())+len(overlay)+1)
	for _, entry := range os.Environ() {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				values[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	for key, value := range overlay {
		values[key] = value
	}
	if _, ok := values["TERM"]; !ok {
		values["TERM"] = "xterm-256color"
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func statusEvent(status string) eventlog.Event {
	payload, _ := json.Marshal(struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}{"status", status})
	return eventlog.Event{Kind: "status", Payload: payload}
}

func errorEvent(err error) eventlog.Event {
	payload, _ := json.Marshal(struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}{"error", err.Error()})
	return eventlog.Event{Kind: "error", Payload: payload}
}
