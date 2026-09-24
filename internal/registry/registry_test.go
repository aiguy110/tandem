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
	"github.com/aiguy110/tandem/internal/runtimeinstall"
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
	if f.fail[r.SessionID] {
		return nil, errors.New("boom")
	}
	a := &regAdapter{events: make(chan eventlog.Event, 16), done: make(chan struct{}), gate: make(chan struct{}, 2), sid: "session-" + r.SessionID}
	f.mu.Lock()
	if f.adapters == nil {
		f.adapters = map[string]*regAdapter{}
	}
	if f.requests == nil {
		f.requests = map[string]agentadapter.StartRequest{}
	}
	f.adapters[r.SessionID] = a
	f.requests[r.SessionID] = r
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
func (a *regAdapter) ExternalSessionID() string { return a.sid }
func (a *regAdapter) PID() int                  { return 1 }

func setup(t *testing.T, f *fakeFactory) (*Registry, *store.Store, config.Config) {
	t.Helper()
	home := t.TempDir()
	db, err := store.Open(filepath.Join(home, "db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Home: home, RuntimeRoot: filepath.Join(home, "runtime"), WorktreesDir: filepath.Join(home, "worktrees"), ACP: config.ACPConfig{Default: "fake"}, Agents: map[string]config.Agent{"fake": {ACP: &config.Launch{Cmd: "fake"}, Terminal: &config.ResumeLaunch{Cmd: "fake"}}}, Harnesses: map[string]config.Harness{}}
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

func TestSummariesIncludeResolvedLaunchProfile(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	spec := existing(t.TempDir())
	spec.Profile = &agentadapter.ProfileSpec{ID: "profile-1", Model: "sonnet", Effort: "high", Permission: "acceptEdits", Snapshot: "snapshot-1"}
	s, err := r.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	summaries := r.Summaries(context.Background())
	if len(summaries) != 1 || summaries[0].ID != s.ID || summaries[0].Profile == nil {
		t.Fatalf("summaries=%+v", summaries)
	}
	if got := summaries[0].Profile; got.ID == "" || got.Model != "sonnet" || got.Effort != "high" || got.Permission != "acceptEdits" || got.Snapshot != "snapshot-1" {
		t.Fatalf("profile=%+v", got)
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
	if err := db.UpsertSession(store.Session{ID: "job-17", Name: "job-17", Spec: raw, CWD: t.TempDir(), Status: "idle", CreatedAt: 1, ClosedAt: &closed}); err != nil {
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
	// Streaming text is intentionally emitted at most four times per second.
	time.Sleep(300 * time.Millisecond)
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
	rec, err := db.Session(stableID)
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
		if err := db.UpsertSession(store.Session{ID: id, Name: id, Spec: raw, CWD: dir, ExternalSessionID: &sid, Status: "working", CreatedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Get("web-1") == nil || r.Get("api-2") != nil {
		t.Fatal("partial restore isolation failed")
	}
	failed, _ := db.Session("api-2")
	if failed.Status != "error" {
		t.Fatalf("failed status=%s", failed.Status)
	}
	ok, err := r.Close(context.Background(), "web-1", false, false, false)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	ok, err = r.Close(context.Background(), "web-1", false, false, false)
	if ok || err != nil {
		t.Fatal(ok, err)
	}
	closed, _ := db.Session("web-1")
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
	sessionID := s.ExternalSessionID()
	if ok, err := r.Close(context.Background(), s.ID, false, false, false); !ok || err != nil {
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
	rec, err := db.Session(s.ID)
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
	rec, err := db.Session(s.ID)
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
	cwdRec, _ := r.store.Session(s.ID)
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
	if retained == nil || retained.Tandem == nil || retained.Tandem.SessionID != s.ID || !retained.Tandem.Live || retained.Tandem.Closed || retained.Tandem.IntegrationRef != targetRef {
		t.Fatalf("live retained ref=%+v", retained)
	}
	ok, err := r.Close(context.Background(), s.ID, false, true, false)
	if ok || !workspace.IsCode(err, "dirty_worktree") {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if r.Get(s.ID) == nil {
		t.Fatal("refused close removed live session")
	}
	ok, err = r.Close(context.Background(), s.ID, true, true, false)
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

func TestResumeTandemSessionRecreatesMissingWorktreeByDurableID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	repo := t.TempDir()
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(repo, "init")
	run(repo, "commit", "--allow-empty", "-m", "init")

	original, err := r.Spawn(context.Background(), agentadapter.Spec{Adapter: "acp", Workspace: workspace.Workspace{Kind: workspace.KindWorktree, Repo: repo}})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.store.Session(original.ID)
	if err != nil || rec == nil {
		t.Fatalf("session record=%+v err=%v", rec, err)
	}
	cwd := rec.CWD
	branch := original.Spec.Workspace.Branch
	if ok, err := r.Close(context.Background(), original.ID, false, true, false); !ok || err != nil {
		t.Fatalf("close worktree ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("closed worktree remains: %v", err)
	}

	resumed, err := r.Resume(context.Background(), original.ID, "fake", cwd, "tandem")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != original.ID {
		t.Fatalf("resumed ID=%s want %s", resumed.ID, original.ID)
	}
	if _, err := os.Stat(cwd); err != nil {
		t.Fatalf("recreated worktree missing: %v", err)
	}
	if got := run(cwd, "branch", "--show-current"); got != branch {
		t.Fatalf("recreated branch=%q want %q", got, branch)
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
	if err := db.UpsertSession(store.Session{ID: "orphan", Name: "orphan", Spec: spec, CWD: cwd, Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	closed, err := r.Close(context.Background(), "orphan", true, true, false)
	if err != nil || !closed {
		t.Fatalf("closed=%v err=%v", closed, err)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("orphaned worktree remains: %v", err)
	}
	rec, err := db.Session("orphan")
	if err != nil || rec == nil || rec.ClosedAt == nil {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
}

// writePiDist lays down a lockfile entry and the dist file EnsureManaged probes,
// so it resolves from the lockfile without shelling out to npm.
func writePiDist(t *testing.T, runtimeRoot, version string, fork *runtimeinstall.LockedFork) string {
	t.Helper()
	root := filepath.Join(runtimeRoot, "agents", "pi", version)
	dist := filepath.Join(root, "node_modules", "pi-acp", "dist", "index.js")
	if err := os.MkdirAll(filepath.Dir(dist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dist, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := runtimeinstall.Lockfile{Version: 1, Agents: map[string]runtimeinstall.LockedAgent{
		"pi": {Package: "pi-acp", Constraint: "~0.0.33", Version: version, Path: root, Fork: fork},
	}}
	raw, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeRoot, "agents.lock.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dist
}

func piSpecPinnedTo(dir, version, entry string) agentadapter.Spec {
	spec := existing(dir)
	spec.Agent = "pi"
	spec.ResolvedLaunch = &agentadapter.ResolvedLaunch{
		ACP:          &agentadapter.Launch{Cmd: "node", Args: []string{entry}},
		Distribution: &agentadapter.Distribution{Source: "npm", Package: "pi-acp", Version: version, Path: filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(entry))))},
	}
	return spec
}

func TestRefreshDistributionRepinsWhenTheResolutionMoved(t *testing.T) {
	r, _, cfg := setup(t, &fakeFactory{})
	// The runtime now resolves pi to a fork build, while the session is still
	// pinned to the published version it was spawned on.
	forkEntry := writePiDist(t, cfg.RuntimeRoot, "0.0.33+fork.abc123def456",
		&runtimeinstall.LockedFork{Repo: "https://github.com/me/pi-acp", Ref: "tandem", Commit: "abc123def456", UpstreamVersion: "0.0.33"})
	oldEntry := filepath.Join(cfg.RuntimeRoot, "agents", "pi", "0.0.31", "node_modules", "pi-acp", "dist", "index.js")
	spec := piSpecPinnedTo(t.TempDir(), "0.0.31", oldEntry)

	from := r.refreshDistribution(context.Background(), "agent-1", &spec)
	if from != "0.0.31" {
		t.Fatalf("expected a repin away from 0.0.31, got %q", from)
	}
	if got := spec.ResolvedLaunch.Distribution.Version; got != "0.0.33+fork.abc123def456" {
		t.Errorf("distribution version=%q", got)
	}
	if got := spec.ResolvedLaunch.ACP.Args[0]; got != forkEntry {
		t.Errorf("acp args[0]=%q want %q", got, forkEntry)
	}
}

func TestRefreshDistributionIsANoopWhenAlreadyCurrent(t *testing.T) {
	r, _, cfg := setup(t, &fakeFactory{})
	entry := writePiDist(t, cfg.RuntimeRoot, "0.0.33", nil)
	spec := piSpecPinnedTo(t.TempDir(), "0.0.33", entry)

	if from := r.refreshDistribution(context.Background(), "agent-1", &spec); from != "" {
		t.Fatalf("expected no repin, got %q", from)
	}
	if got := spec.ResolvedLaunch.ACP.Args[0]; got != entry {
		t.Errorf("acp args[0] changed to %q", got)
	}
}

func TestRefreshDistributionLeavesUnmanagedAndMalformedSpecsAlone(t *testing.T) {
	r, _, _ := setup(t, &fakeFactory{})
	// An unmanaged agent launches an arbitrary command Tandem never provisions.
	custom := existing(t.TempDir())
	custom.Agent = "some-custom-agent"
	custom.ResolvedLaunch = &agentadapter.ResolvedLaunch{ACP: &agentadapter.Launch{Cmd: "custom", Args: []string{"serve"}}}
	if from := r.refreshDistribution(context.Background(), "agent-1", &custom); from != "" {
		t.Fatalf("unmanaged agent was repinned: %q", from)
	}
	if custom.ResolvedLaunch.ACP.Args[0] != "serve" {
		t.Error("unmanaged launch args were rewritten")
	}
	// A spec with nothing to rewrite must not panic.
	bare := existing(t.TempDir())
	bare.Agent = "pi"
	if from := r.refreshDistribution(context.Background(), "agent-1", &bare); from != "" {
		t.Fatalf("bare spec was repinned: %q", from)
	}
}

func TestRestartHarnessAdoptsANewDistributionAndPersistsIt(t *testing.T) {
	f := &fakeFactory{}
	r, db, cfg := setup(t, f)
	dir := t.TempDir()
	oldEntry := filepath.Join(cfg.RuntimeRoot, "agents", "pi", "0.0.31", "node_modules", "pi-acp", "dist", "index.js")
	s, err := r.Spawn(context.Background(), piSpecPinnedTo(dir, "0.0.31", oldEntry))
	if err != nil {
		t.Fatal(err)
	}
	// The operator installs a newer adapter after this session was spawned.
	newEntry := writePiDist(t, cfg.RuntimeRoot, "0.0.33", nil)

	if err := r.RestartHarness(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	// The replacement process must be launched on the new distribution.
	f.mu.Lock()
	req := f.requests[s.ID]
	f.mu.Unlock()
	if got := req.Spec.ResolvedLaunch.ACP.Args[0]; got != newEntry {
		t.Fatalf("restarted on %q, want %q", got, newEntry)
	}
	// ...and the record must carry it, so a later restore does not fall back.
	rec, err := db.Session(s.ID)
	if err != nil || rec == nil {
		t.Fatalf("session record: %v", err)
	}
	var persisted agentadapter.Spec
	if err := json.Unmarshal(rec.Spec, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.ResolvedLaunch.Distribution.Version; got != "0.0.33" {
		t.Fatalf("persisted distribution=%q want 0.0.33", got)
	}
	if got := persisted.ResolvedLaunch.ACP.Args[0]; got != newEntry {
		t.Fatalf("persisted args[0]=%q want %q", got, newEntry)
	}
}

func TestRestoreKeepsThePinnedDistribution(t *testing.T) {
	f := &fakeFactory{}
	r, db, cfg := setup(t, f)
	dir := t.TempDir()
	oldEntry := filepath.Join(cfg.RuntimeRoot, "agents", "pi", "0.0.31", "node_modules", "pi-acp", "dist", "index.js")
	s, err := r.Spawn(context.Background(), piSpecPinnedTo(dir, "0.0.31", oldEntry))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID
	// A newer distribution becomes available while the session is not running.
	writePiDist(t, cfg.RuntimeRoot, "0.0.33", nil)
	r.DisposeAll(context.Background())

	restored, err := New(Options{Store: db, Config: cfg, Factory: f, RingCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.DisposeAll(context.Background()) })
	if err := restored.RestoreAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Restore must not move a live session onto different code behind the
	// operator's back; only an explicit harness restart re-resolves.
	f.mu.Lock()
	req := f.requests[id]
	f.mu.Unlock()
	if got := req.Spec.ResolvedLaunch.ACP.Args[0]; got != oldEntry {
		t.Fatalf("restore changed the distribution to %q; it must stay pinned to %q", got, oldEntry)
	}
}

// gatedFactory holds every Start until release is closed, recording how many
// starts were in flight at once.
type gatedFactory struct {
	fakeFactory
	release  chan struct{}
	mu       sync.Mutex
	inFlight int
	peak     int
}

func (f *gatedFactory) Start(ctx context.Context, r agentadapter.StartRequest) (agentadapter.Adapter, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	f.mu.Unlock()
	<-f.release
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return f.fakeFactory.Start(ctx, r)
}

func TestStartRestoreServesPlaceholdersAndRestoresConcurrently(t *testing.T) {
	f := &gatedFactory{release: make(chan struct{})}
	r, db, _ := setup(t, &f.fakeFactory)
	r.factory = f
	ids := []string{"a-1", "b-2", "c-3"}
	for i, id := range ids {
		dir := t.TempDir()
		raw, _ := json.Marshal(existing(dir))
		if err := db.UpsertSession(store.Session{ID: id, Name: id, Spec: raw, CWD: dir, Status: "idle", CreatedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	var restoredMu sync.Mutex
	var restored []string
	done, err := r.StartRestore(context.Background(), func(id string) {
		restoredMu.Lock()
		restored = append(restored, id)
		restoredMu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every agent is listed while its restore is still blocked.
	summaries := r.Summaries(context.Background())
	if len(summaries) != len(ids) {
		t.Fatalf("summaries during restore = %d, want %d", len(summaries), len(ids))
	}
	for i, summary := range summaries {
		if summary.ID != ids[i] || summary.Name != ids[i] || summary.Status != "idle" {
			t.Fatalf("placeholder summary %d = %+v", i, summary)
		}
	}
	got := make(chan bool)
	go func() { got <- r.Get("b-2") != nil }()
	select {
	case <-got:
		t.Fatal("Get returned before the agent's restore finished")
	case <-time.After(50 * time.Millisecond):
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		peak := f.peak
		f.mu.Unlock()
		if peak == len(ids) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peak concurrent restores = %d, want %d", peak, len(ids))
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(f.release)
	if !<-got {
		t.Fatal("Get did not return the restored agent")
	}
	<-done
	for _, id := range ids {
		if r.Get(id) == nil {
			t.Fatalf("%s was not restored", id)
		}
	}
	restoredMu.Lock()
	defer restoredMu.Unlock()
	if len(restored) != len(ids) {
		t.Fatalf("onRestored calls = %v", restored)
	}
	if n := len(r.Summaries(context.Background())); n != len(ids) {
		t.Fatalf("summaries after restore = %d, want %d", n, len(ids))
	}
}
