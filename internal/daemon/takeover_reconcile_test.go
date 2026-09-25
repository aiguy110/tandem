package daemon

import (
	"testing"

	"github.com/aiguy110/tandem/internal/store"
)

func TestTakeoverReconcilerResolvesOrphansFromLog(t *testing.T) {
	events := map[string][]store.StoredEvent{
		"a": {
			{Seq: 1, Kind: "takeover_request", Payload: `{"kind":"takeover_request","reqId":"tk_1","reason":"login"}`},
			{Seq: 2, Kind: "takeover_resolved", Payload: `{"kind":"takeover_resolved","reqId":"tk_1"}`},
			{Seq: 3, Kind: "takeover_request", Payload: `{"kind":"takeover_request","reqId":"tk_2","reason":"sso"}`},
		},
		"b": {
			{Seq: 1, Kind: "takeover_request", Payload: `{"kind":"takeover_request","reqId":"tk_live","reason":"mfa"}`},
		},
	}
	resolved := map[string]string{}
	abandoned := map[string]bool{}
	r := newTakeoverReconciler()
	r.events = func(id string, _ ...string) ([]store.StoredEvent, error) { return events[id], nil }
	r.live = func(reqID string) bool { return reqID == "tk_live" }
	r.abandon = func(reqID string) { abandoned[reqID] = true }
	r.resolve = func(id, reqID string) { resolved[reqID] = id }
	r.scan("a")
	r.scan("b")
	r.sweep()
	if len(resolved) != 1 || resolved["tk_2"] != "a" || !abandoned["tk_2"] {
		t.Fatalf("resolved = %v abandoned = %v, want only tk_2", resolved, abandoned)
	}
	// The live request stays tracked and is swept once its caller goes away.
	r.live = func(string) bool { return false }
	r.sweep()
	if resolved["tk_live"] != "b" {
		t.Fatalf("resolved = %v, want tk_live swept", resolved)
	}
	delete(resolved, "tk_live")
	r.sweep()
	if len(resolved) != 1 {
		t.Fatalf("sweep re-resolved requests: %v", resolved)
	}
}

func TestTakeoverReconcilerForgetsNormallyResolvedRequests(t *testing.T) {
	r := newTakeoverReconciler()
	r.live = func(string) bool { return false }
	r.abandon = func(string) {}
	r.resolve = func(_, reqID string) { t.Fatalf("resolved %s after it was forgotten", reqID) }
	r.track("a", "tk_1")
	r.forget("tk_1")
	r.sweep()
}
