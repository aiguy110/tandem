package store

import (
	"reflect"
	"testing"
)

func TestBrowserSnapshotCRUD(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.SaveBrowserSnapshot(BrowserSnapshot{ID: "snap-1", Name: "Logged in", Kind: "local", Ref: "/data/snap-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BrowserSnapshot("snap-1")
	if err != nil || got == nil {
		t.Fatalf("BrowserSnapshot = %v, %v", got, err)
	}
	if got.Name != "Logged in" || got.Kind != "local" || got.Ref != "/data/snap-1" || got.CreatedAt == 0 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
	// Rename via re-save keeps the id, updates the name.
	if err := s.SaveBrowserSnapshot(BrowserSnapshot{ID: "snap-1", Name: "Renamed", Kind: "local", Ref: "/data/snap-1"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListBrowserSnapshots()
	if err != nil || len(list) != 1 || list[0].Name != "Renamed" {
		t.Fatalf("ListBrowserSnapshots = %+v, %v", list, err)
	}
	if err := s.DeleteBrowserSnapshot("snap-1"); err != nil {
		t.Fatal(err)
	}
	missing, err := s.BrowserSnapshot("snap-1")
	if err != nil || missing != nil {
		t.Fatalf("expected nil after delete, got %+v, %v", missing, err)
	}
}

func TestProfileResolveRenameRecency(t *testing.T) {
	s, _ := openTestStore(t)
	// No match yet.
	found, err := s.FindProfileByTuple("claude", "", "sonnet", "high", "acceptEdits", "")
	if err != nil || found != nil {
		t.Fatalf("expected no match, got %+v, %v", found, err)
	}
	p := Profile{ID: "prof-1", Name: "claude · sonnet · high · acceptEdits · fresh", AutoNamed: true,
		Agent: "claude", Model: "sonnet", Effort: "high", Permission: "acceptEdits"}
	if err := s.UpsertProfile(p); err != nil {
		t.Fatal(err)
	}
	found, err = s.FindProfileByTuple("claude", "", "sonnet", "high", "acceptEdits", "")
	if err != nil || found == nil || found.ID != "prof-1" || !found.AutoNamed {
		t.Fatalf("FindProfileByTuple = %+v, %v", found, err)
	}
	// A different tuple must not match.
	if other, _ := s.FindProfileByTuple("claude", "", "opus", "high", "acceptEdits", ""); other != nil {
		t.Fatalf("unexpected match for different tuple: %+v", other)
	}
	// Rename clears autoNamed.
	if err := s.RenameProfile("prof-1", "My Claude"); err != nil {
		t.Fatal(err)
	}
	renamed, _ := s.FindProfileByTuple("claude", "", "sonnet", "high", "acceptEdits", "")
	if renamed.Name != "My Claude" || renamed.AutoNamed {
		t.Fatalf("rename not applied: %+v", renamed)
	}
	// Recency per project.
	if err := s.UpsertProfile(Profile{ID: "prof-2", Name: "b", Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchProfile("prof-1", "/repo/a"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchProfile("prof-2", "/repo/a"); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchProfile("prof-1", "/repo/a"); err != nil {
		t.Fatal(err)
	}
	recent, err := s.ProfileRecency("/repo/a")
	if err != nil || !reflect.DeepEqual(recent, []string{"prof-1", "prof-2"}) {
		t.Fatalf("ProfileRecency = %v, %v", recent, err)
	}
	// A different project has no recency.
	if other, _ := s.ProfileRecency("/repo/b"); len(other) != 0 {
		t.Fatalf("expected empty recency, got %v", other)
	}
	// Delete removes recency too.
	if err := s.DeleteProfile("prof-1"); err != nil {
		t.Fatal(err)
	}
	recent, _ = s.ProfileRecency("/repo/a")
	if !reflect.DeepEqual(recent, []string{"prof-2"}) {
		t.Fatalf("recency after delete = %v", recent)
	}
}
