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
	// SpawnRebase starts an agent to move a tracked ACP fork onto a new upstream
	// release, returning the new session id. Nil (or an unset rebase profile)
	// leaves the fork notification informational: it then names the clone and the
	// CLI to run instead of offering a one-click spawn.
	SpawnRebase func(context.Context, runtimeinstall.UpdateInfo) (string, error)
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
		if u.Kind == runtimeinstall.UpdateKindRebase && u.Fork != nil {
			slog.Info("ACP fork is behind upstream", "agent", u.Agent, "fork_rebased_onto", u.CurrentVersion, "upstream_version", u.LatestVersion, "fork_repo", u.Fork.Repo)
			s.opts.Center.Upsert(s.rebaseNotification(u))
			continue
		}
		slog.Info("ACP adapter update available", "agent", u.Agent, "installed_version", u.CurrentVersion,
			"available_version", u.LatestVersion, "within_tested_range", u.Compatible, "tested_range", u.Constraint,
			"newest_published", u.NewestPublished)
		s.opts.Center.Upsert(s.releaseNotification(u))
	}
	for agent := range s.available {
		if !seen[agent] {
			delete(s.available, agent)
			s.opts.Center.Remove(Prefix + agent)
		}
	}
}

// releaseNotification describes an available published update. Tandem checks
// optimistically, so an offer can point past the compatibility range Tandem was
// tested against; when it does, the message says so rather than presenting it as
// a routine upgrade.
func (s *Service) releaseNotification(u runtimeinstall.UpdateInfo) notifications.Notification {
	message := fmt.Sprintf("%s is available (installed %s). New sessions will use it after updating.", u.LatestVersion, u.CurrentVersion)
	if !u.Compatible {
		message = fmt.Sprintf("%s is available (installed %s). It is beyond the range Tandem is tested against (%s), so it may not be compatible — the previous version stays installed and you can roll back.",
			u.LatestVersion, u.CurrentVersion, u.Constraint)
	} else if u.NewestPublished != "" {
		// A newer release exists but sits outside the tested range, so the offer
		// was held back to the compatible one. Say both, so the operator is not
		// left thinking LatestVersion is the newest that exists.
		message += fmt.Sprintf(" %s is also out, beyond Tandem's tested range (%s); pick it in the ACP manager to try it.", u.NewestPublished, u.Constraint)
	}
	title := u.Agent + " ACP update available"
	if !u.Compatible {
		title = u.Agent + " ACP update available (untested)"
	}
	return notifications.Notification{ID: Prefix + u.Agent, Severity: "attention", Title: title, Message: message,
		Actions: []notifications.Action{{ID: "install", Label: "Update", Primary: true}, {ID: "dismiss", Label: "Later"}}}
}

// rebaseNotification describes a fork that has fallen behind upstream. The
// offer is deliberately a spawn, not an automatic install: a new upstream
// release may have made the fork unnecessary, and only an agent reading the diff
// can tell rebase from retire.
func (s *Service) rebaseNotification(u runtimeinstall.UpdateInfo) notifications.Notification {
	message := fmt.Sprintf("%s %s is out, and Tandem carries a fork of this ACP server rebased onto %s.", u.Package, u.LatestVersion, u.CurrentVersion)
	if u.Fork.Reason != "" {
		message += " Fork adds: " + u.Fork.Reason + "."
	}
	actions := []notifications.Action{{ID: "dismiss", Label: "Later"}}
	if s.opts.SpawnRebase != nil {
		message += " An agent can check whether the change was upstreamed and either retire the fork or rebase it."
		actions = append([]notifications.Action{{ID: "rebase", Label: "Spawn rebase agent", Primary: true}}, actions...)
	} else {
		// No profile configured, so say what to run by hand rather than offering
		// a button that cannot work.
		message += fmt.Sprintf(" Set an ACP fork rebase profile to offer this as a spawn, or rebase by hand in %s.", u.Fork.Clone)
	}
	return notifications.Notification{ID: Prefix + u.Agent, Severity: "attention",
		Title: u.Agent + " ACP fork is behind upstream", Message: message, Actions: actions}
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
	case "rebase":
		if !ok {
			return "", fmt.Errorf("update is no longer available")
		}
		if u.Kind != runtimeinstall.UpdateKindRebase || u.Fork == nil {
			return "", fmt.Errorf("agent %s does not track an ACP fork", agent)
		}
		if s.opts.SpawnRebase == nil {
			return "", fmt.Errorf("no ACP fork rebase agent profile is configured")
		}
		s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "attention", Title: "Rebasing " + agent + " ACP fork", Message: "Starting an agent to check whether the change was upstreamed…"})
		sessionID, err := s.opts.SpawnRebase(ctx, u)
		if err != nil {
			slog.Error("ACP fork rebase spawn failed", "agent", agent, "error", err)
			s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "failure", Title: agent + " ACP fork rebase could not start", Message: err.Error(), Actions: []notifications.Action{{ID: "rebase", Label: "Retry", Primary: true}, {ID: "dismiss", Label: "Later"}}})
			return "", err
		}
		// The agent owns the outcome from here: it re-pins the fork or retires it,
		// and the next poll re-raises the notification if neither happened. Keep
		// the entry suppressed for this release so it does not nag mid-rebase.
		s.mu.Lock()
		s.dismissed[agent] = u.LatestVersion
		s.mu.Unlock()
		s.opts.Center.Remove(id)
		slog.Info("ACP fork rebase agent spawned", "agent", agent, "session", sessionID, "upstream_version", u.LatestVersion)
		return sessionID, nil
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
