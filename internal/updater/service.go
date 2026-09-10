package updater

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/notifications"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/workspace"
)

const CheckInterval = 5 * time.Minute

const notificationID = "tandem-update"

type AgentSpawner interface {
	Spawn(context.Context, agentadapter.Spec) (*session.Session, error)
}

type ServiceOptions struct {
	Updater     Options
	Home        string
	Center      *notifications.Center
	Agents      AgentSpawner
	Restart     func()
	Log         io.Writer
	Interval    time.Duration
	Check       func(context.Context, Options) (CheckResult, error)
	Update      func(context.Context, Options) (bool, error)
	NeedsReview func() (bool, error)
}

// Service runs the daemon's periodic update check and handles actions from the
// notifications panel.
type Service struct {
	opts         ServiceOptions
	mu           sync.Mutex
	installed    bool
	configReview bool
	latest       string
}

func NewService(opts ServiceOptions) *Service {
	if opts.Center == nil {
		opts.Center = notifications.New()
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	if opts.Interval <= 0 {
		opts.Interval = CheckInterval
	}
	if opts.Check == nil {
		opts.Check = Check
	}
	if opts.Update == nil {
		opts.Update = UpdateWithResult
	}
	if opts.NeedsReview == nil {
		opts.NeedsReview = func() (bool, error) { return UpdatedBinaryNeedsReview(opts.Updater.Executable) }
	}
	return &Service{opts: opts}
}

func (s *Service) Start(ctx context.Context) {
	go func() {
		s.poll(ctx)
		ticker := time.NewTicker(s.opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.poll(ctx)
			}
		}
	}()
}

func (s *Service) poll(ctx context.Context) {
	s.mu.Lock()
	installed, review := s.installed, s.configReview
	s.mu.Unlock()
	if installed {
		if review {
			needs, err := s.opts.NeedsReview()
			if err != nil {
				fmt.Fprintf(s.opts.Log, "tandem: check updated configuration: %v\n", err)
				return
			}
			if !needs {
				s.mu.Lock()
				s.configReview = false
				s.mu.Unlock()
				s.showRestart()
			}
		}
		return
	}
	result, err := s.opts.Check(ctx, s.opts.Updater)
	if err != nil {
		fmt.Fprintf(s.opts.Log, "tandem: update check failed: %v\n", err)
		return
	}
	if !result.Available {
		return
	}
	s.mu.Lock()
	s.latest = result.LatestVersion
	s.mu.Unlock()
	s.opts.Center.Upsert(notifications.Notification{
		ID: notificationID, Severity: "attention", Title: "Tandem update available",
		Message: fmt.Sprintf("%s is available (running %s).", result.LatestVersion, result.CurrentVersion),
		Actions: []notifications.Action{{ID: "install", Label: "Update", Primary: true}},
	})
}

// HandleAction applies an update notification action. A non-empty returned ID
// is the in-instance setup agent the UI should focus.
func (s *Service) HandleAction(ctx context.Context, id, action string) (string, error) {
	if id != notificationID {
		return "", fmt.Errorf("unknown notification: %s", id)
	}
	allowed := false
	for _, item := range s.opts.Center.List() {
		if item.ID != id {
			continue
		}
		for _, candidate := range item.Actions {
			if candidate.ID == action {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		return "", fmt.Errorf("notification action %q is not currently available", action)
	}
	switch action {
	case "install":
		return "", s.install(ctx)
	case "restart":
		if s.opts.Restart == nil {
			return "", fmt.Errorf("restart is unavailable")
		}
		s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "success", Title: "Restart requested", Message: "Tandem will restart after active agent turns finish."})
		s.opts.Restart()
		return "", nil
	case "configure":
		return s.spawnSetupAgent(ctx)
	case "check_config":
		return "", s.finishInstall()
	default:
		return "", fmt.Errorf("unknown notification action: %s", action)
	}
}

func (s *Service) install(ctx context.Context) error {
	s.mu.Lock()
	if s.installed {
		s.mu.Unlock()
		return nil
	}
	s.installed = true // also serializes concurrent clicks and suppresses old-version polls
	s.mu.Unlock()
	s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "attention", Title: "Updating Tandem", Message: "Downloading and verifying the latest release…"})
	updated, err := s.opts.Update(ctx, s.opts.Updater)
	if err != nil {
		s.mu.Lock()
		s.installed = false
		s.mu.Unlock()
		s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "failure", Title: "Tandem update failed", Message: err.Error(), Actions: []notifications.Action{{ID: "install", Label: "Retry", Primary: true}}})
		return err
	}
	if !updated {
		s.mu.Lock()
		s.installed = false
		s.mu.Unlock()
		s.opts.Center.Remove(notificationID)
		return nil
	}
	return s.finishInstall()
}

func (s *Service) finishInstall() error {
	needsReview, err := s.opts.NeedsReview()
	if err != nil {
		s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "failure", Title: "Tandem updated, but configuration could not be checked", Message: err.Error(), Actions: []notifications.Action{{ID: "check_config", Label: "Retry check", Primary: true}}})
		return err
	}
	if needsReview {
		s.mu.Lock()
		s.configReview = true
		s.mu.Unlock()
		s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "attention", Title: "Configuration review needed", Message: "The new Tandem release has a newer configuration version. Start a setup agent before restarting.", Actions: []notifications.Action{{ID: "configure", Label: "Start setup agent", Primary: true}}})
		return nil
	}
	s.showRestart()
	return nil
}

func (s *Service) showRestart() {
	s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "success", Title: "Tandem update complete", Message: "The installed configuration is compatible. Restart to run the new version.", Actions: []notifications.Action{{ID: "restart", Label: "Restart when idle", Primary: true}}})
}

func (s *Service) spawnSetupAgent(ctx context.Context) (string, error) {
	if s.opts.Agents == nil {
		return "", fmt.Errorf("agent spawning is unavailable")
	}
	s.mu.Lock()
	latest := s.latest
	s.mu.Unlock()
	guide := "https://github.com/aiguy110/tandem/blob/master/docs/setup-agent.md"
	if latest != "" {
		guide = "https://github.com/aiguy110/tandem/blob/" + latest + "/docs/setup-agent.md"
	}
	prompt := fmt.Sprintf(`Help the user update this Tandem installation's configuration for the newly installed release. Read the release-specific guide at %s and inspect the config.yml in your current directory. Explain changes before making them and preserve unrelated agent and harness settings. When the configuration is complete, run "tandem setup --complete" so Tandem can offer the restart.`, guide)
	sess, err := s.opts.Agents.Spawn(ctx, agentadapter.Spec{Adapter: "acp", Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: s.opts.Home}, Name: "setup-update", Task: prompt})
	if err != nil {
		return "", err
	}
	s.opts.Center.Upsert(notifications.Notification{ID: notificationID, Severity: "attention", Title: "Configuration review in progress", Message: "The setup agent is working in this Tandem instance. A restart will be offered after it records the new configuration version."})
	return sess.ID, nil
}

// UpdatedBinaryNeedsReview asks the replacement executable, rather than the
// still-running old process, about its configuration compatibility version.
func UpdatedBinaryNeedsReview(executable string) (bool, error) {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return false, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "setup", "--needs-review").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(output)) == "true", nil
}
