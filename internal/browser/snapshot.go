package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// snapshotSeeder is the optional driver capability for seeding an agent's browser
// state from a captured snapshot ref (a directory for local, a profileId for
// steel). Both bundled drivers implement it.
type snapshotSeeder interface {
	SeedProfile(agentID, ref string)
}

// localProfileDriver is the optional driver capability for locating an agent's
// on-disk user-data-dir, used to copy out a snapshot. Only the local driver has
// a copyable profile directory.
type localProfileDriver interface {
	ProfileDir(agentID string) string
}

type steelProfileDriver interface {
	ProfileID(agentID string) string
}

// CaptureSnapshot records the agent's current browser state into a durable
// snapshot and returns (kind, ref) for persistence. For the local driver it
// deep-copies the live user-data-dir into destDir (kind "local", ref destDir).
// For Steel it records the current server-side profileId (kind "steel", ref
// profileId). The agent's browser must be provisioned.
func (b *Broker) CaptureSnapshot(_ context.Context, agentID, destDir string) (kind, ref string, err error) {
	if !b.driver.IsProvisioned(agentID) {
		return "", "", errors.New("no live browser to capture for this agent")
	}
	switch d := b.driver.(type) {
	case localProfileDriver:
		src := d.ProfileDir(agentID)
		if err := os.MkdirAll(destDir, 0o700); err != nil {
			return "", "", err
		}
		if err := copyTree(src, destDir); err != nil {
			return "", "", fmt.Errorf("copy browser profile: %w", err)
		}
		return "local", destDir, nil
	case steelProfileDriver:
		id := d.ProfileID(agentID)
		if id == "" {
			return "", "", errors.New("steel session has no persisted profile to capture")
		}
		return "steel", id, nil
	default:
		return "", "", errors.New("browser driver does not support snapshot capture")
	}
}

// SeedSnapshot arranges for the agent's next browser provision to start from a
// captured snapshot. kind must match the active driver ("local" ref is a
// directory, "steel" ref is a profileId); a mismatch or empty ref is a no-op
// (fresh state). Must be called before the agent's browser is provisioned.
func (b *Broker) SeedSnapshot(agentID, kind, ref string) {
	seeder, ok := b.driver.(snapshotSeeder)
	if !ok || ref == "" {
		return
	}
	if kind != "" && kind != b.driver.Kind() {
		return
	}
	seeder.SeedProfile(agentID, ref)
}

// copyTree recursively copies regular files and directories from src into dst,
// skipping symlinks/sockets/pipes and Chromium's singleton lock files (which are
// process-specific and would block a fresh launch if copied).
func copyTree(src, dst string) error {
	skip := map[string]bool{"SingletonLock": true, "SingletonSocket": true, "SingletonCookie": true, "lockfile": true}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o700)
		}
		if skip[info.Name()] {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !info.Mode().IsRegular() {
			return nil // skip sockets, pipes, symlinks — not restorable state
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		// A file that vanished mid-copy (browser is live) is non-fatal.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
