package toolregistry

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

type auditRecorder struct {
	mu     sync.Mutex
	before []Invocation
	after  []Outcome
}

func (a *auditRecorder) Before(_ context.Context, invocation Invocation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.before = append(a.before, invocation)
}

func (a *auditRecorder) After(_ context.Context, _ Invocation, outcome Outcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.after = append(a.after, outcome)
}

func TestRegistryDiscoveryInvocationAvailabilityAndAudit(t *testing.T) {
	audit := &auditRecorder{}
	r := New(audit)
	unavailable := errors.New("browser is stopped")
	if err := r.Register(Tool{
		Declaration: Declaration{Name: "playwright.snapshot", Description: "snapshot", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Invoke: func(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"arguments":` + string(arguments) + `}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Tool{
		Declaration: Declaration{Name: "playwright.click", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Available:   func(context.Context) error { return unavailable },
		Invoke:      func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil },
	}); err != nil {
		t.Fatal(err)
	}

	statuses := r.List(t.Context())
	if len(statuses) != 2 || statuses[0].Declaration.Name != "playwright.click" || statuses[0].Available || !statuses[1].Available {
		t.Fatalf("unexpected statuses: %#v", statuses)
	}
	result, err := r.Invoke(t.Context(), "playwright.snapshot", json.RawMessage(`{"fullPage":true}`))
	if err != nil || string(result) != `{"arguments":{"fullPage":true}}` {
		t.Fatalf("Invoke() = %s, %v", result, err)
	}
	if _, err := r.Invoke(t.Context(), "playwright.click", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable Invoke error = %v", err)
	}
	if _, err := r.Invoke(t.Context(), "missing.tool", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Invoke error = %v", err)
	}
	if len(audit.before) != 3 || len(audit.after) != 3 || audit.after[0].Duration < 0 {
		t.Fatalf("audit calls before=%d after=%d", len(audit.before), len(audit.after))
	}
}

func TestRegistryRejectsInvalidAndDuplicateTools(t *testing.T) {
	r := New()
	for _, tool := range []Tool{
		{Declaration: Declaration{Name: "unqualified", InputSchema: json.RawMessage(`{}`)}, Invoke: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }},
		{Declaration: Declaration{Name: "server.tool", InputSchema: json.RawMessage(`no`)}, Invoke: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }},
		{Declaration: Declaration{Name: "server.tool", InputSchema: json.RawMessage(`{}`)}},
	} {
		if err := r.Register(tool); err == nil {
			t.Fatalf("Register(%#v) unexpectedly succeeded", tool)
		}
	}
	tool := Tool{Declaration: Declaration{Name: "server.tool", InputSchema: json.RawMessage(`{}`)}, Invoke: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(tool); err == nil {
		t.Fatal("duplicate Register unexpectedly succeeded")
	}
}
