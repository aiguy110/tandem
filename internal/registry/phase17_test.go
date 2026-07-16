package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

type phaseFactory struct {
	mu            sync.Mutex
	requests      []agentadapter.StartRequest
	adapters      map[string]*phaseAdapter
	failPTY       bool
	failACPReload bool
}

func (f *phaseFactory) Start(_ context.Context, req agentadapter.StartRequest) (agentadapter.Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if req.Spec.Adapter == "pty" && f.failPTY {
		return nil, errors.New("CLI failed")
	}
	if req.Spec.Adapter == "acp" && req.ResumeSessionID != "" && f.failACPReload {
		return nil, errors.New("reload failed")
	}
	sid := req.ResumeSessionID
	if sid == "" && req.Spec.Adapter == "acp" {
		sid = "sess_mock"
	}
	a := &phaseAdapter{events: make(chan eventlog.Event, 16), done: make(chan struct{}), sid: sid, prompt: make(chan struct{})}
	if f.adapters == nil {
		f.adapters = map[string]*phaseAdapter{}
	}
	f.adapters[req.AgentID] = a
	return a, nil
}

type phaseAdapter struct {
	mu     sync.Mutex
	events chan eventlog.Event
	done   chan struct{}
	sid    string
	prompt chan struct{}
	once   sync.Once
}

func (a *phaseAdapter) Capabilities() agentadapter.Capabilities {
	return agentadapter.Capabilities{Structured: a.sid != "", LoadSession: a.sid != ""}
}
func (a *phaseAdapter) Events() <-chan eventlog.Event { return a.events }
func (a *phaseAdapter) Done() <-chan struct{}         { return a.done }
func (a *phaseAdapter) Prompt(ctx context.Context, _ []agentadapter.PromptBlock) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.prompt:
		return "cancelled", nil
	}
}
func (a *phaseAdapter) SendInput(b []byte) error {
	if strings.Contains(string(b), "exit") {
		a.stop()
	}
	return nil
}
func (a *phaseAdapter) Resize(uint16, uint16) error            { return nil }
func (a *phaseAdapter) RespondPermission(string, string) error { return nil }
func (a *phaseAdapter) Interrupt() error {
	select {
	case <-a.prompt:
	default:
		close(a.prompt)
	}
	return nil
}
func (a *phaseAdapter) Close(context.Context) error { a.stop(); return nil }
func (a *phaseAdapter) stop()                       { a.once.Do(func() { close(a.events); close(a.done) }) }
func (a *phaseAdapter) SessionID() string           { return a.sid }
func (a *phaseAdapter) PID() int                    { return 1 }

func phaseSetup(t *testing.T, factory *phaseFactory, launch config.Launch) (*Registry, *store.Store, config.Config) {
	t.Helper()
	home := t.TempDir()
	db, err := store.Open(filepath.Join(home, "db"))
	if err != nil {
		t.Fatal(err)
	}
	terminal := &config.ResumeLaunch{Cmd: "/absolute/mock-resume", Args: []string{"{sessionId}"}}
	cfg := config.Config{Home: home, WorktreesDir: filepath.Join(home, "worktrees"), ACP: config.ACPConfig{Default: "fake", Agents: map[string]config.Launch{"fake": launch}}, Agents: map[string]config.Agent{"fake": {ACP: &launch, Terminal: terminal}}, Profiles: map[string]config.Profile{}}
	r, err := New(Options{Store: db, Config: cfg, Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.DisposeAll(context.Background()); _ = db.Close() })
	return r, db, cfg
}

func TestPhase17CatalogAndResumePaths(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	mock, err := filepath.Abs(filepath.Join("..", "..", "daemon", "src", "mock-acp-agent.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	launch := config.Launch{Cmd: node, Args: []string{mock}}
	f := &phaseFactory{}
	r, db, _ := phaseSetup(t, f, launch)
	cwd := t.TempDir()
	live, err := r.Spawn(context.Background(), agentadapter.Spec{Adapter: "acp", Agent: "fake", Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: cwd}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := r.ResumeCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mockCount int
	var external bool
	var supports bool
	for _, s := range catalog.Sessions {
		if s.SessionID == "sess_mock" {
			mockCount++
			if s.Source != "tandem" {
				t.Fatalf("dedupe winner = %s", s.Source)
			}
		}
		if s.SessionID == "sess_external" {
			external = s.Source == "external"
		}
	}
	for _, a := range catalog.Adapters {
		if a.Agent == "fake" {
			supports = a.SupportsList
		}
	}
	if mockCount != 1 || !external || !supports {
		t.Fatalf("catalog %#v adapters %#v", catalog.Sessions, catalog.Adapters)
	}
	focused, err := r.Resume(context.Background(), "sess_mock", "", "")
	if err != nil || focused != live {
		t.Fatalf("live resume: %v %#v", err, focused)
	}
	if ok, err := r.Close(context.Background(), live.ID, false, false); err != nil || !ok {
		t.Fatal(err)
	}
	restored, err := r.Resume(context.Background(), "sess_mock", "", "")
	if err != nil || restored.ID != live.ID {
		t.Fatalf("closed resume: %v %#v", err, restored)
	}
	imported, err := r.Resume(context.Background(), "sess_external", "fake", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := db.Agent(imported.ID)
	if imported.ID == live.ID || rec == nil || rec.ACPSessionID == nil || *rec.ACPSessionID != "sess_external" {
		t.Fatalf("bad import %#v", rec)
	}
}

func TestPhase17UnsupportedEnumerationDoesNotFailCatalog(t *testing.T) {
	r, _, _ := phaseSetup(t, &phaseFactory{}, config.Launch{Cmd: filepath.Join(t.TempDir(), "missing")})
	catalog, err := r.ResumeCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Adapters) != 2 || catalog.Adapters[0].SupportsList {
		t.Fatalf("adapters %#v", catalog.Adapters)
	}
}

func TestPhase17HandoffBusyInterruptFailureAndReload(t *testing.T) {
	f := &phaseFactory{}
	r, _, _ := phaseSetup(t, f, config.Launch{Cmd: "fake"})
	s, err := r.Spawn(context.Background(), agentadapter.Spec{Adapter: "acp", Agent: "fake", Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	go s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "wait"}})
	deadline := time.Now().Add(time.Second)
	for !s.ActiveTurn() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := r.EnterTerminal(context.Background(), s.ID, false); err == nil || !strings.HasPrefix(err.Error(), "agent_busy:") {
		t.Fatalf("busy error %v", err)
	}
	if err := r.EnterTerminal(context.Background(), s.ID, true); err != nil {
		t.Fatal(err)
	}
	if s.ControlMode() != "terminal" {
		t.Fatalf("mode %s", s.ControlMode())
	}
	resumed, err := r.Resume(context.Background(), "sess_mock", "", "")
	if err != nil || resumed != s {
		t.Fatalf("terminal live resume %v", err)
	}
	f.mu.Lock()
	terminal := f.adapters[s.ID]
	f.mu.Unlock()
	if err := terminal.SendInput([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for s.ControlMode() != "transcript" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.ControlMode() != "transcript" || s.SessionID() != "sess_mock" {
		t.Fatalf("reload mode=%s sid=%s", s.ControlMode(), s.SessionID())
	}
	cmd, _ := r.ResumeCLICommand(s.ID)
	if !filepath.IsAbs(cmd) {
		t.Fatalf("resume command %q", cmd)
	}

	f.failPTY = true
	if err := r.EnterTerminal(context.Background(), s.ID, false); err == nil || !strings.Contains(err.Error(), "CLI failed") {
		t.Fatalf("CLI failure %v", err)
	}
	if s.ControlMode() != "transcript" {
		t.Fatalf("CLI failure recovery mode %s", s.ControlMode())
	}
	f.failPTY, f.failACPReload = false, true
	if err := r.EnterTerminal(context.Background(), s.ID, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	terminal = f.adapters[s.ID]
	f.mu.Unlock()
	terminal.SendInput([]byte("exit"))
	deadline = time.Now().Add(time.Second)
	for s.ControlMode() == "terminal" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.ControlMode() != "switching" {
		t.Fatalf("reload failure mode %s", s.ControlMode())
	}
	var history []eventlog.LoggedEvent
	found := false
	deadline = time.Now().Add(time.Second)
	for !found && time.Now().Before(deadline) {
		history, _ = s.Log.FullHistory()
		for _, le := range history {
			if le.Event.Kind == "error" {
				found = true
			}
		}
		time.Sleep(time.Millisecond)
	}
	if !found {
		b, _ := json.Marshal(history)
		t.Fatalf("missing reload error: %s", b)
	}
}
