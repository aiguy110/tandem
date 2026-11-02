package store

import "testing"

func TestPromptReceiptSurvivesRestart(t *testing.T) {
	s, path := openTestStore(t)
	if claimed, _, err := s.ClaimPromptRequest("a", "id", "hash"); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if claimed, _, err := s.ClaimPromptRequest("a", "id", "hash"); err == nil || claimed {
		t.Fatal("incomplete request must not execute again")
	}
	if err := s.CompletePromptRequest("a", "id", `{"t":"ack"}`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if claimed, receipt, err := reopened.ClaimPromptRequest("a", "id", "hash"); err != nil || claimed || receipt != `{"t":"ack"}` {
		t.Fatalf("replay: %v %q %v", claimed, receipt, err)
	}
	if _, _, err := reopened.ClaimPromptRequest("a", "id", "different"); err == nil {
		t.Fatal("content mismatch accepted")
	}
}
