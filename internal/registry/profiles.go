package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/store"
)

// applyProfile resolves-or-creates the daemon-owned Profile described by the
// spawn spec, records its per-project recency, and arranges for the agent's
// browser to be seeded from the chosen snapshot. It mutates spec.Profile.ID with
// the resolved profile id so it persists on the agent record. A nil spec.Profile
// (older UIs) is a no-op.
func (r *Registry) applyProfile(agentID, project string, spec *agentadapter.Spec) {
	p := spec.Profile
	if p == nil {
		return
	}
	// Seed the browser before it is lazily provisioned on first use.
	if p.Snapshot != "" && r.browser != nil {
		if snap, err := r.store.BrowserSnapshot(p.Snapshot); err == nil && snap != nil {
			r.browser.SeedSnapshot(agentID, snap.Kind, snap.Ref)
		} else {
			p.Snapshot = "" // snapshot deleted meanwhile → fall back to fresh
		}
	}
	existing, err := r.store.FindProfileByTuple(spec.Agent, spec.Harness, p.Model, p.Effort, p.Permission, p.Snapshot)
	if err != nil {
		return
	}
	id := ""
	if existing != nil {
		id = existing.ID
	} else {
		id = "prof-" + randHex(8)
		_ = r.store.UpsertProfile(store.Profile{
			ID: id, Name: r.profileAutoName(spec), AutoNamed: true,
			Agent: spec.Agent, Harness: spec.Harness,
			Model: p.Model, Effort: p.Effort, Permission: p.Permission, SnapshotID: p.Snapshot,
		})
	}
	p.ID = id
	_ = r.store.TouchProfile(id, project)
}

// profileAutoName builds the default concatenated name for a new profile, e.g.
// "Claude · sonnet · high · acceptEdits · Fresh".
func (r *Registry) profileAutoName(spec *agentadapter.Spec) string {
	parts := []string{r.launchLabel(spec.Agent, spec.Harness)}
	for _, v := range []string{spec.Profile.Model, spec.Profile.Effort, spec.Profile.Permission} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	snapLabel := "Fresh"
	if spec.Profile.Snapshot != "" {
		if snap, err := r.store.BrowserSnapshot(spec.Profile.Snapshot); err == nil && snap != nil {
			snapLabel = snap.Name
		}
	}
	parts = append(parts, snapLabel)
	return strings.Join(parts, " · ")
}

func (r *Registry) launchLabel(agent, harness string) string {
	if harness != "" {
		if h, ok := r.config.Harnesses[harness]; ok && h.Name != "" {
			return h.Name
		}
	}
	if a, ok := r.config.Agents[agent]; ok && a.Name != "" {
		return a.Name
	}
	if agent != "" {
		return agent
	}
	return "Agent"
}

// CaptureSnapshot copies the agent's current browser state into a new, named
// snapshot and records it.
func (r *Registry) CaptureSnapshot(ctx context.Context, agentID, name string) (store.BrowserSnapshot, error) {
	if r.browser == nil {
		return store.BrowserSnapshot{}, errors.New("browser is disabled")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Snapshot"
	}
	id := "snap-" + randHex(8)
	destDir := filepath.Join(r.config.Browser.SnapshotRoot, id)
	kind, ref, err := r.browser.CaptureSnapshot(ctx, agentID, destDir)
	if err != nil {
		_ = os.RemoveAll(destDir)
		return store.BrowserSnapshot{}, err
	}
	snap := store.BrowserSnapshot{ID: id, Name: name, Kind: kind, Ref: ref}
	if err := r.store.SaveBrowserSnapshot(snap); err != nil {
		_ = os.RemoveAll(destDir)
		return store.BrowserSnapshot{}, err
	}
	return *mustSnapshot(r.store, id, snap), nil
}

func mustSnapshot(s *store.Store, id string, fallback store.BrowserSnapshot) *store.BrowserSnapshot {
	if got, err := s.BrowserSnapshot(id); err == nil && got != nil {
		return got
	}
	return &fallback
}

// ListSnapshots returns all captured browser snapshots, newest first.
func (r *Registry) ListSnapshots() ([]store.BrowserSnapshot, error) {
	return r.store.ListBrowserSnapshots()
}

// DeleteSnapshot forgets a snapshot and removes its on-disk data (local kind).
func (r *Registry) DeleteSnapshot(id string) error {
	snap, err := r.store.BrowserSnapshot(id)
	if err != nil {
		return err
	}
	if err := r.store.DeleteBrowserSnapshot(id); err != nil {
		return err
	}
	if snap != nil && snap.Kind == "local" && snap.Ref != "" {
		_ = os.RemoveAll(snap.Ref)
	}
	return nil
}

// RestartBrowser starts a new browser session for an existing agent, optionally
// seeded from a saved snapshot. An empty snapshotID requests fresh state.
func (r *Registry) RestartBrowser(ctx context.Context, agentID, snapshotID string) error {
	if r.Get(agentID) == nil {
		return fmt.Errorf("no such agent: %s", agentID)
	}
	if r.browser == nil {
		return errors.New("browser subsystem disabled")
	}
	var kind, ref string
	if snapshotID != "" {
		snap, err := r.store.BrowserSnapshot(snapshotID)
		if err != nil {
			return err
		}
		if snap == nil {
			return fmt.Errorf("no such browser snapshot: %s", snapshotID)
		}
		if snap.Kind != "" && snap.Kind != r.browser.DriverKind() {
			return fmt.Errorf("browser snapshot %q is for the %s driver, not %s", snap.Name, snap.Kind, r.browser.DriverKind())
		}
		kind, ref = snap.Kind, snap.Ref
	}
	return r.browser.Restart(ctx, agentID, kind, ref)
}

// ListProfiles returns all profiles and, for the given project, the profile ids
// ordered by that project's most-recent use.
func (r *Registry) ListProfiles(project string) ([]store.Profile, []string, error) {
	profiles, err := r.store.ListProfiles()
	if err != nil {
		return nil, nil, err
	}
	recent, err := r.store.ProfileRecency(project)
	if err != nil {
		return nil, nil, err
	}
	return profiles, recent, nil
}

// RenameProfile gives a profile a user-chosen name (clearing auto-named).
func (r *Registry) RenameProfile(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("profile name is required")
	}
	return r.store.RenameProfile(id, name)
}

// DeleteProfile removes a profile and its recency records.
func (r *Registry) DeleteProfile(id string) error { return r.store.DeleteProfile(id) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	return hex.EncodeToString(b)
}
