// Package agentupdates discovers and installs updates for Tandem-managed ACP adapters.
package agentupdates

import (
	"context"
	"encoding/json"
	"errors"
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
	// Sync adopts a fork tree published on the fork branch, or retires the fork
	// when the branch says so. Defaults to runtimeinstall.SyncBlessed.
	Sync func(context.Context, config.Config, string, io.Writer) (runtimeinstall.SyncResult, error)
	// RebaseStatePath persists which session was spawned to rebase each fork,
	// so a daemon restart does not re-offer a rebase an agent is already doing.
	// Empty keeps the record in memory only.
	RebaseStatePath string
	// SessionOpen reports whether a session still exists and is not closed. A
	// recorded rebase suppresses its offer only while its session is open; nil
	// treats every recorded session as open.
	SessionOpen func(id string) bool
}
type Service struct {
	opts      Options
	mu        sync.Mutex
	available map[string]runtimeinstall.UpdateInfo
	dismissed map[string]string
	rebases   map[string]rebaseRecord
}

// rebaseRecord is a rebase agent spawned for one offer.
type rebaseRecord struct {
	Offer   string `json:"offer"`
	Session string `json:"session"`
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
	if o.Sync == nil {
		o.Sync = runtimeinstall.SyncBlessed
	}
	return &Service{opts: o, available: map[string]runtimeinstall.UpdateInfo{}, dismissed: map[string]string{}, rebases: loadRebases(o.RebaseStatePath)}
}

func loadRebases(path string) map[string]rebaseRecord {
	out := map[string]rebaseRecord{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not read ACP fork rebase state", "path", path, "error", err)
		}
		return out
	}
	if err := json.Unmarshal(data, &out); err != nil {
		slog.Warn("ignoring unreadable ACP fork rebase state", "path", path, "error", err)
		return map[string]rebaseRecord{}
	}
	return out
}

// saveRebasesLocked writes the rebase records; s.mu must be held.
func (s *Service) saveRebasesLocked() {
	path := s.opts.RebaseStatePath
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(s.rebases, "", "  ")
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		slog.Error("could not persist ACP fork rebase state", "path", path, "error", err)
	}
}

// rebaseInProgressLocked returns the session already rebasing u's fork for
// this offer, forgetting records whose offer moved on or whose session closed.
// s.mu must be held.
func (s *Service) rebaseInProgressLocked(u runtimeinstall.UpdateInfo) (string, bool) {
	rec, ok := s.rebases[u.Agent]
	if !ok {
		return "", false
	}
	if rec.Offer == u.OfferKey() && (s.opts.SessionOpen == nil || s.opts.SessionOpen(rec.Session)) {
		return rec.Session, true
	}
	slog.Info("forgetting finished ACP fork rebase", "agent", u.Agent, "session", rec.Session, "offer", rec.Offer, "current_offer", u.OfferKey())
	delete(s.rebases, u.Agent)
	s.saveRebasesLocked()
	return "", false
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
	var auto []runtimeinstall.UpdateInfo
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		for _, u := range auto {
			s.autoSync(ctx, u)
		}
	}()
	seen := map[string]bool{}
	for _, u := range items {
		seen[u.Agent] = true
		s.available[u.Agent] = u
		if s.dismissed[u.Agent] == u.OfferKey() {
			continue
		}
		if isBlessed(u) {
			slog.Info("blessed ACP fork tree published", "agent", u.Agent, "kind", u.Kind, "repo", u.Fork.Repo, "ref", u.Fork.Ref,
				"pinned", u.Fork.Commit, "tip", u.Blessed.Commit, "follow", u.Fork.Follow)
			if u.Fork.Follow == config.ForkFollowAuto {
				auto = append(auto, u)
				continue
			}
			s.opts.Center.Upsert(s.blessedNotification(u))
			continue
		}
		if u.Kind == runtimeinstall.UpdateKindRebase && u.Fork != nil {
			if session, busy := s.rebaseInProgressLocked(u); busy {
				slog.Info("ACP fork is behind upstream; rebase agent already working", "agent", u.Agent, "session", session, "upstream_version", u.LatestVersion)
				s.opts.Center.Remove(Prefix + u.Agent)
				continue
			}
			slog.Info("ACP fork is behind upstream", "agent", u.Agent, "fork_rebased_onto", u.CurrentVersion, "upstream_version", u.LatestVersion, "fork_repo", u.Fork.Repo)
			s.opts.Center.Upsert(s.rebaseNotification(u))
			continue
		}
		slog.Info("ACP adapter update available", "agent", u.Agent, "installed_version", u.CurrentVersion,
			"available_version", u.LatestVersion)
		s.opts.Center.Upsert(s.releaseNotification(u))
	}
	for agent := range s.available {
		if !seen[agent] {
			delete(s.available, agent)
			s.opts.Center.Remove(Prefix + agent)
		}
	}
}

// releaseNotification recommends updating to the newest published release.
func (s *Service) releaseNotification(u runtimeinstall.UpdateInfo) notifications.Notification {
	message := fmt.Sprintf("%s is available (installed %s). New sessions will use it after updating.", u.LatestVersion, u.CurrentVersion)
	return notifications.Notification{ID: Prefix + u.Agent, Severity: "attention", Title: u.Agent + " ACP update available", Message: message,
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

func isBlessed(u runtimeinstall.UpdateInfo) bool {
	return (u.Kind == runtimeinstall.UpdateKindAdopt || u.Kind == runtimeinstall.UpdateKindRetire) && u.Fork != nil && u.Blessed != nil
}

// blessedNotification offers to follow a tree another instance published on
// the fork branch. Adopting is a re-pin, not a rebase: the review and rebase
// work was done where the tree was published.
func (s *Service) blessedNotification(u runtimeinstall.UpdateInfo) notifications.Notification {
	if u.Kind == runtimeinstall.UpdateKindRetire {
		message := fmt.Sprintf("The %s fork branch at %s says the fork is retired: %s %s carries its change.", u.Fork.Ref, u.Blessed.ShortCommit(), u.Package, u.LatestVersion)
		return notifications.Notification{ID: Prefix + u.Agent, Severity: "attention", Title: u.Agent + " ACP fork was retired upstream", Message: message,
			Actions: []notifications.Action{{ID: "sync", Label: "Return to upstream", Primary: true}, {ID: "dismiss", Label: "Later"}}}
	}
	message := fmt.Sprintf("The %s fork branch now publishes %s rebased onto %s (pinned here: %s on %s), so it does not need rebasing again on this host.",
		u.Fork.Ref, u.Blessed.ShortCommit(), u.LatestVersion, u.Fork.ShortCommit(), u.CurrentVersion)
	if m := u.Blessed.Manifest; m != nil && m.Reason != "" {
		message += " Fork adds: " + m.Reason + "."
	}
	return notifications.Notification{ID: Prefix + u.Agent, Severity: "attention", Title: u.Agent + " ACP fork has a published update", Message: message,
		Actions: []notifications.Action{{ID: "sync", Label: "Adopt", Primary: true}, {ID: "dismiss", Label: "Later"}}}
}

// sync applies a blessed offer and reports the outcome in the notification.
func (s *Service) sync(ctx context.Context, u runtimeinstall.UpdateInfo) error {
	id := Prefix + u.Agent
	s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "attention", Title: "Syncing " + u.Agent + " ACP fork", Message: "Building the published fork tree…"})
	res, err := s.opts.Sync(ctx, s.opts.Config, u.Agent, s.opts.Log)
	if err != nil {
		slog.Error("blessed ACP fork sync failed", "agent", u.Agent, "tip", u.Blessed.Commit, "error", err)
		s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "failure", Title: u.Agent + " ACP fork sync failed", Message: err.Error(),
			Actions: []notifications.Action{{ID: "sync", Label: "Retry", Primary: true}, {ID: "dismiss", Label: "Later"}}})
		return err
	}
	s.mu.Lock()
	delete(s.available, u.Agent)
	s.mu.Unlock()
	var message string
	switch res.Kind {
	case runtimeinstall.UpdateKindRetire:
		message = fmt.Sprintf("Returned to the published %s@%s; fork tracking removed.", res.Locked.Package, res.Locked.Version)
	case runtimeinstall.UpdateKindAdopt:
		message = fmt.Sprintf("New sessions use %s, rebased onto %s.", res.After.ShortCommit(), res.After.UpstreamVersion)
	default:
		message = "Already pinned to the published fork tree."
	}
	slog.Info("blessed ACP fork synced", "agent", u.Agent, "kind", res.Kind, "from_commit", res.Before.Commit, "to_commit", res.After.Commit)
	s.opts.Center.Upsert(notifications.Notification{ID: id, Severity: "success", Title: u.Agent + " ACP fork synced", Message: message})
	return nil
}

// autoSync applies a blessed offer for a fork that follows its branch
// unprompted. A failure falls back to the ordinary offer, with the error.
func (s *Service) autoSync(ctx context.Context, u runtimeinstall.UpdateInfo) {
	slog.Info("auto-following blessed ACP fork", "agent", u.Agent, "tip", u.Blessed.Commit)
	if err := s.sync(ctx, u); err != nil {
		// Do not retry every poll against the same broken tree.
		s.mu.Lock()
		s.dismissed[u.Agent] = u.OfferKey()
		s.mu.Unlock()
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
	if !ok && action != "rollback" {
		// The offer was withdrawn (installed elsewhere, superseded, or the daemon
		// restarted) while a view still showed it. Drop the leftover entry so
		// it cannot sit in the panel with buttons that can only fail.
		s.opts.Center.Remove(id)
		slog.Info("ACP adapter update notification acted on after the offer was withdrawn", "agent", agent, "action", action)
		if action == "dismiss" {
			return "", nil
		}
		return "", fmt.Errorf("update is no longer available")
	}
	switch action {
	case "dismiss":
		s.mu.Lock()
		s.dismissed[agent] = u.OfferKey()
		s.mu.Unlock()
		s.opts.Center.Remove(id)
		slog.Info("ACP adapter update dismissed", "agent", agent, "version", u.LatestVersion)
		return "", nil
	case "install":
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
		s.dismissed[agent] = u.OfferKey()
		s.rebases[agent] = rebaseRecord{Offer: u.OfferKey(), Session: sessionID}
		s.saveRebasesLocked()
		s.mu.Unlock()
		s.opts.Center.Remove(id)
		slog.Info("ACP fork rebase agent spawned", "agent", agent, "session", sessionID, "upstream_version", u.LatestVersion)
		return sessionID, nil
	case "sync":
		if !isBlessed(u) {
			return "", fmt.Errorf("agent %s has no published fork tree to sync", agent)
		}
		return "", s.sync(ctx, u)
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
