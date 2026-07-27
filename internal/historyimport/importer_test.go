package historyimport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/store"
)

func testRunner(t *testing.T, script string) (*Runner, *store.Store, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	root := t.TempDir()
	node := filepath.Join(root, "fake-node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\nset -eu\nIFS= read -r request\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "history"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "tsx", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner, err := New(Options{
		Store: db, Node: node, RuntimeRoot: root, Timeout: 2 * time.Second,
		BaseEnv: []string{"HOME=/fixture/home", "PATH=/bin", "SECRET=must-not-leak"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, db, root
}

func TestImportProtocolPersistsSessionCheckpointAndRun(t *testing.T) {
	runner, db, root := testRunner(t, `
test "${CUSTOM_ENV:-}" = "visible"
test -z "${SECRET:-}"
printf '%s\n' \
'{"type":"hello","protocolVersion":1,"importer":{"id":"fixture","version":2}}' \
'{"type":"begin_session","mode":"replace","sourceKey":"/vendor/s1.jsonl","session":{"id":"s1","cwd":"/repo","title":"Fixture","createdAt":10,"updatedAt":20}}' \
'{"type":"entry","entry":{"id":"m1","ordinal":1,"role":"user","kind":"message","timestamp":12,"text":"find the cobalt turbine"}}' \
'{"type":"entry","entry":{"id":"m2","ordinal":2,"role":"assistant","kind":"message","text":"the turbine is ready"}}' \
'{"type":"end_session","sourceKey":"/vendor/s1.jsonl","checkpoint":{"offset":42}}'
`)
	result, err := runner.Import(context.Background(), "custom", config.History{
		Parser: filepath.Join(root, "parser.ts"), Enabled: true,
		Env: map[string]string{"CUSTOM_ENV": "visible"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 1 || result.Entries != 2 {
		t.Fatalf("result=%+v", result)
	}
	hits, err := db.SearchHistory("cobalt", 10)
	if err != nil || len(hits) != 1 || hits[0].Session.Agent != "custom" || hits[0].Session.Source != "history" {
		t.Fatalf("hits=%#v err=%v", hits, err)
	}
	checkpoints, err := db.HistoryImportCheckpoints("custom")
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("checkpoints=%#v err=%v", checkpoints, err)
	}
	if checkpoints[0].ImporterID != "fixture" || checkpoints[0].ImporterVersion != 2 ||
		string(checkpoints[0].Checkpoint) != `{"offset":42}` || checkpoints[0].LastSuccessAt == nil {
		t.Fatalf("checkpoint=%+v", checkpoints[0])
	}
	run, err := db.LatestHistoryImportRun("custom")
	if err != nil || run == nil || run.CompletedAt == nil || run.SessionsSeen != 1 || run.EntriesSeen != 2 || run.Error != "" {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

func TestIncompleteSessionPreservesPreviousTranscriptAndRecordsDiagnostic(t *testing.T) {
	runner, db, root := testRunner(t, `
printf '%s\n' \
'{"type":"hello","protocolVersion":1,"importer":{"id":"fixture","version":1}}' \
'{"type":"begin_session","mode":"replace","sourceKey":"source","session":{"id":"s1"}}' \
'{"type":"entry","entry":{"id":"new","ordinal":1,"text":"replacement is incomplete"}}'
printf 'vendor detail' >&2
`)
	if err := db.ReplaceHistorySession(
		store.HistorySession{Source: "history", Agent: "custom", ExternalID: "s1", SourceKey: "source", Resumable: true},
		[]store.HistoryEntry{{ExternalID: "old", Ordinal: 1, Text: "durable sentinel"}},
	); err != nil {
		t.Fatal(err)
	}
	_, err := runner.Import(context.Background(), "custom", config.History{
		Parser: filepath.Join(root, "parser.ts"), Enabled: true,
	})
	if err == nil || !strings.Contains(err.Error(), "ended during a session") || !strings.Contains(err.Error(), "vendor detail") {
		t.Fatalf("error=%v", err)
	}
	hits, searchErr := db.SearchHistory("sentinel", 10)
	if searchErr != nil || len(hits) != 1 {
		t.Fatalf("previous transcript lost: hits=%#v err=%v", hits, searchErr)
	}
	if hits, searchErr := db.SearchHistory("incomplete", 10); searchErr != nil || len(hits) != 0 {
		t.Fatalf("partial transcript committed: hits=%#v err=%v", hits, searchErr)
	}
	run, _ := db.LatestHistoryImportRun("custom")
	if run == nil || !strings.Contains(run.Error, "vendor detail") {
		t.Fatalf("failed run=%+v", run)
	}
}

func TestStrictProtocolAndPerAgentFailureIsolation(t *testing.T) {
	runner, db, root := testRunner(t, `
case "$3" in
  *bad.ts)
    printf '%s\n' '{"type":"hello","protocolVersion":1,"importer":{"id":"bad","version":1,"extra":true}}'
    ;;
  *)
    printf '%s\n' \
    '{"type":"hello","protocolVersion":1,"importer":{"id":"good","version":1}}' \
    '{"type":"begin_session","mode":"replace","sourceKey":"good","session":{"id":"good"}}' \
    '{"type":"entry","entry":{"id":"1","ordinal":1,"text":"isolated success"}}' \
    '{"type":"end_session","sourceKey":"good","checkpoint":null}'
    ;;
esac
`)
	failures := runner.ImportAll(context.Background(), map[string]config.Agent{
		"bad":  {History: &config.History{Parser: filepath.Join(root, "bad.ts"), Enabled: true}},
		"good": {History: &config.History{Parser: filepath.Join(root, "good.ts"), Enabled: true}},
	})
	if len(failures) != 1 || failures["bad"] == nil || !strings.Contains(failures["bad"].Error(), "unknown field") {
		t.Fatalf("failures=%v", failures)
	}
	hits, err := db.SearchHistory("isolated", 10)
	if err != nil || len(hits) != 1 || hits[0].Session.Agent != "good" {
		t.Fatalf("good importer did not run: hits=%#v err=%v", hits, err)
	}
}

func TestCheckpointRequestAndTimeout(t *testing.T) {
	runner, db, root := testRunner(t, `
case "$3" in
  *seed.ts)
    printf '%s\n' \
    '{"type":"hello","protocolVersion":1,"importer":{"id":"fixture","version":3}}' \
    '{"type":"begin_session","mode":"replace","sourceKey":"seed","session":{"id":"seed"}}' \
    '{"type":"entry","entry":{"id":"1","ordinal":1,"text":"seed transcript"}}' \
    '{"type":"end_session","sourceKey":"seed","checkpoint":{"offset":9}}'
    ;;
  *)
    printf '%s' "$request" | grep -q '"importerId":"fixture"'
    sleep 1
    ;;
esac
`)
	if _, err := runner.Import(context.Background(), "custom", config.History{Parser: filepath.Join(root, "seed.ts"), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runner.timeout = 20 * time.Millisecond
	_, err := runner.Import(context.Background(), "custom", config.History{Parser: filepath.Join(root, "slow.ts"), Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout error=%v", err)
	}
	checkpoints, cpErr := db.HistoryImportCheckpoints("custom")
	if cpErr != nil || len(checkpoints) != 1 || !json.Valid(checkpoints[0].Checkpoint) {
		t.Fatalf("checkpoints=%#v err=%v", checkpoints, cpErr)
	}
}
