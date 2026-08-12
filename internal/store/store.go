// Package store owns Tandem's durable SQLite representation.
package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var agentSuffix = regexp.MustCompile(`-(\d+)$`)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
        id           TEXT PRIMARY KEY,
        name         TEXT NOT NULL,
        spec         TEXT NOT NULL,
        cwd          TEXT NOT NULL DEFAULT '',
        acpSessionId TEXT,
        status       TEXT NOT NULL,
        createdAt    INTEGER NOT NULL,
        closedAt     INTEGER
      );
CREATE TABLE IF NOT EXISTS events (
        agentId TEXT NOT NULL,
        seq     INTEGER NOT NULL,
        kind    TEXT NOT NULL,
        payload TEXT NOT NULL,
        ts      INTEGER NOT NULL,
        PRIMARY KEY (agentId, seq)
      );
CREATE TABLE IF NOT EXISTS agent_audio_settings (
        agentId  TEXT PRIMARY KEY,
        enabled  INTEGER NOT NULL DEFAULT 0
      );
CREATE TABLE IF NOT EXISTS message_audio (
        agentId   TEXT NOT NULL,
        seq       INTEGER NOT NULL,
        mimeType  TEXT NOT NULL,
        data      BLOB NOT NULL,
        createdAt INTEGER NOT NULL,
        PRIMARY KEY (agentId, seq)
      );
CREATE TABLE IF NOT EXISTS assets (
        id        TEXT PRIMARY KEY,
        mimeType  TEXT NOT NULL,
        size      INTEGER NOT NULL,
        createdAt INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS agent_assets (
        agentId   TEXT NOT NULL,
        assetId   TEXT NOT NULL,
        createdAt INTEGER NOT NULL,
        PRIMARY KEY (agentId, assetId),
        FOREIGN KEY (assetId) REFERENCES assets(id)
      );
CREATE TABLE IF NOT EXISTS browser_sessions (
        agentId   TEXT PRIMARY KEY,
        sessionId TEXT NOT NULL,
        profileId TEXT NOT NULL DEFAULT '',
        cdpUrl    TEXT NOT NULL DEFAULT '',
        updatedAt INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS browser_snapshots (
        id        TEXT PRIMARY KEY,
        name      TEXT NOT NULL,
        kind      TEXT NOT NULL,
        ref       TEXT NOT NULL DEFAULT '',
        createdAt INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS profiles (
        id         TEXT PRIMARY KEY,
        name       TEXT NOT NULL,
        autoNamed  INTEGER NOT NULL DEFAULT 1,
        agent      TEXT NOT NULL DEFAULT '',
        harness    TEXT NOT NULL DEFAULT '',
        model      TEXT NOT NULL DEFAULT '',
        effort     TEXT NOT NULL DEFAULT '',
        permission TEXT NOT NULL DEFAULT '',
        snapshotId TEXT NOT NULL DEFAULT '',
        createdAt  INTEGER NOT NULL,
        lastUsedAt INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS profile_recent (
        project    TEXT NOT NULL,
        profileId  TEXT NOT NULL,
        lastUsedAt INTEGER NOT NULL,
        PRIMARY KEY (project, profileId)
      );
CREATE TABLE IF NOT EXISTS history_sessions (
        id          INTEGER PRIMARY KEY,
        source      TEXT NOT NULL,
        agent       TEXT NOT NULL,
        externalId  TEXT NOT NULL,
        agentId     TEXT NOT NULL DEFAULT '',
        cwd         TEXT NOT NULL DEFAULT '',
        title       TEXT NOT NULL DEFAULT '',
        createdAt   INTEGER,
        updatedAt   INTEGER,
        indexedAt   INTEGER NOT NULL,
        resumable   INTEGER NOT NULL DEFAULT 1,
        sourceKey   TEXT NOT NULL,
        sourceMeta  TEXT NOT NULL DEFAULT '{}',
        importerId  TEXT NOT NULL DEFAULT '',
        importerVersion INTEGER NOT NULL DEFAULT 0,
        missingSince INTEGER,
        UNIQUE (source, agent, externalId)
      );
CREATE TABLE IF NOT EXISTS history_entries (
        id          INTEGER PRIMARY KEY,
        sessionId   INTEGER NOT NULL,
        externalId  TEXT NOT NULL,
        ordinal     INTEGER NOT NULL,
        role        TEXT NOT NULL DEFAULT '',
        kind        TEXT NOT NULL DEFAULT '',
        ts          INTEGER,
        text        TEXT NOT NULL,
        truncated   INTEGER NOT NULL DEFAULT 0,
        UNIQUE (sessionId, externalId),
        FOREIGN KEY (sessionId) REFERENCES history_sessions(id) ON DELETE CASCADE
      );
CREATE VIRTUAL TABLE IF NOT EXISTS history_entries_fts USING fts5(
        text,
        content='history_entries',
        content_rowid='id',
        tokenize='unicode61'
      );
CREATE TRIGGER IF NOT EXISTS history_entries_ai AFTER INSERT ON history_entries BEGIN
        INSERT INTO history_entries_fts(rowid, text) VALUES (new.id, new.text);
      END;
CREATE TRIGGER IF NOT EXISTS history_entries_ad AFTER DELETE ON history_entries BEGIN
        INSERT INTO history_entries_fts(history_entries_fts, rowid, text) VALUES ('delete', old.id, old.text);
      END;
CREATE TRIGGER IF NOT EXISTS history_entries_au AFTER UPDATE ON history_entries BEGIN
        INSERT INTO history_entries_fts(history_entries_fts, rowid, text) VALUES ('delete', old.id, old.text);
        INSERT INTO history_entries_fts(rowid, text) VALUES (new.id, new.text);
      END;
CREATE TABLE IF NOT EXISTS history_import_state (
        agent           TEXT NOT NULL,
        importerId      TEXT NOT NULL,
        importerVersion INTEGER NOT NULL,
        sourceKey       TEXT NOT NULL,
        checkpoint      TEXT NOT NULL,
        lastSuccessAt   INTEGER,
        lastError       TEXT,
        PRIMARY KEY (agent, importerId, sourceKey)
      );
CREATE TABLE IF NOT EXISTS history_import_runs (
        id            INTEGER PRIMARY KEY,
        agent         TEXT NOT NULL,
        startedAt     INTEGER NOT NULL,
        completedAt   INTEGER,
        sessionsSeen  INTEGER NOT NULL DEFAULT 0,
        entriesSeen   INTEGER NOT NULL DEFAULT 0,
        error         TEXT
      );
CREATE TABLE IF NOT EXISTS repository_tool_grants (
        repositoryId TEXT NOT NULL,
        toolName     TEXT NOT NULL,
        approvalId   TEXT NOT NULL DEFAULT '',
        grantedBy    TEXT NOT NULL DEFAULT '',
        grantedAt    INTEGER NOT NULL,
        revokedAt    INTEGER,
        PRIMARY KEY (repositoryId, toolName)
      );
CREATE TABLE IF NOT EXISTS automation_jobs (
        id                  TEXT PRIMARY KEY,
        repositoryId        TEXT NOT NULL,
        scriptPath          TEXT NOT NULL,
        name                TEXT NOT NULL DEFAULT '',
        cron                TEXT NOT NULL,
        timezone            TEXT NOT NULL DEFAULT '',
        browserSnapshotId   TEXT NOT NULL DEFAULT '',
        defaultAgentProfile TEXT NOT NULL DEFAULT '',
        wakePrompt          TEXT NOT NULL DEFAULT '',
        wakeSuppression     TEXT NOT NULL DEFAULT 'until_closed',
        concurrency         TEXT NOT NULL DEFAULT 'skip',
        manifestHash        TEXT NOT NULL DEFAULT '',
        enabled             INTEGER NOT NULL DEFAULT 1,
        createdAt           INTEGER NOT NULL,
        updatedAt           INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS automation_runs (
        id                TEXT PRIMARY KEY,
        jobId             TEXT,
        repositoryId      TEXT NOT NULL,
        scriptPath        TEXT NOT NULL DEFAULT '',
        trigger           TEXT NOT NULL,
        sourceHash        TEXT NOT NULL DEFAULT '',
        browserSnapshotId TEXT NOT NULL DEFAULT '',
        browserCloneId    TEXT NOT NULL DEFAULT '',
        scheduledAt       INTEGER,
        startedAt         INTEGER NOT NULL,
        completedAt       INTEGER,
        outcome           TEXT NOT NULL,
        reason            TEXT NOT NULL DEFAULT '',
        exitCode          INTEGER,
        stdout            TEXT NOT NULL DEFAULT '',
        stderr            TEXT NOT NULL DEFAULT '',
        report            TEXT,
        error             TEXT,
        FOREIGN KEY (jobId) REFERENCES automation_jobs(id) ON DELETE SET NULL
      );
CREATE TABLE IF NOT EXISTS automation_wakeups (
        runId        TEXT PRIMARY KEY,
        jobId        TEXT,
        agentId      TEXT,
        agentProfile TEXT NOT NULL DEFAULT '',
        prompt       TEXT NOT NULL DEFAULT '',
        reason       TEXT NOT NULL,
        context      TEXT NOT NULL DEFAULT '{}',
        status       TEXT NOT NULL,
        createdAt    INTEGER NOT NULL,
        completedAt INTEGER,
        FOREIGN KEY (runId) REFERENCES automation_runs(id) ON DELETE CASCADE,
        FOREIGN KEY (jobId) REFERENCES automation_jobs(id) ON DELETE SET NULL
      );
CREATE TABLE IF NOT EXISTS automation_tool_calls (
        id          TEXT PRIMARY KEY,
        runId       TEXT NOT NULL,
        toolName    TEXT NOT NULL,
        arguments   TEXT NOT NULL DEFAULT '{}',
        result      TEXT,
        error       TEXT,
        startedAt   INTEGER NOT NULL,
        completedAt INTEGER,
        FOREIGN KEY (runId) REFERENCES automation_runs(id) ON DELETE CASCADE
      );`

// Store serializes access through one connection. This makes connection-local
// pragmas deterministic and gives later event sequence allocation one writer.
type Store struct {
	db      *sql.DB
	now     func() time.Time
	eventMu sync.Mutex
}

// MessageAudio is a durable daemon-owned clip for one completed message.
type MessageAudio struct {
	AgentID   string
	Seq       int64
	MIMEType  string
	Data      []byte
	CreatedAt int64
}

func (s *Store) SetAgentAudioEnabled(agentID string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := s.db.Exec(`INSERT INTO agent_audio_settings (agentId, enabled) VALUES (?, ?)
ON CONFLICT(agentId) DO UPDATE SET enabled=excluded.enabled`, agentID, value)
	return err
}

func (s *Store) AgentAudioEnabled(agentID string) (bool, error) {
	var value int
	err := s.db.QueryRow(`SELECT enabled FROM agent_audio_settings WHERE agentId = ?`, agentID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value != 0, err
}

func (s *Store) PutMessageAudio(audio MessageAudio) error {
	if audio.AgentID == "" || audio.Seq < 1 || audio.MIMEType == "" || len(audio.Data) == 0 {
		return errors.New("invalid message audio")
	}
	if audio.CreatedAt == 0 {
		audio.CreatedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO message_audio (agentId, seq, mimeType, data, createdAt) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(agentId, seq) DO UPDATE SET mimeType=excluded.mimeType, data=excluded.data, createdAt=excluded.createdAt`, audio.AgentID, audio.Seq, audio.MIMEType, audio.Data, audio.CreatedAt)
	return err
}

func (s *Store) MessageAudio(agentID string, seq int64) (*MessageAudio, error) {
	var audio MessageAudio
	err := s.db.QueryRow(`SELECT mimeType, data, createdAt FROM message_audio WHERE agentId = ? AND seq = ?`, agentID, seq).Scan(&audio.MIMEType, &audio.Data, &audio.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	audio.AgentID, audio.Seq = agentID, seq
	return &audio, nil
}

// StoredEvent is the store-level representation of a normalized event.
type StoredEvent struct {
	Seq     int64
	Kind    string
	Payload string
	TS      int64
}

type Agent struct {
	ID           string
	Name         string
	Spec         json.RawMessage
	CWD          string
	ACPSessionID *string
	Status       string
	CreatedAt    int64
	ClosedAt     *int64
}

type Asset struct {
	ID       string
	MIMEType string
	Size     int64
}

// BrowserSession is a durable handle to an externalized (Steel) browser session
// so it can be re-attached after a daemon restart instead of being orphaned.
type BrowserSession struct {
	AgentID   string
	SessionID string
	ProfileID string
	CDPURL    string
}

// BrowserSnapshot is a captured, named browser user-data snapshot used to seed a
// new agent's browser at spawn. Kind is "local" (ref = on-disk snapshot dir) or
// "steel" (ref = Steel profileId).
type BrowserSnapshot struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	CreatedAt int64  `json:"createdAt"`
}

// Annotation is a durable, mutable "margin comment" anchored to one transcript
// row (by representative seq) plus the literal selected quote. Annotations
// accumulate in a per-agent review tray and are consumed into a single prompt
// on send (see docs/transcript-annotations.md).
type Annotation struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	Seq       int64  `json:"seq"`
	Role      string `json:"role"`
	Quote     string `json:"quote"`
	Comment   string `json:"comment"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Profile is a daemon-owned, auto-created, renamable bundle of launch settings:
// a harness plus model/effort/permission and an optional browser snapshot seed.
// AutoNamed is true while Name is still the generated concatenation of settings.
type Profile struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AutoNamed  bool   `json:"autoNamed"`
	Agent      string `json:"agent"`
	Harness    string `json:"harness"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	Permission string `json:"permission"`
	SnapshotID string `json:"snapshotId"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
}

// Open initializes or additively migrates a Node-compatible database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, now: time.Now}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = NORMAL", "PRAGMA foreign_keys = ON"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Databases predating the workspace service have no cwd column. Duplicate
	// column is the sole expected error on current databases.
	if _, err := db.Exec("ALTER TABLE agents ADD COLUMN cwd TEXT NOT NULL DEFAULT ''"); err != nil && !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("migrate agents.cwd: %w", err)
	}
	for _, migration := range []struct {
		sql, name string
	}{
		{"ALTER TABLE history_sessions ADD COLUMN importerId TEXT NOT NULL DEFAULT ''", "history_sessions.importerId"},
		{"ALTER TABLE history_sessions ADD COLUMN importerVersion INTEGER NOT NULL DEFAULT 0", "history_sessions.importerVersion"},
		{"ALTER TABLE history_sessions ADD COLUMN missingSince INTEGER", "history_sessions.missingSince"},
		{"ALTER TABLE automation_jobs ADD COLUMN wakeSuppression TEXT NOT NULL DEFAULT 'until_closed'", "automation_jobs.wakeSuppression"},
		{`CREATE TABLE IF NOT EXISTS annotations (
        id        TEXT PRIMARY KEY,
        agentId   TEXT NOT NULL,
        seq       INTEGER NOT NULL,
        role      TEXT NOT NULL,
        quote     TEXT NOT NULL,
        comment   TEXT NOT NULL DEFAULT '',
        createdAt INTEGER NOT NULL,
        updatedAt INTEGER NOT NULL
      );
CREATE INDEX IF NOT EXISTS annotations_agent ON annotations(agentId);`, "annotations table"},
	} {
		if _, err := db.Exec(migration.sql); err != nil && !isDuplicateColumn(err) {
			db.Close()
			return nil, fmt.Errorf("migrate %s: %w", migration.name, err)
		}
	}
	// Phase-6 ownership columns can be reconstructed from the atomically
	// persisted source checkpoints, avoiding a needless rewrite (and later
	// grace-period purge) of unchanged transcripts on upgrade.
	if _, err := db.Exec(`UPDATE history_sessions
SET importerId = COALESCE((SELECT his.importerId FROM history_import_state his
      WHERE his.agent = history_sessions.agent AND his.sourceKey = history_sessions.sourceKey
      ORDER BY his.lastSuccessAt DESC LIMIT 1), ''),
    importerVersion = COALESCE((SELECT his.importerVersion FROM history_import_state his
      WHERE his.agent = history_sessions.agent AND his.sourceKey = history_sessions.sourceKey
      ORDER BY his.lastSuccessAt DESC LIMIT 1), 0)
WHERE source = 'history' AND importerId = ''`); err != nil {
		db.Close()
		return nil, fmt.Errorf("backfill history importer ownership: %w", err)
	}
	if err := s.backfillTandemHistory(); err != nil {
		db.Close()
		return nil, fmt.Errorf("index tandem history: %w", err)
	}
	return s, nil
}

func isDuplicateColumn(err error) bool {
	return regexp.MustCompile(`(?i)duplicate column name`).MatchString(err.Error())
}

func (s *Store) UpsertAgent(a Agent) error {
	if !json.Valid(a.Spec) {
		return errors.New("agent spec is not valid JSON")
	}
	var compactSpec bytes.Buffer
	if err := json.Compact(&compactSpec, a.Spec); err != nil {
		return fmt.Errorf("compact agent spec: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO agents (id, name, spec, cwd, acpSessionId, status, createdAt, closedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, spec=excluded.spec, cwd=excluded.cwd,
acpSessionId=excluded.acpSessionId, status=excluded.status, closedAt=excluded.closedAt`,
		a.ID, a.Name, compactSpec.String(), a.CWD, a.ACPSessionID, a.Status, a.CreatedAt, a.ClosedAt); err != nil {
		return err
	}
	if err := upsertTandemHistorySession(tx, a, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetStatus(id, status string) error {
	_, err := s.db.Exec("UPDATE agents SET status = ? WHERE id = ?", status, id)
	return err
}

func (s *Store) SetAgentName(id, name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE agents SET name = ? WHERE id = ?", name, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return errors.New("no such agent")
	}
	if _, err := tx.Exec("UPDATE history_sessions SET title = ? WHERE source = 'tandem' AND agentId = ?", name, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetSessionID(id, sessionID string) error {
	_, err := s.db.Exec("UPDATE agents SET acpSessionId = ? WHERE id = ?", sessionID, id)
	return err
}

func (s *Store) CloseAgent(id string) error {
	_, err := s.db.Exec("UPDATE agents SET status = 'idle', closedAt = ? WHERE id = ?", s.now().UnixMilli(), id)
	return err
}

func (s *Store) ReopenAgent(id string) error {
	_, err := s.db.Exec("UPDATE agents SET closedAt = NULL, status = 'idle' WHERE id = ?", id)
	return err
}

func (s *Store) DeleteAgent(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM agent_assets WHERE agentId = ?", "DELETE FROM events WHERE agentId = ?", "DELETE FROM agent_audio_settings WHERE agentId = ?", "DELETE FROM message_audio WHERE agentId = ?", "DELETE FROM browser_sessions WHERE agentId = ?", "DELETE FROM annotations WHERE agentId = ?", "DELETE FROM history_sessions WHERE source = 'tandem' AND agentId = ?", "DELETE FROM agents WHERE id = ?"} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveBrowserSession records (or updates) an agent's externalized browser
// session so it survives a daemon restart. The signature is primitive-typed so
// the browser package's SessionStore interface is satisfied structurally.
func (s *Store) SaveBrowserSession(agentID, sessionID, profileID, cdpURL string) error {
	_, err := s.db.Exec(`INSERT INTO browser_sessions (agentId, sessionId, profileId, cdpUrl, updatedAt)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(agentId) DO UPDATE SET sessionId=excluded.sessionId, profileId=excluded.profileId, cdpUrl=excluded.cdpUrl, updatedAt=excluded.updatedAt`,
		agentID, sessionID, profileID, cdpURL, s.now().UnixMilli())
	return err
}

// DeleteBrowserSession forgets an agent's persisted browser session (session
// ended, or re-attach failed because it was already gone).
func (s *Store) DeleteBrowserSession(agentID string) error {
	_, err := s.db.Exec("DELETE FROM browser_sessions WHERE agentId = ?", agentID)
	return err
}

// ListBrowserSessions returns every persisted browser session, for re-attach on
// daemon startup.
func (s *Store) ListBrowserSessions() ([]BrowserSession, error) {
	rows, err := s.db.Query("SELECT agentId, sessionId, profileId, cdpUrl FROM browser_sessions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BrowserSession, 0)
	for rows.Next() {
		var b BrowserSession
		if err := rows.Scan(&b.AgentID, &b.SessionID, &b.ProfileID, &b.CDPURL); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveBrowserSnapshot records (or replaces) a captured browser snapshot.
func (s *Store) SaveBrowserSnapshot(snap BrowserSnapshot) error {
	if snap.CreatedAt == 0 {
		snap.CreatedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO browser_snapshots (id, name, kind, ref, createdAt)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind, ref=excluded.ref`,
		snap.ID, snap.Name, snap.Kind, snap.Ref, snap.CreatedAt)
	return err
}

// ListBrowserSnapshots returns every captured snapshot, newest first.
func (s *Store) ListBrowserSnapshots() ([]BrowserSnapshot, error) {
	rows, err := s.db.Query("SELECT id, name, kind, ref, createdAt FROM browser_snapshots ORDER BY createdAt DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BrowserSnapshot, 0)
	for rows.Next() {
		var b BrowserSnapshot
		if err := rows.Scan(&b.ID, &b.Name, &b.Kind, &b.Ref, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BrowserSnapshot returns one snapshot by id, or (nil, nil) if absent.
func (s *Store) BrowserSnapshot(id string) (*BrowserSnapshot, error) {
	var b BrowserSnapshot
	err := s.db.QueryRow("SELECT id, name, kind, ref, createdAt FROM browser_snapshots WHERE id = ?", id).
		Scan(&b.ID, &b.Name, &b.Kind, &b.Ref, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// DeleteBrowserSnapshot forgets a snapshot. Profiles referencing it fall back to
// a fresh browser at spawn (the seed lookup tolerates a missing snapshot).
func (s *Store) DeleteBrowserSnapshot(id string) error {
	_, err := s.db.Exec("DELETE FROM browser_snapshots WHERE id = ?", id)
	return err
}

// UpsertProfile inserts or replaces a profile by id.
func (s *Store) UpsertProfile(p Profile) error {
	now := s.now().UnixMilli()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	if p.LastUsedAt == 0 {
		p.LastUsedAt = now
	}
	_, err := s.db.Exec(`INSERT INTO profiles (id, name, autoNamed, agent, harness, model, effort, permission, snapshotId, createdAt, lastUsedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, autoNamed=excluded.autoNamed, agent=excluded.agent, harness=excluded.harness,
  model=excluded.model, effort=excluded.effort, permission=excluded.permission, snapshotId=excluded.snapshotId, lastUsedAt=excluded.lastUsedAt`,
		p.ID, p.Name, boolToInt(p.AutoNamed), p.Agent, p.Harness, p.Model, p.Effort, p.Permission, p.SnapshotID, p.CreatedAt, p.LastUsedAt)
	return err
}

// ListProfiles returns every profile, most-recently-used first.
func (s *Store) ListProfiles() ([]Profile, error) {
	rows, err := s.db.Query(`SELECT id, name, autoNamed, agent, harness, model, effort, permission, snapshotId, createdAt, lastUsedAt
FROM profiles ORDER BY lastUsedAt DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Profile, 0)
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// FindProfileByTuple returns the profile whose settings exactly match, or
// (nil, nil) if none — the dedup key for resolve-or-create at spawn.
func (s *Store) FindProfileByTuple(agent, harness, model, effort, permission, snapshotID string) (*Profile, error) {
	row := s.db.QueryRow(`SELECT id, name, autoNamed, agent, harness, model, effort, permission, snapshotId, createdAt, lastUsedAt
FROM profiles WHERE agent=? AND harness=? AND model=? AND effort=? AND permission=? AND snapshotId=? LIMIT 1`,
		agent, harness, model, effort, permission, snapshotID)
	p, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// RenameProfile sets a user-chosen name and clears the auto-named flag.
func (s *Store) RenameProfile(id, name string) error {
	_, err := s.db.Exec("UPDATE profiles SET name=?, autoNamed=0 WHERE id=?", name, id)
	return err
}

// DeleteProfile removes a profile and its recency records.
func (s *Store) DeleteProfile(id string) error {
	if _, err := s.db.Exec("DELETE FROM profile_recent WHERE profileId=?", id); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM profiles WHERE id=?", id)
	return err
}

// TouchProfile bumps a profile's lastUsedAt and records per-project recency.
func (s *Store) TouchProfile(id, project string) error {
	now := s.now().UnixMilli()
	if _, err := s.db.Exec("UPDATE profiles SET lastUsedAt=? WHERE id=?", now, id); err != nil {
		return err
	}
	if project == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO profile_recent (project, profileId, lastUsedAt) VALUES (?, ?, ?)
ON CONFLICT(project, profileId) DO UPDATE SET lastUsedAt=excluded.lastUsedAt`, project, id, now)
	return err
}

// ProfileRecency returns profile ids used in a project, most recent first.
func (s *Store) ProfileRecency(project string) ([]string, error) {
	rows, err := s.db.Query("SELECT profileId FROM profile_recent WHERE project=? ORDER BY lastUsedAt DESC", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanProfile(row interface{ Scan(...any) error }) (*Profile, error) {
	var p Profile
	var auto int
	if err := row.Scan(&p.ID, &p.Name, &auto, &p.Agent, &p.Harness, &p.Model, &p.Effort, &p.Permission, &p.SnapshotID, &p.CreatedAt, &p.LastUsedAt); err != nil {
		return nil, err
	}
	p.AutoNamed = auto != 0
	return &p, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) Agent(id string) (*Agent, error) {
	return scanAgent(s.db.QueryRow("SELECT id, name, spec, cwd, acpSessionId, status, createdAt, closedAt FROM agents WHERE id = ?", id))
}

func (s *Store) AgentBySessionID(sessionID string) (*Agent, error) {
	return scanAgent(s.db.QueryRow("SELECT id, name, spec, cwd, acpSessionId, status, createdAt, closedAt FROM agents WHERE acpSessionId = ? ORDER BY createdAt DESC LIMIT 1", sessionID))
}

func (s *Store) LiveAgents() ([]Agent, error) {
	return s.agents("SELECT id, name, spec, cwd, acpSessionId, status, createdAt, closedAt FROM agents WHERE closedAt IS NULL ORDER BY createdAt")
}

func (s *Store) AllAgents() ([]Agent, error) {
	return s.agents("SELECT id, name, spec, cwd, acpSessionId, status, createdAt, closedAt FROM agents ORDER BY COALESCE(closedAt, createdAt) DESC")
}

func (s *Store) agents(query string) ([]Agent, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Agent, 0)
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		if a != nil {
			out = append(out, *a)
		}
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanAgent(row scanner) (*Agent, error) {
	var a Agent
	var spec string
	var session sql.NullString
	var closed sql.NullInt64
	if err := row.Scan(&a.ID, &a.Name, &spec, &a.CWD, &session, &a.Status, &a.CreatedAt, &closed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !json.Valid([]byte(spec)) {
		return nil, fmt.Errorf("agent %q has malformed spec JSON", a.ID)
	}
	a.Spec = json.RawMessage(spec)
	if session.Valid {
		a.ACPSessionID = &session.String
	}
	if closed.Valid {
		a.ClosedAt = &closed.Int64
	}
	return &a, nil
}

func (s *Store) MaxAgentSuffix() (int, error) {
	rows, err := s.db.Query("SELECT id FROM agents")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		m := agentSuffix.FindStringSubmatch(id)
		if len(m) == 2 {
			var n int
			fmt.Sscanf(m[1], "%d", &n)
			if n > max {
				max = n
			}
		}
	}
	return max, rows.Err()
}

func (s *Store) PutAsset(agentID string, asset Asset) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	if _, err := tx.Exec("INSERT OR IGNORE INTO assets (id, mimeType, size, createdAt) VALUES (?, ?, ?, ?)", asset.ID, asset.MIMEType, asset.Size, now); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO agent_assets (agentId, assetId, createdAt) VALUES (?, ?, ?)", agentID, asset.ID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AgentAsset(agentID, assetID string) (*Asset, error) {
	var a Asset
	err := s.db.QueryRow(`SELECT a.id, a.mimeType, a.size FROM assets a
JOIN agent_assets aa ON aa.assetId = a.id WHERE aa.agentId = ? AND a.id = ?`, agentID, assetID).Scan(&a.ID, &a.MIMEType, &a.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// AppendEvent atomically allocates the next per-agent sequence and writes the
// event. The lock deliberately covers allocation and INSERT so concurrent
// EventLog instances cannot observe and reuse the same MAX(seq).
func (s *Store) AppendEvent(agentID, kind, payload string, ts int64) (int64, error) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if !json.Valid([]byte(payload)) {
		return 0, errors.New("event payload is not valid JSON")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE agentId = ?", agentID).Scan(&seq); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO events (agentId, seq, kind, payload, ts) VALUES (?, ?, ?, ?, ?)", agentID, seq, kind, payload, ts); err != nil {
		return 0, err
	}
	if err := indexTandemEvent(tx, agentID, seq, kind, payload, ts, s.now().UnixMilli()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// RangeEvents returns events strictly newer than afterSeq in sequence order.
func (s *Store) RangeEvents(agentID string, afterSeq int64) ([]StoredEvent, error) {
	rows, err := s.db.Query("SELECT seq, kind, payload, ts FROM events WHERE agentId = ? AND seq > ? ORDER BY seq", agentID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]StoredEvent, 0)
	for rows.Next() {
		var event StoredEvent
		if err := rows.Scan(&event.Seq, &event.Kind, &event.Payload, &event.TS); err != nil {
			return nil, err
		}
		if !json.Valid([]byte(event.Payload)) {
			return nil, fmt.Errorf("event %q/%d has malformed payload JSON", agentID, event.Seq)
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) EventBounds(agentID string) (min, max int64, err error) {
	err = s.db.QueryRow("SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM events WHERE agentId = ?", agentID).Scan(&min, &max)
	return
}

// UpsertAnnotation inserts or replaces a transcript annotation by id.
func (s *Store) UpsertAnnotation(a Annotation) error {
	_, err := s.db.Exec(`INSERT INTO annotations (id, agentId, seq, role, quote, comment, createdAt, updatedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET agentId=excluded.agentId, seq=excluded.seq, role=excluded.role,
  quote=excluded.quote, comment=excluded.comment, createdAt=excluded.createdAt, updatedAt=excluded.updatedAt`,
		a.ID, a.AgentID, a.Seq, a.Role, a.Quote, a.Comment, a.CreatedAt, a.UpdatedAt)
	return err
}

// ListAnnotations returns an agent's annotations ordered by seq then createdAt,
// matching transcript order for the same row.
func (s *Store) ListAnnotations(agentID string) ([]Annotation, error) {
	rows, err := s.db.Query(`SELECT id, agentId, seq, role, quote, comment, createdAt, updatedAt
FROM annotations WHERE agentId = ? ORDER BY seq, createdAt`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Annotation, 0)
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(&a.ID, &a.AgentID, &a.Seq, &a.Role, &a.Quote, &a.Comment, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAnnotation removes one annotation by id.
func (s *Store) DeleteAnnotation(id string) error {
	_, err := s.db.Exec("DELETE FROM annotations WHERE id = ?", id)
	return err
}

// DeleteAnnotationsForAgent removes every annotation for an agent — the
// "sending consumes them" step, or agent deletion — returning the count
// removed.
func (s *Store) DeleteAnnotationsForAgent(agentID string) (int, error) {
	result, err := s.db.Exec("DELETE FROM annotations WHERE agentId = ?", agentID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// Close checkpoints WAL contents into the main file for clean handoff to Node.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	_, checkpointErr := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	closeErr := s.db.Close()
	s.db = nil
	return errors.Join(checkpointErr, closeErr)
}
