package agentupdates

import (
	"context"
	"io"
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
	prompt := RebasePrompt(rebaseUpdate(), "/rt/forks/pi")
	for _, want := range []string{
		"tandem acp upstream pi",      // the retire path
		"tandem acp fork pi --commit", // the re-pin path
		"#114",                        // the PRs to check first
		"Emits usage_update",          // why the fork exists
		"git rebase main",             // the rebase itself
		"/rt/forks/pi",                // where to work
		"npm run typecheck && npm run lint && npm test", // the checks
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("rebase prompt is missing %q", want)
		}
	}
	// Retiring must be presented before rebasing, or an agent will rebase by reflex.
	if strings.Index(prompt, "upstreamed") > strings.Index(prompt, "git rebase main") {
		t.Error("the upstreaming check must come before the rebase instructions")
	}
}

func TestReleaseNotificationDistinguishesTestedFromUntested(t *testing.T) {
	for _, tc := range []struct {
		name          string
		update        runtimeinstall.UpdateInfo
		wantTitle     string
		wantInMessage []string
	}{
		{
			name: "a compatible update reads as routine",
			update: runtimeinstall.UpdateInfo{Agent: "codex", Package: "codex-acp", Constraint: "^1.8.0",
				CurrentVersion: "1.8.0", LatestVersion: "1.12.0", Compatible: true, Kind: runtimeinstall.UpdateKindRelease},
			wantTitle:     "codex ACP update available",
			wantInMessage: []string{"1.12.0 is available", "installed 1.8.0"},
		},
		{
			// The claude case: pinned ^0.70.0, upstream on 0.79.0. Surfacing it is
			// the point of the optimistic check, but it must not look routine.
			name: "an update beyond the tested range says so",
			update: runtimeinstall.UpdateInfo{Agent: "claude", Package: "claude-agent-acp", Constraint: "^0.70.0",
				CurrentVersion: "0.70.0", LatestVersion: "0.79.0", Compatible: false, Kind: runtimeinstall.UpdateKindRelease},
			wantTitle:     "claude ACP update available (untested)",
			wantInMessage: []string{"0.79.0 is available", "beyond the range Tandem is tested against (^0.70.0)", "roll back"},
		},
		{
			name: "a compatible offer still mentions a newer untested release",
			update: runtimeinstall.UpdateInfo{Agent: "pi", Package: "pi-acp", Constraint: "~0.0.31",
				CurrentVersion: "0.0.31", LatestVersion: "0.0.33", Compatible: true, NewestPublished: "0.1.0",
				Kind: runtimeinstall.UpdateKindRelease},
			wantTitle:     "pi ACP update available",
			wantInMessage: []string{"0.0.33 is available", "0.1.0 is also out", "beyond Tandem's tested range (~0.0.31)"},
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
				CurrentVersion: "0.70.0", LatestVersion: "0.79.0", Compatible: false, Kind: runtimeinstall.UpdateKindRelease}}, nil
		},
		Install: func(_ context.Context, _ config.Config, agent, version string, _ io.Writer) (runtimeinstall.LockedAgent, error) {
			installed = agent + "@" + version
			return runtimeinstall.LockedAgent{Version: version}, nil
		}})
	s.poll(context.Background())
	items := center.List()
	if len(items) != 1 || !strings.Contains(items[0].Title, "(untested)") {
		t.Fatalf("notification=%#v", items)
	}
	// An untested offer is still installable — optimism is only useful if acting
	// on it works.
	if _, err := s.HandleAction(context.Background(), items[0].ID, "install"); err != nil {
		t.Fatal(err)
	}
	if installed != "claude@0.79.0" {
		t.Fatalf("installed=%q", installed)
	}
}
