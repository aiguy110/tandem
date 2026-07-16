package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tandem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.db != nil {
			s.Close()
		}
	})
	return s, path
}

func ptr[T any](v T) *T { return &v }

func TestFreshSchemaPragmasAndAgentLifecycle(t *testing.T) {
	s, _ := openTestStore(t)
	s.now = func() time.Time { return time.UnixMilli(1700000009999) }

	for name, want := range map[string]string{"journal_mode": "wal", "synchronous": "1", "foreign_keys": "1", "user_version": "0"} {
		var got string
		if err := s.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if strings.ToLower(got) != want {
			t.Errorf("pragma %s=%q, want %q", name, got, want)
		}
	}
	var tables []string
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if want := []string{"agent_assets", "agents", "assets", "events"}; !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables=%v want %v", tables, want)
	}

	a1 := Agent{ID: "api-2", Name: "old", Spec: json.RawMessage(`{"adapter":"acp"}`), CWD: "/tmp/a", Status: "working", CreatedAt: 1700000000000}
	a2 := Agent{ID: "api-10", Name: "ten", Spec: json.RawMessage(`{"adapter":"acp","workspace":{"kind":"existing","cwd":"/tmp"}}`), CWD: "/tmp", ACPSessionID: ptr("session-shared"), Status: "error", CreatedAt: 1700000001000, ClosedAt: ptr(int64(1700000002000))}
	if err := s.UpsertAgent(a1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgent(a2); err != nil {
		t.Fatal(err)
	}
	a1.Name, a1.Status, a1.ACPSessionID = "new", "blocked", ptr("session-shared")
	if err := s.UpsertAgent(a1); err != nil {
		t.Fatal(err)
	}
	got, err := s.Agent("api-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new" || got.CreatedAt != a1.CreatedAt || got.ACPSessionID == nil {
		t.Fatalf("unexpected upsert result: %#v", got)
	}
	bySession, err := s.AgentBySessionID("session-shared")
	if err != nil || bySession.ID != "api-10" {
		t.Fatalf("latest session row=%#v err=%v", bySession, err)
	}
	if err := s.SetStatus(a1.ID, "error"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionID(a1.ID, "session-new"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseAgent(a1.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Agent(a1.ID)
	if got.Status != "idle" || got.ClosedAt == nil || *got.ClosedAt != 1700000009999 {
		t.Fatalf("close result: %#v", got)
	}
	if err := s.ReopenAgent(a1.ID); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveAgents()
	if err != nil || len(live) != 1 || live[0].ID != a1.ID {
		t.Fatalf("live=%#v err=%v", live, err)
	}
	all, err := s.AllAgents()
	if err != nil || len(all) != 2 || all[0].ID != a2.ID {
		t.Fatalf("all=%#v err=%v", all, err)
	}
	max, err := s.MaxAgentSuffix()
	if err != nil || max != 10 {
		t.Fatalf("max=%d err=%v", max, err)
	}
}

func TestFreshSchemaMatchesNodeContract(t *testing.T) {
	s, _ := openTestStore(t)
	type schemaRow struct {
		Type      string  `json:"type"`
		Name      string  `json:"name"`
		TableName string  `json:"tableName"`
		SQL       *string `json:"sql"`
	}
	var fixture struct {
		Schema []schemaRow `json:"schema"`
	}
	contractPath := filepath.Join("..", "..", "daemon", "migration-contract", "fixtures", "sqlite-schema.json")
	b, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT type, name, tbl_name, sql FROM sqlite_master WHERE type IN ('table', 'index') AND (type = 'index' OR name NOT LIKE 'sqlite_%') ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make([]schemaRow, 0)
	for rows.Next() {
		var r schemaRow
		var statement sql.NullString
		if err := rows.Scan(&r.Type, &r.Name, &r.TableName, &statement); err != nil {
			t.Fatal(err)
		}
		if statement.Valid {
			r.SQL = &statement.String
		}
		got = append(got, r)
	}
	if !reflect.DeepEqual(got, fixture.Schema) {
		text := func(v *string) string {
			if v == nil {
				return "<nil>"
			}
			return *v
		}
		for i := range got {
			if !reflect.DeepEqual(got[i], fixture.Schema[i]) {
				t.Errorf("schema row %s differs\n got SQL: %q\nwant SQL: %q", got[i].Name, text(got[i].SQL), text(fixture.Schema[i].SQL))
			}
		}
		t.FailNow()
	}
}

func TestAssetAssociationsAndDeleteAgent(t *testing.T) {
	s, _ := openTestStore(t)
	for _, id := range []string{"api-1", "api-2"} {
		if err := s.UpsertAgent(Agent{ID: id, Name: id, Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	asset := Asset{ID: strings.Repeat("a", 64), MIMEType: "image/png", Size: 68}
	if err := s.PutAsset("api-1", asset); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AgentAsset("api-1", asset.ID); err != nil || got == nil || *got != asset {
		t.Fatalf("owned asset=%#v err=%v", got, err)
	}
	if got, err := s.AgentAsset("api-2", asset.ID); err != nil || got != nil {
		t.Fatalf("cross-agent asset=%#v err=%v", got, err)
	}
	if err := s.PutAsset("api-2", asset); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent("api-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Agent("api-1"); got != nil {
		t.Fatalf("deleted agent remains: %#v", got)
	}
	if got, err := s.AgentAsset("api-2", asset.ID); err != nil || got == nil {
		t.Fatalf("shared physical asset removed: %#v %v", got, err)
	}
}

func TestMigratesDatabaseWithoutCWD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agents (id TEXT PRIMARY KEY, name TEXT NOT NULL, spec TEXT NOT NULL, acpSessionId TEXT, status TEXT NOT NULL, createdAt INTEGER NOT NULL, closedAt INTEGER);
INSERT INTO agents VALUES ('legacy-1','legacy-1','{}',NULL,'idle',123,NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.Agent("legacy-1")
	if err != nil || a.CWD != "" {
		t.Fatalf("legacy row=%#v err=%v", a, err)
	}
}

func TestMalformedRowsAreReported(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := s.db.Exec("INSERT INTO agents VALUES ('bad-json','bad-json','{','',NULL,'idle',1,NULL), ('bad-time','bad-time','{}','',NULL,'idle','never',NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Agent("bad-json"); err == nil || !strings.Contains(err.Error(), "malformed spec JSON") {
		t.Fatalf("bad JSON error=%v", err)
	}
	if _, err := s.Agent("bad-time"); err == nil {
		t.Fatal("malformed timestamp was accepted")
	}
	if err := s.UpsertAgent(Agent{ID: "x", Spec: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed spec write was accepted")
	}
}

func TestCloseCheckpointsWAL(t *testing.T) {
	s, path := openTestStore(t)
	if err := s.UpsertAgent(Agent{ID: "api-1", Name: "api-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("WAL not created: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("WAL remains non-empty: %d", info.Size())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM agents").Scan(&count); err != nil || count != 1 {
		t.Fatalf("checkpointed count=%d err=%v", count, err)
	}
}

func TestNodeGoNodeCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture command paths are POSIX")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(repo, "daemon", "migration-contract", "store-compat.ts")
	if _, err := os.Stat(filepath.Join(repo, "daemon", "node_modules", "tsx")); err != nil {
		t.Skip("run npm install in daemon to enable Node-Go compatibility acceptance")
	}
	path := filepath.Join(t.TempDir(), "compat.db")
	runNode := func(mode string) {
		t.Helper()
		cmd := exec.Command("node", "--import", "tsx", helper, mode, path)
		cmd.Dir = filepath.Join(repo, "daemon")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node %s: %v\n%s", mode, err, out)
		}
	}
	runNode("create")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Agent("node-1")
	if err != nil || a == nil || a.CWD != "/fixtures/node" || a.ACPSessionID == nil {
		t.Fatalf("Node row=%#v err=%v", a, err)
	}
	if err := s.SetStatus("node-1", "blocked"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionID("node-1", "go-session"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAsset("node-1", Asset{ID: strings.Repeat("b", 64), MIMEType: "image/jpeg", Size: 99}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgent(Agent{ID: "go-2", Name: "go-2", Spec: json.RawMessage(`{"adapter":"acp","agent":"codex"}`), CWD: "/fixtures/go", Status: "working", CreatedAt: 1700000020000}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseAgent("go-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReopenAgent("go-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	runNode("check")
}
