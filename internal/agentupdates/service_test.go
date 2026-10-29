package agentupdates

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
)

func TestServiceOffersAndInstallsUpdate(t *testing.T) {
	center := notifications.New()
	installed := ""
	s := NewService(Options{Center: center, Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
		return []runtimeinstall.UpdateInfo{{Agent: "codex", CurrentVersion: "1.8.0", LatestVersion: "1.9.0"}}, nil
	}, Install: func(_ context.Context, _ config.Config, agent, version string, _ io.Writer) (runtimeinstall.LockedAgent, error) {
		installed = agent + "@" + version
		return runtimeinstall.LockedAgent{Version: version}, nil
	}})
	s.poll(context.Background())
	items := center.List()
	if len(items) != 1 || items[0].ID != "agent-update:codex" || items[0].Actions[0].ID != "install" {
		t.Fatalf("notification=%#v", items)
	}
	if _, err := s.HandleAction(context.Background(), items[0].ID, "install"); err != nil {
		t.Fatal(err)
	}
	if installed != "codex@1.9.0" {
		t.Fatalf("installed=%q", installed)
	}
	if len(center.List()) != 0 {
		t.Fatalf("completed update notification was not dismissed: %#v", center.List())
	}
}

func TestDismissSuppressesOnlyCurrentVersion(t *testing.T) {
	center := notifications.New()
	latest := "1.9.0"
	s := NewService(Options{Center: center, Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
		return []runtimeinstall.UpdateInfo{{Agent: "codex", CurrentVersion: "1.8.0", LatestVersion: latest}}, nil
	}})
	s.poll(context.Background())
	if _, err := s.HandleAction(context.Background(), "agent-update:codex", "dismiss"); err != nil {
		t.Fatal(err)
	}
	s.poll(context.Background())
	if len(center.List()) != 0 {
		t.Fatal("dismissed version returned")
	}
	latest = "1.10.0"
	s.poll(context.Background())
	if len(center.List()) != 1 {
		t.Fatal("newer version was suppressed")
	}
}

func rebaseUpdate() runtimeinstall.UpdateInfo {
	return runtimeinstall.UpdateInfo{
		Agent: "pi", Package: "pi-acp", CurrentVersion: "0.0.33", LatestVersion: "0.0.34",
		Kind: runtimeinstall.UpdateKindRebase,
		Fork: &config.ACPFork{Repo: "https://github.com/me/pi-acp", Ref: "tandem",
			Commit: "52f61f3850a883b9b5a3bb3245908bd5d95b7d43", UpstreamPackage: "pi-acp",
			UpstreamVersion: "0.0.33", Reason: "Emits usage_update", UpstreamPRs: []int{114}},
	}
}

func TestForkRebaseOffersSpawnRatherThanInstall(t *testing.T) {
	center := notifications.New()
	installed, spawned := "", ""
	s := NewService(Options{Center: center,
		Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
			return []runtimeinstall.UpdateInfo{rebaseUpdate()}, nil
		},
		Install: func(_ context.Context, _ config.Config, agent, version string, _ io.Writer) (runtimeinstall.LockedAgent, error) {
			installed = agent + "@" + version
			return runtimeinstall.LockedAgent{Version: version}, nil
		},
		SpawnRebase: func(_ context.Context, u runtimeinstall.UpdateInfo) (string, error) {
			spawned = u.Agent + "->" + u.LatestVersion
			return "session-1", nil
		}})
	s.poll(context.Background())
	items := center.List()
	if len(items) != 1 || items[0].ID != "agent-update:pi" {
		t.Fatalf("notification=%#v", items)
	}
	if items[0].Actions[0].ID != "rebase" || !items[0].Actions[0].Primary {
		t.Fatalf("expected a primary rebase action, got %#v", items[0].Actions)
	}
	sessionID, err := s.HandleAction(context.Background(), items[0].ID, "rebase")
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-1" || spawned != "pi->0.0.34" {
		t.Fatalf("sessionID=%q spawned=%q", sessionID, spawned)
	}
	// A fork must never be silently replaced by the published release.
	if installed != "" {
		t.Fatalf("install was called for a fork-tracked agent: %q", installed)
	}
	if len(center.List()) != 0 {
		t.Fatalf("notification not cleared after spawn: %#v", center.List())
	}
	// The release stays suppressed so the operator is not nagged mid-rebase.
	s.poll(context.Background())
	if len(center.List()) != 0 {
		t.Fatalf("re-raised while the rebase agent is working: %#v", center.List())
	}
}

// A daemon restart must not re-offer a rebase an agent is still working on,
// and closing that agent's session re-offers it.
func TestForkRebaseInProgressSurvivesRestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "rebases.json")
	open := map[string]bool{}
	newService := func(center *notifications.Center) *Service {
		return NewService(Options{Center: center, RebaseStatePath: state,
			SessionOpen: func(id string) bool { return open[id] },
			Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
				return []runtimeinstall.UpdateInfo{rebaseUpdate()}, nil
			},
			SpawnRebase: func(context.Context, runtimeinstall.UpdateInfo) (string, error) {
				open["session-1"] = true
				return "session-1", nil
			}})
	}
	center := notifications.New()
	s := newService(center)
	s.poll(context.Background())
	if _, err := s.HandleAction(context.Background(), "agent-update:pi", "rebase"); err != nil {
		t.Fatal(err)
	}
	restarted := notifications.New()
	s = newService(restarted)
	s.poll(context.Background())
	if len(restarted.List()) != 0 {
		t.Fatalf("re-offered after restart while the rebase agent is open: %#v", restarted.List())
	}
	open["session-1"] = false
	s.poll(context.Background())
	if items := restarted.List(); len(items) != 1 || items[0].Actions[0].ID != "rebase" {
		t.Fatalf("not re-offered after the rebase session closed: %#v", items)
	}
}

func TestForkRebaseWithoutProfileIsInformational(t *testing.T) {
	center := notifications.New()
	s := NewService(Options{Center: center, Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
		return []runtimeinstall.UpdateInfo{rebaseUpdate()}, nil
	}})
	s.poll(context.Background())
	items := center.List()
	if len(items) != 1 {
		t.Fatalf("notification=%#v", items)
	}
	for _, a := range items[0].Actions {
		if a.ID == "rebase" {
			t.Fatal("offered a rebase spawn with no profile configured")
		}
	}
	if _, err := s.HandleAction(context.Background(), items[0].ID, "rebase"); err == nil {
		t.Fatal("expected rebase to fail without a configured profile")
	}
}

func TestRebasePromptLeadsWithUpstreamCheck(t *testing.T) {
	prompt := RebasePrompt(rebaseUpdate(), "/rt/forks/pi", "/opt/bin/tandem")
	for _, want := range []string{
		"/opt/bin/tandem acp upstream pi",                                                // the retire path, by absolute path
		"/opt/bin/tandem acp sync pi --check",                                            // another instance may have done it
		"/opt/bin/tandem acp manifest pi --rebased-onto 0.0.34 --write tandem-fork.json", // publishing it
		"--retired-for",                 // publishing a retirement
		"/opt/bin/tandem acp sync pi\n", // the re-pin path, from the branch
		"#114",                          // the PRs to check first
		"--no-prs",                      // refreshing the PR list
		"Emits usage_update",            // why the fork exists
		"git rebase upstream/main",      // the rebase itself
		"/rt/forks/pi",                  // where to work
		"npm run typecheck && npm run lint && npm test",  // the checks
		"git -C /rt/forks/pi reset --hard origin/tandem", // syncing the clone
		"Finish with your worktree clean",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("rebase prompt is missing %q", want)
		}
	}
	// Checking the shared branch comes before any review work.
	if strings.Index(prompt, "acp sync pi --check") > strings.Index(prompt, "git fetch upstream") {
		t.Error("the shared-branch check must come before the upstreaming check")
	}
	// Retiring must be presented before rebasing, or an agent will rebase by reflex.
	if strings.Index(prompt, "upstreamed") > strings.Index(prompt, "git rebase upstream/main") {
		t.Error("the upstreaming check must come before the rebase instructions")
	}
}

func TestReleaseNotification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		update        runtimeinstall.UpdateInfo
		wantTitle     string
		wantInMessage []string
	}{
		{
			name: "a compatible update reads as routine",
			update: runtimeinstall.UpdateInfo{Agent: "codex", Package: "codex-acp", Constraint: "^1.8.0",
				CurrentVersion: "1.8.0", LatestVersion: "1.12.0", Kind: runtimeinstall.UpdateKindRelease},
			wantTitle:     "codex ACP update available",
			wantInMessage: []string{"1.12.0 is available", "installed 1.8.0"},
		},
		{
			// The claude case: pinned ^0.70.0, upstream on 0.79.0. Such releases
			// have not broken in practice, so the offer reads as routine.
			name: "an update beyond the constraint reads as routine",
			update: runtimeinstall.UpdateInfo{Agent: "claude", Package: "claude-agent-acp", Constraint: "^0.70.0",
				CurrentVersion: "0.70.0", LatestVersion: "0.79.0", Kind: runtimeinstall.UpdateKindRelease},
			wantTitle:     "claude ACP update available",
			wantInMessage: []string{"0.79.0 is available", "installed 0.70.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(Options{Center: notifications.New()})
			n := s.releaseNotification(tc.update)
			if n.Title != tc.wantTitle {
				t.Errorf("title=%q want %q", n.Title, tc.wantTitle)
			}
			for _, want := range tc.wantInMessage {
				if !strings.Contains(n.Message, want) {
					t.Errorf("message missing %q\ngot: %s", want, n.Message)
				}
			}
			// Whatever the compatibility, installing stays a one-click action.
			if len(n.Actions) == 0 || n.Actions[0].ID != "install" || !n.Actions[0].Primary {
				t.Errorf("expected a primary install action, got %#v", n.Actions)
			}
		})
	}
}

func TestPollOffersUntestedUpdateEndToEnd(t *testing.T) {
	center := notifications.New()
	installed := ""
	s := NewService(Options{Center: center,
		Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
			return []runtimeinstall.UpdateInfo{{Agent: "claude", Package: "claude-agent-acp", Constraint: "^0.70.0",
				CurrentVersion: "0.70.0", LatestVersion: "0.79.0", Kind: runtimeinstall.UpdateKindRelease}}, nil
		},
		Install: func(_ context.Context, _ config.Config, agent, version string, _ io.Writer) (runtimeinstall.LockedAgent, error) {
			installed = agent + "@" + version
			return runtimeinstall.LockedAgent{Version: version}, nil
		}})
	s.poll(context.Background())
	items := center.List()
	if len(items) != 1 || strings.Contains(items[0].Title+items[0].Message, "tested") {
		t.Fatalf("notification=%#v", items)
	}
	// An offer beyond the constraint carries no warning and is installable.
	if _, err := s.HandleAction(context.Background(), items[0].ID, "install"); err != nil {
		t.Fatal(err)
	}
	if installed != "claude@0.79.0" {
		t.Fatalf("installed=%q", installed)
	}
}

func TestActionOnWithdrawnUpdateClearsNotification(t *testing.T) {
	center := notifications.New()
	s := NewService(Options{Center: center})
	center.Upsert(notifications.Notification{ID: "agent-update:claude", Title: "claude ACP update available"})
	if _, err := s.HandleAction(context.Background(), "agent-update:claude", "dismiss"); err != nil {
		t.Fatalf("dismissing a withdrawn update should succeed: %v", err)
	}
	if len(center.List()) != 0 {
		t.Fatalf("withdrawn notification remained: %#v", center.List())
	}
	center.Upsert(notifications.Notification{ID: "agent-update:claude", Title: "claude ACP update available"})
	if _, err := s.HandleAction(context.Background(), "agent-update:claude", "install"); err == nil {
		t.Fatal("installing a withdrawn update should report it")
	}
	if len(center.List()) != 0 {
		t.Fatalf("withdrawn notification remained after install: %#v", center.List())
	}
}

func adoptUpdate() runtimeinstall.UpdateInfo {
	u := rebaseUpdate()
	u.Kind = runtimeinstall.UpdateKindAdopt
	u.Blessed = &runtimeinstall.BlessedFork{Commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Manifest: &runtimeinstall.BlessedManifest{Schema: 1, UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.34"}}
	return u
}

func TestBlessedForkOffersAdoptInsteadOfRebase(t *testing.T) {
	center := notifications.New()
	synced, spawned := "", false
	s := NewService(Options{Center: center,
		Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
			return []runtimeinstall.UpdateInfo{adoptUpdate()}, nil
		},
		SpawnRebase: func(context.Context, runtimeinstall.UpdateInfo) (string, error) {
			spawned = true
			return "", nil
		},
		Sync: func(_ context.Context, _ config.Config, agent string, _ io.Writer) (runtimeinstall.SyncResult, error) {
			synced = agent
			return runtimeinstall.SyncResult{Kind: runtimeinstall.UpdateKindAdopt,
				After: config.ACPFork{Commit: "bbbbbbbbbbbb", UpstreamVersion: "0.0.34"}}, nil
		}})
	s.poll(context.Background())
	items := center.List()
	if len(items) != 1 || items[0].Actions[0].ID != "sync" || !items[0].Actions[0].Primary {
		t.Fatalf("notification=%#v", items)
	}
	for _, a := range items[0].Actions {
		if a.ID == "rebase" {
			t.Fatal("offered a rebase for a tree that was already published")
		}
	}
	if _, err := s.HandleAction(context.Background(), items[0].ID, "sync"); err != nil {
		t.Fatal(err)
	}
	if synced != "pi" || spawned {
		t.Fatalf("synced=%q spawned=%v", synced, spawned)
	}
	if items := center.List(); len(items) != 1 || items[0].Severity != "success" {
		t.Fatalf("expected a success notification, got %#v", items)
	}
}

func TestBlessedForkAutoFollow(t *testing.T) {
	center := notifications.New()
	calls := 0
	fail := true
	s := NewService(Options{Center: center,
		Check: func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error) {
			u := adoptUpdate()
			u.Fork.Follow = config.ForkFollowAuto
			return []runtimeinstall.UpdateInfo{u}, nil
		},
		Sync: func(context.Context, config.Config, string, io.Writer) (runtimeinstall.SyncResult, error) {
			calls++
			if fail {
				return runtimeinstall.SyncResult{}, errors.New("build failed")
			}
			return runtimeinstall.SyncResult{Kind: runtimeinstall.UpdateKindAdopt}, nil
		}})
	s.poll(context.Background())
	if calls != 1 {
		t.Fatalf("auto-follow did not sync: calls=%d", calls)
	}
	if items := center.List(); len(items) != 1 || items[0].Severity != "failure" {
		t.Fatalf("expected the failure to be surfaced, got %#v", items)
	}
	// A broken tree is not retried every poll.
	s.poll(context.Background())
	if calls != 1 {
		t.Fatalf("retried a failed auto-follow: calls=%d", calls)
	}
}
