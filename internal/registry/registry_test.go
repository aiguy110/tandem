package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

type fakeFactory struct {
	mu       sync.Mutex
	fail     map[string]bool
	adapters map[string]*regAdapter
	requests map[string]agentadapter.StartRequest
}

func (f *fakeFactory) Start(_ context.Context, r agentadapter.StartRequest) (agentadapter.Adapter, error) {
	if f.fail[r.AgentID] {
		return nil, errors.New("boom")
	}
	a := &regAdapter{events: make(chan eventlog.Event, 16), done: make(chan struct{}), gate: make(chan struct{}, 2), sid: "session-" + r.AgentID}
	f.mu.Lock()
	if f.adapters == nil {
		f.adapters = map[string]*regAdapter{}
	}
	if f.requests == nil {
		f.requests = map[string]agentadapter.StartRequest{}
	}
	f.adapters[r.AgentID] = a
	f.requests[r.AgentID] = r
	f.mu.Unlock()
	return a, nil
}

type regAdapter struct {
	events chan eventlog.Event
	done   chan struct{}
	gate   chan struct{}
	sid    string
	once   sync.Once
	mu     sync.Mutex
	modes  []string
	config map[string]any
}

func (a *regAdapter) Capabilities() agentadapter.Capabilities {
	return agentadapter.Capabilities{Structured: true, LoadSession: true}
}
func (a *regAdapter) Events() <-chan eventlog.Event { return a.events }
func (a *regAdapter) Done() <-chan struct{}         { return a.done }
func (a *regAdapter) Prompt(ctx context.Context, _ []agentadapter.PromptBlock) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.gate:
		return "end_turn", nil
	}
}
func (a *regAdapter) SendInput([]byte) error                 { return nil }
func (a *regAdapter) Resize(uint16, uint16) error            { return nil }
func (a *regAdapter) RespondPermission(string, string) error { return nil }
func (a *regAdapter) Interrupt() error                       { return nil }
func (a *regAdapter) SetMode(_ context.Context, mode string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.modes = append(a.modes, mode)
	return nil
}
func (a *regAdapter) SetConfigOption(_ context.Context, id string, value any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.config == nil {
		a.config = make(map[string]any)
	}
	a.config[id] = value
	return nil
}
func (a *regAdapter) Close(context.Context) error {
	a.once.Do(func() { close(a.events); close(a.done) })
	return nil
}
func (a *regAdapter) SessionID() string { return a.sid }
func (a *regAdapter) PID() int          { return 1 }

func setup(t *testing.T, f *fakeFactory) (*Registry, *store.Store, config.Config) {
	t.Helper()
	home := t.TempDir()
	db, err := store.Open(filepath.Join(home, "db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Home: home, WorktreesDir: filepath.Join(home, "worktrees"), ACP: config.ACPConfig{Default: "fake"}, Agents: map[string]config.Agent{"fake": {ACP: &config.Launch{Cmd: "fake"}, Terminal: &config.ResumeLaunch{Cmd: "fake"}}}, Harnesses: map[string]config.Harness{}}
	r, err := New(Options{Store: db, Config: cfg, Factory: f, RingCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.DisposeAll(context.Background()); db.Close() })
	return r, db, cfg
}
func existing(dir string) agentadapter.Spec {
	return agentadapter.Spec{Adapter: "acp", Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: dir}}
}

func TestSummariesReportUnknownWhenGitStateIsUnavailable(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	summaries := r.Summaries(context.Background())
	if len(summaries) != 1 || summaries[0].ID != s.ID || summaries[0].Workspace.GitState != "unknown" {
		t.Fatalf("summaries=%+v", summaries)
	}
}

func TestListWorkspaceEntriesIsRelativeSortedAndContained(t *testing.T) {
	r, _, _ := setup(t, &fakeFactory{})
	parent := t.TempDir()
	cwd := filepath.Join(parent, "workspace")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "README.md"), []byte("readme"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := r.Spawn(context.Background(), existing(cwd))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.ListWorkspaceEntries(context.Background(), s.ID, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []WorkspaceEntry{{Path: "src", IsDir: true}, {Path: "README.md"}}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries=%+v want=%+v", entries, want)
	}
	if err := os.Mkdir(filepath.Join(parent, "sibling"), 0o755); err != nil {
		t.Fatal(err)
	}
	parentEntries, err := r.ListWorkspaceEntries(context.Background(), s.ID, "..")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parentEntries, []WorkspaceEntry{{Path: "../sibling", IsDir: true}, {Path: "../workspace", IsDir: true}}) {
		t.Fatalf("parent entries=%+v", parentEntries)
	}
	if _, err := r.ListWorkspaceEntries(context.Background(), s.ID, "../../outside"); err == nil {
		t.Fatal("expected path escaping the workspace parent to be rejected")
	}
	homeEntries, err := r.ListWorkspaceEntries(context.Background(), s.ID, "~")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range homeEntries {
		if !strings.HasPrefix(entry.Path, "~/") {
			t.Fatalf("home entry path=%q, want ~/ prefix", entry.Path)
		}
	}
	if _, err := r.ListWorkspaceEntries(context.Background(), s.ID, "~/.."); err == nil {
		t.Fatal("expected path escaping the home directory to be rejected")
	}
}

func TestCounterSeededAcrossClosedAndMultiAgentIsolation(t *testing.T) {
	f := &fakeFactory{}
	_, db, cfg := setup(t, f)
	raw, _ := json.Marshal(existing(t.TempDir()))
	closed := time.Now().UnixMilli()
	if err := db.UpsertAgent(store.Agent{ID: "job-17", Name: "job-17", Spec: raw, CWD: t.TempDir(), Status: "idle", CreatedAt: 1, ClosedAt: &closed}); err != nil {
		t.Fatal(err)
	}
	r2, err := New(Options{Store: db, Config: cfg, Factory: f})
	if err != nil {
		t.Fatal(err)
	}
	a, err := r2.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r2.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "chandrasekhar-18" || b.ID != "bell-19" {
		t.Fatalf("names %s %s", a.ID, b.ID)
	}
	ev := func(text string) eventlog.Event {
		p, _ := json.Marshal(map[string]string{"kind": "message_chunk", "text": text})
		return eventlog.Event{Kind: "message_chunk", Payload: p}
	}
	f.adapters[a.ID].events <- ev("a")
	f.adapters[b.ID].events <- ev("b")
	time.Sleep(10 * time.Millisecond)
	ha, _ := a.Log.FullHistory()
	hb, _ := b.Log.FullHistory()
	if len(ha) != 1 || len(hb) != 1 || ha[0].Seq != 1 || hb[0].Seq != 1 {
		t.Fatalf("histories %#v %#v", ha, hb)
	}
	r2.DisposeAll(context.Background())
}

func TestRenameChangesDisplayNameWithoutChangingStableID(t *testing.T) {
	r, db, _ := setup(t, &fakeFactory{})
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	stableID := s.ID
	if err := r.Rename(stableID, "checkout investigation"); err != nil {
		t.Fatal(err)
	}
	if got := s.DisplayName(); got != "checkout investigation" {
		t.Fatalf("display name = %q", got)
	}
	if s.ID != stableID || s.Spec.Name != stableID {
		t.Fatalf("rename changed stable identity: id=%q spec.name=%q", s.ID, s.Spec.Name)
	}
	rec, err := db.Agent(stableID)
	if err != nil || rec == nil || rec.Name != "checkout investigation" {
		t.Fatalf("persisted agent=%+v err=%v", rec, err)
	}
}

func TestPartialRestoreAndRepeatedClose(t *testing.T) {
	f := &fakeFactory{fail: map[string]bool{"api-2": true}}
	r, db, _ := setup(t, f)
	for i, id := range []string{"web-1", "api-2"} {
		dir := t.TempDir()
		spec := existing(dir)
		raw, _ := json.Marshal(spec)
		sid := "old-" + id
		if err := db.UpsertAgent(store.Agent{ID: id, Name: id, Spec: raw, CWD: dir, ACPSessionID: &sid, Status: "working", CreatedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Get("web-1") == nil || r.Get("api-2") != nil {
		t.Fatal("partial restore isolation failed")
	}
	failed, _ := db.Agent("api-2")
	if failed.Status != "error" {
		t.Fatalf("failed status=%s", failed.Status)
	}
	ok, err := r.Close(context.Background(), "web-1", false, false)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	ok, err = r.Close(context.Background(), "web-1", false, false)
	if ok || err != nil {
		t.Fatal(ok, err)
	}
	closed, _ := db.Agent("web-1")
	if closed.ClosedAt == nil {
		t.Fatal("close transition not persisted")
	}
}

func TestRestoreUnpromptedAgentStartsFreshACPSession(t *testing.T) {
	f := &fakeFactory{}
	r, db, cfg := setup(t, f)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.DisposeAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	f2 := &fakeFactory{}
	r2, err := New(Options{Store: db, Config: cfg, Factory: f2, RingCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.DisposeAll(context.Background())
	if err = r2.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r2.Get(s.ID) == nil {
		t.Fatal("unprompted agent was not restored")
	}
	if got := f2.requests[s.ID].ResumeSessionID; got != "" {
		t.Fatalf("unprompted agent resumed ACP session %q, want a fresh session", got)
	}
}

func TestRestorePromptedAgentResumesACPSession(t *testing.T) {
	f := &fakeFactory{}
	r, db, cfg := setup(t, f)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	f.adapters[s.ID].gate <- struct{}{}
	if _, err = s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if err = r.DisposeAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	f2 := &fakeFactory{}
	r2, err := New(Options{Store: db, Config: cfg, Factory: f2, RingCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.DisposeAll(context.Background())
	if err = r2.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f2.requests[s.ID].ResumeSessionID; got != "session-"+s.ID {
		t.Fatalf("prompted agent resumed ACP session %q", got)
	}
}

func TestResumeUnpromptedTandemSessionStartsFreshACPSession(t *testing.T) {
	f := &fakeFactory{}
	r, db, _ := setup(t, f)
	spec := existing(t.TempDir())
	s, err := r.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := s.SessionID()
	if ok, err := r.Close(context.Background(), s.ID, false, false); !ok || err != nil {
		t.Fatal(ok, err)
	}

	resumed, err := r.Resume(context.Background(), sessionID, "", spec.Workspace.CWD, "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != s.ID {
		t.Fatalf("resumed wrong agent: got %s want %s", resumed.ID, s.ID)
	}
	if got := f.requests[s.ID].ResumeSessionID; got != "" {
		t.Fatalf("unprompted tandem session resumed ACP session %q, want a fresh session", got)
	}
	rec, err := db.Agent(s.ID)
	if err != nil || rec == nil || rec.ClosedAt != nil {
		t.Fatalf("agent not reopened: %+v err=%v", rec, err)
	}
}

func TestSessionConfigPersistsAndReapplies(t *testing.T) {
	f := &fakeFactory{}
	r, db, cfg := setup(t, f)
	initial, _ := json.Marshal(persistedSessionConfig{ModeID: "agent", ConfigOptions: map[string]any{"model": "one"}})
	spec := existing(t.TempDir())
	spec.SessionConfig = initial
	s, err := r.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	first := f.adapters[s.ID]
	if len(first.modes) != 1 || first.modes[0] != "agent" || first.config["model"] != "one" {
		t.Fatalf("initial config not applied: modes=%v config=%v", first.modes, first.config)
	}
	if err = r.SetMode(context.Background(), s.ID, "read-only"); err != nil {
		t.Fatal(err)
	}
	if err = r.SetConfigOption(context.Background(), s.ID, "model", "two"); err != nil {
		t.Fatal(err)
	}
	rec, err := db.Agent(s.ID)
	if err != nil || rec == nil {
		t.Fatal(err)
	}
	var stored agentadapter.Spec
	if err = json.Unmarshal(rec.Spec, &stored); err != nil {
		t.Fatal(err)
	}
	var saved persistedSessionConfig
	if err = json.Unmarshal(stored.SessionConfig, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ModeID != "read-only" || saved.ConfigOptions["model"] != "two" {
		t.Fatalf("persisted config = %+v", saved)
	}
	if err = r.DisposeAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeFactory{}
	r2, err := New(Options{Store: db, Config: cfg, Factory: f2, RingCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.DisposeAll(context.Background())
	if err = r2.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored := f2.adapters[s.ID]
	if restored == nil || len(restored.modes) != 1 || restored.modes[0] != "read-only" || restored.config["model"] != "two" {
		t.Fatalf("restored config: %#v", restored)
	}
}

func TestRecoverSessionConfigPrefersModeOption(t *testing.T) {
	_, db, _ := setup(t, &fakeFactory{})
	log, err := eventlog.New("legacy", db, 2)
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"session_config","modes":{"currentModeId":"stale"},"configOptions":[{"id":"mode","category":"mode","currentValue":"agent-full-access"},{"id":"reasoning_effort","category":"thought_level","currentValue":"high"}]}`)
	if _, err = log.Append(eventlog.Event{Kind: "session_config", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var recovered persistedSessionConfig
	if err = json.Unmarshal(recoverSessionConfig(log), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.ModeID != "" || recovered.ConfigOptions["mode"] != "agent-full-access" || recovered.ConfigOptions["reasoning_effort"] != "high" {
		t.Fatalf("recovered config = %+v", recovered)
	}
}

func TestDeferredShutdownWaitsForTurn(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "work"}})
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for r.ActiveTurnCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if r.WaitForIdle(ctx) {
		t.Fatal("reported idle during active turn")
	}
	f.adapters[s.ID].gate <- struct{}{}
	<-done
	if !r.WaitForIdle(context.Background()) {
		t.Fatal("did not become idle")
	}
}

func TestDirtyWorktreeRefusesClose(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init")
	run("commit", "--allow-empty", "-m", "init")
	s, err := r.Spawn(context.Background(), agentadapter.Spec{Adapter: "acp", Workspace: workspace.Workspace{Kind: workspace.KindWorktree, Repo: repo}})
	if err != nil {
		t.Fatal(err)
	}
	cwdRec, _ := r.store.Agent(s.ID)
	work := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwdRec.CWD
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e")
		if out, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("worktree git %v: %v %s", args, runErr, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cwdRec.CWD, "committed"), []byte("work"), 0644); err != nil {
		t.Fatal(err)
	}
	work("add", "committed")
	work("commit", "-m", "agent work")
	if err := os.WriteFile(filepath.Join(cwdRec.CWD, "dirty"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	preview, err := r.ClosePreview(context.Background(), s.ID)
	targetRef := s.Spec.Workspace.Integration.Ref
	if err != nil || preview == nil || !strings.Contains(preview.Uncommitted, "dirty") || !strings.Contains(preview.Unmerged, "agent work") || preview.TargetRef != targetRef {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	refs, err := r.ListGitRefs(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var retained *workspace.GitRefInfo
	for i := range refs {
		if refs[i].Ref == "refs/heads/"+s.Spec.Workspace.Branch {
			retained = &refs[i]
		}
	}
	if retained == nil || retained.Tandem == nil || retained.Tandem.AgentID != s.ID || !retained.Tandem.Live || retained.Tandem.Closed || retained.Tandem.IntegrationRef != targetRef {
		t.Fatalf("live retained ref=%+v", retained)
	}
	ok, err := r.Close(context.Background(), s.ID, false, true)
	if ok || !workspace.IsCode(err, "dirty_worktree") {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Get(s.ID) == nil {
		t.Fatal("refused close removed live session")
	}
	ok, err = r.Close(context.Background(), s.ID, true, true)
	if !ok || err != nil {
		t.Fatalf("forced close ok=%v err=%v", ok, err)
	}
	refs, err = r.ListGitRefs(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	retained = nil
	for i := range refs {
		if refs[i].Ref == "refs/heads/"+s.Spec.Workspace.Branch {
			retained = &refs[i]
		}
	}
	if retained == nil || retained.Tandem == nil || retained.Tandem.Live || !retained.Tandem.Closed || retained.Tandem.IntegrationRef != targetRef {
		t.Fatalf("closed retained ref=%+v", retained)
	}
}

func TestACPEnvironmentInheritsDaemonAndAppliesLaunchOverlay(t *testing.T) {
	t.Setenv("TANDEM_INHERITED_FIXTURE", "parent")
	t.Setenv("TANDEM_OVERLAID_FIXTURE", "parent")
	got := map[string]string{}
	for _, entry := range envList(map[string]string{"TANDEM_OVERLAID_FIXTURE": "launch", "TANDEM_ADDED_FIXTURE": "added"}) {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}
	if got["TANDEM_INHERITED_FIXTURE"] != "parent" || got["TANDEM_OVERLAID_FIXTURE"] != "launch" || got["TANDEM_ADDED_FIXTURE"] != "added" {
		t.Fatalf("merged environment = %#v", got)
	}
}

func TestCloseForceRemovesDurableOrphanWithoutLiveSession(t *testing.T) {
	f := &fakeFactory{}
	r, db, cfg := setup(t, f)
	cwd := filepath.Join(cfg.WorktreesDir, "missing-parent", "orphan")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(agentadapter.Spec{Adapter: "acp", Workspace: workspace.Workspace{
		Kind: workspace.KindWorktree,
		Repo: filepath.Join(t.TempDir(), "missing-parent"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAgent(store.Agent{ID: "orphan", Name: "orphan", Spec: spec, CWD: cwd, Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	closed, err := r.Close(context.Background(), "orphan", true, true)
	if err != nil || !closed {
		t.Fatalf("closed=%v err=%v", closed, err)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("orphaned worktree remains: %v", err)
	}
	rec, err := db.Agent("orphan")
	if err != nil || rec == nil || rec.ClosedAt == nil {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
}
