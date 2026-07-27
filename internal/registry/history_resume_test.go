package registry

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

func addImportedHistory(t *testing.T, db *store.Store, agent, id, cwd, title string, resumable bool) {
	t.Helper()
	updated := time.Now().UnixMilli()
	err := db.ReplaceHistorySession(store.HistorySession{
		Source: "history", Agent: agent, ExternalID: id, CWD: cwd, Title: title,
		UpdatedAt: &updated, Resumable: resumable, SourceKey: agent + "/" + id,
		SourceMeta: json.RawMessage(`{}`),
	}, []store.HistoryEntry{{ExternalID: "1", Ordinal: 1, Role: "user", Kind: "message", Text: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
}

func historyRegistry(t *testing.T, factory *phaseFactory, agents map[string]config.Agent) (*Registry, *store.Store) {
	t.Helper()
	home := t.TempDir()
	db, err := store.Open(filepath.Join(home, "db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Home: home, WorktreesDir: filepath.Join(home, "worktrees"),
		ACP:       config.ACPConfig{Default: "fake", Agents: map[string]config.Launch{}},
		Agents:    agents,
		Harnesses: map[string]config.Harness{},
	}
	for id, agent := range agents {
		if agent.ACP != nil {
			cfg.ACP.Agents[id] = *agent.ACP
		}
	}
	r, err := New(Options{Store: db, Config: cfg, Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.DisposeAll(context.Background()); _ = db.Close() })
	return r, db
}

func historyAgent(mode string) config.Agent {
	acp := &config.Launch{Cmd: "fake"}
	terminal := &config.ResumeLaunch{Cmd: "/absolute/mock-resume", Args: []string{"--resume", "{sessionId}", "--cwd", "{cwd}"}}
	return config.Agent{
		ACP: acp, Terminal: terminal,
		History: &config.History{Parser: "/absolute/parser.ts", Resume: mode, Enabled: true},
	}
}

func TestHistoryCatalogIdentityPrecedenceAndEnrichment(t *testing.T) {
	f := &phaseFactory{}
	r, db := historyRegistry(t, f, map[string]config.Agent{
		"fake":  historyAgent("auto"),
		"other": historyAgent("auto"),
	})
	cwd := t.TempDir()
	live, err := r.Spawn(context.Background(), agentadapter.Spec{
		Adapter: "acp", Agent: "fake",
		Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: cwd},
	})
	if err != nil {
		t.Fatal(err)
	}
	addImportedHistory(t, db, "fake", "sess_mock", cwd, "Imported title", true)
	addImportedHistory(t, db, "other", "sess_mock", cwd, "Other vendor title", true)

	catalog, err := r.ResumeCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var fake, other *ResumableSession
	for i := range catalog.Sessions {
		s := &catalog.Sessions[i]
		if s.Agent == "fake" && s.SessionID == "sess_mock" {
			fake = s
		}
		if s.Agent == "other" && s.SessionID == "sess_mock" {
			other = s
		}
	}
	if fake == nil || fake.Source != "tandem" || fake.AgentID != live.ID ||
		fake.Title != "Imported title" || !fake.Resumable {
		t.Fatalf("Tandem precedence failed: %#v", fake)
	}
	if other == nil || other.Source != "history" || other.Title != "Other vendor title" {
		t.Fatalf("cross-agent identity collapsed: %#v", other)
	}

	if ok, err := r.Close(context.Background(), live.ID, false, false); err != nil || !ok {
		t.Fatal(err)
	}
	catalog, err = r.ResumeCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range catalog.Sessions {
		if s.Agent == "fake" && s.SessionID == "sess_mock" &&
			(s.Source != "tandem" || s.AgentID != live.ID || s.Closed == nil || !*s.Closed) {
			t.Fatalf("closed Tandem precedence failed: %#v", s)
		}
	}
	newLive, err := r.Spawn(context.Background(), agentadapter.Spec{
		Adapter: "acp", Agent: "fake",
		Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err = r.ResumeCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range catalog.Sessions {
		if s.Agent == "fake" && s.SessionID == "sess_mock" && s.AgentID != newLive.ID {
			t.Fatalf("live did not outrank closed Tandem row: %#v", s)
		}
	}
}

func TestHistoryOnlyFailureExplanations(t *testing.T) {
	agent := historyAgent("terminal")
	agent.Terminal.Args = nil
	r, db := historyRegistry(t, &phaseFactory{}, map[string]config.Agent{"fake": agent})
	addImportedHistory(t, db, "fake", "history-only", t.TempDir(), "History only", true)
	addImportedHistory(t, db, "fake", "vendor-disabled", t.TempDir(), "Disabled", false)

	catalog, err := r.ResumeCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, s := range catalog.Sessions {
		if s.Source == "history" {
			if s.Resumable || !s.HistoryOnly {
				t.Fatalf("expected history-only session: %#v", s)
			}
			reasons[s.SessionID] = s.ResumeError
		}
	}
	if !strings.Contains(reasons["history-only"], "resumeArgs") ||
		!strings.Contains(reasons["vendor-disabled"], "importer marked") {
		t.Fatalf("failure explanations: %#v", reasons)
	}
	if _, err := r.Resume(context.Background(), "history-only", "fake", t.TempDir(), "history"); err == nil || !strings.Contains(err.Error(), "resumeArgs") {
		t.Fatalf("resume failure = %v", err)
	}
}

func TestHistoryTerminalResumeUsesConfiguredArgs(t *testing.T) {
	f := &phaseFactory{}
	r, db := historyRegistry(t, f, map[string]config.Agent{"fake": historyAgent("terminal")})
	cwd := t.TempDir()
	addImportedHistory(t, db, "fake", "terminal-session", cwd, "Terminal", true)

	s, err := r.Resume(context.Background(), "terminal-session", "fake", t.TempDir(), "history")
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec.Adapter != "pty" {
		t.Fatalf("adapter = %q", s.Spec.Adapter)
	}
	f.mu.Lock()
	requests := append([]agentadapter.StartRequest{}, f.requests...)
	f.mu.Unlock()
	if len(requests) != 1 || requests[0].Spec.Adapter != "pty" {
		t.Fatalf("requests = %#v", requests)
	}
	args := requests[0].Spec.ResolvedLaunch.Terminal.StartArgs
	if strings.Join(args, " ") != "--resume terminal-session --cwd "+cwd {
		t.Fatalf("terminal args = %#v", args)
	}
	rec, _ := db.Agent(s.ID)
	if rec == nil || rec.ACPSessionID == nil || *rec.ACPSessionID != "terminal-session" {
		t.Fatalf("durable terminal resume = %#v", rec)
	}
}

func TestHistoryAutoACPFailureCleansUpBeforeTerminalFallback(t *testing.T) {
	f := &phaseFactory{failACPReload: true}
	r, db := historyRegistry(t, f, map[string]config.Agent{"fake": historyAgent("auto")})
	cwd := t.TempDir()
	addImportedHistory(t, db, "fake", "auto-session", cwd, "Auto", true)

	s, err := r.Resume(context.Background(), "auto-session", "fake", cwd, "history")
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec.Adapter != "pty" {
		t.Fatalf("fallback adapter = %q", s.Spec.Adapter)
	}
	rows, err := db.AllAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != s.ID {
		t.Fatalf("failed ACP left persisted state: %#v", rows)
	}
	f.mu.Lock()
	requests := append([]agentadapter.StartRequest{}, f.requests...)
	f.mu.Unlock()
	if len(requests) != 2 || requests[0].Spec.Adapter != "acp" || requests[1].Spec.Adapter != "pty" {
		t.Fatalf("fallback requests = %#v", requests)
	}
}

func TestHistoryExplicitACPResume(t *testing.T) {
	f := &phaseFactory{}
	r, db := historyRegistry(t, f, map[string]config.Agent{"fake": historyAgent("acp")})
	cwd := t.TempDir()
	addImportedHistory(t, db, "fake", "acp-session", cwd, "ACP", true)
	s, err := r.Resume(context.Background(), "acp-session", "fake", cwd, "history")
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec.Adapter != "acp" || s.SessionID() != "acp-session" {
		t.Fatalf("ACP resume = adapter %q session %q", s.Spec.Adapter, s.SessionID())
	}
}
