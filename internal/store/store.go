// Package store owns Tandem's durable SQLite representation. Its schema and
// query semantics intentionally mirror daemon/src/db.ts so either daemon may
// open a database last written by the other one.
package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
      );`

// Store serializes access through one connection. This makes connection-local
// pragmas deterministic and gives later event sequence allocation one writer.
type Store struct {
	db  *sql.DB
	now func() time.Time
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
	for _, q := range []string{"DELETE FROM agent_assets WHERE agentId = ?", "DELETE FROM events WHERE agentId = ?", "DELETE FROM agents WHERE id = ?"} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
