package automation

import (
	"encoding/json"
	"testing"
	"time"

	storepkg "github.com/aiguy110/tandem/internal/store"
)

func TestNextOccurrenceIntervalAndCron(t *testing.T) {
	base := time.Date(2026, time.August, 2, 12, 3, 17, 0, time.UTC)
	got, err := nextOccurrence("every 5m", "UTC", base)
	if err != nil || !got.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("interval=%s err=%v", got, err)
	}
	got, err = nextOccurrence("*/5 * * * *", "UTC", base)
	want := time.Date(2026, time.August, 2, 12, 5, 0, 0, time.UTC)
	if err != nil || !got.Equal(want) {
		t.Fatalf("cron=%s want=%s err=%v", got, want, err)
	}
}

func TestNextOccurrenceUsesTimezone(t *testing.T) {
	base := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	got, err := nextOccurrence("0 9 * * *", "America/New_York", base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hour() != 9 || got.Location().String() != "America/New_York" {
		t.Fatalf("got=%s (%s)", got, got.Location())
	}
}

func TestParseCronFieldRejectsInvalidRanges(t *testing.T) {
	if _, err := parseCronField("61", 0, 59); err == nil {
		t.Fatal("expected out-of-range error")
	}
	if _, err := parseCronField("*/0", 0, 59); err == nil {
		t.Fatal("expected invalid step error")
	}
}

func TestSchedulerReconcilesInterruptedRuns(t *testing.T) {
	service, repo := testAutomationService(t)
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	job := storepkg.AutomationJob{ID: "job-1", RepositoryID: repo.ID, ScriptPath: ".tandem/scripts/check.ts", Name: "check", Cron: "every 5m", Enabled: true}
	if err := service.Store.UpsertAutomationJob(job); err != nil {
		t.Fatal(err)
	}
	jobID := job.ID
	if err := service.Store.SaveAutomationRun(storepkg.AutomationRun{ID: "run-1", JobID: &jobID, RepositoryID: repo.ID, ScriptPath: job.ScriptPath, Trigger: "scheduled", StartedAt: now.Add(-time.Minute).UnixMilli(), Outcome: "running"}); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{Service: service, Now: service.Now}
	scheduler.reconcileInterruptedRuns()
	run, err := service.Store.AutomationRun("run-1")
	if err != nil || run == nil {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if run.Outcome != "runner_failed" || run.Reason != "daemon_restart" || run.CompletedAt == nil || run.Error == nil {
		t.Fatalf("run=%+v", run)
	}
}

func TestSchedulerWakeSuppressionUntilClosed(t *testing.T) {
	service, repo := testAutomationService(t)
	job := storepkg.AutomationJob{ID: "job-1", RepositoryID: repo.ID, ScriptPath: ".tandem/scripts/check.ts", Cron: "every 5m", Enabled: true}
	if err := service.Store.UpsertAutomationJob(job); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.UpsertSession(storepkg.Session{ID: "agent-1", Name: "wake", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	jobID, sessionID := job.ID, "agent-1"
	if err := service.Store.SaveAutomationRun(storepkg.AutomationRun{ID: "run-1", JobID: &jobID, RepositoryID: repo.ID, ScriptPath: job.ScriptPath, Trigger: "scheduled", StartedAt: 1, Outcome: "succeeded_wake_requested"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.SaveAutomationWakeup(storepkg.AutomationWakeup{RunID: "run-1", JobID: &jobID, SessionID: &sessionID, Status: "working", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{Service: service}
	if !scheduler.activeWakeup(job) {
		t.Fatal("an idle but open wake agent must suppress later ticks")
	}
	if err := service.Store.CloseSession(sessionID); err != nil {
		t.Fatal(err)
	}
	if scheduler.activeWakeup(job) {
		t.Fatal("a closed wake agent must no longer suppress later ticks")
	}
}

func TestSchedulerWakeSuppressionWhileActiveAllowsIdleAgent(t *testing.T) {
	service, repo := testAutomationService(t)
	job := storepkg.AutomationJob{ID: "job-1", RepositoryID: repo.ID, ScriptPath: ".tandem/scripts/check.ts", Cron: "every 5m", WakeSuppression: WakeSuppressionWhileActive, Enabled: true}
	if err := service.Store.UpsertAutomationJob(job); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.UpsertSession(storepkg.Session{ID: "agent-1", Name: "wake", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	jobID, sessionID := job.ID, "agent-1"
	if err := service.Store.SaveAutomationRun(storepkg.AutomationRun{ID: "run-1", JobID: &jobID, RepositoryID: repo.ID, ScriptPath: job.ScriptPath, Trigger: "scheduled", StartedAt: 1, Outcome: "succeeded_wake_requested"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.SaveAutomationWakeup(storepkg.AutomationWakeup{RunID: "run-1", JobID: &jobID, SessionID: &sessionID, Status: "working", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if (&Scheduler{Service: service}).activeWakeup(job) {
		t.Fatal("while_active must not suppress an idle wake agent")
	}
}
