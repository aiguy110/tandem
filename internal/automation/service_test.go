package automation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	storepkg "github.com/aiguy110/tandem/internal/store"
)

func testAutomationService(t *testing.T) (*Service, repository) {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	db, err := storepkg.Open(filepath.Join(t.TempDir(), "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for automation service tests")
	}
	identity, err := resolveRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: db, Runner: Runner{NodeCommand: node}, Token: "secret"}, identity
}

func TestServiceExecutePersistsQuietAndFailedOutcomes(t *testing.T) {
	service, repo := testAutomationService(t)
	quiet, err := service.execute(context.Background(), repo, "<evaluate>", []byte(`
import { report } from "tandem:runtime";
console.log("checked");
report({wakeAgent: false, context: {count: 0}});
	`), Manifest{Name: "quiet"}, nil, "evaluate", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Status != "succeeded_quiet" || quiet.Stdout != "checked\n" || quiet.Report == nil || quiet.Report.WakeAgent {
		t.Fatalf("quiet=%+v", quiet)
	}
	storedQuiet, err := service.Store.AutomationRun(quiet.RunID)
	if err != nil || storedQuiet == nil || storedQuiet.Outcome != "succeeded_quiet" {
		t.Fatalf("stored quiet=%+v err=%v", storedQuiet, err)
	}

	failed, err := service.execute(context.Background(), repo, "<evaluate>", []byte(`
console.error("boom");
process.exit(7);
	`), Manifest{Name: "failed"}, nil, "evaluate", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.ExitCode != 7 || failed.Stderr != "boom\n" {
		t.Fatalf("failed=%+v", failed)
	}
	storedFailed, err := service.Store.AutomationRun(failed.RunID)
	if err != nil || storedFailed == nil || storedFailed.Reason != "script_failed" {
		t.Fatalf("stored failed=%+v err=%v", storedFailed, err)
	}
}

func TestRegisterJobPersistsWakeSuppression(t *testing.T) {
	service, repo := testAutomationService(t)
	job, err := service.registerJob(repo, Script{Path: ".tandem/scripts/check.ts", Manifest: Manifest{
		Name:     "check",
		Schedule: &ScheduleSpec{Cron: "every 5m"},
		Wake:     &WakeSpec{AgentProfile: "triage", Suppression: WakeSuppressionWhileActive},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if job.WakeSuppression != WakeSuppressionWhileActive {
		t.Fatalf("job=%+v", job)
	}
	stored, err := service.Store.AutomationJob(job.ID)
	if err != nil || stored == nil || stored.WakeSuppression != WakeSuppressionWhileActive {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestServiceHTTPRequiresBearerToken(t *testing.T) {
	service := &Service{Token: "secret"}
	request := httptest.NewRequest(http.MethodPost, "/internal/automation/run", nil)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResolveRepositorySharesIdentityAcrossWorktree(t *testing.T) {
	repo := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Tandem", "GIT_AUTHOR_EMAIL=tandem@example.test", "GIT_COMMITTER_NAME=Tandem", "GIT_COMMITTER_EMAIL=tandem@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit(repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "add", "README.md")
	runGit(repo, "commit", "-qm", "initial")
	worktree := filepath.Join(t.TempDir(), "worktree")
	runGit(repo, "worktree", "add", "-qb", "automation-test", worktree)
	mainID, err := resolveRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	worktreeID, err := resolveRepository(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	if mainID.ID != worktreeID.ID {
		t.Fatalf("main=%q worktree=%q", mainID.ID, worktreeID.ID)
	}
}
