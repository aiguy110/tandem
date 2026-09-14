package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestStore(t testing.TB) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tandem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// A daemon shutdown closes the store while sessions can still be draining
// events, so late writes have to come back as errors instead of panicking.
func TestCloseIsIdempotentAndReportsLateWrites(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := s.AppendEvent("agent_1", "status", `{"kind":"status"}`, 1700000000000); err == nil {
		t.Fatal("append after close should fail")
	}
	if _, err := s.Session("agent_1"); err == nil {
		t.Fatal("read after close should fail")
	}
}

func ptr[T any](v T) *T { return &v }

func TestFreshSchemaPragmasAndAgentLifecycle(t *testing.T) {
	s, _ := openTestStore(t)
	s.now = func() time.Time { return time.UnixMilli(1700000009999) }

	// user_version is Tandem's repair marker, not a schema number: Open bumps
	// it as one-time data fixups land (1 = message_audio duration reset).
	for name, want := range map[string]string{"journal_mode": "wal", "synchronous": "1", "foreign_keys": "1", "user_version": "1"} {
		var got string
		if err := s.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if strings.ToLower(got) != want {
			t.Errorf("pragma %s=%q, want %q", name, got, want)
		}
	}
	var tables []string
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'history_entries_fts_%' ORDER BY name")
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
	if want := []string{"agent_assets", "agent_audio_settings", "agents", "annotations", "assets", "audio_position", "automation_jobs", "automation_runs", "automation_tool_calls", "automation_wakeups", "browser_sessions", "browser_snapshots", "events", "federation_master", "federation_slaves", "history_entries", "history_entries_fts", "history_import_runs", "history_import_state", "history_sessions", "message_audio", "profile_recent", "profiles", "repository_tool_grants"}; !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables=%v want %v", tables, want)
	}

	a1 := Session{ID: "api-2", Name: "old", Spec: json.RawMessage(`{"adapter":"acp"}`), CWD: "/tmp/a", Status: "working", CreatedAt: 1700000000000}
	a2 := Session{ID: "api-10", Name: "ten", Spec: json.RawMessage(`{"adapter":"acp","workspace":{"kind":"existing","cwd":"/tmp"}}`), CWD: "/tmp", ExternalSessionID: ptr("session-shared"), Status: "error", CreatedAt: 1700000001000, ClosedAt: ptr(int64(1700000002000))}
	if err := s.UpsertSession(a1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSession(a2); err != nil {
		t.Fatal(err)
	}
	a1.Name, a1.Status, a1.ExternalSessionID = "new", "blocked", ptr("session-shared")
	if err := s.UpsertSession(a1); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session("api-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new" || got.CreatedAt != a1.CreatedAt || got.ExternalSessionID == nil {
		t.Fatalf("unexpected upsert result: %#v", got)
	}
	bySession, err := s.SessionByExternalSessionID("session-shared")
	if err != nil || bySession.ID != "api-10" {
		t.Fatalf("latest session row=%#v err=%v", bySession, err)
	}
	if err := s.SetStatus(a1.ID, "error"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExternalSessionID(a1.ID, "session-new"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseSession(a1.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Session(a1.ID)
	if got.Status != "idle" || got.ClosedAt == nil || *got.ClosedAt != 1700000009999 {
		t.Fatalf("close result: %#v", got)
	}
	if err := s.ReopenSession(a1.ID); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveSessions()
	if err != nil || len(live) != 1 || live[0].ID != a1.ID {
		t.Fatalf("live=%#v err=%v", live, err)
	}
	all, err := s.AllSessions()
	if err != nil || len(all) != 2 || all[0].ID != a2.ID {
		t.Fatalf("all=%#v err=%v", all, err)
	}
	max, err := s.MaxSessionSuffix()
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
	contractPath := filepath.Join("testdata", "sqlite-schema.json")
	b, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT type, name, tbl_name, sql FROM sqlite_master WHERE type IN ('table', 'index') AND (type = 'index' OR name NOT LIKE 'sqlite_%') AND name NOT LIKE 'history_entries_fts_%' ORDER BY type, name")
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
	// Audio cache tables are Go-daemon-owned rather than part of the historical
	// Node schema contract captured by this fixture.
	keep := func(rows []schemaRow) []schemaRow {
		out := make([]schemaRow, 0, len(rows))
		for _, row := range rows {
			if row.TableName == "agent_audio_settings" || row.TableName == "message_audio" || row.TableName == "audio_position" || row.TableName == "federation_master" || row.TableName == "federation_slaves" {
				continue
			}
			out = append(out, row)
		}
		return out
	}
	got, fixture.Schema = keep(got), keep(fixture.Schema)
	if !reflect.DeepEqual(got, fixture.Schema) {
		text := func(v *string) string {
			if v == nil {
				return "<nil>"
			}
			return *v
		}
		for i := 0; i < len(got) && i < len(fixture.Schema); i++ {
			if !reflect.DeepEqual(got[i], fixture.Schema[i]) {
				t.Errorf("schema row %s differs\n got SQL: %q\nwant SQL: %q", got[i].Name, text(got[i].SQL), text(fixture.Schema[i].SQL))
			}
		}
		t.Errorf("schema row count=%d want=%d", len(got), len(fixture.Schema))
		t.FailNow()
	}
}

func TestAssetAssociationsAndDeleteAgent(t *testing.T) {
	s, _ := openTestStore(t)
	for _, id := range []string{"api-1", "api-2"} {
		if err := s.UpsertSession(Session{ID: id, Name: id, Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	asset := Asset{ID: strings.Repeat("a", 64), MIMEType: "image/png", Size: 68}
	if err := s.PutAsset("api-1", asset); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionAsset("api-1", asset.ID); err != nil || got == nil || *got != asset {
		t.Fatalf("owned asset=%#v err=%v", got, err)
	}
	if got, err := s.SessionAsset("api-2", asset.ID); err != nil || got != nil {
		t.Fatalf("cross-agent asset=%#v err=%v", got, err)
	}
	if err := s.PutAsset("api-2", asset); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession("api-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Session("api-1"); got != nil {
		t.Fatalf("deleted agent remains: %#v", got)
	}
	if got, err := s.SessionAsset("api-2", asset.ID); err != nil || got == nil {
		t.Fatalf("shared physical asset removed: %#v %v", got, err)
	}
}

func TestMessageAudioPersistsUntilAgentDeletion(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "audio-1", Name: "audio", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if enabled, after, err := s.SessionAudioEnabled("audio-1"); err != nil || enabled || after != 0 {
		t.Fatalf("default enabled=%v after=%d err=%v", enabled, after, err)
	}
	if err := s.SetSessionAudioEnabled("audio-1", true, 6); err != nil {
		t.Fatal(err)
	}
	if enabled, after, err := s.SessionAudioEnabled("audio-1"); err != nil || !enabled || after != 6 {
		t.Fatalf("saved enabled=%v after=%d err=%v", enabled, after, err)
	}
	if err := s.PutMessageAudio(MessageAudio{SessionID: "audio-1", Seq: 7, MIMEType: "audio/mpeg", Data: []byte("clip")}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMessageAudio(MessageAudio{SessionID: "audio-1", Seq: 9, MIMEType: "audio/mpeg", Data: []byte("later")}); err != nil {
		t.Fatal(err)
	}
	if seqs, err := s.MessageAudioSeqs("audio-1"); err != nil || !reflect.DeepEqual(seqs, []int64{7, 9}) {
		t.Fatalf("audio seqs=%v err=%v", seqs, err)
	}
	got, err := s.MessageAudio("audio-1", 7)
	if err != nil || got == nil || string(got.Data) != "clip" {
		t.Fatalf("audio=%#v err=%v", got, err)
	}
	if err := s.DeleteSession("audio-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MessageAudio("audio-1", 7); err != nil || got != nil {
		t.Fatalf("audio survived delete: %#v err=%v", got, err)
	}
}

func TestBrowserSessionPersistence(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "a-1", Name: "a-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBrowserSession("a-1", "sess-1", "prof-1", "ws://host/1"); err != nil {
		t.Fatal(err)
	}
	// Re-saving the same agent updates in place (single row, PRIMARY KEY).
	if err := s.SaveBrowserSession("a-1", "sess-2", "prof-2", "ws://host/2"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListBrowserSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (BrowserSession{SessionID: "a-1", DriverSessionID: "sess-2", ProfileID: "prof-2", CDPURL: "ws://host/2"}) {
		t.Fatalf("sessions = %#v", got)
	}
	// Deleting the agent cascades to its browser session.
	if err := s.SaveBrowserSession("a-2", "sess-3", "", ""); err != nil {
		// a-2 has no agents row; the session table has no FK, so this still saves.
		t.Fatal(err)
	}
	if err := s.DeleteBrowserSession("a-1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListBrowserSessions()
	if len(got) != 1 || got[0].SessionID != "a-2" {
		t.Fatalf("after delete = %#v", got)
	}
}

func TestDeleteAgentRemovesBrowserSession(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "a-1", Name: "a-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBrowserSession("a-1", "sess-1", "prof-1", "ws://host/1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession("a-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListBrowserSessions(); len(got) != 0 {
		t.Fatalf("browser session survived agent delete: %#v", got)
	}
}

func TestAnnotationCRUD(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "a-1", Name: "a-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	a1 := Annotation{ID: "ann-1", SessionID: "a-1", Seq: 5, Role: "assistant", Quote: "hello", Comment: "clarify this", CreatedAt: 100, UpdatedAt: 100}
	a2 := Annotation{ID: "ann-2", SessionID: "a-1", Seq: 3, Role: "user", Quote: "world", Comment: "", CreatedAt: 50, UpdatedAt: 50}
	if err := s.UpsertAnnotation(a1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAnnotation(a2); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAnnotations("a-1")
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by seq then createdAt, not insertion order.
	if len(got) != 2 || got[0].ID != "ann-2" || got[1].ID != "ann-1" {
		t.Fatalf("list order = %#v", got)
	}
	// Upsert-by-id updates in place (edit comment).
	a1.Comment = "actually never mind"
	a1.UpdatedAt = 200
	if err := s.UpsertAnnotation(a1); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListAnnotations("a-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Comment != "actually never mind" || got[1].UpdatedAt != 200 {
		t.Fatalf("update-in-place = %#v", got)
	}
	if err := s.DeleteAnnotation("ann-2"); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListAnnotations("a-1")
	if err != nil || len(got) != 1 || got[0].ID != "ann-1" {
		t.Fatalf("after single delete = %#v err=%v", got, err)
	}
	if err := s.UpsertAnnotation(Annotation{ID: "ann-3", SessionID: "a-1", Seq: 1, Role: "tool", Quote: "q", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteAnnotationsForSession("a-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cleared count = %d, want 2", n)
	}
	got, err = s.ListAnnotations("a-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("after clear = %#v err=%v", got, err)
	}
}

func TestDeleteAgentRemovesAnnotations(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "a-1", Name: "a-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAnnotation(Annotation{ID: "ann-1", SessionID: "a-1", Seq: 1, Role: "assistant", Quote: "q", Comment: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession("a-1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAnnotations("a-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("annotations survived agent delete: %#v err=%v", got, err)
	}
}

func TestAudioPositionRoundTripAndClear(t *testing.T) {
	s, _ := openTestStore(t)
	s.now = func() time.Time { return time.UnixMilli(5000) }
	if err := s.UpsertSession(Session{ID: "a-1", Name: "a-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AudioPosition("a-1"); err != nil || got != nil {
		t.Fatalf("expected no position before any write: %#v err=%v", got, err)
	}
	updatedAt, err := s.SetAudioPosition("a-1", 4, 12500)
	if err != nil || updatedAt != 5000 {
		t.Fatalf("SetAudioPosition = %d, %v", updatedAt, err)
	}
	got, err := s.AudioPosition("a-1")
	if err != nil || got == nil || got.Seq != 4 || got.PositionMs != 12500 || got.UpdatedAt != 5000 {
		t.Fatalf("position=%#v err=%v", got, err)
	}
	// Last-write-wins: a second write for the same agent replaces, not adds.
	s.now = func() time.Time { return time.UnixMilli(9000) }
	if _, err := s.SetAudioPosition("a-1", 7, 300); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AudioPosition("a-1"); err != nil || got == nil || got.Seq != 7 || got.PositionMs != 300 || got.UpdatedAt != 9000 {
		t.Fatalf("updated position=%#v err=%v", got, err)
	}
	// seq: 0 means "no active section" and clears the stored row entirely.
	if _, err := s.SetAudioPosition("a-1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AudioPosition("a-1"); err != nil || got != nil {
		t.Fatalf("expected position cleared by seq=0: %#v err=%v", got, err)
	}
}

func TestDeleteAgentRemovesAudioPosition(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "a-1", Name: "a-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAudioPosition("a-1", 3, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession("a-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AudioPosition("a-1"); err != nil || got != nil {
		t.Fatalf("audio position survived agent delete: %#v err=%v", got, err)
	}
}

func TestMigratesAudioPositionTable(t *testing.T) {
	// audio_position is a wholly new table (added the same way "annotations"
	// was: only via the additive migrations loop's CREATE TABLE IF NOT
	// EXISTS), so a pre-existing database without it must still pick it up.
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE agents (id TEXT PRIMARY KEY, name TEXT NOT NULL, spec TEXT NOT NULL, acpSessionId TEXT, status TEXT NOT NULL, createdAt INTEGER NOT NULL, closedAt INTEGER);
INSERT INTO agents VALUES ('legacy-1','legacy-1','{}',NULL,'idle',123,NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SetAudioPosition("legacy-1", 2, 4000); err != nil {
		t.Fatal(err)
	}
	got, err := s.AudioPosition("legacy-1")
	if err != nil || got == nil || got.Seq != 2 || got.PositionMs != 4000 {
		t.Fatalf("migrated position=%#v err=%v", got, err)
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
	a, err := s.Session("legacy-1")
	if err != nil || a.CWD != "" {
		t.Fatalf("legacy row=%#v err=%v", a, err)
	}
}

func TestMigratesAutomationWakeSuppression(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE automation_jobs (
        id TEXT PRIMARY KEY, repositoryId TEXT NOT NULL, scriptPath TEXT NOT NULL,
        name TEXT NOT NULL DEFAULT '', cron TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT '',
        browserSnapshotId TEXT NOT NULL DEFAULT '', defaultAgentProfile TEXT NOT NULL DEFAULT '',
        wakePrompt TEXT NOT NULL DEFAULT '', concurrency TEXT NOT NULL DEFAULT 'skip',
        manifestHash TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
        createdAt INTEGER NOT NULL, updatedAt INTEGER NOT NULL
      );
INSERT INTO automation_jobs (id, repositoryId, scriptPath, cron, createdAt, updatedAt)
VALUES ('legacy-job', 'repo', 'watch.ts', 'every 5m', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.AutomationJob("legacy-job")
	if err != nil || job == nil || job.WakeSuppression != "until_closed" {
		t.Fatalf("legacy job=%#v err=%v", job, err)
	}
}

func TestMigratesMessageAudioDurationMs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE message_audio (
        agentId   TEXT NOT NULL,
        seq       INTEGER NOT NULL,
        mimeType  TEXT NOT NULL,
        data      BLOB NOT NULL,
        createdAt INTEGER NOT NULL,
        PRIMARY KEY (agentId, seq)
      );
INSERT INTO message_audio (agentId, seq, mimeType, data, createdAt) VALUES ('legacy-1', 4, 'audio/mpeg', X'0102', 100)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.MessageAudio("legacy-1", 4)
	if err != nil || got == nil || got.DurationMs != 0 {
		t.Fatalf("legacy row=%#v err=%v", got, err)
	}
	clips, err := s.MessageAudioClips("legacy-1")
	if err != nil || len(clips) != 1 || clips[0].Seq != 4 || clips[0].DurationMs != 0 {
		t.Fatalf("legacy clips=%#v err=%v", clips, err)
	}
	// A subsequent write with a known duration must persist through the
	// migrated column.
	if err := s.PutMessageAudio(MessageAudio{SessionID: "legacy-1", Seq: 4, MIMEType: "audio/mpeg", Data: []byte{1, 2}, DurationMs: 1500}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MessageAudio("legacy-1", 4); err != nil || got.DurationMs != 1500 {
		t.Fatalf("updated row=%#v err=%v", got, err)
	}
}

// Durations written before the MP3 parser's bitrate tables were corrected are
// about half the true clip length. Open clears them exactly once so the
// read-triggered backfill recomputes from the stored bytes; a second Open must
// leave freshly written durations alone.
func TestOpenResetsStaleMessageAudioDurationsOnce(t *testing.T) {
	s, path := openTestStore(t)
	if err := s.PutMessageAudio(MessageAudio{SessionID: "audio-3", Seq: 2, MIMEType: "audio/mpeg", Data: []byte("clip"), DurationMs: 5903}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.MessageAudio("audio-3", 2)
	if err != nil || got == nil || got.DurationMs != 0 {
		t.Fatalf("stale duration should be cleared: row=%#v err=%v", got, err)
	}
	if err := reopened.UpdateMessageAudioDuration("audio-3", 2, 11424); err != nil {
		t.Fatal(err)
	}
	reopened.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, err := again.MessageAudio("audio-3", 2); err != nil || got.DurationMs != 11424 {
		t.Fatalf("repair must not repeat: row=%#v err=%v", got, err)
	}
}

func TestUpdateMessageAudioDurationBackfillsExistingRow(t *testing.T) {
	s, _ := openTestStore(t)
	if err := s.PutMessageAudio(MessageAudio{SessionID: "audio-2", Seq: 1, MIMEType: "audio/mpeg", Data: []byte("clip")}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MessageAudio("audio-2", 1); err != nil || got.DurationMs != 0 {
		t.Fatalf("expected unknown duration on write: row=%#v err=%v", got, err)
	}
	if err := s.UpdateMessageAudioDuration("audio-2", 1, 2500); err != nil {
		t.Fatal(err)
	}
	got, err := s.MessageAudio("audio-2", 1)
	if err != nil || got == nil || got.DurationMs != 2500 {
		t.Fatalf("backfilled row=%#v err=%v", got, err)
	}
	if clips, err := s.MessageAudioClips("audio-2"); err != nil || len(clips) != 1 || clips[0].DurationMs != 2500 {
		t.Fatalf("clips=%#v err=%v", clips, err)
	}
}

func TestMalformedRowsAreReported(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := s.db.Exec("INSERT INTO agents VALUES ('bad-json','bad-json','{','',NULL,'idle',1,NULL), ('bad-time','bad-time','{}','',NULL,'idle','never',NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("bad-json"); err == nil || !strings.Contains(err.Error(), "malformed spec JSON") {
		t.Fatalf("bad JSON error=%v", err)
	}
	if _, err := s.Session("bad-time"); err == nil {
		t.Fatal("malformed timestamp was accepted")
	}
	if err := s.UpsertSession(Session{ID: "x", Spec: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed spec write was accepted")
	}
}

func TestCloseCheckpointsWAL(t *testing.T) {
	s, path := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "api-1", Name: "api-1", Spec: json.RawMessage(`{}`), Status: "idle", CreatedAt: 1}); err != nil {
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
