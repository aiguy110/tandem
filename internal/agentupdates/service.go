// Package agentupdates discovers and installs updates for Tandem-managed ACP adapters.
package agentupdates

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
)

const Prefix = "agent-update:"
const CheckInterval = 6 * time.Hour

type Options struct {
	Config   config.Config
	Center   *notifications.Center
	Log      io.Writer
	Interval time.Duration
	Check    func(context.Context, config.Config) ([]runtimeinstall.UpdateInfo, error)
	Install  func(context.Context, config.Config, string, string, io.Writer) (runtimeinstall.LockedAgent, error)
	Rollback func(context.Context, config.Config, string) (runtimeinstall.LockedAgent, error)
}
type Service struct {
	opts      Options
	mu        sync.Mutex
	available map[string]runtimeinstall.UpdateInfo
	dismissed map[string]string
}

func (s *Service) Catalog(ctx context.Context) ([]runtimeinstall.AdapterStatus, error) {
	return runtimeinstall.AdapterCatalog(ctx, s.opts.Config)
}
func (s *Service) InstallVersion(ctx context.Context, agent, version string) error {
	_, err := s.opts.Install(ctx, s.opts.Config, agent, version, s.opts.Log)
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.available, agent)
	s.mu.Unlock()
	s.opts.Center.Remove(Prefix + agent)
	slog.Info("ACP adapter version selected", "agent", agent, "version", version)
	return nil
}

func NewService(o Options) *Service {
	if o.Center == nil {
		o.Center = notifications.New()
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	if o.Interval <= 0 {
		o.Interval = CheckInterval
	}
	if o.Check == nil {
		o.Check = runtimeinstall.CheckUpdates
	}
	if o.Install == nil {
		o.Install = runtimeinstall.InstallUpdate
	}
	if o.Rollback == nil {
		o.Rollback = runtimeinstall.Rollback
	}
	return &Service{opts: o, available: map[string]runtimeinstall.UpdateInfo{}, dismissed: map[string]string{}}
}
func (s *Service) Start(ctx context.Context) {
	go func() {
		s.poll(ctx)
		t := time.NewTicker(s.opts.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.poll(ctx)
			}
		}
	}()
}
func (s *Service) poll(ctx context.Context) {
	items, err := s.opts.Check(ctx, s.opts.Config)
	if err != nil {
		slog.Error("ACP adapter update check failed", "error", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, u := range items {
		seen[u.Agent] = true
		s.available[u.Agent] = u
		if s.dismissed[u.Agent] == u.LatestVersion {
			continue
		}
		slog.Info("ACP adapter update available", "agent", u.Agent, "installed_version", u.CurrentVersion, "available_version", u.LatestVersion)
		s.opts.Center.Upsert(notifications.Notification{ID: Prefix + u.Agent, Severity: "attention", Title: u.Agent + " ACP update available", Message: fmt.Sprintf("%s is available (installed %s). New sessions will use it after updating.", u.LatestVersion, u.CurrentVersion), Actions: []notifications.Action{{ID: "install", Label: "Update", Primary: true}, {ID: "dismiss", Label: "Later"}}})
	}
	for agent := range s.available {
		if !seen[agent] {
			delete(s.available, agent)
			s.opts.Center.Remove(Prefix + agent)
		}
	}
}
func (s *Service) HandleAction(ctx context.Context, id, action string) (string, error) {
	if !strings.HasPrefix(id, Prefix) {
		return "", fmt.Errorf("unknown notification: %s", id)
	}
	agent := strings.TrimPrefix(id, Prefix)
	s.mu.Lock()
	u, ok := s.available[agent]
	s.mu.Unlock()
	switch action {
	case "dismiss":
		if !ok {
			return "", fmt.Errorf("update is no longer available")
		}
		s.mu.Lock()
		s.dismissed[agent] = u.LatestVersion
		s.mu.Unlock()
		s.opts.Center.Remove(id)
		slog.Info("ACP adapter update dismissed", "agent", agent, "version", u.LatestVersion)
		return "", nil
	case "install":
		if !ok {
			return "", fmt.Errorf("update is no longer available")
		}
		s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "attention", Title: "Updating " + agent + " ACP", Message: "Installing the new version beside the existing distribution…"})
		locked, err := s.opts.Install(ctx, s.opts.Config, agent, u.LatestVersion, s.opts.Log)
		if err != nil {
			slog.Error("ACP adapter update failed", "agent", agent, "from_version", u.CurrentVersion, "to_version", u.LatestVersion, "error", err)
			s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "failure", Title: agent + " ACP update failed", Message: err.Error(), Actions: []notifications.Action{{ID: "install", Label: "Retry", Primary: true}}})
			return "", err
		}
		s.mu.Lock()
		delete(s.available, agent)
		s.mu.Unlock()
		s.opts.Center.Remove(id)
		slog.Info("ACP adapter update installed", "agent", agent, "from_version", u.CurrentVersion, "to_version", locked.Version)
		return "", nil
	case "rollback":
		locked, err := s.opts.Rollback(ctx, s.opts.Config, agent)
		if err != nil {
			slog.Error("ACP adapter rollback failed", "agent", agent, "error", err)
			return "", err
		}
		s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "success", Title: agent + " ACP rolled back", Message: fmt.Sprintf("New sessions will use %s.", locked.Version)})
		slog.Info("ACP adapter rollback complete", "agent", agent, "version", locked.Version)
		return "", nil
	default:
		return "", fmt.Errorf("unknown notification action: %s", action)
	}
}
