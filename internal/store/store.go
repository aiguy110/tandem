// Package store owns Tandem's durable SQLite representation.
package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var agentSuffix = regexp.MustCompile(`-(\d+)$`)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
        id           TEXT PRIMARY KEY,
        name         TEXT NOT NULL,
        spec         TEXT NOT NULL,
        cwd          TEXT NOT NULL DEFAULT '',
        externalSessionId TEXT,
        status       TEXT NOT NULL,
        createdAt    INTEGER NOT NULL,
        closedAt     INTEGER
      );
CREATE TABLE IF NOT EXISTS events (
        sessionId TEXT NOT NULL,
        seq     INTEGER NOT NULL,
        kind    TEXT NOT NULL,
        payload TEXT NOT NULL,
        ts      INTEGER NOT NULL,
        PRIMARY KEY (sessionId, seq)
      );
CREATE TABLE IF NOT EXISTS session_audio_settings (
        sessionId         TEXT PRIMARY KEY,
        enabled         INTEGER NOT NULL DEFAULT 0,
        enabledAfterSeq INTEGER NOT NULL DEFAULT 0
      );
CREATE TABLE IF NOT EXISTS message_audio (
        sessionId    TEXT NOT NULL,
        seq        INTEGER NOT NULL,
        mimeType   TEXT NOT NULL,
        data       BLOB NOT NULL,
        durationMs INTEGER NOT NULL DEFAULT 0,
        createdAt  INTEGER NOT NULL,
        PRIMARY KEY (sessionId, seq)
      );
CREATE TABLE IF NOT EXISTS assets (
        id        TEXT PRIMARY KEY,
        mimeType  TEXT NOT NULL,
        size      INTEGER NOT NULL,
        createdAt INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS session_assets (
        sessionId   TEXT NOT NULL,
        assetId   TEXT NOT NULL,
        createdAt INTEGER NOT NULL,
        PRIMARY KEY (sessionId, assetId),
        FOREIGN KEY (assetId) REFERENCES assets(id)
      );
CREATE TABLE IF NOT EXISTS browser_sessions (
        sessionId       TEXT PRIMARY KEY,
        driverSessionId TEXT NOT NULL,
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
        sessionId     TEXT NOT NULL DEFAULT '',
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
        historySessionId INTEGER NOT NULL,
        externalId  TEXT NOT NULL,
        ordinal     INTEGER NOT NULL,
        role        TEXT NOT NULL DEFAULT '',
        kind        TEXT NOT NULL DEFAULT '',
        ts          INTEGER,
        text        TEXT NOT NULL,
        truncated   INTEGER NOT NULL DEFAULT 0,
        UNIQUE (historySessionId, externalId),
        FOREIGN KEY (historySessionId) REFERENCES history_sessions(id) ON DELETE CASCADE
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
        sessionId      TEXT,
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

// migrateLegacySessionNames renames the pre-rename "agent" tables and columns
// to their session names. Every step is guarded by a catalog lookup, so this is
// idempotent and safe on a fresh database (where nothing matches) or one that
// was interrupted midway.
func migrateLegacySessionNames(db *sql.DB) error {
	tableExists := func(name string) (bool, error) {
		var found string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	columnExists := func(table, column string) (bool, error) {
		rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return false, err
			}
			if name == column {
				return true, rows.Err()
			}
		}
		return false, rows.Err()
	}

	// Column renames run against the legacy table names, so they must precede
	// the table renames below. Within browser_sessions the order is load
	// bearing: its own driver handle has to vacate "sessionId" before the
	// session id can claim it.
	for _, c := range []struct{ table, from, to string }{
		{"browser_sessions", "sessionId", "driverSessionId"},
		{"browser_sessions", "agentId", "sessionId"},
		{"agents", "acpSessionId", "externalSessionId"},
		{"events", "agentId", "sessionId"},
		{"agent_audio_settings", "agentId", "sessionId"},
		{"message_audio", "agentId", "sessionId"},
		{"agent_assets", "agentId", "sessionId"},
		{"history_sessions", "agentId", "sessionId"},
		{"history_entries", "sessionId", "historySessionId"},
		{"automation_wakeups", "agentId", "sessionId"},
		{"annotations", "agentId", "sessionId"},
		{"audio_position", "agentId", "sessionId"},
	} {
		switch present, err := tableExists(c.table); {
		case err != nil:
			return fmt.Errorf("inspect %s: %w", c.table, err)
		case !present:
			continue
		}
		switch present, err := columnExists(c.table, c.from); {
		case err != nil:
			return fmt.Errorf("inspect %s.%s: %w", c.table, c.from, err)
		case !present:
			continue
		}
		// The destination already existing means this rename is done. The
		// check is required, not just an optimization: browser_sessions.
		// sessionId is a legacy driver handle *and* the post-rename session
		// id, so "from exists" alone would rename an already-correct column.
		switch present, err := columnExists(c.table, c.to); {
		case err != nil:
			return fmt.Errorf("inspect %s.%s: %w", c.table, c.to, err)
		case present:
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", c.table, c.from, c.to)); err != nil {
			return fmt.Errorf("rename %s.%s: %w", c.table, c.from, err)
		}
	}

	for _, t := range []struct{ from, to string }{
		{"agents", "sessions"},
		{"agent_audio_settings", "session_audio_settings"},
		{"agent_assets", "session_assets"},
	} {
		switch present, err := tableExists(t.from); {
		case err != nil:
			return fmt.Errorf("inspect %s: %w", t.from, err)
		case !present:
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s RENAME TO %s", t.from, t.to)); err != nil {
			return fmt.Errorf("rename table %s: %w", t.from, err)
		}
	}

	// The renamed column carries the old index along with it; the additive
	// migrations create annotations_session in its place.
	if _, err := db.Exec("DROP INDEX IF EXISTS annotations_agent"); err != nil {
		return fmt.Errorf("drop annotations_agent: %w", err)
	}
	return nil
}

// Store serializes access through one connection. This makes connection-local
// pragmas deterministic and gives later event sequence allocation one writer.
type Store struct {
	db      *sql.DB
	now     func() time.Time
	eventMu sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

// MessageAudio is a durable daemon-owned clip for one completed message.
// DurationMs is the clip's playback length in milliseconds, computed once
// from the rendered bytes by internal/voice.Duration. 0 means unknown —
// either the format could not be parsed, or the row predates duration
// computation and has not yet been read (see MessageAudio's lazy backfill
// note below).
type MessageAudio struct {
	SessionID  string
	Seq        int64
	MIMEType   string
	Data       []byte
	DurationMs int64
	CreatedAt  int64
}

// MessageAudioClip is the lightweight (no audio bytes) metadata for one
// cached clip, used to hydrate player controls without downloading audio.
type MessageAudioClip struct {
	Seq        int64
	DurationMs int64
}

// SetSessionAudioEnabled records the transcript boundary after which automatic
// speech is allowed. A toggle must never retroactively render older replies.
func (s *Store) SetSessionAudioEnabled(sessionID string, enabled bool, enabledAfterSeq int64) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := s.db.Exec(`INSERT INTO session_audio_settings (sessionId, enabled, enabledAfterSeq) VALUES (?, ?, ?)
ON CONFLICT(sessionId) DO UPDATE SET enabled=excluded.enabled, enabledAfterSeq=excluded.enabledAfterSeq`, sessionID, value, enabledAfterSeq)
	return err
}

func (s *Store) SessionAudioEnabled(sessionID string) (enabled bool, enabledAfterSeq int64, err error) {
	var value int
	err = s.db.QueryRow(`SELECT enabled, enabledAfterSeq FROM session_audio_settings WHERE sessionId = ?`, sessionID).Scan(&value, &enabledAfterSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	return value != 0, enabledAfterSeq, err
}

func (s *Store) PutMessageAudio(audio MessageAudio) error {
	if audio.SessionID == "" || audio.Seq < 1 || audio.MIMEType == "" || len(audio.Data) == 0 {
		return errors.New("invalid message audio")
	}
	if audio.CreatedAt == 0 {
		audio.CreatedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO message_audio (sessionId, seq, mimeType, data, durationMs, createdAt) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(sessionId, seq) DO UPDATE SET mimeType=excluded.mimeType, data=excluded.data, durationMs=excluded.durationMs, createdAt=excluded.createdAt`, audio.SessionID, audio.Seq, audio.MIMEType, audio.Data, audio.DurationMs, audio.CreatedAt)
	return err
}

func (s *Store) MessageAudio(sessionID string, seq int64) (*MessageAudio, error) {
	var audio MessageAudio
	err := s.db.QueryRow(`SELECT mimeType, data, durationMs, createdAt FROM message_audio WHERE sessionId = ? AND seq = ?`, sessionID, seq).
		Scan(&audio.MIMEType, &audio.Data, &audio.DurationMs, &audio.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	audio.SessionID, audio.Seq = sessionID, seq
	return &audio, nil
}

// UpdateMessageAudioDuration persists a duration computed after the fact —
// the lazy-backfill path for rows written before duration computation
// existed. It is a no-op (not an error) if the row is gone.
func (s *Store) UpdateMessageAudioDuration(sessionID string, seq int64, durationMs int64) error {
	_, err := s.db.Exec(`UPDATE message_audio SET durationMs = ? WHERE sessionId = ? AND seq = ?`, durationMs, sessionID, seq)
	return err
}

// AudioPosition is the last known playback position of one agent's audio
// player — one row per agent (not per message), last-write-wins, so the
// player can resume across a session switch or a closed tab.
type AudioPosition struct {
	SessionID  string
	Seq        int64
	PositionMs int64
	UpdatedAt  int64
}

// SetAudioPosition persists (or, for seq == 0, clears) an agent's audio
// playback position. seq == 0 means "no active section" — a deliberate
// reset, not just an unset value — so it deletes the row rather than storing
// a zero seq. Returns the daemon-assigned updatedAt used for the write (the
// caller may use it to describe the change, e.g. in a broadcast, without a
// second read).
func (s *Store) SetAudioPosition(sessionID string, seq, positionMs int64) (int64, error) {
	updatedAt := s.now().UnixMilli()
	if seq == 0 {
		_, err := s.db.Exec(`DELETE FROM audio_position WHERE sessionId = ?`, sessionID)
		return updatedAt, err
	}
	_, err := s.db.Exec(`INSERT INTO audio_position (sessionId, seq, positionMs, updatedAt) VALUES (?, ?, ?, ?)
ON CONFLICT(sessionId) DO UPDATE SET seq=excluded.seq, positionMs=excluded.positionMs, updatedAt=excluded.updatedAt`,
		sessionID, seq, positionMs, updatedAt)
	return updatedAt, err
}

// AudioPosition returns the persisted playback position for an agent, or nil
// if nothing is stored (never played, or explicitly cleared via seq == 0).
func (s *Store) AudioPosition(sessionID string) (*AudioPosition, error) {
	var pos AudioPosition
	err := s.db.QueryRow(`SELECT seq, positionMs, updatedAt FROM audio_position WHERE sessionId = ?`, sessionID).
		Scan(&pos.Seq, &pos.PositionMs, &pos.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pos.SessionID = sessionID
	return &pos, nil
}

// MessageAudioSeqs lists the transcript messages that already have durable
// rendered audio. The bytes remain private to the authenticated audio route;
// this is just the metadata needed to rehydrate player controls on another
// client.
func (s *Store) MessageAudioSeqs(sessionID string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT seq FROM message_audio WHERE sessionId = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		seqs = append(seqs, seq)
	}
	return seqs, rows.Err()
}

// MessageAudioClips is MessageAudioSeqs plus each clip's known duration, for
// spacing timeline tick marks without downloading audio. A durationMs of 0
// means unknown (unparseable format, or a pre-duration row not yet read
// through MessageAudio's lazy backfill).
func (s *Store) MessageAudioClips(sessionID string) ([]MessageAudioClip, error) {
	rows, err := s.db.Query(`SELECT seq, durationMs FROM message_audio WHERE sessionId = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clips []MessageAudioClip
	for rows.Next() {
		var clip MessageAudioClip
		if err := rows.Scan(&clip.Seq, &clip.DurationMs); err != nil {
			return nil, err
		}
		clips = append(clips, clip)
	}
	return clips, rows.Err()
}

// StoredEvent is the store-level representation of a normalized event.
type StoredEvent struct {
	Seq     int64
	Kind    string
	Payload string
	TS      int64
}

type Session struct {
	ID                string
	Name              string
	Spec              json.RawMessage
	CWD               string
	ExternalSessionID *string
	Status            string
	CreatedAt         int64
	ClosedAt          *int64
}

type Asset struct {
	ID       string
	MIMEType string
	Size     int64
}

// BrowserSession is a durable handle to an externalized (Steel) browser session
// so it can be re-attached after a daemon restart instead of being orphaned.
type BrowserSession struct {
	SessionID string
	// DriverSessionID is the browser driver's own session handle (Steel's
	// sessionId / the local Chromium session), not a Tandem session id.
	DriverSessionID string
	ProfileID       string
	CDPURL          string
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
	SessionID string `json:"sessionId"`
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
	// Legacy databases call a session an "agent". Rename in place *before* the
	// schema DDL, so CREATE TABLE IF NOT EXISTS cannot leave an empty
	// "sessions" beside a populated "agents".
	if err := migrateLegacySessionNames(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Databases predating the workspace service have no cwd column. Duplicate
	// column is the sole expected error on current databases.
	if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN cwd TEXT NOT NULL DEFAULT ''"); err != nil && !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("migrate sessions.cwd: %w", err)
	}
	for _, migration := range []struct {
		sql, name string
	}{
		{"ALTER TABLE sessions ADD COLUMN railRank INTEGER", "sessions.railRank"},
		{"ALTER TABLE history_sessions ADD COLUMN importerId TEXT NOT NULL DEFAULT ''", "history_sessions.importerId"},
		{"ALTER TABLE history_sessions ADD COLUMN importerVersion INTEGER NOT NULL DEFAULT 0", "history_sessions.importerVersion"},
		{"ALTER TABLE history_sessions ADD COLUMN missingSince INTEGER", "history_sessions.missingSince"},
		{"ALTER TABLE automation_jobs ADD COLUMN wakeSuppression TEXT NOT NULL DEFAULT 'until_closed'", "automation_jobs.wakeSuppression"},
		{"ALTER TABLE message_audio ADD COLUMN durationMs INTEGER NOT NULL DEFAULT 0", "message_audio.durationMs"},
		{`CREATE TABLE IF NOT EXISTS annotations (
        id        TEXT PRIMARY KEY,
        sessionId   TEXT NOT NULL,
        seq       INTEGER NOT NULL,
        role      TEXT NOT NULL,
        quote     TEXT NOT NULL,
        comment   TEXT NOT NULL DEFAULT '',
        createdAt INTEGER NOT NULL,
        updatedAt INTEGER NOT NULL
      );
CREATE INDEX IF NOT EXISTS annotations_session ON annotations(sessionId);`, "annotations table"},
		{`CREATE TABLE IF NOT EXISTS audio_position (
        sessionId    TEXT PRIMARY KEY,
        seq        INTEGER NOT NULL,
        positionMs INTEGER NOT NULL,
        updatedAt  INTEGER NOT NULL
      );`, "audio_position table"},
		{`CREATE TABLE IF NOT EXISTS federation_master (
        singleton   INTEGER PRIMARY KEY CHECK (singleton = 1),
        url         TEXT NOT NULL,
        hostId      TEXT NOT NULL,
        credential  TEXT NOT NULL,
        updatedAt   INTEGER NOT NULL
      );
CREATE TABLE IF NOT EXISTS federation_slaves (
        id          TEXT PRIMARY KEY,
        name        TEXT NOT NULL DEFAULT '',
        endpoint    TEXT NOT NULL DEFAULT '',
        credential  TEXT NOT NULL DEFAULT '',
        status      TEXT NOT NULL,
        requestedAt INTEGER NOT NULL,
        acceptedAt  INTEGER,
        lastSeenAt  INTEGER,
        protocolVersion INTEGER NOT NULL DEFAULT 0,
        buildVersion    TEXT NOT NULL DEFAULT ''
      );`, "federation tables"},
		// These follow the CREATE above: a database predating the federation
		// tables has nothing to alter until they exist.
		{"ALTER TABLE federation_slaves ADD COLUMN protocolVersion INTEGER NOT NULL DEFAULT 0", "federation_slaves.protocolVersion"},
		{"ALTER TABLE federation_slaves ADD COLUMN buildVersion TEXT NOT NULL DEFAULT ''", "federation_slaves.buildVersion"},
	} {
		if _, err := db.Exec(migration.sql); err != nil && !isDuplicateColumn(err) {
			db.Close()
			return nil, fmt.Errorf("migrate %s: %w", migration.name, err)
		}
	}
	// Existing enabled threads predate the boundary. Treat their current event
	// head as the cutoff so upgrading does not unexpectedly synthesize speech
	// for old transcript messages.
	if _, err := db.Exec("ALTER TABLE session_audio_settings ADD COLUMN enabledAfterSeq INTEGER NOT NULL DEFAULT 0"); err == nil {
		if _, err := db.Exec(`UPDATE session_audio_settings
SET enabledAfterSeq = COALESCE((SELECT MAX(seq) FROM events WHERE events.sessionId = session_audio_settings.sessionId), 0)
WHERE enabled != 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate session_audio_settings.enabledAfterSeq: %w", err)
		}
	} else if !isDuplicateColumn(err) {
		db.Close()
		return nil, fmt.Errorf("migrate session_audio_settings.enabledAfterSeq: %w", err)
	}
	// Clip durations persisted before the MP3 frame walk's bitrate tables were
	// fixed are roughly half the true length (see internal/voice/duration.go).
	// Zeroing them once re-arms the read-triggered backfill, which recomputes
	// from the stored bytes — no provider work, no re-render. user_version is
	// unused otherwise, so it doubles as the "already repaired" marker.
	var audioDurationRepair int
	if err := db.QueryRow("PRAGMA user_version").Scan(&audioDurationRepair); err != nil {
		db.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if audioDurationRepair < 1 {
		if _, err := db.Exec("UPDATE message_audio SET durationMs = 0"); err != nil {
			db.Close()
			return nil, fmt.Errorf("reset message audio durations: %w", err)
		}
		if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
			db.Close()
			return nil, fmt.Errorf("record schema version: %w", err)
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

func (s *Store) UpsertSession(a Session) error {
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
	if _, err = tx.Exec(`INSERT INTO sessions (id, name, spec, cwd, externalSessionId, status, createdAt, closedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, spec=excluded.spec, cwd=excluded.cwd,
externalSessionId=excluded.externalSessionId, status=excluded.status, closedAt=excluded.closedAt`,
		a.ID, a.Name, compactSpec.String(), a.CWD, a.ExternalSessionID, a.Status, a.CreatedAt, a.ClosedAt); err != nil {
		return err
	}
	if err := upsertTandemHistorySession(tx, a, s.now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetStatus(id, status string) error {
	_, err := s.db.Exec("UPDATE sessions SET status = ? WHERE id = ?", status, id)
	return err
}

func (s *Store) SetSessionName(id, name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE sessions SET name = ? WHERE id = ?", name, id)
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
	if _, err := tx.Exec("UPDATE history_sessions SET title = ? WHERE source = 'tandem' AND sessionId = ?", name, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionRailRanks returns the persisted sessions-rail position of every live
// session that has one. Lower ranks sort first; sessions predating rail ranks
// are absent and ranked by the registry on boot.
func (s *Store) SessionRailRanks() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT id, railRank FROM sessions WHERE closedAt IS NULL AND railRank IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var rank int64
		if err := rows.Scan(&id, &rank); err != nil {
			return nil, err
		}
		out[id] = rank
	}
	return out, rows.Err()
}

// SetSessionRailRanks persists sessions-rail positions atomically, so a
// reorder is never observed half-applied after a restart.
func (s *Store) SetSessionRailRanks(ranks map[string]int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, rank := range ranks {
		if _, err := tx.Exec("UPDATE sessions SET railRank = ? WHERE id = ?", rank, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetExternalSessionID(id, sessionID string) error {
	_, err := s.db.Exec("UPDATE sessions SET externalSessionId = ? WHERE id = ?", sessionID, id)
	return err
}

func (s *Store) CloseSession(id string) error {
	_, err := s.db.Exec("UPDATE sessions SET status = 'idle', closedAt = ? WHERE id = ?", s.now().UnixMilli(), id)
	return err
}

func (s *Store) ReopenSession(id string) error {
	_, err := s.db.Exec("UPDATE sessions SET closedAt = NULL, status = 'idle' WHERE id = ?", id)
	return err
}

func (s *Store) DeleteSession(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM session_assets WHERE sessionId = ?", "DELETE FROM events WHERE sessionId = ?", "DELETE FROM session_audio_settings WHERE sessionId = ?", "DELETE FROM message_audio WHERE sessionId = ?", "DELETE FROM browser_sessions WHERE sessionId = ?", "DELETE FROM annotations WHERE sessionId = ?", "DELETE FROM audio_position WHERE sessionId = ?", "DELETE FROM history_sessions WHERE source = 'tandem' AND sessionId = ?", "DELETE FROM sessions WHERE id = ?"} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveBrowserSession records (or updates) an agent's externalized browser
// session so it survives a daemon restart. The signature is primitive-typed so
// the browser package's SessionStore interface is satisfied structurally.
func (s *Store) SaveBrowserSession(sessionID, driverSessionID, profileID, cdpURL string) error {
	_, err := s.db.Exec(`INSERT INTO browser_sessions (sessionId, driverSessionId, profileId, cdpUrl, updatedAt)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(sessionId) DO UPDATE SET driverSessionId=excluded.driverSessionId, profileId=excluded.profileId, cdpUrl=excluded.cdpUrl, updatedAt=excluded.updatedAt`,
		sessionID, driverSessionID, profileID, cdpURL, s.now().UnixMilli())
	return err
}

// DeleteBrowserSession forgets an agent's persisted browser session (session
// ended, or re-attach failed because it was already gone).
func (s *Store) DeleteBrowserSession(sessionID string) error {
	_, err := s.db.Exec("DELETE FROM browser_sessions WHERE sessionId = ?", sessionID)
	return err
}

// ListBrowserSessions returns every persisted browser session, for re-attach on
// daemon startup.
func (s *Store) ListBrowserSessions() ([]BrowserSession, error) {
	rows, err := s.db.Query("SELECT sessionId, driverSessionId, profileId, cdpUrl FROM browser_sessions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BrowserSession, 0)
	for rows.Next() {
		var b BrowserSession
		if err := rows.Scan(&b.SessionID, &b.DriverSessionID, &b.ProfileID, &b.CDPURL); err != nil {
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

// FindProfileByTupleName narrows FindProfileByTuple to one identity within
// those settings: an empty name matches only the auto-named profile, while a
// non-empty name matches only a user-named profile carrying exactly that name.
// Several profiles may share settings (one auto-named, any number user-named),
// so an unnamed spawn never silently resolves to someone's named profile.
func (s *Store) FindProfileByTupleName(agent, harness, model, effort, permission, snapshotID, name string) (*Profile, error) {
	query := `SELECT id, name, autoNamed, agent, harness, model, effort, permission, snapshotId, createdAt, lastUsedAt
FROM profiles WHERE agent=? AND harness=? AND model=? AND effort=? AND permission=? AND snapshotId=?`
	args := []any{agent, harness, model, effort, permission, snapshotID}
	if name == "" {
		query += " AND autoNamed=1"
	} else {
		query += " AND autoNamed=0 AND name=?"
		args = append(args, name)
	}
	p, err := scanProfile(s.db.QueryRow(query+" ORDER BY lastUsedAt DESC LIMIT 1", args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// Profile returns one profile by id, or (nil, nil) if it does not exist.
func (s *Store) Profile(id string) (*Profile, error) {
	p, err := scanProfile(s.db.QueryRow(`SELECT id, name, autoNamed, agent, harness, model, effort, permission, snapshotId, createdAt, lastUsedAt
FROM profiles WHERE id=?`, id))
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

// ForgetProfileRecency drops a profile from one project's recency list without
// deleting the profile itself, which other projects may still use.
func (s *Store) ForgetProfileRecency(id, project string) error {
	_, err := s.db.Exec("DELETE FROM profile_recent WHERE project=? AND profileId=?", project, id)
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

// AllProfileRecency returns every project's profile ids, most recent first,
// so a client can render all repos' recent profiles from one query.
func (s *Store) AllProfileRecency() (map[string][]string, error) {
	rows, err := s.db.Query("SELECT project, profileId FROM profile_recent ORDER BY project, lastUsedAt DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var project, id string
		if err := rows.Scan(&project, &id); err != nil {
			return nil, err
		}
		out[project] = append(out[project], id)
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

func (s *Store) Session(id string) (*Session, error) {
	return scanSession(s.db.QueryRow("SELECT id, name, spec, cwd, externalSessionId, status, createdAt, closedAt FROM sessions WHERE id = ?", id))
}

func (s *Store) SessionByExternalSessionID(sessionID string) (*Session, error) {
	return scanSession(s.db.QueryRow("SELECT id, name, spec, cwd, externalSessionId, status, createdAt, closedAt FROM sessions WHERE externalSessionId = ? ORDER BY createdAt DESC LIMIT 1", sessionID))
}

func (s *Store) LiveSessions() ([]Session, error) {
	return s.sessions("SELECT id, name, spec, cwd, externalSessionId, status, createdAt, closedAt FROM sessions WHERE closedAt IS NULL ORDER BY createdAt")
}

func (s *Store) AllSessions() ([]Session, error) {
	return s.sessions("SELECT id, name, spec, cwd, externalSessionId, status, createdAt, closedAt FROM sessions ORDER BY COALESCE(closedAt, createdAt) DESC")
}

func (s *Store) sessions(query string) ([]Session, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		a, err := scanSession(rows)
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

func scanSession(row scanner) (*Session, error) {
	var a Session
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
		a.ExternalSessionID = &session.String
	}
	if closed.Valid {
		a.ClosedAt = &closed.Int64
	}
	return &a, nil
}

func (s *Store) MaxSessionSuffix() (int, error) {
	rows, err := s.db.Query("SELECT id FROM sessions")
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

func (s *Store) PutAsset(sessionID string, asset Asset) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	if _, err := tx.Exec("INSERT OR IGNORE INTO assets (id, mimeType, size, createdAt) VALUES (?, ?, ?, ?)", asset.ID, asset.MIMEType, asset.Size, now); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO session_assets (sessionId, assetId, createdAt) VALUES (?, ?, ?)", sessionID, asset.ID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SessionAsset(sessionID, assetID string) (*Asset, error) {
	var a Asset
	err := s.db.QueryRow(`SELECT a.id, a.mimeType, a.size FROM assets a
JOIN session_assets aa ON aa.assetId = a.id WHERE aa.sessionId = ? AND a.id = ?`, sessionID, assetID).Scan(&a.ID, &a.MIMEType, &a.Size)
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
func (s *Store) AppendEvent(sessionID, kind, payload string, ts int64) (int64, error) {
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
	if err := tx.QueryRow("SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE sessionId = ?", sessionID).Scan(&seq); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO events (sessionId, seq, kind, payload, ts) VALUES (?, ?, ?, ?, ?)", sessionID, seq, kind, payload, ts); err != nil {
		return 0, err
	}
	if err := indexTandemEvent(tx, sessionID, seq, kind, payload, ts, s.now().UnixMilli()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// RangeEvents returns events strictly newer than afterSeq in sequence order.
func (s *Store) RangeEvents(sessionID string, afterSeq int64) ([]StoredEvent, error) {
	rows, err := s.db.Query("SELECT seq, kind, payload, ts FROM events WHERE sessionId = ? AND seq > ? ORDER BY seq", sessionID, afterSeq)
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
			return nil, fmt.Errorf("event %q/%d has malformed payload JSON", sessionID, event.Seq)
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// EventsOfKinds returns every persisted event of the given kinds in sequence
// order. It lets reconcilers inspect sparse event types without loading a
// session's whole transcript.
func (s *Store) EventsOfKinds(sessionID string, kinds ...string) ([]StoredEvent, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := []any{sessionID}
	marks := make([]string, len(kinds))
	for i, kind := range kinds {
		marks[i] = "?"
		args = append(args, kind)
	}
	rows, err := s.db.Query("SELECT seq, kind, payload, ts FROM events WHERE sessionId = ? AND kind IN ("+strings.Join(marks, ",")+") ORDER BY seq", args...)
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
		out = append(out, event)
	}
	return out, rows.Err()
}

// LatestEventOfKind returns the newest persisted event of a kind, or nil when
// the agent has never logged one. Callers use it to re-seed in-memory state
// (for example the last reported context usage) after a daemon restart without
// replaying the whole history.
func (s *Store) LatestEventOfKind(sessionID, kind string) (*StoredEvent, error) {
	var event StoredEvent
	err := s.db.QueryRow("SELECT seq, kind, payload, ts FROM events WHERE sessionId = ? AND kind = ? ORDER BY seq DESC LIMIT 1", sessionID, kind).
		Scan(&event.Seq, &event.Kind, &event.Payload, &event.TS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(event.Payload)) {
		return nil, fmt.Errorf("event %q/%d has malformed payload JSON", sessionID, event.Seq)
	}
	return &event, nil
}

func (s *Store) EventBounds(sessionID string) (min, max int64, err error) {
	err = s.db.QueryRow("SELECT COALESCE(MIN(seq), 0), COALESCE(MAX(seq), 0) FROM events WHERE sessionId = ?", sessionID).Scan(&min, &max)
	return
}

// UpsertAnnotation inserts or replaces a transcript annotation by id.
func (s *Store) UpsertAnnotation(a Annotation) error {
	_, err := s.db.Exec(`INSERT INTO annotations (id, sessionId, seq, role, quote, comment, createdAt, updatedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET sessionId=excluded.sessionId, seq=excluded.seq, role=excluded.role,
  quote=excluded.quote, comment=excluded.comment, createdAt=excluded.createdAt, updatedAt=excluded.updatedAt`,
		a.ID, a.SessionID, a.Seq, a.Role, a.Quote, a.Comment, a.CreatedAt, a.UpdatedAt)
	return err
}

// ListAnnotations returns an agent's annotations ordered by seq then createdAt,
// matching transcript order for the same row.
func (s *Store) ListAnnotations(sessionID string) ([]Annotation, error) {
	rows, err := s.db.Query(`SELECT id, sessionId, seq, role, quote, comment, createdAt, updatedAt
FROM annotations WHERE sessionId = ? ORDER BY seq, createdAt`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Annotation, 0)
	for rows.Next() {
		var a Annotation
		if err := rows.Scan(&a.ID, &a.SessionID, &a.Seq, &a.Role, &a.Quote, &a.Comment, &a.CreatedAt, &a.UpdatedAt); err != nil {
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

// DeleteAnnotationsForSession removes every annotation for an agent — the
// "sending consumes them" step, or agent deletion — returning the count
// removed.
func (s *Store) DeleteAnnotationsForSession(sessionID string) (int, error) {
	result, err := s.db.Exec("DELETE FROM annotations WHERE sessionId = ?", sessionID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// Close checkpoints WAL contents into the main file for clean handoff to Node.
//
// The handle is deliberately left in place rather than cleared: sessions and
// their pump goroutines can still be draining when the daemon shuts down, and
// a closed *sql.DB reports "sql: database is closed" to those late writers,
// while a nil one panics the process on the way out.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_, checkpointErr := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	closeErr := s.db.Close()
	return errors.Join(checkpointErr, closeErr)
}
