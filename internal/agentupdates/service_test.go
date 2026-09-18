package agentupdates

import (
	"context"
	"io"
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
