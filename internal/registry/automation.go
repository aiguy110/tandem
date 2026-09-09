package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

// SpawnProfile starts a fresh worktree agent from a durable profile. profileRef
// accepts an exact id or an unambiguous case-insensitive display name. This is
// the daemon-side equivalent of applying a saved profile in SpawnPalette and is
// used by automation wakeups and failure recovery.
func (r *Registry) SpawnProfile(ctx context.Context, profileRef, repo, task string) (*session.Session, error) {
	return r.spawnProfile(ctx, profileRef, repo, task, "", "")
}

// SpawnProfileSeeded is SpawnProfile with a one-run browser-state seed. The
// browser is provisioned before the initial prompt so a temporary seed may be
// removed as soon as this method returns without racing the awakened agent.
func (r *Registry) SpawnProfileSeeded(ctx context.Context, profileRef, repo, task, browserKind, browserRef string) (*session.Session, error) {
	return r.spawnProfile(ctx, profileRef, repo, task, browserKind, browserRef)
}

func (r *Registry) spawnProfile(ctx context.Context, profileRef, repo, task, browserKind, browserRef string) (*session.Session, error) {
	profile, err := r.resolveProfileRef(profileRef)
	if err != nil {
		return nil, err
	}
	config, err := r.profileSessionConfig(ctx, *profile, repo)
	if err != nil {
		return nil, err
	}
	spec := agentadapter.Spec{
		Adapter:       "acp",
		Agent:         profile.Agent,
		Harness:       profile.Harness,
		Workspace:     workspace.Workspace{Kind: workspace.KindWorktree, Repo: repo},
		SessionConfig: config,
		Profile: &agentadapter.ProfileSpec{
			ID: profile.ID, Model: profile.Model, Effort: profile.Effort,
			Permission: profile.Permission, Snapshot: profile.SnapshotID,
		},
	}
	agent, err := r.Spawn(ctx, spec)
	if err != nil {
		return nil, err
	}
	if browserRef != "" {
		if r.browser == nil {
			return nil, fmt.Errorf("browser subsystem is disabled")
		}
		r.browser.SeedSnapshot(agent.ID, browserKind, browserRef)
		if err := r.browser.EnsureProvisioned(ctx, agent.ID); err != nil {
			_, _ = r.Close(context.Background(), agent.ID, true, true, false)
			return nil, fmt.Errorf("seed wake agent browser: %w", err)
		}
	}
	if task != "" {
		go agent.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: task}})
	}
	return agent, nil
}

func (r *Registry) resolveProfileRef(ref string) (*store.Profile, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("agent profile is required")
	}
	profiles, err := r.store.ListProfiles()
	if err != nil {
		return nil, err
	}
	for i := range profiles {
		if profiles[i].ID == ref {
			return &profiles[i], nil
		}
	}
	var match *store.Profile
	for i := range profiles {
		if strings.EqualFold(profiles[i].Name, ref) {
			if match != nil {
				return nil, fmt.Errorf("agent profile name %q is ambiguous", ref)
			}
			match = &profiles[i]
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no such agent profile: %s", ref)
	}
	return match, nil
}

func (r *Registry) profileSessionConfig(ctx context.Context, profile store.Profile, cwd string) (json.RawMessage, error) {
	options, err := r.SpawnOptions(ctx, profile.Agent, profile.Harness, nil, cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve profile %q options: %w", profile.Name, err)
	}
	saved := persistedSessionConfig{ConfigOptions: map[string]any{}}
	if profile.Permission != "" {
		var modes struct {
			Available []struct {
				ID string `json:"id"`
			} `json:"availableModes"`
		}
		if len(options.Modes) > 0 && string(options.Modes) != "null" {
			if err := json.Unmarshal(options.Modes, &modes); err != nil {
				return nil, fmt.Errorf("decode profile permission options: %w", err)
			}
		}
		for _, mode := range modes.Available {
			if mode.ID == profile.Permission {
				saved.ModeID = profile.Permission
				break
			}
		}
		if saved.ModeID == "" {
			return nil, fmt.Errorf("profile %q permission mode %q is no longer available", profile.Name, profile.Permission)
		}
	}
	for _, raw := range options.ConfigOptions {
		var option struct {
			ID       string `json:"id"`
			Category string `json:"category"`
			Type     string `json:"type"`
			Options  []struct {
				Value string `json:"value"`
			} `json:"options"`
		}
		if json.Unmarshal(raw, &option) != nil || option.Type != "select" {
			continue
		}
		value := ""
		switch option.Category {
		case "model":
			value = profile.Model
		case "thought_level":
			value = profile.Effort
		}
		if value == "" {
			continue
		}
		available := false
		for _, candidate := range option.Options {
			if candidate.Value == value {
				available = true
				break
			}
		}
		if !available {
			return nil, fmt.Errorf("profile %q option %q is no longer available", profile.Name, value)
		}
		saved.ConfigOptions[option.ID] = value
	}
	if len(saved.ConfigOptions) == 0 {
		saved.ConfigOptions = nil
	}
	return json.Marshal(saved)
}
