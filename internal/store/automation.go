package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// RepositoryToolGrant is a durable, repository-scoped authorization to call
// one Tandem-brokered tool. ApprovalID links the grant to the user-visible
// approval that created it; Tandem does not treat it as authorization itself.
type RepositoryToolGrant struct {
	RepositoryID string `json:"repositoryId"`
	ToolName     string `json:"toolName"`
	ApprovalID   string `json:"approvalId"`
	GrantedBy    string `json:"grantedBy"`
	GrantedAt    int64  `json:"grantedAt"`
	RevokedAt    *int64 `json:"revokedAt,omitempty"`
}

// AutomationJob is the registered scheduling configuration parsed from a
// repository script's frontmatter. ManifestHash lets callers detect that the
// current frontmatter differs without making source hashes an authorization
// boundary.
type AutomationJob struct {
	ID                  string `json:"id"`
	RepositoryID        string `json:"repositoryId"`
	ScriptPath          string `json:"scriptPath"`
	Name                string `json:"name"`
	Cron                string `json:"cron"`
	Timezone            string `json:"timezone"`
	BrowserSnapshotID   string `json:"browserSnapshotId"`
	DefaultAgentProfile string `json:"defaultAgentProfile"`
	WakePrompt          string `json:"wakePrompt"`
	WakeSuppression     string `json:"wakeSuppression"`
	Concurrency         string `json:"concurrency"`
	ManifestHash        string `json:"manifestHash"`
	Enabled             bool   `json:"enabled"`
	CreatedAt           int64  `json:"createdAt"`
	UpdatedAt           int64  `json:"updatedAt"`
}

// AutomationRun records both executions and non-executing scheduled
// occurrences (for example awaiting approval or skipped for concurrency).
// Report contains the last structured tandem:runtime report emitted by the
// script and is nil when no report was emitted.
type AutomationRun struct {
	ID                string          `json:"id"`
	JobID             *string         `json:"jobId,omitempty"`
	RepositoryID      string          `json:"repositoryId"`
	ScriptPath        string          `json:"scriptPath"`
	Trigger           string          `json:"trigger"`
	SourceHash        string          `json:"sourceHash"`
	BrowserSnapshotID string          `json:"browserSnapshotId"`
	BrowserCloneID    string          `json:"browserCloneId"`
	ScheduledAt       *int64          `json:"scheduledAt,omitempty"`
	StartedAt         int64           `json:"startedAt"`
	CompletedAt       *int64          `json:"completedAt,omitempty"`
	Outcome           string          `json:"outcome"`
	Reason            string          `json:"reason"`
	ExitCode          *int            `json:"exitCode,omitempty"`
	Stdout            string          `json:"stdout"`
	Stderr            string          `json:"stderr"`
	Report            json.RawMessage `json:"report,omitempty"`
	Error             *string         `json:"error,omitempty"`
}

// AutomationWakeup links a run to the agent selected by report() (or the
// frontmatter default). AgentProfile therefore records the effective profile,
// including a per-report override. Reason distinguishes an intentional report
// from script failure.
type AutomationWakeup struct {
	RunID        string          `json:"runId"`
	JobID        *string         `json:"jobId,omitempty"`
	SessionID      *string         `json:"sessionId,omitempty"`
	AgentProfile string          `json:"agentProfile"`
	Prompt       string          `json:"prompt"`
	Reason       string          `json:"reason"`
	Context      json.RawMessage `json:"context"`
	Status       string          `json:"status"`
	CreatedAt    int64           `json:"createdAt"`
	CompletedAt  *int64          `json:"completedAt,omitempty"`
}

// AutomationToolCall is the user-visible audit record for one tool invocation
// made through tandem:runtime. Result is nil until completion or when the call
// failed without a structured result.
type AutomationToolCall struct {
	ID          string          `json:"id"`
	RunID       string          `json:"runId"`
	ToolName    string          `json:"toolName"`
	Arguments   json.RawMessage `json:"arguments"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *string         `json:"error,omitempty"`
	StartedAt   int64           `json:"startedAt"`
	CompletedAt *int64          `json:"completedAt,omitempty"`
}

func (s *Store) GrantRepositoryTool(grant RepositoryToolGrant) error {
	if grant.GrantedAt == 0 {
		grant.GrantedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO repository_tool_grants (repositoryId, toolName, approvalId, grantedBy, grantedAt, revokedAt)
VALUES (?, ?, ?, ?, ?, NULL)
ON CONFLICT(repositoryId, toolName) DO UPDATE SET approvalId=excluded.approvalId, grantedBy=excluded.grantedBy,
grantedAt=excluded.grantedAt, revokedAt=NULL`,
		grant.RepositoryID, grant.ToolName, grant.ApprovalID, grant.GrantedBy, grant.GrantedAt)
	return err
}

func (s *Store) RevokeRepositoryTool(repositoryID, toolName string) error {
	_, err := s.db.Exec("UPDATE repository_tool_grants SET revokedAt=? WHERE repositoryId=? AND toolName=? AND revokedAt IS NULL", s.now().UnixMilli(), repositoryID, toolName)
	return err
}

func (s *Store) RepositoryHasToolGrant(repositoryID, toolName string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM repository_tool_grants WHERE repositoryId=? AND toolName=? AND revokedAt IS NULL)`, repositoryID, toolName).Scan(&exists)
	return exists != 0, err
}

// RepositoryToolGrantRecord returns the current grant record even when it has
// been revoked, so approval history remains inspectable.
func (s *Store) RepositoryToolGrantRecord(repositoryID, toolName string) (*RepositoryToolGrant, error) {
	var grant RepositoryToolGrant
	var revokedAt sql.NullInt64
	err := s.db.QueryRow(`SELECT repositoryId, toolName, approvalId, grantedBy, grantedAt, revokedAt
FROM repository_tool_grants WHERE repositoryId=? AND toolName=?`, repositoryID, toolName).
		Scan(&grant.RepositoryID, &grant.ToolName, &grant.ApprovalID, &grant.GrantedBy, &grant.GrantedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	setOptionalInt64(&grant.RevokedAt, revokedAt)
	return &grant, nil
}

func (s *Store) RepositoryToolGrants(repositoryID string) ([]RepositoryToolGrant, error) {
	rows, err := s.db.Query(`SELECT repositoryId, toolName, approvalId, grantedBy, grantedAt, revokedAt
FROM repository_tool_grants WHERE repositoryId=? AND revokedAt IS NULL ORDER BY toolName`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RepositoryToolGrant, 0)
	for rows.Next() {
		var grant RepositoryToolGrant
		var revokedAt sql.NullInt64
		if err := rows.Scan(&grant.RepositoryID, &grant.ToolName, &grant.ApprovalID, &grant.GrantedBy, &grant.GrantedAt, &revokedAt); err != nil {
			return nil, err
		}
		setOptionalInt64(&grant.RevokedAt, revokedAt)
		out = append(out, grant)
	}
	return out, rows.Err()
}

func (s *Store) UpsertAutomationJob(job AutomationJob) error {
	now := s.now().UnixMilli()
	if job.CreatedAt == 0 {
		job.CreatedAt = now
	}
	if job.UpdatedAt == 0 {
		job.UpdatedAt = now
	}
	if job.Concurrency == "" {
		job.Concurrency = "skip"
	}
	if job.WakeSuppression == "" {
		job.WakeSuppression = "until_closed"
	}
	_, err := s.db.Exec(`INSERT INTO automation_jobs
(id, repositoryId, scriptPath, name, cron, timezone, browserSnapshotId, defaultAgentProfile, wakePrompt, wakeSuppression, concurrency, manifestHash, enabled, createdAt, updatedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET repositoryId=excluded.repositoryId, scriptPath=excluded.scriptPath,
name=excluded.name, cron=excluded.cron, timezone=excluded.timezone, browserSnapshotId=excluded.browserSnapshotId,
defaultAgentProfile=excluded.defaultAgentProfile, wakePrompt=excluded.wakePrompt, wakeSuppression=excluded.wakeSuppression, concurrency=excluded.concurrency,
manifestHash=excluded.manifestHash, enabled=excluded.enabled, updatedAt=excluded.updatedAt`,
		job.ID, job.RepositoryID, job.ScriptPath, job.Name, job.Cron, job.Timezone, job.BrowserSnapshotID,
		job.DefaultAgentProfile, job.WakePrompt, job.WakeSuppression, job.Concurrency, job.ManifestHash, boolToInt(job.Enabled), job.CreatedAt, job.UpdatedAt)
	return err
}

func (s *Store) AutomationJob(id string) (*AutomationJob, error) {
	return scanAutomationJob(s.db.QueryRow(automationJobSelect+" WHERE id=?", id))
}

func (s *Store) AutomationJobs(repositoryID string) ([]AutomationJob, error) {
	query := automationJobSelect
	var args []any
	if repositoryID != "" {
		query += " WHERE repositoryId=?"
		args = append(args, repositoryID)
	}
	query += " ORDER BY createdAt, id"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AutomationJob, 0)
	for rows.Next() {
		job, err := scanAutomationJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	return out, rows.Err()
}

func (s *Store) SetAutomationJobEnabled(id string, enabled bool) error {
	_, err := s.db.Exec("UPDATE automation_jobs SET enabled=?, updatedAt=? WHERE id=?", boolToInt(enabled), s.now().UnixMilli(), id)
	return err
}

func (s *Store) DeleteAutomationJob(id string) error {
	_, err := s.db.Exec("DELETE FROM automation_jobs WHERE id=?", id)
	return err
}

const automationJobSelect = `SELECT id, repositoryId, scriptPath, name, cron, timezone, browserSnapshotId,
defaultAgentProfile, wakePrompt, wakeSuppression, concurrency, manifestHash, enabled, createdAt, updatedAt FROM automation_jobs`

func scanAutomationJob(row scanner) (*AutomationJob, error) {
	var job AutomationJob
	var enabled int
	if err := row.Scan(&job.ID, &job.RepositoryID, &job.ScriptPath, &job.Name, &job.Cron, &job.Timezone,
		&job.BrowserSnapshotID, &job.DefaultAgentProfile, &job.WakePrompt, &job.WakeSuppression, &job.Concurrency, &job.ManifestHash,
		&enabled, &job.CreatedAt, &job.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	job.Enabled = enabled != 0
	return &job, nil
}

// SaveAutomationRun inserts a run or persists its latest lifecycle state.
func (s *Store) SaveAutomationRun(run AutomationRun) error {
	report, err := nullableJSON(run.Report, "automation report")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO automation_runs
(id, jobId, repositoryId, scriptPath, trigger, sourceHash, browserSnapshotId, browserCloneId, scheduledAt, startedAt, completedAt, outcome, reason, exitCode, stdout, stderr, report, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET jobId=excluded.jobId, repositoryId=excluded.repositoryId, scriptPath=excluded.scriptPath,
trigger=excluded.trigger, sourceHash=excluded.sourceHash, browserSnapshotId=excluded.browserSnapshotId, browserCloneId=excluded.browserCloneId,
scheduledAt=excluded.scheduledAt, startedAt=excluded.startedAt, completedAt=excluded.completedAt,
outcome=excluded.outcome, reason=excluded.reason, exitCode=excluded.exitCode, stdout=excluded.stdout, stderr=excluded.stderr,
report=excluded.report, error=excluded.error`,
		run.ID, run.JobID, run.RepositoryID, run.ScriptPath, run.Trigger, run.SourceHash, run.BrowserSnapshotID,
		run.BrowserCloneID, run.ScheduledAt, run.StartedAt, run.CompletedAt, run.Outcome, run.Reason, run.ExitCode, run.Stdout, run.Stderr, report, run.Error)
	return err
}

func (s *Store) AutomationRun(id string) (*AutomationRun, error) {
	return scanAutomationRun(s.db.QueryRow(automationRunSelect+" WHERE id=?", id))
}

// ActiveAutomationRun returns the newest unfinished run for a job. The
// scheduler uses this to implement concurrency without relying on memory that
// is lost during daemon restart.
func (s *Store) ActiveAutomationRun(jobID string) (*AutomationRun, error) {
	return scanAutomationRun(s.db.QueryRow(automationRunSelect+" WHERE jobId=? AND completedAt IS NULL ORDER BY startedAt DESC LIMIT 1", jobID))
}

// AutomationRuns returns newest runs first. An empty jobID includes all jobs;
// limit <= 0 means no limit.
func (s *Store) AutomationRuns(jobID string, limit int) ([]AutomationRun, error) {
	query := automationRunSelect
	var args []any
	if jobID != "" {
		query += " WHERE jobId=?"
		args = append(args, jobID)
	}
	query += " ORDER BY startedAt DESC, id DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AutomationRun, 0)
	for rows.Next() {
		run, err := scanAutomationRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

const automationRunSelect = `SELECT id, jobId, repositoryId, scriptPath, trigger, sourceHash, browserSnapshotId,
browserCloneId, scheduledAt, startedAt, completedAt, outcome, reason, exitCode, stdout, stderr, report, error FROM automation_runs`

func scanAutomationRun(row scanner) (*AutomationRun, error) {
	var run AutomationRun
	var jobID, report, runError sql.NullString
	var scheduledAt, completedAt, exitCode sql.NullInt64
	if err := row.Scan(&run.ID, &jobID, &run.RepositoryID, &run.ScriptPath, &run.Trigger, &run.SourceHash,
		&run.BrowserSnapshotID, &run.BrowserCloneID, &scheduledAt, &run.StartedAt, &completedAt, &run.Outcome, &run.Reason, &exitCode,
		&run.Stdout, &run.Stderr, &report, &runError); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	setOptionalString(&run.JobID, jobID)
	setOptionalInt64(&run.ScheduledAt, scheduledAt)
	setOptionalInt64(&run.CompletedAt, completedAt)
	if exitCode.Valid {
		v := int(exitCode.Int64)
		run.ExitCode = &v
	}
	if report.Valid {
		if !json.Valid([]byte(report.String)) {
			return nil, fmt.Errorf("automation run %q has malformed report JSON", run.ID)
		}
		run.Report = json.RawMessage(report.String)
	}
	setOptionalString(&run.Error, runError)
	return &run, nil
}

func (s *Store) SaveAutomationWakeup(wakeup AutomationWakeup) error {
	context, err := requiredJSON(wakeup.Context, "automation wakeup context")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO automation_wakeups
(runId, jobId, sessionId, agentProfile, prompt, reason, context, status, createdAt, completedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(runId) DO UPDATE SET jobId=excluded.jobId, sessionId=excluded.sessionId, agentProfile=excluded.agentProfile,
prompt=excluded.prompt, reason=excluded.reason, context=excluded.context, status=excluded.status, completedAt=excluded.completedAt`,
		wakeup.RunID, wakeup.JobID, wakeup.SessionID, wakeup.AgentProfile, wakeup.Prompt, wakeup.Reason,
		context, wakeup.Status, wakeup.CreatedAt, wakeup.CompletedAt)
	return err
}

func (s *Store) AutomationWakeup(runID string) (*AutomationWakeup, error) {
	return scanAutomationWakeup(s.db.QueryRow(automationWakeupSelect+" WHERE runId=?", runID))
}

// AutomationWakeups returns a job's wakeups newest first. Lifecycle filtering
// belongs to the service because agent terminal-state policy is not a storage
// concern.
func (s *Store) AutomationWakeups(jobID string) ([]AutomationWakeup, error) {
	rows, err := s.db.Query(automationWakeupSelect+" WHERE jobId=? ORDER BY createdAt DESC, runId DESC", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AutomationWakeup, 0)
	for rows.Next() {
		wakeup, err := scanAutomationWakeup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *wakeup)
	}
	return out, rows.Err()
}

const automationWakeupSelect = `SELECT runId, jobId, sessionId, agentProfile, prompt, reason, context, status, createdAt, completedAt FROM automation_wakeups`

func scanAutomationWakeup(row scanner) (*AutomationWakeup, error) {
	var wakeup AutomationWakeup
	var jobID, sessionID sql.NullString
	var completedAt sql.NullInt64
	var context string
	if err := row.Scan(&wakeup.RunID, &jobID, &sessionID, &wakeup.AgentProfile, &wakeup.Prompt, &wakeup.Reason,
		&context, &wakeup.Status, &wakeup.CreatedAt, &completedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !json.Valid([]byte(context)) {
		return nil, fmt.Errorf("automation wakeup for run %q has malformed context JSON", wakeup.RunID)
	}
	wakeup.Context = json.RawMessage(context)
	setOptionalString(&wakeup.JobID, jobID)
	setOptionalString(&wakeup.SessionID, sessionID)
	setOptionalInt64(&wakeup.CompletedAt, completedAt)
	return &wakeup, nil
}

func (s *Store) SaveAutomationToolCall(call AutomationToolCall) error {
	arguments, err := requiredJSON(call.Arguments, "automation tool call arguments")
	if err != nil {
		return err
	}
	result, err := nullableJSON(call.Result, "automation tool call result")
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO automation_tool_calls
(id, runId, toolName, arguments, result, error, startedAt, completedAt) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET runId=excluded.runId, toolName=excluded.toolName, arguments=excluded.arguments,
result=excluded.result, error=excluded.error, startedAt=excluded.startedAt, completedAt=excluded.completedAt`,
		call.ID, call.RunID, call.ToolName, arguments, result, call.Error, call.StartedAt, call.CompletedAt)
	return err
}

func (s *Store) AutomationToolCalls(runID string) ([]AutomationToolCall, error) {
	rows, err := s.db.Query(`SELECT id, runId, toolName, arguments, result, error, startedAt, completedAt
FROM automation_tool_calls WHERE runId=? ORDER BY startedAt, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AutomationToolCall, 0)
	for rows.Next() {
		var call AutomationToolCall
		var arguments string
		var result, callError sql.NullString
		var completedAt sql.NullInt64
		if err := rows.Scan(&call.ID, &call.RunID, &call.ToolName, &arguments, &result, &callError, &call.StartedAt, &completedAt); err != nil {
			return nil, err
		}
		if !json.Valid([]byte(arguments)) || result.Valid && !json.Valid([]byte(result.String)) {
			return nil, fmt.Errorf("automation tool call %q has malformed JSON", call.ID)
		}
		call.Arguments = json.RawMessage(arguments)
		if result.Valid {
			call.Result = json.RawMessage(result.String)
		}
		setOptionalString(&call.Error, callError)
		setOptionalInt64(&call.CompletedAt, completedAt)
		out = append(out, call)
	}
	return out, rows.Err()
}

func requiredJSON(raw json.RawMessage, label string) (string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("%s is not valid JSON", label)
	}
	return string(raw), nil
}

func nullableJSON(raw json.RawMessage, label string) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s is not valid JSON", label)
	}
	return string(raw), nil
}

func setOptionalString(dst **string, value sql.NullString) {
	if value.Valid {
		v := value.String
		*dst = &v
	}
}

func setOptionalInt64(dst **int64, value sql.NullInt64) {
	if value.Valid {
		v := value.Int64
		*dst = &v
	}
}
