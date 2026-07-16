// Package terminalhost implements the daemon-owned side of ACP terminal
// services. Process output has two independent bounded views: the relatively
// small ACP polling view and Tandem's durable UI scrollback.
package terminalhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/eventlog"
	proc "github.com/aiguy110/tandem/internal/process"
)

const (
	DefaultOutputByteLimit = 1 << 20
	DefaultScrollbackCap   = 8 << 20
	defaultShutdownTimeout = 2 * time.Second
)

var ErrClosed = errors.New("terminal host is closed")

type EventAppender interface {
	Append(eventlog.Event) (eventlog.LoggedEvent, error)
}

type Options struct {
	DefaultCwd      string
	ScrollbackCap   int
	EventLog        EventAppender
	ShutdownTimeout time.Duration
}

type EnvVariable struct {
	Name  string
	Value string
}

type CreateOptions struct {
	Command         string
	Args            []string
	Cwd             string
	Env             []EnvVariable
	OutputByteLimit *int
}

// Exit mirrors ACP TerminalExitStatus. A signal exit has a nil ExitCode and a
// normal exit has a nil Signal.
type Exit struct {
	ExitCode *int    `json:"exitCode"`
	Signal   *string `json:"signal"`
}

// Output is a bounded view. StartOffset and EndOffset are absolute byte
// offsets in the complete terminal stream and are daemon-only metadata.
type Output struct {
	Output      string
	Truncated   bool
	ExitStatus  *Exit
	StartOffset int64
	EndOffset   int64
}

type Scrollback struct {
	Output      string
	Truncated   bool
	StartOffset int64
	EndOffset   int64
}

type terminal struct {
	id       string
	pty      *proc.PTY
	acpLimit int
	cap      int

	mu         sync.RWMutex
	buffer     []byte
	produced   int64
	truncated  bool
	exit       *Exit
	done       chan struct{}
	released   bool
	releaseOne sync.Once
}

type Host struct {
	opts Options
	ctx  context.Context
	stop context.CancelFunc

	mu      sync.RWMutex
	terms   map[string]*terminal
	counter uint64
	closed  bool
	emitErr error
}

func New(opts Options) (*Host, error) {
	if opts.EventLog == nil {
		return nil, errors.New("terminal event log is required")
	}
	if opts.ScrollbackCap < 0 {
		return nil, errors.New("terminal scrollback cap cannot be negative")
	}
	if opts.ScrollbackCap == 0 {
		opts.ScrollbackCap = DefaultScrollbackCap
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultShutdownTimeout
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Host{opts: opts, ctx: ctx, stop: stop, terms: make(map[string]*terminal)}, nil
}

func (h *Host) Create(ctx context.Context, opts CreateOptions) (string, error) {
	if ctx == nil {
		return "", errors.New("terminal create: nil context")
	}
	if opts.Command == "" {
		return "", errors.New("terminal create: command is required")
	}
	limit := DefaultOutputByteLimit
	if opts.OutputByteLimit != nil {
		limit = *opts.OutputByteLimit
	}
	if limit < 0 {
		return "", errors.New("terminal create: output byte limit cannot be negative")
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return "", ErrClosed
	}
	h.counter++
	id := "term_" + strconv.FormatUint(h.counter, 10)
	t := &terminal{id: id, acpLimit: limit, cap: h.opts.ScrollbackCap, done: make(chan struct{})}
	h.mu.Unlock()

	cwd := opts.Cwd
	if cwd == "" {
		cwd = h.opts.DefaultCwd
	}
	env := append([]string(nil), os.Environ()...)
	for _, item := range opts.Env {
		env = setEnv(env, item.Name, item.Value)
	}
	argv := append([]string{opts.Command}, opts.Args...)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	pty, err := proc.StartPTY(h.ctx, proc.Spec{Argv: argv, Dir: cwd, Env: env}, proc.Size{Rows: 24, Cols: 80}, proc.Options{ShutdownTimeout: h.opts.ShutdownTimeout})
	if err != nil {
		return "", fmt.Errorf("terminal create: %w", err)
	}
	t.pty = pty
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = pty.Dispose(context.Background())
		return "", ErrClosed
	}
	h.terms[id] = t
	h.mu.Unlock()
	go h.capture(t)
	return id, nil
}

func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if len(env[i]) >= len(prefix) && env[i][:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func (h *Host) capture(t *terminal) {
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := t.pty.Read(buf)
			if n > 0 {
				h.onChunk(t, buf[:n])
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					// Unix PTYs commonly report EIO when the slave closes. The
					// process primitive normalizes that to EOF.
				}
				return
			}
		}
	}()

	processExit, err := t.pty.Wait(context.Background())
	<-readDone
	exit := normalizeExit(processExit, err)
	t.mu.Lock()
	t.exit = &exit
	close(t.done)
	t.mu.Unlock()
	// Wait has reaped the child; Dispose now only closes the PTY master.
	_ = t.pty.Dispose(context.Background())
}

func normalizeExit(exit proc.Exit, waitErr error) Exit {
	if exit.Signal != "" {
		signal := exit.Signal
		// node-pty reports its numeric signal and the Node TerminalHost
		// stringifies that value. Preserve that observable preferred-backend
		// behavior while the process primitive also retains a human label.
		if exit.SignalNumber != 0 {
			signal = strconv.Itoa(exit.SignalNumber)
		}
		return Exit{Signal: &signal}
	}
	code := exit.Code
	if waitErr != nil && code == 0 {
		code = -1
	}
	return Exit{ExitCode: &code}
}

func (h *Host) onChunk(t *terminal, chunk []byte) {
	data := append([]byte(nil), chunk...)
	t.mu.Lock()
	t.produced += int64(len(data))
	t.buffer = append(t.buffer, data...)
	if len(t.buffer) > t.cap {
		cut := charBoundary(t.buffer, len(t.buffer)-t.cap)
		t.buffer = append([]byte(nil), t.buffer[cut:]...)
		t.truncated = true
	}
	truncated := t.truncated
	t.mu.Unlock()

	payload, err := json.Marshal(struct {
		Kind      string `json:"kind"`
		TermID    string `json:"termId"`
		Chunk     string `json:"chunk"`
		Truncated bool   `json:"truncated"`
	}{"terminal_output", t.id, string(data), truncated})
	if err == nil {
		_, err = h.opts.EventLog.Append(eventlog.Event{Kind: "terminal_output", Payload: payload})
	}
	if err != nil {
		h.mu.Lock()
		h.emitErr = errors.Join(h.emitErr, err)
		h.mu.Unlock()
	}
}

func charBoundary(data []byte, start int) int {
	for start < len(data) && data[start]&0xc0 == 0x80 {
		start++
	}
	return start
}

func (h *Host) Output(id string) (Output, error) {
	t, err := h.get(id)
	if err != nil {
		return Output{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	start := 0
	if len(t.buffer) > t.acpLimit {
		start = charBoundary(t.buffer, len(t.buffer)-t.acpLimit)
	}
	end := t.produced
	absoluteStart := end - int64(len(t.buffer)-start)
	return Output{Output: string(t.buffer[start:]), Truncated: absoluteStart > 0, ExitStatus: cloneExit(t.exit), StartOffset: absoluteStart, EndOffset: end}, nil
}

func (h *Host) Scrollback(id string) (Scrollback, error) {
	t, err := h.get(id)
	if err != nil {
		return Scrollback{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	start := t.produced - int64(len(t.buffer))
	return Scrollback{Output: string(t.buffer), Truncated: t.truncated, StartOffset: start, EndOffset: t.produced}, nil
}

func (h *Host) WaitForExit(ctx context.Context, id string) (Exit, error) {
	if ctx == nil {
		return Exit{}, errors.New("terminal wait: nil context")
	}
	t, err := h.get(id)
	if err != nil {
		return Exit{}, err
	}
	select {
	case <-t.done:
		t.mu.RLock()
		exit := *cloneExit(t.exit)
		t.mu.RUnlock()
		return exit, nil
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
}

func (h *Host) Kill(id string) error {
	t, err := h.get(id)
	if err != nil {
		return err
	}
	select {
	case <-t.done:
		return nil
	default:
		return t.pty.Kill()
	}
}

func (h *Host) Release(id string) error {
	t, err := h.lookup(id)
	if err != nil {
		// Node terminal/release is idempotent even for an unknown terminal.
		return nil
	}
	t.mu.Lock()
	t.released = true
	t.mu.Unlock()
	t.releaseOne.Do(func() { _ = h.Kill(id) })
	return nil
}

func (h *Host) ExitStatus(id string) (*Exit, error) {
	t, err := h.get(id)
	if err != nil {
		return nil, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneExit(t.exit), nil
}

func (h *Host) Has(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.terms[id]
	return ok
}

func (h *Host) EventError() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.emitErr
}

// Close tears down all live terminals and releases all retained buffers. It is
// the agent-disposal boundary; Release alone intentionally retains scrollback.
func (h *Host) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("terminal close: nil context")
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.stop()
	terms := make([]*terminal, 0, len(h.terms))
	for _, t := range h.terms {
		terms = append(terms, t)
	}
	h.mu.Unlock()

	for _, t := range terms {
		select {
		case <-t.done:
		default:
			_ = t.pty.Kill()
		}
	}
	for _, t := range terms {
		select {
		case <-t.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.mu.Lock()
	h.terms = make(map[string]*terminal)
	h.mu.Unlock()
	return h.EventError()
}

func (h *Host) get(id string) (*terminal, error) { return h.lookup(id) }

func (h *Host) lookup(id string) (*terminal, error) {
	h.mu.RLock()
	t := h.terms[id]
	h.mu.RUnlock()
	if t == nil {
		return nil, fmt.Errorf("no such terminal: %s", id)
	}
	return t, nil
}

func cloneExit(exit *Exit) *Exit {
	if exit == nil {
		return nil
	}
	out := *exit
	if exit.ExitCode != nil {
		value := *exit.ExitCode
		out.ExitCode = &value
	}
	if exit.Signal != nil {
		value := *exit.Signal
		out.Signal = &value
	}
	return &out
}
