package daemon

import (
	"testing"

	"github.com/aiguy110/tandem/internal/store"
)

func TestReconcileOrphanedSecrets(t *testing.T) {
	events := []store.StoredEvent{
		{Kind: "secret_request", Payload: `{"requestId":"old"}`},
		{Kind: "secret_request", Payload: `{"requestId":"done"}`},
		{Kind: "secret_resolved", Payload: `{"requestId":"done"}`},
	}
	var resolved []string
	reconcileOrphanedSecrets("s1", func(string, ...string) ([]store.StoredEvent, error) { return events, nil }, func(sessionID, requestID string) { resolved = append(resolved, sessionID+":"+requestID) })
	if len(resolved) != 1 || resolved[0] != "s1:old" {
		t.Fatalf("resolved = %#v", resolved)
	}
}
