// Package toolregistry provides a daemon-owned catalog of callable tools.
package toolregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound    = errors.New("tool not found")
	ErrUnavailable = errors.New("tool unavailable")
)

// Declaration is the stable metadata exposed to automation clients.
type Declaration struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// Status combines a declaration with its current runtime availability.
type Status struct {
	Declaration Declaration `json:"declaration"`
	Available   bool        `json:"available"`
	Reason      string      `json:"reason,omitempty"`
}

// Handler implements one tool. Arguments and results remain JSON so the
// registry does not lose MCP content types or server-specific extensions.
type Handler func(context.Context, json.RawMessage) (json.RawMessage, error)

// Availability is evaluated at lookup/invocation time. A nil function means
// available. A non-nil error is retained as the human-readable reason.
type Availability func(context.Context) error

type Tool struct {
	Declaration Declaration
	Invoke      Handler
	Available   Availability
}

// Invocation is supplied to audit hooks. Arguments are copied before hooks
// are called, so a hook may retain them safely.
type Invocation struct {
	Name      string
	Arguments json.RawMessage
	StartedAt time.Time
}

type Outcome struct {
	Result   json.RawMessage
	Err      error
	Duration time.Duration
}

// AuditHook observes every call that reaches Invoke, including unknown and
// unavailable tools. Hooks should return promptly; they run synchronously.
type AuditHook interface {
	Before(context.Context, Invocation)
	After(context.Context, Invocation, Outcome)
}

// Registry is safe for concurrent registration, discovery, and invocation.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	hooks []AuditHook
}

func New(hooks ...AuditHook) *Registry {
	return &Registry{tools: make(map[string]Tool), hooks: append([]AuditHook(nil), hooks...)}
}

func (r *Registry) AddAuditHook(h AuditHook) {
	if h == nil {
		return
	}
	r.mu.Lock()
	r.hooks = append(r.hooks, h)
	r.mu.Unlock()
}

// Register adds a tool. Duplicate names are rejected to prevent one MCP
// server from silently shadowing another.
func (r *Registry) Register(tool Tool) error {
	if err := validateTool(tool); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[tool.Declaration.Name]; exists {
		return fmt.Errorf("register tool %q: already registered", tool.Declaration.Name)
	}
	tool.Declaration = cloneDeclaration(tool.Declaration)
	r.tools[tool.Declaration.Name] = tool
	return nil
}

func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	delete(r.tools, name)
	r.mu.Unlock()
}

// List returns a deterministic snapshot of declarations and availability.
func (r *Registry) List(ctx context.Context) []Status {
	r.mu.RLock()
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	r.mu.RUnlock()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Declaration.Name < tools[j].Declaration.Name })
	statuses := make([]Status, 0, len(tools))
	for _, tool := range tools {
		status := Status{Declaration: cloneDeclaration(tool.Declaration), Available: true}
		if tool.Available != nil {
			if err := tool.Available(ctx); err != nil {
				status.Available, status.Reason = false, err.Error()
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (r *Registry) Lookup(ctx context.Context, name string) (Status, bool) {
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return Status{}, false
	}
	status := Status{Declaration: cloneDeclaration(tool.Declaration), Available: true}
	if tool.Available != nil {
		if err := tool.Available(ctx); err != nil {
			status.Available, status.Reason = false, err.Error()
		}
	}
	return status, true
}

func (r *Registry) Invoke(ctx context.Context, name string, arguments json.RawMessage) (result json.RawMessage, err error) {
	inv := Invocation{Name: name, Arguments: cloneRaw(arguments), StartedAt: time.Now()}
	r.mu.RLock()
	tool, ok := r.tools[name]
	hooks := append([]AuditHook(nil), r.hooks...)
	r.mu.RUnlock()
	for _, hook := range hooks {
		hook.Before(ctx, inv)
	}
	defer func() {
		out := Outcome{Result: cloneRaw(result), Err: err, Duration: time.Since(inv.StartedAt)}
		for _, hook := range hooks {
			hook.After(ctx, inv, out)
		}
	}()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if tool.Available != nil {
		if cause := tool.Available(ctx); cause != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, name, cause)
		}
	}
	result, err = tool.Invoke(ctx, cloneRaw(arguments))
	return result, err
}

func validateTool(tool Tool) error {
	name := tool.Declaration.Name
	if name == "" || strings.TrimSpace(name) != name || strings.Count(name, ".") < 1 {
		return fmt.Errorf("tool name %q must be qualified as server.tool", name)
	}
	if tool.Invoke == nil {
		return fmt.Errorf("tool %q has no handler", name)
	}
	if len(tool.Declaration.InputSchema) == 0 || !json.Valid(tool.Declaration.InputSchema) {
		return fmt.Errorf("tool %q has invalid input schema", name)
	}
	if len(tool.Declaration.OutputSchema) > 0 && !json.Valid(tool.Declaration.OutputSchema) {
		return fmt.Errorf("tool %q has invalid output schema", name)
	}
	return nil
}

func cloneDeclaration(d Declaration) Declaration {
	d.InputSchema = cloneRaw(d.InputSchema)
	d.OutputSchema = cloneRaw(d.OutputSchema)
	return d
}

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }
