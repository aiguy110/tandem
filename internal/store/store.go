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
      );`

// Store serializes access through one connection. This makes connection-local
// pragmas deterministic and gives later event sequence allocation one writer.
type Store struct {
	db      *sql.DB
	now     func() time.Time
	eventMu sync.Mutex
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
	_, err := s.db.Exec(`INSERT INTO agents (id, name, spec, cwd, acpSessionId, status, createdAt, closedAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, spec=excluded.spec, cwd=excluded.cwd,
acpSessionId=excluded.acpSessionId, status=excluded.status, closedAt=excluded.closedAt`,
		a.ID, a.Name, compactSpec.String(), a.CWD, a.ACPSessionID, a.Status, a.CreatedAt, a.ClosedAt)
	return err
}

func (s *Store) SetStatus(id, status string) error {
	_, err := s.db.Exec("UPDATE agents SET status = ? WHERE id = ?", status, id)
	return err
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
	for _, q := range []string{"DELETE FROM agent_assets WHERE agentId = ?", "DELETE FROM events WHERE agentId = ?", "DELETE FROM browser_sessions WHERE agentId = ?", "DELETE FROM agents WHERE id = ?"} {
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
