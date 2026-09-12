package registry

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"commit", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// waitForUserMessage polls the durable log because the spawn-time prompt is
// dispatched on its own goroutine.
func waitForUserMessage(t *testing.T, db *store.Store, agentID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		log, err := eventlog.New(agentID, db, 1)
		if err != nil {
			t.Fatal(err)
		}
		history, err := log.FullHistory()
		if err != nil {
			t.Fatal(err)
		}
		for _, logged := range history {
			if logged.Event.Kind != "user_message" {
				continue
			}
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(logged.Event.Payload, &payload) == nil {
				return payload.Text
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent %s never recorded a first user message", agentID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandoffSeedsTheFirstMessageFromTheSourceTranscript(t *testing.T) {
	f := &fakeFactory{}
	r, db, _ := setup(t, f)
	ctx := context.Background()

	source, err := r.Spawn(ctx, existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	go source.Prompt(ctx, []agentadapter.PromptBlock{{Type: "text", Text: "port the parser to the new API"}})
	if got := waitForUserMessage(t, db, source.ID); got != "port the parser to the new API" {
		t.Fatalf("source prompt not recorded, got %q", got)
	}
	f.adapters[source.ID].events <- eventlog.Event{
		Kind:    "message_chunk",
		Payload: json.RawMessage(`{"kind":"message_chunk","text":"Started on parser.go, not finished."}`),
	}

	spec := existing(t.TempDir())
	spec.HandoffFrom = source.ID
	spec.Task = "keep going"
	received, err := r.Spawn(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	first := waitForUserMessage(t, db, received.ID)
	for _, want := range []string{
		"port the parser to the new API",
		"Started on parser.go, not finished.",
		source.Name,
		"## New instruction from the user",
		"keep going",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("hand-off message is missing %q:\n%s", want, first)
		}
	}
	// The transcript is dispatched, not persisted onto the spec: the agent row
	// would otherwise carry a copy of the whole conversation.
	rec, err := db.Agent(received.ID)
	if err != nil || rec == nil {
		t.Fatalf("agent row: %v", err)
	}
	if strings.Contains(string(rec.Spec), "Started on parser.go") {
		t.Errorf("hand-off transcript was persisted into the spec: %s", rec.Spec)
	}
}

func TestBriefHandoffModeIsPlumbedThroughTheSpec(t *testing.T) {
	f := &fakeFactory{}
	r, db, _ := setup(t, f)
	ctx := context.Background()

	source, err := r.Spawn(ctx, existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	go source.Prompt(ctx, []agentadapter.PromptBlock{{Type: "text", Text: "port the parser"}})
	waitForUserMessage(t, db, source.ID)
	f.adapters[source.ID].events <- eventlog.Event{
		Kind:    "tool_call",
		Payload: json.RawMessage(`{"kind":"tool_call","id":"t1","title":"Read parser.go","status":"completed"}`),
	}
	f.adapters[source.ID].events <- eventlog.Event{
		Kind:    "message_chunk",
		Payload: json.RawMessage(`{"kind":"message_chunk","text":"Half done."}`),
	}

	spec := existing(t.TempDir())
	spec.HandoffFrom = source.ID
	spec.HandoffMode = "brief"
	received, err := r.Spawn(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	first := waitForUserMessage(t, db, received.ID)
	if !strings.Contains(first, "1 tool call") || strings.Contains(first, "Read parser.go") {
		t.Errorf("want a counted tool call, not a summarized one:\n%s", first)
	}
	if !strings.Contains(first, "Half done.") {
		t.Errorf("brief hand-off dropped the turn's closing message:\n%s", first)
	}
}

func TestHandoffFromAnUnknownAgentFailsBeforeProvisioning(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	spec := existing(t.TempDir())
	spec.HandoffFrom = "nobody"
	if _, err := r.Spawn(context.Background(), spec); err == nil {
		t.Fatal("want an error for an unknown hand-off source")
	}
}

func TestAgentsShareAWorktreeUntilTheLastOneLeaves(t *testing.T) {
	f := &fakeFactory{}
	r, _, _ := setup(t, f)
	ctx := context.Background()

	repo := gitRepo(t)
	first, err := r.Spawn(ctx, agentadapter.Spec{Adapter: "acp", Workspace: workspace.Workspace{Kind: workspace.KindWorktree, Repo: repo}})
	if err != nil {
		t.Fatal(err)
	}
	cwd := r.cwds[first.ID]
	if cwd == "" {
		t.Fatal("first agent has no working directory")
	}

	second, err := r.Spawn(ctx, existing(cwd))
	if err != nil {
		t.Fatalf("spawning into an occupied worktree must be allowed: %v", err)
	}
	// The joining agent inherits the worktree descriptor rather than degrading
	// to an anonymous existing directory, so git state and merge target survive.
	if got := second.Spec.Workspace; got.Kind != workspace.KindWorktree || got.Branch != first.Spec.Workspace.Branch {
		t.Fatalf("joined workspace = %+v, want the first agent's worktree", got)
	}
	if got := r.Cohabitants(cwd, first.ID); len(got) != 1 || got[0] != second.Name {
		t.Fatalf("cohabitants of %s = %v, want [%s]", cwd, got, second.Name)
	}
	preview, err := r.ClosePreview(ctx, first.ID)
	if err != nil || preview == nil {
		t.Fatalf("close preview: %v", err)
	}
	if len(preview.Cohabitants) != 1 || preview.Cohabitants[0] != second.Name {
		t.Fatalf("preview cohabitants = %v, want [%s]", preview.Cohabitants, second.Name)
	}

	if _, err := r.Close(ctx, first.ID, false, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err != nil {
		t.Fatalf("worktree was removed while another agent still occupies it: %v", err)
	}
	if _, err := r.Close(ctx, second.ID, false, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("worktree survived its last occupant: %v", err)
	}
}
