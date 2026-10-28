package runtimeinstall

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

// blessedOrigin creates a bare fork remote whose `tandem` branch tip carries
// manifest (or no manifest when nil), returning a file:// URL and the tip.
// Partial-clone filtering is enabled on the remote so ReadBlessed's lazy blob
// fetch is exercised the way a GitHub remote would serve it.
func blessedOrigin(t *testing.T, manifest *BlessedManifest) (string, string) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	origin := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=tandem", origin},
		{"-C", origin, "config", "uploadpack.allowFilter", "true"},
		{"-C", origin, "config", "uploadpack.allowAnySHA1InWant", "true"},
	} {
		if err := runGit(ctx, t.TempDir(), os.Stderr, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed := t.TempDir()
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if manifest != nil {
		b, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(seed, BlessedManifestFile), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=tandem"},
		{"add", "."},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--quiet", "-m", "seed"},
		{"push", "--quiet", "file://" + origin, "tandem"},
	} {
		if err := runGit(ctx, seed, os.Stderr, args...); err != nil {
			t.Fatalf("seed %v: %v", args, err)
		}
	}
	tip, err := gitOutput(ctx, seed, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + origin, strings.TrimSpace(tip)
}

func TestReadBlessedReadsManifestAtBranchTip(t *testing.T) {
	want := BlessedManifest{Schema: 1, UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.34", Reason: "Emits usage_update", UpstreamPRs: []int{114}}
	repo, tip := blessedOrigin(t, &want)
	cache := filepath.Join(t.TempDir(), "pi.git")

	b, err := readBlessed(context.Background(), cache, repo, "tandem")
	if err != nil {
		t.Fatal(err)
	}
	if b.Commit != tip || b.Manifest == nil || b.Manifest.UpstreamVersion != "0.0.34" || b.Manifest.UpstreamPRs[0] != 114 {
		t.Fatalf("blessed = %+v manifest=%+v, want tip %s", b, b.Manifest, tip)
	}
	// The cache is reused across polls.
	if again, err := readBlessed(context.Background(), cache, repo, "tandem"); err != nil || again.Commit != tip {
		t.Fatalf("second read = %+v err=%v", again, err)
	}
}

func TestReadBlessedWithoutManifestIsNotAnError(t *testing.T) {
	repo, tip := blessedOrigin(t, nil)
	b, err := readBlessed(context.Background(), filepath.Join(t.TempDir(), "pi.git"), repo, "tandem")
	if err != nil {
		t.Fatal(err)
	}
	if b.Commit != tip || b.Manifest != nil {
		t.Fatalf("blessed = %+v", b)
	}
}

func TestReadBlessedRejectsInvalidManifest(t *testing.T) {
	repo, _ := blessedOrigin(t, &BlessedManifest{Schema: 1, UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.34", PackageDir: "../escape"})
	if _, err := readBlessed(context.Background(), filepath.Join(t.TempDir(), "pi.git"), repo, "tandem"); err == nil {
		t.Fatal("expected an invalid packageDir to be rejected")
	}
}

func TestBlessedOffer(t *testing.T) {
	local := config.ACPFork{Repo: "https://github.com/me/pi-acp", Ref: "tandem", Commit: "aaaaaaa", UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.33"}
	manifest := func(version string) *BlessedManifest {
		return &BlessedManifest{Schema: 1, UpstreamPackage: "pi-acp", UpstreamVersion: version}
	}
	cases := []struct {
		name string
		b    BlessedFork
		want string
	}{
		{"no manifest", BlessedFork{Commit: "bbbbbbb"}, ""},
		{"already pinned", BlessedFork{Commit: "aaaaaaa", Manifest: manifest("0.0.34")}, ""},
		{"newer release", BlessedFork{Commit: "bbbbbbb", Manifest: manifest("0.0.34")}, UpdateKindAdopt},
		// A new carried change on the same baseline is still worth following.
		{"same release, new tree", BlessedFork{Commit: "bbbbbbb", Manifest: manifest("0.0.33")}, UpdateKindAdopt},
		{"branch is behind this instance", BlessedFork{Commit: "bbbbbbb", Manifest: manifest("0.0.32")}, ""},
		{"package switched", BlessedFork{Commit: "bbbbbbb", Manifest: &BlessedManifest{UpstreamPackage: "other", UpstreamVersion: "1.0.0"}}, ""},
		{"retired", BlessedFork{Commit: "bbbbbbb", Manifest: &BlessedManifest{UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.33", RetiredFor: "^0.0.35"}}, UpdateKindRetire},
	}
	for _, tc := range cases {
		if got := blessedOffer(local, tc.b); got != tc.want {
			t.Errorf("%s: offer = %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestBlessedApplyKeepsLocalPlacement(t *testing.T) {
	local := config.ACPFork{Repo: "https://github.com/me/pi-acp", Ref: "tandem", Commit: "aaaaaaa", UpstreamPackage: "pi-acp",
		UpstreamVersion: "0.0.33", Clone: "/work/pi", Follow: config.ForkFollowAuto, UpstreamPRs: []int{1, 2}}
	b := BlessedFork{Commit: "bbbbbbb", Manifest: &BlessedManifest{UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.34", UpstreamPRs: []int{2}, BuildScript: "none"}}
	got := b.Apply(local)
	if got.Commit != "bbbbbbb" || got.UpstreamVersion != "0.0.34" || len(got.UpstreamPRs) != 1 || got.BuildScript != "none" {
		t.Fatalf("applied = %+v", got)
	}
	if got.Repo != local.Repo || got.Ref != local.Ref || got.Clone != local.Clone || got.Follow != local.Follow {
		t.Fatalf("instance-local fields changed: %+v", got)
	}
	// The manifest round-trips the shared fields.
	if m := ManifestFor(got); m.UpstreamVersion != "0.0.34" || m.BuildScript != "none" || m.Schema != BlessedManifestSchema {
		t.Fatalf("manifest = %+v", m)
	}
}
