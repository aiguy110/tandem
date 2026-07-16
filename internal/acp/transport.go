// Package acp implements the protocol-neutral ACP subprocess transport.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"sync"
)

const jsonRPCVersion = "2.0"

// Config describes an ACP-speaking child process. Stderr is copied verbatim to
// Stderr; it is never mixed into the newline-delimited protocol stream.
type Config struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
	Stderr  io.Writer
}

// Request is a JSON-RPC request initiated by the ACP agent. The receiver must
// eventually call Respond or RespondError with the same ID.
type Request struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Notification is a JSON-RPC notification initiated by the ACP agent.
type Notification struct {
	Method string
	Params json.RawMessage
}

// ProtocolError reports a bad inbound line or an otherwise valid response for
// an unknown request ID. These errors do not tear down a healthy child.
type ProtocolError struct {
	Kind string
	Line string
	ID   string
	Err  error
}

func (e *ProtocolError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("acp: %s response id %s", e.Kind, e.ID)
	}
	return fmt.Sprintf("acp: %s: %v", e.Kind, e.Err)
}

type response struct {
	result json.RawMessage
	err    error
}

// RPCError is an error returned by the remote JSON-RPC endpoint.
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string { return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message) }

// Transport owns one ACP subprocess and its newline-delimited JSON-RPC stream.
// Requests, Notifications, and Errors must be drained by the caller.
type Transport struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	done     chan struct{}
	readDone chan struct{}
	wait     error

	requests      chan Request
	notifications chan Notification
	errors        chan error

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan response
	stop    context.CancelFunc
	once    sync.Once
}

// Start launches an ACP child and begins reading its stdout. Cancellation of
// ctx terminates the child and rejects every pending call.
func Start(ctx context.Context, cfg Config) (*Transport, error) {
	if cfg.Command == "" {
		return nil, errors.New("acp: command is required")
	}
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, cfg.Command, cfg.Args...)
	cmd.Dir, cmd.Env = cfg.Dir, cfg.Env
	if !nilWriter(cfg.Stderr) {
		cmd.Stderr = cfg.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("acp start: %w", err)
	}

	t := &Transport{
		cmd: cmd, in: stdin, done: make(chan struct{}), readDone: make(chan struct{}), requests: make(chan Request, 16),
		notifications: make(chan Notification, 32), errors: make(chan error, 16),
		pending: make(map[string]chan response), stop: cancel,
	}
	go t.read(stdout)
	go t.reap()
	return t, nil
}

func nilWriter(w io.Writer) bool {
	if w == nil {
		return true
	}
	v := reflect.ValueOf(w)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (t *Transport) Requests() <-chan Request           { return t.requests }
func (t *Transport) Notifications() <-chan Notification { return t.notifications }
func (t *Transport) Errors() <-chan error               { return t.errors }
func (t *Transport) PID() int                           { return t.cmd.Process.Pid }

// Call sends a request and unmarshals its result into result. A canceled call
// is removed from the pending set; a later response is reported as unknown.
func (t *Transport) Call(ctx context.Context, method string, params, result any) error {
	t.mu.Lock()
	t.nextID++
	id := fmt.Sprintf("%d", t.nextID)
	ch := make(chan response, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	if err := t.write(rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage(id), Method: method, Params: marshalRaw(params)}); err != nil {
		t.remove(id)
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(r.result, result); err != nil {
			return fmt.Errorf("acp decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		t.remove(id)
		return ctx.Err()
	case <-t.done:
		t.remove(id)
		return t.exitError()
	}
}

func (t *Transport) Notify(method string, params any) error {
	return t.write(rpcMessage{JSONRPC: jsonRPCVersion, Method: method, Params: marshalRaw(params)})
}

func (t *Transport) Respond(id json.RawMessage, result any) error {
	raw := marshalRaw(result)
	if raw == nil {
		raw = json.RawMessage("null")
	}
	return t.write(rpcMessage{JSONRPC: jsonRPCVersion, ID: id, Result: raw})
}

func (t *Transport) RespondError(id json.RawMessage, code int, message string, data any) error {
	e := &wireError{Code: code, Message: message}
	if data != nil {
		e.Data = marshalRaw(data)
	}
	return t.write(rpcMessage{JSONRPC: jsonRPCVersion, ID: id, Error: e})
}

// Wait returns after the child exits. A zero exit status is nil; EOF alone is
// not considered failure, though pending calls are still rejected.
func (t *Transport) Wait() error { <-t.done; return t.wait }

// Close terminates the subprocess and waits for it to be reaped.
func (t *Transport) Close() error { t.stop(); return t.Wait() }

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

func marshalRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("acp: cannot marshal RPC value: %v", err))
	}
	return b
}

func (t *Transport) write(m rpcMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	select {
	case <-t.done:
		return t.exitError()
	default:
	}
	if _, err := t.in.Write(b); err != nil {
		return fmt.Errorf("acp write: %w", err)
	}
	return nil
}

func (t *Transport) read(r io.Reader) {
	defer close(t.readDone)
	s := bufio.NewScanner(r)
	// ACP messages can contain images and large tool results. Scanner's default
	// 64 KiB token limit is not protocol-safe.
	s.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != jsonRPCVersion {
			if err == nil {
				err = errors.New("jsonrpc must be 2.0")
			}
			t.report(&ProtocolError{Kind: "malformed message", Line: string(line), Err: err})
			continue
		}
		t.handle(m)
	}
	if err := s.Err(); err != nil {
		t.report(fmt.Errorf("acp read: %w", err))
	}
}

func (t *Transport) handle(m rpcMessage) {
	if m.Method != "" {
		if len(m.ID) == 0 {
			t.notifications <- Notification{Method: m.Method, Params: m.Params}
		} else {
			t.requests <- Request{ID: m.ID, Method: m.Method, Params: m.Params}
		}
		return
	}
	if len(m.ID) == 0 || (m.Error == nil && m.Result == nil) {
		t.report(&ProtocolError{Kind: "malformed message", Err: errors.New("message is neither request, notification, nor response")})
		return
	}
	id := string(m.ID)
	t.mu.Lock()
	ch, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
	}
	t.mu.Unlock()
	if !ok {
		t.report(&ProtocolError{Kind: "unknown", ID: id})
		return
	}
	if m.Error != nil {
		ch <- response{err: &RPCError{Code: m.Error.Code, Message: m.Error.Message, Data: m.Error.Data}}
	} else {
		ch <- response{result: m.Result}
	}
}

func (t *Transport) remove(id string) { t.mu.Lock(); delete(t.pending, id); t.mu.Unlock() }
func (t *Transport) report(err error) {
	select {
	case t.errors <- err:
	default:
	}
}

func (t *Transport) reap() {
	err := t.cmd.Wait()
	<-t.readDone
	t.once.Do(func() {
		t.mu.Lock()
		t.wait = err
		pending := t.pending
		t.pending = make(map[string]chan response)
		t.mu.Unlock()
		for _, ch := range pending {
			ch <- response{err: t.exitError()}
		}
		close(t.done)
		close(t.requests)
		close(t.notifications)
		close(t.errors)
	})
}

func (t *Transport) exitError() error {
	if t.wait != nil {
		return fmt.Errorf("acp child exited: %w", t.wait)
	}
	return io.EOF
}
