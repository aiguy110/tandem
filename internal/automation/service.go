package automation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/registry"
	storepkg "github.com/aiguy110/tandem/internal/store"
)

// ToolSession is a run-scoped view of MCP servers. Implementations expose only
// the tools requested by the script and approved for its repository.
type ToolSession interface {
	Invoke(context.Context, string, json.RawMessage) (any, error)
	Close() error
}

type ToolSessionFactory func(context.Context, string, string, []string, string) (ToolSession, error)

type Service struct {
	Store          *storepkg.Store
	Agents         *registry.Registry
	Runner         Runner
	Tools          ToolSessionFactory
	CaptureBrowser func(context.Context, string) (kind, ref string, cleanup func(), err error)
	Now            func() time.Time
	Token          string
}

type RunRequest struct {
	SessionID    string   `json:"agentId"`
	WorkspaceCWD string   `json:"workspaceCwd"`
	Path         string   `json:"path"`
	Args         []string `json:"args,omitempty"`
}

type EvaluateRequest struct {
	SessionID    string           `json:"agentId"`
	WorkspaceCWD string           `json:"workspaceCwd"`
	Source       string           `json:"source"`
	Args         []string         `json:"args,omitempty"`
	Tools        []ToolPermission `json:"requestedTools,omitempty"`
	Snapshot     string           `json:"browserSnapshot,omitempty"`
}

type PreapproveRequest struct {
	SessionID        string `json:"agentId"`
	WorkspaceCWD     string `json:"workspaceCwd"`
	Path             string `json:"path"`
	RegisterSchedule bool   `json:"registerSchedule,omitempty"`
}

type InvocationResult struct {
	RunID        string          `json:"runId,omitempty"`
	Status       string          `json:"status"`
	Stdout       string          `json:"stdout,omitempty"`
	Stderr       string          `json:"stderr,omitempty"`
	ExitCode     int             `json:"exitCode,omitempty"`
	DurationMS   int64           `json:"durationMs,omitempty"`
	Report       *Report         `json:"report,omitempty"`
	WokenAgentID string          `json:"wokenAgentId,omitempty"`
	AgentProfile string          `json:"agentProfile,omitempty"`
	Context      json.RawMessage `json:"context,omitempty"`
	JobID        string          `json:"jobId,omitempty"`
	ApprovalID   string          `json:"approvalId,omitempty"`
}

type repository struct{ ID, Root string }

func (s *Service) Run(ctx context.Context, req RunRequest) (InvocationResult, error) {
	repo, sess, err := s.repositoryAndSession(ctx, req.SessionID, req.WorkspaceCWD)
	if err != nil {
		return InvocationResult{}, err
	}
	script, err := LoadScript(repo.Root, req.Path)
	if err != nil {
		return InvocationResult{}, err
	}
	approvalID, err := s.ensureGrants(ctx, sess, repo, script.Path, script.Manifest.Tools)
	if err != nil {
		return InvocationResult{Status: "rejected", ApprovalID: approvalID}, err
	}
	return s.execute(ctx, repo, script.Path, script.Source, script.Manifest, req.Args, "direct", nil, nil)
}

// ServeHTTP implements the authenticated contract consumed by tandem's
// internal scripts MCP bridge.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	want, got := "Bearer "+s.Token, r.Header.Get("Authorization")
	if s.Token == "" || len(want) != len(got) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		writeAutomationJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		writeAutomationJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	var result InvocationResult
	var err error
	switch strings.TrimPrefix(r.URL.Path, "/internal/automation/") {
	case "run":
		var req RunRequest
		if decodeErr := dec.Decode(&req); decodeErr != nil {
			err = fmt.Errorf("invalid run request: %w", decodeErr)
		} else {
			result, err = s.Run(r.Context(), req)
		}
	case "evaluate":
		var req EvaluateRequest
		if decodeErr := dec.Decode(&req); decodeErr != nil {
			err = fmt.Errorf("invalid evaluate request: %w", decodeErr)
		} else {
			result, err = s.Evaluate(r.Context(), req)
		}
	case "preapprove":
		var req PreapproveRequest
		if decodeErr := dec.Decode(&req); decodeErr != nil {
			err = fmt.Errorf("invalid preapprove request: %w", decodeErr)
		} else {
			result, err = s.Preapprove(r.Context(), req)
		}
	default:
		writeAutomationJSON(w, http.StatusNotFound, map[string]string{"error": "unknown automation operation"})
		return
	}
	if err != nil {
		writeAutomationJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "result": result})
		return
	}
	writeAutomationJSON(w, http.StatusOK, result)
}

func writeAutomationJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Service) Evaluate(ctx context.Context, req EvaluateRequest) (InvocationResult, error) {
	repo, sess, err := s.repositoryAndSession(ctx, req.SessionID, req.WorkspaceCWD)
	if err != nil {
		return InvocationResult{}, err
	}
	manifest := Manifest{Name: "ephemeral evaluation", Tools: req.Tools}
	if req.Snapshot != "" {
		manifest.Browser = &BrowserSpec{Snapshot: req.Snapshot}
	}
	if err := manifest.Validate(); err != nil {
		return InvocationResult{}, err
	}
	if _, err := s.ensureGrants(ctx, sess, repo, "<evaluate>", req.Tools); err != nil {
		return InvocationResult{Status: "rejected"}, err
	}
	return s.execute(ctx, repo, "<evaluate>", []byte(req.Source), manifest, req.Args, "evaluate", nil, nil)
}

func (s *Service) Preapprove(ctx context.Context, req PreapproveRequest) (InvocationResult, error) {
	repo, sess, err := s.repositoryAndSession(ctx, req.SessionID, req.WorkspaceCWD)
	if err != nil {
		return InvocationResult{}, err
	}
	script, err := LoadScript(repo.Root, req.Path)
	if err != nil {
		return InvocationResult{}, err
	}
	approvalID, err := s.ensureGrants(ctx, sess, repo, script.Path, script.Manifest.Tools)
	if err != nil {
		return InvocationResult{Status: "rejected", ApprovalID: approvalID}, err
	}
	result := InvocationResult{Status: "approved", ApprovalID: approvalID}
	if req.RegisterSchedule {
		job, err := s.registerJob(repo, script)
		if err != nil {
			return result, err
		}
		result.JobID = job.ID
	}
	return result, nil
}

func (s *Service) repositoryAndSession(ctx context.Context, sessionID, cwd string) (repository, interface {
	RequestPermission(context.Context, string, string, []agentadapter.ApprovalOption) (string, error)
}, error) {
	if s.Store == nil || s.Agents == nil {
		return repository{}, nil, errors.New("automation service is unavailable")
	}
	sess := s.Agents.Get(sessionID)
	if sess == nil {
		return repository{}, nil, fmt.Errorf("no such agent: %s", sessionID)
	}
	if strings.TrimSpace(cwd) == "" {
		return repository{}, nil, errors.New("workspaceCwd is required")
	}
	repo, err := resolveRepository(ctx, cwd)
	return repo, sess, err
}

func resolveRepository(ctx context.Context, cwd string) (repository, error) {
	root, err := gitOutput(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, fmt.Errorf("automation requires a Git repository: %w", err)
	}
	common, err := gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return repository{}, fmt.Errorf("resolve repository identity: %w", err)
	}
	realCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		return repository{}, fmt.Errorf("resolve repository common directory: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return repository{}, fmt.Errorf("resolve repository root: %w", err)
	}
	return repository{ID: filepath.Clean(realCommon), Root: realRoot}, nil
}

func gitOutput(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *Service) ensureGrants(ctx context.Context, sess interface {
	RequestPermission(context.Context, string, string, []agentadapter.ApprovalOption) (string, error)
}, repo repository, scriptPath string, requested []ToolPermission) (string, error) {
	missing := make([]ToolPermission, 0)
	for _, tool := range requested {
		granted, err := s.Store.RepositoryHasToolGrant(repo.ID, tool.Name)
		if err != nil {
			return "", err
		}
		if !granted {
			missing = append(missing, tool)
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	approvalID := "automation-grant-" + randomID()
	var details strings.Builder
	fmt.Fprintf(&details, "Allow %s to use Tandem MCP tools for repository %s?", scriptPath, filepath.Base(repo.Root))
	for _, tool := range missing {
		fmt.Fprintf(&details, "\n• %s — %s", tool.Name, tool.Reason)
	}
	details.WriteString("\nScripts are trusted programs with the Tandem process's normal host access; this grant controls only brokered MCP calls.")
	choice, err := sess.RequestPermission(ctx, approvalID, details.String(), []agentadapter.ApprovalOption{
		{OptionID: "allow", Name: "Allow all"}, {OptionID: "deny", Name: "Deny"},
	})
	if err != nil {
		return approvalID, err
	}
	if choice != "allow" {
		return approvalID, errors.New("repository MCP tool grant denied")
	}
	for _, tool := range missing {
		if err := s.Store.GrantRepositoryTool(storepkg.RepositoryToolGrant{RepositoryID: repo.ID, ToolName: tool.Name, ApprovalID: approvalID, GrantedBy: "user"}); err != nil {
			return approvalID, err
		}
	}
	return approvalID, nil
}

func (s *Service) execute(ctx context.Context, repo repository, scriptPath string, source []byte, manifest Manifest, args []string, trigger string, jobID *string, scheduledAt *int64) (InvocationResult, error) {
	now := s.now()
	runID := "run-" + randomID()
	sourceSum := sha256.Sum256(source)
	run := storepkg.AutomationRun{ID: runID, JobID: jobID, RepositoryID: repo.ID, ScriptPath: scriptPath,
		Trigger: trigger, SourceHash: hex.EncodeToString(sourceSum[:]), ScheduledAt: scheduledAt, StartedAt: now.UnixMilli(), Outcome: "running"}
	if manifest.Browser != nil {
		run.BrowserSnapshotID = manifest.Browser.Snapshot
		run.BrowserCloneID = runID
	}
	if err := s.Store.SaveAutomationRun(run); err != nil {
		return InvocationResult{}, err
	}

	names := make([]string, len(manifest.Tools))
	for i := range manifest.Tools {
		names[i] = manifest.Tools[i].Name
	}
	var tools ToolSession
	var err error
	if len(names) > 0 {
		if s.Tools == nil {
			err = errors.New("MCP tools are unavailable to automation")
		} else {
			tools, err = s.Tools(ctx, runID, repo.Root, names, run.BrowserSnapshotID)
		}
		if err != nil {
			return s.failRun(run, "runner_failed", err)
		}
		defer tools.Close()
	}
	runner := s.Runner
	runner.ToolHandler = s.auditToolHandler(runID, names, tools)
	var process Result
	if scriptPath == "<evaluate>" {
		process, err = runner.Evaluate(ctx, repo.Root, source, args)
	} else {
		process, err = runner.Run(ctx, repo.Root, scriptPath, args)
	}
	if err != nil {
		return s.failRun(run, "runner_failed", err)
	}
	completed := s.now().UnixMilli()
	exitCode := process.ExitCode
	run.CompletedAt, run.ExitCode = &completed, &exitCode
	run.Stdout, run.Stderr = process.Stdout, process.Stderr
	if process.Report != nil {
		run.Report, _ = json.Marshal(process.Report)
	}
	result := InvocationResult{RunID: runID, Stdout: process.Stdout, Stderr: process.Stderr, ExitCode: process.ExitCode,
		DurationMS: process.Duration.Milliseconds(), Report: process.Report}

	profile, reason := "", ""
	if process.ExitCode != 0 {
		run.Outcome = "failed"
		reason = "script_failed"
		if manifest.Wake != nil {
			profile = manifest.Wake.AgentProfile
		}
	} else if process.Report != nil && process.Report.WakeAgent {
		profile = process.Report.EffectiveAgentProfile(manifest)
		if profile == "" {
			return s.failRun(run, "runner_failed", errors.New("wakeAgent requested without an agent profile"))
		}
		run.Outcome, reason = "succeeded_wake_requested", "report"
	} else {
		run.Outcome = "succeeded_quiet"
	}
	run.Reason = reason
	if err := s.Store.SaveAutomationRun(run); err != nil {
		return InvocationResult{}, err
	}
	result.Status = run.Outcome
	if profile != "" {
		var browserKind, browserRef string
		cleanup := func() {}
		if run.BrowserCloneID != "" && s.CaptureBrowser != nil {
			browserKind, browserRef, cleanup, err = s.CaptureBrowser(ctx, run.BrowserCloneID)
			if err != nil {
				return result, fmt.Errorf("capture browser clone for wake: %w", err)
			}
		}
		defer cleanup()
		sessionID, wakeErr := s.wake(ctx, repo, run, manifest, profile, reason, process.Report, browserKind, browserRef)
		if wakeErr != nil {
			return result, wakeErr
		}
		result.WokenAgentID, result.AgentProfile = sessionID, profile
	}
	return result, nil
}

func (s *Service) auditToolHandler(runID string, allowed []string, tools ToolSession) ToolHandler {
	if tools == nil {
		return nil
	}
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	return func(ctx context.Context, name string, arguments json.RawMessage) (any, error) {
		if !set[name] {
			return nil, fmt.Errorf("tool %q was not declared for this script", name)
		}
		started := s.now().UnixMilli()
		call := storepkg.AutomationToolCall{ID: "call-" + randomID(), RunID: runID, ToolName: name, Arguments: arguments, StartedAt: started}
		_ = s.Store.SaveAutomationToolCall(call)
		value, err := tools.Invoke(ctx, name, arguments)
		completed := s.now().UnixMilli()
		call.CompletedAt = &completed
		if err != nil {
			message := err.Error()
			call.Error = &message
		} else {
			call.Result, _ = json.Marshal(value)
		}
		_ = s.Store.SaveAutomationToolCall(call)
		return value, err
	}
}

func (s *Service) wake(ctx context.Context, repo repository, run storepkg.AutomationRun, manifest Manifest, profile, reason string, report *Report, browserKind, browserRef string) (string, error) {
	contextValue := json.RawMessage(`{}`)
	if report != nil && len(report.Context) > 0 {
		contextValue = report.Context
	}
	prompt := ""
	if manifest.Wake != nil {
		prompt = manifest.Wake.Prompt
	}
	metadata, _ := json.MarshalIndent(map[string]any{"reason": reason, "runId": run.ID, "repository": repo.Root,
		"scriptPath": run.ScriptPath, "exitCode": run.ExitCode, "stdout": run.Stdout, "stderr": run.Stderr,
		"context": json.RawMessage(contextValue)}, "", "  ")
	task := strings.TrimSpace(prompt) + "\n\nAutomation handoff:\n```json\n" + string(metadata) + "\n```"
	wakeup := storepkg.AutomationWakeup{RunID: run.ID, JobID: run.JobID, AgentProfile: profile, Prompt: prompt,
		Reason: reason, Context: contextValue, Status: "spawning", CreatedAt: s.now().UnixMilli()}
	if err := s.Store.SaveAutomationWakeup(wakeup); err != nil {
		return "", err
	}
	agent, err := s.Agents.SpawnProfileSeeded(ctx, profile, repo.Root, task, browserKind, browserRef)
	if err != nil {
		wakeup.Status = "failed"
		_ = s.Store.SaveAutomationWakeup(wakeup)
		return "", err
	}
	wakeup.SessionID, wakeup.Status = &agent.ID, "working"
	if err := s.Store.SaveAutomationWakeup(wakeup); err != nil {
		return agent.ID, err
	}
	return agent.ID, nil
}

func (s *Service) failRun(run storepkg.AutomationRun, outcome string, cause error) (InvocationResult, error) {
	completed := s.now().UnixMilli()
	message := cause.Error()
	run.CompletedAt, run.Outcome, run.Error = &completed, outcome, &message
	_ = s.Store.SaveAutomationRun(run)
	return InvocationResult{RunID: run.ID, Status: outcome}, cause
}

func (s *Service) registerJob(repo repository, script Script) (storepkg.AutomationJob, error) {
	if script.Manifest.Schedule == nil {
		return storepkg.AutomationJob{}, errors.New("script has no schedule to register")
	}
	if _, err := nextOccurrence(script.Manifest.Schedule.Cron, script.Manifest.Schedule.Timezone, s.now()); err != nil {
		return storepkg.AutomationJob{}, err
	}
	manifestJSON := MarshalRegisteredSpec(script.Manifest)
	manifestSum := sha256.Sum256(manifestJSON)
	jobID := "job-" + randomID()
	createdAt := int64(0)
	if jobs, err := s.Store.AutomationJobs(repo.ID); err == nil {
		for _, existing := range jobs {
			if existing.ScriptPath == script.Path {
				jobID, createdAt = existing.ID, existing.CreatedAt
				break
			}
		}
	}
	job := storepkg.AutomationJob{ID: jobID, RepositoryID: repo.ID, ScriptPath: script.Path,
		Name: script.Manifest.Name, Cron: script.Manifest.Schedule.Cron, Timezone: script.Manifest.Schedule.Timezone,
		Concurrency: script.Manifest.Schedule.Concurrency, ManifestHash: hex.EncodeToString(manifestSum[:]), Enabled: true, CreatedAt: createdAt}
	if job.Concurrency == "" {
		job.Concurrency = "skip"
	}
	if script.Manifest.Browser != nil {
		job.BrowserSnapshotID = script.Manifest.Browser.Snapshot
	}
	if script.Manifest.Wake != nil {
		job.DefaultAgentProfile, job.WakePrompt = script.Manifest.Wake.AgentProfile, script.Manifest.Wake.Prompt
		job.WakeSuppression = script.Manifest.Wake.Suppression
	}
	if job.WakeSuppression == "" {
		job.WakeSuppression = WakeSuppressionUntilClosed
	}
	if err := s.Store.UpsertAutomationJob(job); err != nil {
		return storepkg.AutomationJob{}, err
	}
	return job, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func randomID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
