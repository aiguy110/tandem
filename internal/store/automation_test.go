package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRepositoryToolGrants(t *testing.T) {
	s, _ := openTestStore(t)
	s.now = func() time.Time { return time.UnixMilli(100) }

	if err := s.GrantRepositoryTool(RepositoryToolGrant{RepositoryID: "repo-a", ToolName: "browser.find", ApprovalID: "approval-1", GrantedBy: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantRepositoryTool(RepositoryToolGrant{RepositoryID: "repo-a", ToolName: "browser.click", GrantedAt: 50}); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantRepositoryTool(RepositoryToolGrant{RepositoryID: "repo-b", ToolName: "browser.find"}); err != nil {
		t.Fatal(err)
	}
	has, err := s.RepositoryHasToolGrant("repo-a", "browser.find")
	if err != nil || !has {
		t.Fatalf("has=%v err=%v", has, err)
	}
	grants, err := s.RepositoryToolGrants("repo-a")
	if err != nil {
		t.Fatal(err)
	}
	want := []RepositoryToolGrant{
		{RepositoryID: "repo-a", ToolName: "browser.click", GrantedAt: 50},
		{RepositoryID: "repo-a", ToolName: "browser.find", ApprovalID: "approval-1", GrantedBy: "user-1", GrantedAt: 100},
	}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("grants=%#v want=%#v", grants, want)
	}
	if err := s.RevokeRepositoryTool("repo-a", "browser.find"); err != nil {
		t.Fatal(err)
	}
	has, err = s.RepositoryHasToolGrant("repo-a", "browser.find")
	if err != nil || has {
		t.Fatalf("after revoke has=%v err=%v", has, err)
	}
	revoked, err := s.RepositoryToolGrantRecord("repo-a", "browser.find")
	if err != nil || revoked == nil || revoked.RevokedAt == nil || *revoked.RevokedAt != 100 {
		t.Fatalf("revoked=%#v err=%v", revoked, err)
	}
}

func TestAutomationJobLifecycle(t *testing.T) {
	s, _ := openTestStore(t)
	s.now = func() time.Time { return time.UnixMilli(100) }
	job := AutomationJob{
		ID: "job-1", RepositoryID: "repo-a", ScriptPath: ".tandem/scripts/check.ts", Name: "check",
		Cron: "*/5 * * * *", Timezone: "America/New_York", BrowserSnapshotID: "snap-1",
		DefaultAgentProfile: "triage", WakePrompt: "Review the result", ManifestHash: "abc", Enabled: true,
	}
	if err := s.UpsertAutomationJob(job); err != nil {
		t.Fatal(err)
	}
	got, err := s.AutomationJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.CreatedAt != 100 || got.UpdatedAt != 100 || got.Concurrency != "skip" || !got.Enabled {
		t.Fatalf("job=%#v", got)
	}
	s.now = func() time.Time { return time.UnixMilli(200) }
	job.Cron, job.CreatedAt, job.UpdatedAt = "0 * * * *", 999, 0
	if err := s.UpsertAutomationJob(job); err != nil {
		t.Fatal(err)
	}
	got, _ = s.AutomationJob(job.ID)
	if got.CreatedAt != 100 || got.UpdatedAt != 200 || got.Cron != "0 * * * *" {
		t.Fatalf("updated job=%#v", got)
	}
	if err := s.SetAutomationJobEnabled(job.ID, false); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.AutomationJobs("repo-a")
	if err != nil || len(jobs) != 1 || jobs[0].Enabled || jobs[0].UpdatedAt != 200 {
		t.Fatalf("jobs=%#v err=%v", jobs, err)
	}
	missing, err := s.AutomationJob("missing")
	if err != nil || missing != nil {
		t.Fatalf("missing=%#v err=%v", missing, err)
	}
	if err := s.DeleteAutomationJob(job.ID); err != nil {
		t.Fatal(err)
	}
	missing, err = s.AutomationJob(job.ID)
	if err != nil || missing != nil {
		t.Fatalf("deleted=%#v err=%v", missing, err)
	}
}

func TestAutomationRunWakeupAndToolAudit(t *testing.T) {
	s, _ := openTestStore(t)
	job := AutomationJob{ID: "job-1", RepositoryID: "repo-a", ScriptPath: ".tandem/scripts/check.ts", Cron: "@hourly", Enabled: true}
	if err := s.UpsertAutomationJob(job); err != nil {
		t.Fatal(err)
	}
	scheduled, completed := int64(10), int64(30)
	exitCode := 0
	run := AutomationRun{
		ID: "run-1", JobID: ptr("job-1"), RepositoryID: "repo-a", ScriptPath: job.ScriptPath,
		Trigger: "schedule", SourceHash: "source-1", BrowserSnapshotID: "snap-1", BrowserCloneID: "clone-1", ScheduledAt: &scheduled,
		StartedAt: 20, CompletedAt: &completed, Outcome: "succeeded_wake_requested", Reason: "report", ExitCode: &exitCode,
		Stdout: "found\n", Report: json.RawMessage(`{"wakeAgent":true,"agentProfile":"special","context":{"messageId":"m-1"}}`),
	}
	if err := s.SaveAutomationRun(run); err != nil {
		t.Fatal(err)
	}
	agentID := "agent-9"
	wakeup := AutomationWakeup{
		RunID: run.ID, JobID: run.JobID, AgentID: &agentID, AgentProfile: "special", Prompt: "Review it",
		Reason: "report", Context: json.RawMessage(`{"messageId":"m-1"}`), Status: "working", CreatedAt: 31,
	}
	if err := s.SaveAutomationWakeup(wakeup); err != nil {
		t.Fatal(err)
	}
	callDone := int64(26)
	call := AutomationToolCall{ID: "call-1", RunID: run.ID, ToolName: "browser.find", Arguments: json.RawMessage(`{"text":"reply"}`), Result: json.RawMessage(`{"count":1}`), StartedAt: 25, CompletedAt: &callDone}
	if err := s.SaveAutomationToolCall(call); err != nil {
		t.Fatal(err)
	}

	gotRun, err := s.AutomationRun(run.ID)
	if err != nil || !reflect.DeepEqual(gotRun, &run) {
		t.Fatalf("run=%#v want=%#v err=%v", gotRun, &run, err)
	}
	gotWakeup, err := s.AutomationWakeup(run.ID)
	if err != nil || !reflect.DeepEqual(gotWakeup, &wakeup) {
		t.Fatalf("wakeup=%#v want=%#v err=%v", gotWakeup, &wakeup, err)
	}
	wakeups, err := s.AutomationWakeups(job.ID)
	if err != nil || !reflect.DeepEqual(wakeups, []AutomationWakeup{wakeup}) {
		t.Fatalf("wakeups=%#v err=%v", wakeups, err)
	}
	calls, err := s.AutomationToolCalls(run.ID)
	if err != nil || !reflect.DeepEqual(calls, []AutomationToolCall{call}) {
		t.Fatalf("calls=%#v want=%#v err=%v", calls, []AutomationToolCall{call}, err)
	}
	runs, err := s.AutomationRuns(job.ID, 1)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	active := AutomationRun{ID: "run-active", JobID: ptr(job.ID), RepositoryID: "repo-a", ScriptPath: job.ScriptPath, Trigger: "schedule", StartedAt: 40, Outcome: "running"}
	if err := s.SaveAutomationRun(active); err != nil {
		t.Fatal(err)
	}
	gotActive, err := s.ActiveAutomationRun(job.ID)
	if err != nil || !reflect.DeepEqual(gotActive, &active) {
		t.Fatalf("active=%#v want=%#v err=%v", gotActive, &active, err)
	}

	badRun := run
	badRun.ID, badRun.Report = "bad", json.RawMessage(`{`)
	if err := s.SaveAutomationRun(badRun); err == nil {
		t.Fatal("malformed report was accepted")
	}
	badWakeup := wakeup
	badWakeup.Context = json.RawMessage(`]`)
	if err := s.SaveAutomationWakeup(badWakeup); err == nil {
		t.Fatal("malformed wakeup context was accepted")
	}
	badCall := call
	badCall.Arguments = json.RawMessage(`nope`)
	if err := s.SaveAutomationToolCall(badCall); err == nil {
		t.Fatal("malformed tool arguments were accepted")
	}
}
