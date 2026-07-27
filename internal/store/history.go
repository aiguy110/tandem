package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const tandemHistorySource = "tandem"

var historyTerms = regexp.MustCompile(`[\pL\pN_]+`)

// HistorySession is the vendor-neutral description of a resumable transcript.
// ExternalID is opaque to Tandem. SourceKey identifies the physical source used
// for incremental import and may differ from the resume identifier.
type HistorySession struct {
	Source     string
	Agent      string
	ExternalID string
	AgentID    string
	CWD        string
	Title      string
	CreatedAt  *int64
	UpdatedAt  *int64
	IndexedAt  int64
	Resumable  bool
	SourceKey  string
	SourceMeta json.RawMessage
}

// HistoryEntry is one normalized, searchable unit in a transcript.
type HistoryEntry struct {
	ExternalID string
	Ordinal    int64
	Role       string
	Kind       string
	Timestamp  *int64
	Text       string
	Truncated  bool
}

type Highlight struct {
	Start int
	End   int
}

type HistoryExcerpt struct {
	Text       string
	Highlights []Highlight
}

type HistorySearchHit struct {
	Session    HistorySession
	EntryID    int64
	ExternalID string
	Ordinal    int64
	Role       string
	Kind       string
	Timestamp  *int64
	Score      float64
	Match      HistoryExcerpt
	Before     *HistoryExcerpt
	After      *HistoryExcerpt
}

// ReplaceHistorySession atomically replaces a transcript. A failed validation
// or insert leaves the last successfully indexed version untouched.
func (s *Store) ReplaceHistorySession(session HistorySession, entries []HistoryEntry) error {
	if session.Source == "" || session.Agent == "" || session.ExternalID == "" || session.SourceKey == "" {
		return errors.New("history session source, agent, external ID, and source key are required")
	}
	if len(session.SourceMeta) == 0 {
		session.SourceMeta = json.RawMessage(`{}`)
	}
	if !json.Valid(session.SourceMeta) {
		return errors.New("history session source metadata is not valid JSON")
	}
	if session.IndexedAt == 0 {
		session.IndexedAt = s.now().UnixMilli()
	}
	for _, entry := range entries {
		if entry.ExternalID == "" {
			return errors.New("history entry external ID is required")
		}
		if strings.TrimSpace(entry.Text) == "" {
			return errors.New("history entry text is required")
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sessionID, err := replaceHistorySessionRow(tx, session)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM history_entries WHERE sessionId = ?", sessionID); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := tx.Exec(`INSERT INTO history_entries
(sessionId, externalId, ordinal, role, kind, ts, text, truncated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, entry.ExternalID, entry.Ordinal, entry.Role, entry.Kind, entry.Timestamp, entry.Text, entry.Truncated); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func replaceHistorySessionRow(tx *sql.Tx, session HistorySession) (int64, error) {
	_, err := tx.Exec(`INSERT INTO history_sessions
(source, agent, externalId, agentId, cwd, title, createdAt, updatedAt, indexedAt, resumable, sourceKey, sourceMeta)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source, agent, externalId) DO UPDATE SET
agentId=excluded.agentId, cwd=excluded.cwd, title=excluded.title, createdAt=excluded.createdAt,
updatedAt=excluded.updatedAt, indexedAt=excluded.indexedAt, resumable=excluded.resumable,
sourceKey=excluded.sourceKey, sourceMeta=excluded.sourceMeta`,
		session.Source, session.Agent, session.ExternalID, session.AgentID, session.CWD, session.Title,
		session.CreatedAt, session.UpdatedAt, session.IndexedAt, session.Resumable, session.SourceKey, string(session.SourceMeta))
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow("SELECT id FROM history_sessions WHERE source = ? AND agent = ? AND externalId = ?",
		session.Source, session.Agent, session.ExternalID).Scan(&id)
	return id, err
}

// SearchHistory performs a literal token-prefix FTS search. User input is
// tokenized and rebuilt rather than passed to SQLite as FTS syntax.
func (s *Store) SearchHistory(query string, limit int) ([]HistorySearchHit, error) {
	match := safeHistoryMatch(query)
	if match == "" {
		return []HistorySearchHit{}, nil
	}
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT
hs.source, hs.agent, hs.externalId, hs.agentId, hs.cwd, hs.title, hs.createdAt, hs.updatedAt,
hs.indexedAt, hs.resumable, hs.sourceKey, hs.sourceMeta,
he.id, he.externalId, he.ordinal, he.role, he.kind, he.ts,
bm25(history_entries_fts), snippet(history_entries_fts, 0, char(1), char(2), ' … ', 32)
FROM history_entries_fts
JOIN history_entries he ON he.id = history_entries_fts.rowid
JOIN history_sessions hs ON hs.id = he.sessionId
WHERE history_entries_fts MATCH ?
ORDER BY bm25(history_entries_fts), COALESCE(he.ts, hs.updatedAt, hs.createdAt, 0) DESC
LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search history: %w", err)
	}
	hits := make([]HistorySearchHit, 0, limit)
	for rows.Next() {
		var hit HistorySearchHit
		var created, updated, timestamp sql.NullInt64
		var resumable bool
		var sourceMeta string
		var marked string
		if err := rows.Scan(
			&hit.Session.Source, &hit.Session.Agent, &hit.Session.ExternalID, &hit.Session.AgentID,
			&hit.Session.CWD, &hit.Session.Title, &created, &updated, &hit.Session.IndexedAt,
			&resumable, &hit.Session.SourceKey, &sourceMeta,
			&hit.EntryID, &hit.ExternalID, &hit.Ordinal, &hit.Role, &hit.Kind, &timestamp,
			&hit.Score, &marked,
		); err != nil {
			rows.Close()
			return nil, err
		}
		hit.Session.CreatedAt = nullableInt64(created)
		hit.Session.UpdatedAt = nullableInt64(updated)
		hit.Session.Resumable = resumable
		hit.Session.SourceMeta = json.RawMessage(sourceMeta)
		hit.Timestamp = nullableInt64(timestamp)
		hit.Match = parseMarkedExcerpt(marked)
		hits = append(hits, hit)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range hits {
		before, after, err := s.historyContext(hits[i].EntryID)
		if err != nil {
			return nil, err
		}
		hits[i].Before, hits[i].After = before, after
	}
	return hits, nil
}

func safeHistoryMatch(query string) string {
	terms := historyTerms.FindAllString(query, 12)
	for i := range terms {
		if len(terms[i]) > 128 {
			terms[i] = terms[i][:128]
			for !utf8.ValidString(terms[i]) {
				terms[i] = terms[i][:len(terms[i])-1]
			}
		}
		terms[i] = `"` + strings.ReplaceAll(terms[i], `"`, `""`) + `"*`
	}
	return strings.Join(terms, " AND ")
}

func parseMarkedExcerpt(marked string) HistoryExcerpt {
	var out strings.Builder
	var highlights []Highlight
	start := -1
	runePos := 0
	for _, r := range marked {
		switch r {
		case '\x01':
			start = runePos
		case '\x02':
			if start >= 0 {
				highlights = append(highlights, Highlight{Start: start, End: runePos})
				start = -1
			}
		default:
			out.WriteRune(r)
			runePos++
		}
	}
	return HistoryExcerpt{Text: out.String(), Highlights: highlights}
}

func (s *Store) historyContext(entryID int64) (*HistoryExcerpt, *HistoryExcerpt, error) {
	var sessionID, ordinal int64
	if err := s.db.QueryRow("SELECT sessionId, ordinal FROM history_entries WHERE id = ?", entryID).Scan(&sessionID, &ordinal); err != nil {
		return nil, nil, err
	}
	query := func(order, comparison string) (*HistoryExcerpt, error) {
		var text string
		err := s.db.QueryRow("SELECT text FROM history_entries WHERE sessionId = ? AND ordinal "+comparison+" ? ORDER BY ordinal "+order+" LIMIT 1", sessionID, ordinal).Scan(&text)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &HistoryExcerpt{Text: clipHistoryText(text, 320)}, nil
	}
	before, err := query("DESC", "<")
	if err != nil {
		return nil, nil, err
	}
	after, err := query("ASC", ">")
	return before, after, err
}

func clipHistoryText(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func tandemAgent(spec json.RawMessage) string {
	var parsed struct {
		Agent string `json:"agent"`
	}
	if json.Unmarshal(spec, &parsed) != nil || parsed.Agent == "" {
		return "tandem"
	}
	return parsed.Agent
}

func upsertTandemHistorySession(tx *sql.Tx, agent Agent, indexedAt int64) error {
	updatedAt := agent.CreatedAt
	if agent.ClosedAt != nil {
		updatedAt = *agent.ClosedAt
	}
	sourceMeta, _ := json.Marshal(map[string]string{"agentId": agent.ID})
	_, err := replaceHistorySessionRow(tx, HistorySession{
		Source: tandemHistorySource, Agent: tandemAgent(agent.Spec), ExternalID: agent.ID,
		AgentID: agent.ID, CWD: agent.CWD, Title: agent.Name, CreatedAt: &agent.CreatedAt,
		UpdatedAt: &updatedAt, IndexedAt: indexedAt, Resumable: true,
		SourceKey: agent.ID, SourceMeta: sourceMeta,
	})
	return err
}

func indexTandemEvent(tx *sql.Tx, agentID string, seq int64, kind, payload string, ts, indexedAt int64) error {
	role, text := tandemEventText(kind, payload)
	if text == "" {
		return nil
	}
	var agent Agent
	var spec string
	if err := tx.QueryRow(`SELECT id, name, spec, cwd, acpSessionId, status, createdAt, closedAt
FROM agents WHERE id = ?`, agentID).Scan(&agent.ID, &agent.Name, &spec, &agent.CWD, &agent.ACPSessionID, &agent.Status, &agent.CreatedAt, &agent.ClosedAt); err != nil {
		// EventLog is intentionally usable without a registry-owned agent row
		// (tests and embedders rely on that). Such streams remain durable but
		// lack enough metadata to become resume-history sessions.
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	agent.Spec = json.RawMessage(spec)
	if err := upsertTandemHistorySession(tx, agent, indexedAt); err != nil {
		return err
	}
	var sessionID int64
	if err := tx.QueryRow("SELECT id FROM history_sessions WHERE source = ? AND agent = ? AND externalId = ?",
		tandemHistorySource, tandemAgent(agent.Spec), agentID).Scan(&sessionID); err != nil {
		return err
	}
	if role == "assistant" {
		var previousID int64
		var previousRole string
		err := tx.QueryRow(`SELECT id, role FROM history_entries
WHERE sessionId = ? ORDER BY ordinal DESC LIMIT 1`, sessionID).Scan(&previousID, &previousRole)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && previousRole == "assistant" {
			// ACP emits assistant text as arbitrary chunks. Coalescing a
			// contiguous run preserves phrase search across chunk boundaries.
			_, err = tx.Exec(`UPDATE history_entries
SET ordinal = ?, ts = ?, text = text || ? WHERE id = ?`, seq, ts, text, previousID)
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO history_entries
(sessionId, externalId, ordinal, role, kind, ts, text, truncated) VALUES (?, ?, ?, ?, ?, ?, ?, 0)
ON CONFLICT(sessionId, externalId) DO UPDATE SET ordinal=excluded.ordinal, role=excluded.role,
kind=excluded.kind, ts=excluded.ts, text=excluded.text`,
		sessionID, strconv.FormatInt(seq, 10), seq, role, kind, ts, text)
	return err
}

func tandemEventText(kind, payload string) (role, text string) {
	if kind != "user_message" && kind != "message_chunk" {
		return "", ""
	}
	var value struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(payload), &value) != nil || strings.TrimSpace(value.Text) == "" {
		return "", ""
	}
	if kind == "user_message" {
		return "user", value.Text
	}
	return "assistant", value.Text
}

func (s *Store) backfillTandemHistory() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT a.id, a.name, a.spec, a.cwd, a.acpSessionId, a.status, a.createdAt, a.closedAt,
e.seq, e.kind, e.payload, e.ts
FROM agents a JOIN events e ON e.agentId = a.id
WHERE e.kind IN ('user_message', 'message_chunk')
AND e.seq > COALESCE((
	SELECT MAX(he.ordinal) FROM history_sessions hs
	JOIN history_entries he ON he.sessionId = hs.id
	WHERE hs.source = 'tandem' AND hs.agentId = a.id
), 0)
ORDER BY a.id, e.seq`)
	if err != nil {
		return err
	}
	type row struct {
		agent   Agent
		seq, ts int64
		kind    string
		payload string
	}
	var pending []row
	for rows.Next() {
		var item row
		var spec string
		if err := rows.Scan(&item.agent.ID, &item.agent.Name, &spec, &item.agent.CWD, &item.agent.ACPSessionID,
			&item.agent.Status, &item.agent.CreatedAt, &item.agent.ClosedAt, &item.seq, &item.kind, &item.payload, &item.ts); err != nil {
			rows.Close()
			return err
		}
		item.agent.Spec = json.RawMessage(spec)
		pending = append(pending, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	indexedAt := s.now().UnixMilli()
	for _, item := range pending {
		if err := indexTandemEvent(tx, item.agent.ID, item.seq, item.kind, item.payload, item.ts, indexedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
