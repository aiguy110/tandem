package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	storepkg "github.com/aiguy110/tandem/internal/store"
)

// Scheduler polls durable jobs and computes only future occurrences. It keeps
// transient next-fire and one-pending queue state in memory; durable run rows
// explain every execution or skip.
type Scheduler struct {
	Service      *Service
	PollInterval time.Duration
	Now          func() time.Time

	mu      sync.Mutex
	next    map[string]time.Time
	running map[string]bool
	pending map[string]bool
}

func (s *Scheduler) Start(ctx context.Context) {
	if s.Service == nil || s.Service.Store == nil {
		return
	}
	if s.PollInterval <= 0 {
		s.PollInterval = 15 * time.Second
	}
	s.mu.Lock()
	if s.next == nil {
		s.next, s.running, s.pending = map[string]time.Time{}, map[string]bool{}, map[string]bool{}
	}
	s.mu.Unlock()
	s.reconcileInterruptedRuns()
	s.tick(ctx)
	go func() {
		ticker := time.NewTicker(s.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
}

// A script child cannot survive its owning daemon. Resolve durable in-flight
// job runs explicitly on startup so they neither look active forever nor imply
// that Tandem can resume an orphaned process.
func (s *Scheduler) reconcileInterruptedRuns() {
	jobs, err := s.Service.Store.AutomationJobs("")
	if err != nil {
		return
	}
	for _, job := range jobs {
		run, lookupErr := s.Service.Store.ActiveAutomationRun(job.ID)
		if lookupErr != nil || run == nil {
			continue
		}
		completed := s.now().UnixMilli()
		message := "automation run was interrupted by daemon restart"
		run.CompletedAt = &completed
		run.Outcome = "runner_failed"
		run.Reason = "daemon_restart"
		run.Error = &message
		_ = s.Service.Store.SaveAutomationRun(*run)
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := s.now()
	jobs, err := s.Service.Store.AutomationJobs("")
	if err != nil {
		return
	}
	known := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		known[job.ID] = true
		if !job.Enabled {
			continue
		}
		s.mu.Lock()
		next, exists := s.next[job.ID]
		if !exists {
			next, err = nextOccurrence(job.Cron, job.Timezone, now)
			if err == nil {
				s.next[job.ID] = next
			}
			s.mu.Unlock()
			continue // startup/registration never replays an already-due tick
		}
		if now.Before(next) {
			s.mu.Unlock()
			continue
		}
		scheduled := next
		s.next[job.ID], err = nextOccurrence(job.Cron, job.Timezone, now)
		active := s.running[job.ID]
		if active && job.Concurrency == "queue" {
			s.pending[job.ID] = true
		}
		s.mu.Unlock()
		if err != nil {
			continue
		}
		if active {
			s.recordSkip(job, scheduled, "skipped_concurrency", "previous script execution is active")
			continue
		}
		if s.activeWakeup(job) {
			s.recordSkip(job, scheduled, "skipped_agent_active", s.wakeSuppressionReason(job))
			continue
		}
		s.startRun(ctx, job, scheduled)
	}
	s.mu.Lock()
	for id := range s.next {
		if !known[id] {
			delete(s.next, id)
			delete(s.running, id)
			delete(s.pending, id)
		}
	}
	s.mu.Unlock()
}

func (s *Scheduler) startRun(ctx context.Context, job storepkg.AutomationJob, scheduled time.Time) {
	s.mu.Lock()
	s.running[job.ID] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.running[job.ID] = false
			queued := s.pending[job.ID]
			delete(s.pending, job.ID)
			s.mu.Unlock()
			if queued && !s.activeWakeup(job) {
				s.startRun(ctx, job, s.now())
			}
		}()
		_ = s.runJob(ctx, job, scheduled)
	}()
}

func (s *Scheduler) runJob(ctx context.Context, job storepkg.AutomationJob, scheduled time.Time) error {
	repoRoot, err := repositoryRootFromID(job.RepositoryID)
	if err != nil {
		s.recordSkip(job, scheduled, "runner_failed", err.Error())
		return err
	}
	script, err := LoadScript(repoRoot, job.ScriptPath)
	if err != nil {
		s.recordSkip(job, scheduled, "runner_failed", err.Error())
		return err
	}
	for _, tool := range script.Manifest.Tools {
		granted, grantErr := s.Service.Store.RepositoryHasToolGrant(job.RepositoryID, tool.Name)
		if grantErr != nil || !granted {
			reason := "repository MCP grant is required for " + tool.Name
			if grantErr != nil {
				reason = grantErr.Error()
			}
			s.recordSkip(job, scheduled, "awaiting_approval", reason)
			return grantErr
		}
	}
	effective := script.Manifest
	if job.BrowserSnapshotID != "" {
		effective.Browser = &BrowserSpec{Snapshot: job.BrowserSnapshotID}
	} else {
		effective.Browser = nil
	}
	if job.DefaultAgentProfile != "" || job.WakePrompt != "" {
		effective.Wake = &WakeSpec{AgentProfile: job.DefaultAgentProfile, Prompt: job.WakePrompt}
	} else {
		effective.Wake = nil
	}
	jobID := job.ID
	scheduledAt := scheduled.UnixMilli()
	_, err = s.Service.execute(ctx, repository{ID: job.RepositoryID, Root: repoRoot}, script.Path, script.Source, effective, nil, "scheduled", &jobID, &scheduledAt)
	return err
}

func (s *Scheduler) recordSkip(job storepkg.AutomationJob, scheduled time.Time, outcome, reason string) {
	when := scheduled.UnixMilli()
	now := s.now().UnixMilli()
	sourceHash := ""
	if root, err := repositoryRootFromID(job.RepositoryID); err == nil {
		if script, loadErr := LoadScript(root, job.ScriptPath); loadErr == nil {
			sum := sha256.Sum256(script.Source)
			sourceHash = hex.EncodeToString(sum[:])
		}
	}
	message := reason
	_ = s.Service.Store.SaveAutomationRun(storepkg.AutomationRun{ID: "run-" + randomID(), JobID: &job.ID,
		RepositoryID: job.RepositoryID, ScriptPath: job.ScriptPath, Trigger: "scheduled", SourceHash: sourceHash,
		BrowserSnapshotID: job.BrowserSnapshotID, ScheduledAt: &when, StartedAt: now, CompletedAt: &now,
		Outcome: outcome, Reason: reason, Error: &message})
}

func (s *Scheduler) activeWakeup(job storepkg.AutomationJob) bool {
	wakeups, err := s.Service.Store.AutomationWakeups(job.ID)
	if err != nil {
		return false
	}
	for _, wakeup := range wakeups {
		if wakeup.SessionID == nil {
			continue
		}
		switch job.WakeSuppression {
		case "", WakeSuppressionUntilClosed:
			agent, agentErr := s.Service.Store.Session(*wakeup.SessionID)
			if agentErr == nil && agent != nil && agent.ClosedAt == nil {
				return true
			}
		case WakeSuppressionWhileActive:
			if s.Service.Agents == nil {
				continue
			}
			agent := s.Service.Agents.Get(*wakeup.SessionID)
			if agent == nil {
				continue
			}
			status := string(agent.Status())
			if status == "working" || status == "blocked" {
				return true
			}
		}
	}
	return false
}

func (s *Scheduler) wakeSuppressionReason(job storepkg.AutomationJob) string {
	if job.WakeSuppression == WakeSuppressionWhileActive {
		return "a linked wake agent is working or blocked"
	}
	return "a linked wake agent remains open"
}

func repositoryRootFromID(repositoryID string) (string, error) {
	clean := filepath.Clean(repositoryID)
	if filepath.Base(clean) != ".git" {
		return "", fmt.Errorf("cannot derive worktree root from repository identity %q", repositoryID)
	}
	root := filepath.Dir(clean)
	if _, err := resolveRepository(context.Background(), root); err != nil {
		return "", err
	}
	return root, nil
}

func nextOccurrence(expression, timezone string, after time.Time) (time.Time, error) {
	location := time.Local
	if timezone != "" {
		var err error
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return time.Time{}, fmt.Errorf("load timezone %q: %w", timezone, err)
		}
	}
	expression = strings.TrimSpace(expression)
	if strings.HasPrefix(expression, "every ") {
		duration, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(expression, "every ")))
		if err != nil || duration <= 0 {
			return time.Time{}, fmt.Errorf("invalid interval schedule %q", expression)
		}
		return after.Add(duration), nil
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("schedule must be 'every <duration>' or a five-field cron expression")
	}
	sets := make([]map[int]bool, 5)
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i := range fields {
		set, err := parseCronField(fields[i], ranges[i][0], ranges[i][1])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron field %q: %w", fields[i], err)
		}
		sets[i] = set
	}
	candidate := after.In(location).Truncate(time.Minute).Add(time.Minute)
	deadline := candidate.AddDate(5, 0, 0)
	for candidate.Before(deadline) {
		weekday := int(candidate.Weekday())
		dayOfMonth, dayOfWeek := sets[2][candidate.Day()], sets[4][weekday]
		dayMatches := dayOfMonth && dayOfWeek
		if fields[2] != "*" && fields[4] != "*" {
			// Traditional five-field cron treats day-of-month and day-of-week as
			// alternatives when both are restricted.
			dayMatches = dayOfMonth || dayOfWeek
		}
		if sets[0][candidate.Minute()] && sets[1][candidate.Hour()] && dayMatches && sets[3][int(candidate.Month())] {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron expression has no occurrence within five years")
}

func parseCronField(field string, min, max int) (map[int]bool, error) {
	values := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		step := 1
		base := part
		if left, right, ok := strings.Cut(part, "/"); ok {
			base = left
			parsed, err := strconv.Atoi(right)
			if err != nil || parsed <= 0 {
				return nil, fmt.Errorf("invalid step")
			}
			step = parsed
		}
		lo, hi := min, max
		if base != "*" {
			if left, right, ok := strings.Cut(base, "-"); ok {
				var err error
				lo, err = strconv.Atoi(left)
				if err != nil {
					return nil, err
				}
				hi, err = strconv.Atoi(right)
				if err != nil {
					return nil, err
				}
			} else {
				value, err := strconv.Atoi(base)
				if err != nil {
					return nil, err
				}
				lo, hi = value, value
			}
		}
		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("value outside %d-%d", min, max)
		}
		for value := lo; value <= hi; value += step {
			values[value] = true
		}
	}
	return values, nil
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// MarshalRegisteredSpec produces the stable subset used to detect frontmatter
// changes that require explicit schedule synchronization.
func MarshalRegisteredSpec(manifest Manifest) []byte {
	raw, _ := json.Marshal(struct {
		Schedule *ScheduleSpec `json:"schedule,omitempty"`
		Browser  *BrowserSpec  `json:"browser,omitempty"`
		Wake     *WakeSpec     `json:"wake,omitempty"`
	}{manifest.Schedule, manifest.Browser, manifest.Wake})
	return raw
}
