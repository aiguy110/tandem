package registry

import (
	"path/filepath"
	"testing"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Config{Agents: map[string]config.Agent{"claude": {Name: "Claude"}}}
	return &Registry{store: db, config: cfg}
}

func TestApplyProfileResolvesCreatesDedupsAndRecords(t *testing.T) {
	r := testRegistry(t)
	spec := agentadapter.Spec{Agent: "claude", Profile: &agentadapter.ProfileSpec{Model: "sonnet", Effort: "high", Permission: "acceptEdits"}}
	r.applyProfile("agent-1", "/repo", &spec)
	if spec.Profile.ID == "" {
		t.Fatal("expected a resolved profile id")
	}
	profiles, recent, err := r.ListProfiles("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(profiles))
	}
	if got, want := profiles[0].Name, "Claude · sonnet · high · acceptEdits · Fresh"; got != want {
		t.Fatalf("auto-name = %q, want %q", got, want)
	}
	if !profiles[0].AutoNamed {
		t.Fatal("expected AutoNamed profile")
	}
	if len(recent) != 1 || recent[0] != spec.Profile.ID {
		t.Fatalf("recency = %v, want [%s]", recent, spec.Profile.ID)
	}

	// Identical settings reuse the same profile (dedup).
	spec2 := agentadapter.Spec{Agent: "claude", Profile: &agentadapter.ProfileSpec{Model: "sonnet", Effort: "high", Permission: "acceptEdits"}}
	r.applyProfile("agent-2", "/repo", &spec2)
	if spec2.Profile.ID != spec.Profile.ID {
		t.Fatalf("expected dedup to same id, got %s vs %s", spec2.Profile.ID, spec.Profile.ID)
	}
	profiles, _, _ = r.ListProfiles("/repo")
	if len(profiles) != 1 {
		t.Fatalf("dedup should keep 1 profile, got %d", len(profiles))
	}

	// Different settings create a new profile.
	spec3 := agentadapter.Spec{Agent: "claude", Profile: &agentadapter.ProfileSpec{Model: "opus"}}
	r.applyProfile("agent-3", "/repo", &spec3)
	profiles, _, _ = r.ListProfiles("/repo")
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}
}

func TestApplyProfileSnapshotNaming(t *testing.T) {
	r := testRegistry(t)
	if err := r.store.SaveBrowserSnapshot(store.BrowserSnapshot{ID: "snap-1", Name: "Logged in", Kind: "local", Ref: "/data/snap-1"}); err != nil {
		t.Fatal(err)
	}
	// No browser broker wired, but a saved snapshot still contributes to identity
	// and naming.
	spec := agentadapter.Spec{Agent: "claude", Profile: &agentadapter.ProfileSpec{Snapshot: "snap-1"}}
	r.applyProfile("agent-1", "/repo", &spec)
	profiles, _, _ := r.ListProfiles("/repo")
	if len(profiles) != 1 || profiles[0].Name != "Claude · Logged in" {
		t.Fatalf("snapshot profile = %+v", profiles)
	}
	if profiles[0].SnapshotID != "snap-1" {
		t.Fatalf("snapshotId = %q", profiles[0].SnapshotID)
	}
}
