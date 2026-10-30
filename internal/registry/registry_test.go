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

	"github.com/aiguy110/tandem/internal/acpadapter"
	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/messaging"
	"github.com/aiguy110/tandem/internal/runtimeinstall"
	"github.com/aiguy110/tandem/internal/session"
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
	events     chan eventlog.Event
	done       chan struct{}
	gate       chan struct{}
	sid        string
	once       sync.Once
	mu         sync.Mutex
	modes      []string
	config     map[string]any
	interrupts int
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
func (a *regAdapter) Interrupt() error {
	a.mu.Lock()
	a.interrupts++
	a.mu.Unlock()
	select {
	case a.gate <- struct{}{}:
	default:
	}
	return nil
}
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
func (a *regAdapter) SessionState() acpadapter.SessionState {
	return acpadapter.SessionState{}
}

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

func TestSpawnOptionsUsesSelectedManagedDistribution(t *testing.T) {
	f := &fakeFactory{}
	r, _, cfg := setup(t, f)
	// The shipped launch names the legacy shared node_modules tree; spawning
	// replaces it with the distribution the lockfile selects.
	legacy := filepath.Join(cfg.RuntimeRoot, "node_modules", "pi-acp", "dist", "index.js")
	r.config.Agents["pi"] = config.Agent{ACP: &config.Launch{Cmd: "bun", Args: []string{legacy}}}
	entry := writePiDist(t, cfg.RuntimeRoot, "0.0.33", nil)

	if _, err := r.SpawnOptions(context.Background(), "pi", "", nil, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	req := f.requests["spawn-options-pi"]
	if req.Spec.ResolvedLaunch == nil || req.Spec.ResolvedLaunch.Distribution == nil {
		t.Fatalf("spawn-options request did not pin a managed distribution: %+v", req.Spec)
	}
	if got := req.Spec.ResolvedLaunch.Distribution.Version; got != "0.0.33" {
		t.Errorf("distribution version=%q want 0.0.33", got)
	}
	if got := req.Spec.ResolvedLaunch.ACP.Args[0]; got != entry {
		t.Errorf("ACP entry=%q want %q", got, entry)
	}
}

// An operator override that launches a managed agent some other way (here omp
// from PATH) must not be rewritten onto a managed distribution.
func TestSpawnOptionsLeavesOverriddenLaunchAlone(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	r.config.Agents["omp"] = config.Agent{ACP: &config.Launch{Cmd: "omp", Args: []string{"acp"}}}

	if _, err := r.SpawnOptions(context.Background(), "omp", "", nil, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	launch := f.requests["spawn-options-omp"].Spec.ResolvedLaunch
	if launch == nil || launch.Distribution != nil || launch.ACP.Cmd != "omp" || len(launch.ACP.Args) != 1 || launch.ACP.Args[0] != "acp" {
		t.Fatalf("overridden launch was rewritten: %+v", launch)
	}
}

func TestSummariesPreferLiveSessionDuringRestoreOverlap(t *testing.T) {
	r, _, _ := setup(t, &fakeFactory{})
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.restoring[s.ID] = restoringAgent{}
	r.mu.Unlock()
	summaries := r.Summaries(context.Background())
	if len(summaries) != 1 || summaries[0].ID != s.ID {
		t.Fatalf("expected one live summary during restore overlap, got %+v", summaries)
	}
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
	// Streaming text is intentionally emitted at most four times per second,
	// so poll rather than sleeping a fixed interval on a loaded machine.
	var ha, hb []eventlog.LoggedEvent
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		ha, _ = a.Log.FullHistory()
		hb, _ = b.Log.FullHistory()
		if len(ha) > 0 && len(hb) > 0 {
			break
		}
	}
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

func TestSessionsRailOrderIsNewestFirstAndReorderPersists(t *testing.T) {
	r, db, cfg := setup(t, &fakeFactory{})
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		s, err := r.Spawn(ctx, existing(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	order := func(r *Registry) []string {
		var out []string
		for _, s := range r.Summaries(ctx) {
			out = append(out, s.ID)
		}
		return out
	}
	if got, want := order(r), []string{ids[2], ids[1], ids[0]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spawn order = %v, want newest first %v", got, want)
	}
	if err := r.Reorder(ids[2], ids[0], true); err != nil {
		t.Fatal(err)
	}
	want := []string{ids[1], ids[0], ids[2]}
	if got := order(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("reordered = %v, want %v", got, want)
	}
	// A fresh registry over the same database (a daemon restart) keeps it.
	restarted, err := New(Options{Store: db, Config: cfg, Factory: &fakeFactory{}, RingCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.DisposeAll(context.Background()) })
	if err := restarted.RestoreAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := order(restarted); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored order = %v, want %v", got, want)
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
	// Restore launches nothing, so a broken harness only surfaces when that
	// agent is woken, and it does not take its neighbours down with it.
	if r.Get("web-1") == nil || r.Get("api-2") == nil {
		t.Fatal("agents were not restored dormant")
	}
	if err := r.Get("web-1").Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Get("api-2").Wake(context.Background()); err == nil || !r.Get("api-2").Dormant() {
		t.Fatalf("failed wake = %v, dormant = %v", err, r.Get("api-2").Dormant())
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
	restored := r2.Get(s.ID)
	if restored == nil {
		t.Fatal("unprompted agent was not restored")
	}
	if err = restored.Wake(context.Background()); err != nil {
		t.Fatal(err)
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
	if err = r2.Get(s.ID).Wake(context.Background()); err != nil {
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
	if err = r2.Get(s.ID).Wake(context.Background()); err != nil {
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
	f.mu.Lock()
	delete(f.requests, id)
	f.mu.Unlock()
	if err := restored.Get(id).Wake(context.Background()); err != nil {
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

func (f *fakeFactory) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestStartRestoreLeavesACPAgentsDormantUntilUsed(t *testing.T) {
	f := &fakeFactory{}
	r, db, _ := setup(t, f)
	ids := []string{"a-1", "b-2", "c-3"}
	for i, id := range ids {
		dir := t.TempDir()
		raw, _ := json.Marshal(existing(dir))
		status := "working"
		if i == 2 {
			status = "error"
		}
		if err := db.UpsertSession(store.Session{ID: id, Name: id, Spec: raw, CWD: dir, Status: status, CreatedAt: int64(i + 1)}); err != nil {
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
	<-done
	if n := f.startCount(); n != 0 {
		t.Fatalf("restore launched %d agent processes, want none", n)
	}
	for _, id := range ids {
		s := r.Get(id)
		if s == nil || !s.Dormant() {
			t.Fatalf("%s not restored dormant: %v", id, s)
		}
		// The turn (or failed process) died with the previous daemon.
		if s.Status() != session.Idle {
			t.Fatalf("%s status = %s, want idle", id, s.Status())
		}
	}
	if rec, _ := db.Session("c-3"); rec == nil || rec.Status != "idle" {
		t.Fatalf("stale error status was not cleared durably: %+v", rec)
	}
	restoredMu.Lock()
	if len(restored) != len(ids) {
		t.Fatalf("onRestored calls = %v", restored)
	}
	restoredMu.Unlock()
	if n := len(r.Summaries(context.Background())); n != len(ids) {
		t.Fatalf("summaries after restore = %d, want %d", n, len(ids))
	}

	// A prompt wakes only the agent it is sent to.
	receipt, err := r.Get("b-2").EnqueuePrompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.startCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("prompt did not wake the agent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	f.adapters["b-2"].gate <- struct{}{}
	f.mu.Unlock()
	_ = receipt
	for r.Get("b-2").ActiveTurn() || r.Get("b-2").Dormant() {
		if time.Now().After(deadline) {
			t.Fatal("woken agent did not finish its turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.startCount(); n != 1 || r.Get("a-1").Dormant() == false {
		t.Fatalf("starts = %d, a-1 dormant = %v; want only b-2 woken", n, r.Get("a-1").Dormant())
	}
}

func TestIdleAgentHibernatesAndWakesOnPrompt(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	first := f.adapters[s.ID]
	first.gate <- struct{}{}
	if _, err = s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	// Recently active agents are left alone.
	r.reapIdle(context.Background(), time.Hour)
	if s.Dormant() {
		t.Fatal("hibernated an agent that was not idle long enough")
	}
	r.reapIdle(context.Background(), 0)
	if !s.Dormant() {
		t.Fatal("idle agent was not hibernated")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("hibernation did not stop the agent process")
	}
	if got := s.ExternalSessionID(); got != "session-"+s.ID {
		t.Fatalf("dormant agent lost its external session id: %q", got)
	}

	f.mu.Lock()
	delete(f.requests, s.ID)
	f.mu.Unlock()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			f.mu.Lock()
			a := f.adapters[s.ID]
			f.mu.Unlock()
			if a != first {
				a.gate <- struct{}{}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, err = s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "again"}}); err != nil {
		t.Fatal(err)
	}
	if s.Dormant() {
		t.Fatal("prompt did not wake the agent")
	}
	if got := f.requests[s.ID].ResumeSessionID; got != "session-"+s.ID {
		t.Fatalf("woken agent resumed %q, want its prior session", got)
	}
}

func TestBusyAgentIsNotHibernated(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueuePrompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: "long"}}); err != nil {
		t.Fatal(err)
	}
	r.reapIdle(context.Background(), 0)
	if s.Dormant() {
		t.Fatal("hibernated an agent with a running turn")
	}
	f.adapters[s.ID].gate <- struct{}{}
}

type stubMessaging struct {
	watched, closed []string
}

func (m *stubMessaging) SummaryState(id string) ([]messaging.WaitingOn, int) {
	return []messaging.WaitingOn{{RequestID: "req_1", To: messaging.Address{Host: "h", Agent: "x"}}}, 2
}
func (m *stubMessaging) Unlisted(id string) bool  { return true }
func (m *stubMessaging) Watch(s *session.Session) { m.watched = append(m.watched, s.ID) }
func (m *stubMessaging) SessionClosed(id string)  { m.closed = append(m.closed, id) }

func TestMessagingHooksDecorateSummariesWatchAndCloseSessions(t *testing.T) {
	r, _, _ := setup(t, &fakeFactory{})
	hooks := &stubMessaging{}
	r.SetMessaging(hooks)
	s, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks.watched) != 1 || hooks.watched[0] != s.ID {
		t.Fatalf("watched = %v", hooks.watched)
	}
	got := r.Summaries(context.Background())
	if len(got) != 1 || got[0].OpenAsks != 2 || len(got[0].WaitingOn) != 1 || got[0].WaitingOn[0].RequestID != "req_1" || !got[0].Unlisted {
		t.Fatalf("summaries = %+v", got)
	}
	if r.CWD(s.ID) == "" {
		t.Fatal("CWD is empty for a live session")
	}
	if ok, err := r.Close(context.Background(), s.ID, false, false, false); !ok || err != nil {
		t.Fatalf("close = %v, %v", ok, err)
	}
	if len(hooks.closed) != 1 || hooks.closed[0] != s.ID {
		t.Fatalf("closed = %v", hooks.closed)
	}
	// Daemon shutdown (DisposeAll) is not a close.
	r2, _, _ := setup(t, &fakeFactory{})
	hooks2 := &stubMessaging{}
	r2.SetMessaging(hooks2)
	if _, err := r2.Spawn(context.Background(), existing(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	_ = r2.DisposeAll(context.Background())
	if len(hooks2.closed) != 0 {
		t.Fatalf("DisposeAll reported closes: %v", hooks2.closed)
	}
}
