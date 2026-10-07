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
func waitForUserMessage(t *testing.T, db *store.Store, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		log, err := eventlog.New(sessionID, db, 1)
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
			t.Fatalf("agent %s never recorded a first user message", sessionID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForEventKind polls the durable log because events pushed onto the fake
// adapter's channel are recorded asynchronously by the registry.
func waitForEventKind(t *testing.T, db *store.Store, sessionID, kind string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		log, err := eventlog.New(sessionID, db, 1)
		if err != nil {
			t.Fatal(err)
		}
		history, err := log.FullHistory()
		if err != nil {
			t.Fatal(err)
		}
		for _, logged := range history {
			if logged.Event.Kind == kind {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent %s never recorded a %q event", sessionID, kind)
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
	if _, err := source.AppendEvent(eventlog.Event{
		Kind:    "message_chunk",
		Payload: json.RawMessage(`{"kind":"message_chunk","text":"Started on parser.go, not finished."}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AppendEvent(eventlog.Event{
		Kind:    "tool_call",
		Payload: json.RawMessage(`{"kind":"tool_call","id":"delegate-1","title":"Delegate parser tests","status":"in_progress"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AppendEvent(eventlog.Event{
		Kind:    "message_chunk",
		Payload: json.RawMessage(`{"kind":"message_chunk","parentId":"delegate-1","text":"Checking parser tests."}`),
	}); err != nil {
		t.Fatal(err)
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
		"## Tandem session",
		".tandem/scripts/",
		"tandem-scripts",
		"agent-docs/README.md",
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
	rec, err := db.Session(received.ID)
	if err != nil || rec == nil {
		t.Fatalf("agent row: %v", err)
	}
	if strings.Contains(string(rec.Spec), "Started on parser.go") {
		t.Errorf("hand-off transcript was persisted into the spec: %s", rec.Spec)
	}
	log, err := eventlog.New(received.ID, db, 1)
	if err != nil {
		t.Fatal(err)
	}
	provenance, ok, err := log.LatestOfKind("handoff_received")
	if err != nil || !ok {
		t.Fatalf("durable hand-off provenance: ok=%v err=%v", ok, err)
	}
	var got struct {
		SourceSessionID string `json:"sourceSessionId"`
		CutoffSeq       int64  `json:"cutoffSeq"`
		Mode            string `json:"mode"`
		RendererVersion int    `json:"rendererVersion"`
		SourceWasActive bool   `json:"sourceWasActive"`
		Delegations     struct {
			Running int `json:"running"`
		} `json:"delegations"`
	}
	if err := json.Unmarshal(provenance.Event.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.SourceSessionID != source.ID || got.CutoffSeq <= 0 || got.Mode != "full" || got.RendererVersion != 2 || !got.SourceWasActive || got.Delegations.Running != 1 {
		t.Fatalf("provenance = %#v", got)
	}
	if strings.Contains(string(provenance.Event.Payload), "Started on parser.go") {
		t.Fatalf("provenance exposed transcript content: %s", provenance.Event.Payload)
	}
}

func TestFirstPromptAddsTandemGuideBeforeTheUserTask(t *testing.T) {
	got := firstPrompt("", "fix the parser")
	for _, want := range []string{"## Tandem session", "tandem-scripts", "agent-docs/README.md", "## User task", "fix the parser"} {
		if !strings.Contains(got, want) {
			t.Errorf("first prompt is missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "fix the parser") {
		t.Errorf("user task must be last:\n%s", got)
	}
	if got := firstPrompt("", ""); got != "" {
		t.Errorf("firstPrompt with no task or hand-off = %q, want empty", got)
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
	waitForEventKind(t, db, source.ID, "message_chunk")

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

func TestHandoffFromIdleSourceDoesNotInterruptIt(t *testing.T) {
	f := &fakeFactory{}
	r, db, _ := setup(t, f)
	source, err := r.Spawn(context.Background(), existing(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"user_message","text":"resume this idle session"}`)
	if _, err := source.AppendEvent(eventlog.Event{Kind: "user_message", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	spec := existing(t.TempDir())
	spec.HandoffFrom = source.ID
	received, err := r.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.adapters[source.ID].interrupts; got != 0 {
		t.Fatalf("idle source interrupted %d times", got)
	}
	log, err := eventlog.New(received.ID, db, 1)
	if err != nil {
		t.Fatal(err)
	}
	provenance, ok, err := log.LatestOfKind("handoff_received")
	if err != nil || !ok {
		t.Fatalf("durable hand-off provenance: ok=%v err=%v", ok, err)
	}
	var got struct {
		SourceWasActive bool `json:"sourceWasActive"`
	}
	if err := json.Unmarshal(provenance.Event.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.SourceWasActive {
		t.Fatal("idle source recorded as active")
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
