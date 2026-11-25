package browser

import (
	"sync"
	"testing"
	"time"
)

func TestBrokerAgentWaitingOnlyWhileUserHoldsControl(t *testing.T) {
	b := NewBroker(nil, BrokerConfig{})
	var mu sync.Mutex
	var states []BrowserState
	off := b.OnState("s", func(state BrowserState) {
		mu.Lock()
		states = append(states, state)
		mu.Unlock()
	})
	defer off()
	last := func() BrowserState {
		mu.Lock()
		defer mu.Unlock()
		return states[len(states)-1]
	}

	b.NoteAgentWaiting("s")
	if b.State("s").AgentWaiting {
		t.Fatal("agent-owned browser reported the agent waiting")
	}
	b.Grab("s")
	b.NoteAgentWaiting("s")
	if !last().AgentWaiting || !b.State("s").AgentWaiting {
		t.Fatalf("user-held browser with a blocked call: %+v", last())
	}
	if err := b.Release("s"); err != nil {
		t.Fatal(err)
	}
	if got := last(); got.AgentWaiting || got.ControlOwner != ControlAgent {
		t.Fatalf("state after release = %+v", got)
	}
	// A re-grab must not resurrect the earlier report.
	b.Grab("s")
	if b.State("s").AgentWaiting {
		t.Fatal("stale waiting report survived release")
	}
}

func TestBrokerAgentWaitingLapsesWithoutReports(t *testing.T) {
	b := NewBroker(nil, BrokerConfig{})
	cleared := make(chan struct{}, 1)
	b.Grab("s")
	b.NoteAgentWaiting("s")
	off := b.OnState("s", func(state BrowserState) {
		if !state.AgentWaiting {
			select {
			case cleared <- struct{}{}:
			default:
			}
		}
	})
	defer off()
	select {
	case <-cleared:
		t.Fatal("initial state should still report waiting")
	default:
	}
	select {
	case <-cleared:
	case <-time.After(agentWaitingTTL + 2*time.Second):
		t.Fatal("waiting flag never lapsed")
	}
}
