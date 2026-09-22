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

func TestApplyProfileCustomizeNeverMutatesSource(t *testing.T) {
	r := testRegistry(t)
	spawn := func(sessionID string, p agentadapter.ProfileSpec) string {
		spec := agentadapter.Spec{Agent: "claude", Profile: &p}
		r.applyProfile(sessionID, "/repo", &spec)
		return spec.Profile.ID
	}
	auto := spawn("a1", agentadapter.ProfileSpec{Model: "sonnet"})
	if err := r.RenameProfile(auto, "Everyday"); err != nil {
		t.Fatal(err)
	}
	// Picking the named profile by id reuses it.
	if got := spawn("a2", agentadapter.ProfileSpec{ID: auto, Model: "sonnet"}); got != auto {
		t.Fatalf("picked named profile resolved to %s, want %s", got, auto)
	}
	// Customizing it without a name yields a new auto-named profile, even when
	// the settings are unchanged, and leaves the named one intact.
	unnamed := spawn("a3", agentadapter.ProfileSpec{Model: "sonnet"})
	if unnamed == auto {
		t.Fatal("unnamed spawn reused a user-named profile")
	}
	changed := spawn("a4", agentadapter.ProfileSpec{ID: auto, Model: "opus"})
	if changed == auto || changed == unnamed {
		t.Fatalf("customized settings resolved to existing profile %s", changed)
	}
	// An explicit name creates a separately named profile, then reuses it.
	named := spawn("a5", agentadapter.ProfileSpec{Model: "opus", Name: "Deep"})
	if named == changed {
		t.Fatal("explicitly named spawn reused the auto-named profile")
	}
	if again := spawn("a6", agentadapter.ProfileSpec{Model: "opus", Name: "Deep"}); again != named {
		t.Fatalf("named re-spawn resolved to %s, want %s", again, named)
	}
	source, _ := r.store.Profile(auto)
	if source == nil || source.Name != "Everyday" || source.Model != "sonnet" {
		t.Fatalf("source profile mutated: %+v", source)
	}
	created, _ := r.store.Profile(named)
	if created == nil || created.Name != "Deep" || created.AutoNamed {
		t.Fatalf("named profile = %+v", created)
	}
}

func TestForgetProfileDropsOnlyThatProjectsRecency(t *testing.T) {
	r := testRegistry(t)
	spec := agentadapter.Spec{Agent: "claude", Profile: &agentadapter.ProfileSpec{Model: "sonnet"}}
	r.applyProfile("a1", "/repo-a", &spec)
	id := spec.Profile.ID
	r.applyProfile("a2", "/repo-b", &agentadapter.Spec{Agent: "claude", Profile: &agentadapter.ProfileSpec{Model: "sonnet"}})
	if err := r.ForgetProfile(id, "/repo-a"); err != nil {
		t.Fatal(err)
	}
	profiles, recentA, _ := r.ListProfiles("/repo-a")
	if len(recentA) != 0 || len(profiles) != 1 {
		t.Fatalf("after forget: profiles=%d recentA=%v", len(profiles), recentA)
	}
	if _, recentB, _ := r.ListProfiles("/repo-b"); len(recentB) != 1 || recentB[0] != id {
		t.Fatalf("repo-b recency = %v", recentB)
	}
	if err := r.ForgetProfile(id, ""); err == nil {
		t.Fatal("expected an error without a project")
	}
}
