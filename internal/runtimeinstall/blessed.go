package runtimeinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiguy110/tandem/internal/config"
)

// A tracked fork's git branch is shared by every Tandem instance that tracks
// it, but each instance's pin (commit, baseline release, open PRs) lives in its
// own config.yml. The blessed-fork manifest closes that gap: whoever publishes
// a rebase also commits BlessedManifestFile at the tip of the fork branch, so
// every other instance can read the branch tip, see what it is rebased onto,
// and adopt it instead of redoing the same rebase.

// BlessedManifestFile is the manifest's path at the root of the fork branch.
const BlessedManifestFile = "tandem-fork.json"

// BlessedManifestSchema is the manifest format version this daemon writes.
const BlessedManifestSchema = 1

// BlessedManifest describes the fork tree at the tip of the fork branch. It is
// the shareable part of config.ACPFork: the commit is the tip itself, and the
// repo, ref and clone are where each instance found it.
type BlessedManifest struct {
	Schema          int    `json:"schema"`
	UpstreamPackage string `json:"upstreamPackage"`
	UpstreamVersion string `json:"upstreamVersion"`
	UpstreamRepo    string `json:"upstreamRepo,omitempty"`
	Reason          string `json:"reason,omitempty"`
	UpstreamPRs     []int  `json:"upstreamPrs,omitempty"`
	PackageDir      string `json:"packageDir,omitempty"`
	BuildScript     string `json:"buildScript,omitempty"`
	// RetiredFor is set once the fork is no longer needed: the published version
	// range that carries its change. Instances seeing it return to upstream.
	RetiredFor string `json:"retiredFor,omitempty"`
}

// ManifestFor is the manifest describing fork as currently recorded.
func ManifestFor(fork config.ACPFork) BlessedManifest {
	return BlessedManifest{Schema: BlessedManifestSchema, UpstreamPackage: fork.UpstreamPackage,
		UpstreamVersion: fork.UpstreamVersion, UpstreamRepo: fork.UpstreamRepo, Reason: fork.Reason,
		UpstreamPRs: fork.UpstreamPRs, PackageDir: fork.PackageDir, BuildScript: fork.BuildScript}
}

// BlessedFork is what the fork branch currently publishes.
type BlessedFork struct {
	// Commit is the branch tip.
	Commit string `json:"commit"`
	// Manifest is nil when the tip carries no manifest, which is how every fork
	// branch looked before manifests existed. Such a tip is never adopted: with
	// no baseline recorded, there is no telling whether it is newer.
	Manifest *BlessedManifest `json:"manifest,omitempty"`
}

// Apply returns fork re-pinned to this blessed tree. Repo, ref, clone and the
// follow policy stay as this instance configured them.
func (b BlessedFork) Apply(fork config.ACPFork) config.ACPFork {
	m := b.Manifest
	fork.Commit = b.Commit
	fork.UpstreamPackage, fork.UpstreamVersion = m.UpstreamPackage, m.UpstreamVersion
	fork.UpstreamRepo, fork.Reason, fork.UpstreamPRs = m.UpstreamRepo, m.Reason, m.UpstreamPRs
	fork.PackageDir, fork.BuildScript = m.PackageDir, m.BuildScript
	return fork
}

// ShortCommit abbreviates the tip like config.ACPFork.ShortCommit.
func (b BlessedFork) ShortCommit() string {
	if len(b.Commit) <= 12 {
		return b.Commit
	}
	return b.Commit[:12]
}

// ReadBlessed fetches the fork branch tip and its manifest. It keeps a
// shallow, blob-filtered bare cache per agent so a poll transfers one commit's
// tree listing and the manifest blob, never the whole fork.
func ReadBlessed(ctx context.Context, cfg config.Config, agent string, fork config.ACPFork) (BlessedFork, error) {
	return readBlessed(ctx, filepath.Join(cfg.RuntimeRoot, "forks", ".blessed", agent+".git"), fork.Repo, fork.Ref)
}

func readBlessed(ctx context.Context, cache, repo, ref string) (BlessedFork, error) {
	if _, err := os.Stat(filepath.Join(cache, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(cache, 0o755); err != nil {
			return BlessedFork{}, err
		}
		if err := runGit(ctx, cache, io.Discard, "init", "--quiet", "--bare"); err != nil {
			return BlessedFork{}, err
		}
	} else if err != nil {
		return BlessedFork{}, err
	}
	// A named remote, not a bare URL: git records it as the partial clone's
	// promisor, which is what lets cat-file fetch the filtered-out manifest blob.
	if existing, err := gitOutput(ctx, cache, "remote", "get-url", "origin"); err != nil {
		if err := runGit(ctx, cache, io.Discard, "remote", "add", "origin", repo); err != nil {
			return BlessedFork{}, err
		}
	} else if strings.TrimSpace(existing) != repo {
		if err := runGit(ctx, cache, io.Discard, "remote", "set-url", "origin", repo); err != nil {
			return BlessedFork{}, err
		}
	}
	var out strings.Builder
	if err := runGit(ctx, cache, &out, "fetch", "--quiet", "--no-tags", "--depth", "1", "--filter=blob:none",
		"origin", "+refs/heads/"+ref+":refs/blessed/tip"); err != nil {
		return BlessedFork{}, fmt.Errorf("fetch fork branch %s of %s: %w: %s", ref, repo, err, strings.TrimSpace(out.String()))
	}
	tip, err := gitOutput(ctx, cache, "rev-parse", "refs/blessed/tip")
	if err != nil {
		return BlessedFork{}, fmt.Errorf("resolve fork branch tip: %w", err)
	}
	b := BlessedFork{Commit: strings.TrimSpace(tip)}
	if _, err := gitOutput(ctx, cache, "cat-file", "-e", "refs/blessed/tip:"+BlessedManifestFile); err != nil {
		// cat-file -e on a missing path fails without a lazy fetch, so a branch
		// without a manifest reads as "nothing blessed" rather than an error.
		return b, nil
	}
	raw, err := gitOutput(ctx, cache, "cat-file", "blob", "refs/blessed/tip:"+BlessedManifestFile)
	if err != nil {
		return BlessedFork{}, fmt.Errorf("read %s at %s: %w", BlessedManifestFile, b.ShortCommit(), err)
	}
	var m BlessedManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return BlessedFork{}, fmt.Errorf("parse %s at %s: %w", BlessedManifestFile, b.ShortCommit(), err)
	}
	if m.Schema > BlessedManifestSchema {
		return BlessedFork{}, fmt.Errorf("%s at %s uses schema %d; this Tandem understands up to %d", BlessedManifestFile, b.ShortCommit(), m.Schema, BlessedManifestSchema)
	}
	if m.RetiredFor == "" {
		// Validate the shared fields with a stand-in remote; the real one is
		// whatever this instance configured.
		probe := BlessedFork{Commit: b.Commit, Manifest: &m}.Apply(config.ACPFork{Repo: "https://blessed.invalid", Ref: ref})
		if err := probe.Validate(); err != nil {
			return BlessedFork{}, fmt.Errorf("%s at %s: %w", BlessedManifestFile, b.ShortCommit(), err)
		}
	}
	b.Manifest = &m
	return b, nil
}

// blessedOffer decides what the blessed tip means for this instance's record,
// returning the update kind to offer, or "" when there is nothing to offer.
func blessedOffer(fork config.ACPFork, b BlessedFork) string {
	if b.Manifest == nil || b.Commit == fork.Commit {
		return ""
	}
	if b.Manifest.RetiredFor != "" {
		return UpdateKindRetire
	}
	if b.Manifest.UpstreamPackage != fork.UpstreamPackage {
		// A package switch is a re-plan, not a rebase to follow blindly.
		return ""
	}
	if semverNewer(fork.UpstreamVersion, b.Manifest.UpstreamVersion) {
		// This instance is ahead of what was published (an unpushed local
		// rebase, or a stale branch); following would be a downgrade.
		return ""
	}
	return UpdateKindAdopt
}

// SyncResult reports what SyncBlessed did.
type SyncResult struct {
	// Kind is UpdateKindAdopt, UpdateKindRetire, or "" when already in sync.
	Kind    string
	Blessed BlessedFork
	Before  config.ACPFork
	After   config.ACPFork
	// Locked is the installed distribution after a retire.
	Locked LockedAgent
}

// SyncBlessed brings an agent's fork record in line with its fork branch:
// re-pinning to the published tip, or returning to upstream when the branch
// says the fork is retired. An adopted tree is built before the call returns,
// and a failed build restores the previous record, so a bad publish cannot
// strand new sessions.
func SyncBlessed(ctx context.Context, cfg config.Config, agent string, log io.Writer) (SyncResult, error) {
	fork, tracked, err := ForkFor(cfg, agent)
	if err != nil {
		return SyncResult{}, err
	}
	if !tracked {
		return SyncResult{}, fmt.Errorf("agent %s is not tracking a fork", agent)
	}
	b, err := ReadBlessed(ctx, cfg, agent, fork)
	if err != nil {
		return SyncResult{}, err
	}
	res := SyncResult{Blessed: b, Before: fork, After: fork}
	if b.Manifest == nil {
		return res, fmt.Errorf("fork branch %s of %s has no %s at %s; nothing is blessed to sync from", fork.Ref, fork.Repo, BlessedManifestFile, b.ShortCommit())
	}
	if b.Commit == fork.Commit {
		return res, nil
	}
	if b.Manifest.RetiredFor != "" {
		version, err := ResolveVersion(ctx, cfg, agent, b.Manifest.RetiredFor)
		if err != nil {
			return res, err
		}
		slog.Info("retiring ACP fork as its blessed branch directs", "agent", agent, "repo", fork.Repo, "tip", b.Commit, "retired_for", b.Manifest.RetiredFor, "version", version)
		locked, err := ReturnToUpstream(ctx, cfg, agent, version, log)
		if err != nil {
			return res, err
		}
		res.Kind, res.Locked = UpdateKindRetire, locked
		return res, nil
	}
	next := b.Apply(fork)
	if next.UpstreamPackage != fork.UpstreamPackage {
		return res, fmt.Errorf("blessed fork at %s switched package from %s to %s; re-track it with 'tandem acp fork' deliberately", b.ShortCommit(), fork.UpstreamPackage, next.UpstreamPackage)
	}
	if _, err := config.SaveACPFork(cfg.Home, agent, next); err != nil {
		return res, err
	}
	slog.Info("adopting blessed ACP fork", "agent", agent, "repo", fork.Repo, "ref", fork.Ref,
		"from_commit", fork.Commit, "to_commit", next.Commit, "from_version", fork.UpstreamVersion, "to_version", next.UpstreamVersion)
	if _, err := EnsureManaged(ctx, cfg, agent, log); err != nil {
		slog.Error("blessed ACP fork failed to build; restoring previous pin", "agent", agent, "commit", next.Commit, "error", err)
		if _, saveErr := config.SaveACPFork(cfg.Home, agent, fork); saveErr != nil {
			return res, fmt.Errorf("build blessed fork %s failed (%v) and restoring the previous pin also failed: %w", b.ShortCommit(), err, saveErr)
		}
		return res, fmt.Errorf("build blessed fork %s: %w (previous pin %s kept)", b.ShortCommit(), err, fork.ShortCommit())
	}
	res.Kind, res.After = UpdateKindAdopt, next
	return res, nil
}
