package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

type fakeFactory struct {
	mu       sync.Mutex
	fail     map[string]bool
	adapters map[string]*regAdapter
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
	f.adapters[r.AgentID] = a
	f.mu.Unlock()
	return a, nil
}

type regAdapter struct {
	events chan eventlog.Event
	done   chan struct{}
	gate   chan struct{}
	sid    string
	once   sync.Once
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
	cfg := config.Config{Home: home, WorktreesDir: filepath.Join(home, "worktrees"), ACP: config.ACPConfig{Default: "fake"}, Agents: map[string]config.Agent{"fake": {ACP: &config.Launch{Cmd: "fake"}, Terminal: &config.ResumeLaunch{Cmd: "fake"}}}, Profiles: map[string]config.Profile{}}
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
	if a.ID != "api-18" || b.ID != "db-19" {
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
