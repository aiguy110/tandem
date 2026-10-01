package store

import (
	"database/sql"
	"errors"
)

// Agent messaging persistence (docs/agent-messaging.md). Every table is
// prefixed agent_msg_. They are created by Open's additive migrations rather
// than the historical schema contract, like the federation tables.

const agentMessagingSchema = `
CREATE TABLE IF NOT EXISTS agent_msg_links (
        id            TEXT PRIMARY KEY,
        fromHost      TEXT NOT NULL,
        fromAgent     TEXT NOT NULL,
        fromName      TEXT NOT NULL DEFAULT '',
        toSession     TEXT NOT NULL,
        delivery      TEXT NOT NULL DEFAULT 'steer',
        budgetPerHour INTEGER NOT NULL DEFAULT 60,
        maxHops       INTEGER NOT NULL DEFAULT 20,
        paused        INTEGER NOT NULL DEFAULT 0,
        source        TEXT NOT NULL DEFAULT 'user',
        createdAt     INTEGER NOT NULL,
        UNIQUE (fromHost, fromAgent, toSession)
      );
CREATE INDEX IF NOT EXISTS agent_msg_links_to ON agent_msg_links(toSession);
CREATE TABLE IF NOT EXISTS agent_msg_inbound (
        id         TEXT PRIMARY KEY,
        toSession  TEXT NOT NULL,
        linkId     TEXT NOT NULL DEFAULT '',
        fromHost   TEXT NOT NULL DEFAULT '',
        fromAgent  TEXT NOT NULL DEFAULT '',
        kind       TEXT NOT NULL,
        status     TEXT NOT NULL,
        receivedAt INTEGER NOT NULL
      );
CREATE INDEX IF NOT EXISTS agent_msg_inbound_link ON agent_msg_inbound(linkId, receivedAt);
CREATE TABLE IF NOT EXISTS agent_msg_outbox (
        id            TEXT PRIMARY KEY,
        fromSession   TEXT NOT NULL DEFAULT '',
        toHost        TEXT NOT NULL,
        toAgent       TEXT NOT NULL,
        kind          TEXT NOT NULL,
        requestId     TEXT NOT NULL DEFAULT '',
        envelope      TEXT NOT NULL,
        status        TEXT NOT NULL,
        result        TEXT NOT NULL DEFAULT '',
        error         TEXT NOT NULL DEFAULT '',
        attempts      INTEGER NOT NULL DEFAULT 0,
        createdAt     INTEGER NOT NULL,
        nextAttemptAt INTEGER NOT NULL,
        updatedAt     INTEGER NOT NULL
      );
CREATE INDEX IF NOT EXISTS agent_msg_outbox_status ON agent_msg_outbox(status, createdAt);
CREATE TABLE IF NOT EXISTS agent_msg_requests (
        requestId  TEXT PRIMARY KEY,
        session    TEXT NOT NULL,
        toHost     TEXT NOT NULL,
        toAgent    TEXT NOT NULL,
        toName     TEXT NOT NULL DEFAULT '',
        threadId   TEXT NOT NULL DEFAULT '',
        messageId  TEXT NOT NULL DEFAULT '',
        status     TEXT NOT NULL,
        timeoutSec INTEGER NOT NULL DEFAULT 0,
        createdAt  INTEGER NOT NULL,
        deadline   INTEGER NOT NULL,
        closedAt   INTEGER
      );
CREATE INDEX IF NOT EXISTS agent_msg_requests_open ON agent_msg_requests(status, deadline);
CREATE TABLE IF NOT EXISTS agent_msg_obligations (
        requestId      TEXT PRIMARY KEY,
        session        TEXT NOT NULL,
        askerHost      TEXT NOT NULL,
        askerAgent     TEXT NOT NULL,
        askerName      TEXT NOT NULL DEFAULT '',
        threadId       TEXT NOT NULL DEFAULT '',
        messageId      TEXT NOT NULL DEFAULT '',
        hop            INTEGER NOT NULL DEFAULT 0,
        armedPrompt    TEXT NOT NULL DEFAULT '',
        reminded       INTEGER NOT NULL DEFAULT 0,
        reminderPrompt TEXT NOT NULL DEFAULT '',
        status         TEXT NOT NULL,
        createdAt      INTEGER NOT NULL,
        closedAt       INTEGER
      );
CREATE INDEX IF NOT EXISTS agent_msg_obligations_session ON agent_msg_obligations(session, status);
CREATE TABLE IF NOT EXISTS agent_msg_link_requests (
        id        TEXT PRIMARY KEY,
        role      TEXT NOT NULL,
        session   TEXT NOT NULL,
        peerHost  TEXT NOT NULL,
        peerAgent TEXT NOT NULL,
        peerName  TEXT NOT NULL DEFAULT '',
        reason    TEXT NOT NULL DEFAULT '',
        createdAt INTEGER NOT NULL,
        UNIQUE (role, session, peerHost, peerAgent)
      );
CREATE TABLE IF NOT EXISTS agent_msg_settings (
        sessionId TEXT PRIMARY KEY,
        listed    INTEGER NOT NULL DEFAULT 1,
        card      TEXT NOT NULL DEFAULT ''
      );
CREATE TABLE IF NOT EXISTS agent_msg_threads (
        sessionId TEXT NOT NULL,
        threadId  TEXT NOT NULL,
        hop       INTEGER NOT NULL,
        updatedAt INTEGER NOT NULL,
        PRIMARY KEY (sessionId, threadId)
      );
CREATE TABLE IF NOT EXISTS agent_msg_meta (
        key   TEXT PRIMARY KEY,
        value TEXT NOT NULL
      );`

// AgentMsgLink is a directed grant from one agent (possibly on another host)
// to a local session, enforced on the recipient's host.
type AgentMsgLink struct {
	ID            string
	FromHost      string
	FromAgent     string
	FromName      string
	ToSession     string
	Delivery      string
	BudgetPerHour int
	MaxHops       int
	Paused        bool
	Source        string
	CreatedAt     int64
}

// UpsertAgentMsgLink creates the (from, to) link or updates its settings,
// keeping the existing ID and creation time. It returns the stored row.
func (s *Store) UpsertAgentMsgLink(l AgentMsgLink) (AgentMsgLink, error) {
	if l.ID == "" || l.FromHost == "" || l.FromAgent == "" || l.ToSession == "" {
		return AgentMsgLink{}, errors.New("invalid agent message link")
	}
	if l.CreatedAt == 0 {
		l.CreatedAt = s.now().UnixMilli()
	}
	paused := 0
	if l.Paused {
		paused = 1
	}
	if _, err := s.db.Exec(`INSERT INTO agent_msg_links (id, fromHost, fromAgent, fromName, toSession, delivery, budgetPerHour, maxHops, paused, source, createdAt)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(fromHost, fromAgent, toSession) DO UPDATE SET fromName=CASE WHEN excluded.fromName != '' THEN excluded.fromName ELSE fromName END,
delivery=excluded.delivery, budgetPerHour=excluded.budgetPerHour, maxHops=excluded.maxHops, paused=excluded.paused, source=excluded.source`,
		l.ID, l.FromHost, l.FromAgent, l.FromName, l.ToSession, l.Delivery, l.BudgetPerHour, l.MaxHops, paused, l.Source, l.CreatedAt); err != nil {
		return AgentMsgLink{}, err
	}
	stored, err := s.AgentMsgLink(l.FromHost, l.FromAgent, l.ToSession)
	if err != nil {
		return AgentMsgLink{}, err
	}
	return *stored, nil
}

const agentMsgLinkColumns = `id, fromHost, fromAgent, fromName, toSession, delivery, budgetPerHour, maxHops, paused, source, createdAt`

func scanAgentMsgLink(row interface{ Scan(...any) error }) (AgentMsgLink, error) {
	var l AgentMsgLink
	var paused int
	err := row.Scan(&l.ID, &l.FromHost, &l.FromAgent, &l.FromName, &l.ToSession, &l.Delivery, &l.BudgetPerHour, &l.MaxHops, &paused, &l.Source, &l.CreatedAt)
	l.Paused = paused != 0
	return l, err
}

// AgentMsgLink returns the link granting (fromHost, fromAgent) access to
// toSession, or nil.
func (s *Store) AgentMsgLink(fromHost, fromAgent, toSession string) (*AgentMsgLink, error) {
	l, err := scanAgentMsgLink(s.db.QueryRow(`SELECT `+agentMsgLinkColumns+` FROM agent_msg_links WHERE fromHost = ? AND fromAgent = ? AND toSession = ?`, fromHost, fromAgent, toSession))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// AgentMsgLinks lists the inbound links of one session, oldest first.
func (s *Store) AgentMsgLinks(toSession string) ([]AgentMsgLink, error) {
	rows, err := s.db.Query(`SELECT `+agentMsgLinkColumns+` FROM agent_msg_links WHERE toSession = ? ORDER BY createdAt, id`, toSession)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMsgLink{}
	for rows.Next() {
		l, err := scanAgentMsgLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteAgentMsgLink removes one link and reports whether it existed.
func (s *Store) DeleteAgentMsgLink(toSession, fromHost, fromAgent string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM agent_msg_links WHERE toSession = ? AND fromHost = ? AND fromAgent = ?`, toSession, fromHost, fromAgent)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AgentMsgInbound records an accepted inbound envelope. It is the dedupe key
// and the budget ledger (rows with a LinkID count against that link).
type AgentMsgInbound struct {
	ID         string
	ToSession  string
	LinkID     string
	FromHost   string
	FromAgent  string
	Kind       string
	Status     string
	ReceivedAt int64
}

func (s *Store) AgentMsgInbound(id string) (*AgentMsgInbound, error) {
	var r AgentMsgInbound
	err := s.db.QueryRow(`SELECT id, toSession, linkId, fromHost, fromAgent, kind, status, receivedAt FROM agent_msg_inbound WHERE id = ?`, id).
		Scan(&r.ID, &r.ToSession, &r.LinkID, &r.FromHost, &r.FromAgent, &r.Kind, &r.Status, &r.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AddAgentMsgInbound stores r unless its ID is already present and reports
// whether it was inserted.
func (s *Store) AddAgentMsgInbound(r AgentMsgInbound) (bool, error) {
	if r.ReceivedAt == 0 {
		r.ReceivedAt = s.now().UnixMilli()
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO agent_msg_inbound (id, toSession, linkId, fromHost, fromAgent, kind, status, receivedAt) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ToSession, r.LinkID, r.FromHost, r.FromAgent, r.Kind, r.Status, r.ReceivedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AgentMsgInboundSince counts messages accepted over a link at or after since
// (unix milliseconds).
func (s *Store) AgentMsgInboundSince(linkID string, since int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM agent_msg_inbound WHERE linkId = ? AND receivedAt >= ?`, linkID, since).Scan(&n)
	return n, err
}

// PruneAgentMsgInbound drops dedupe/budget rows older than before.
func (s *Store) PruneAgentMsgInbound(before int64) error {
	_, err := s.db.Exec(`DELETE FROM agent_msg_inbound WHERE receivedAt < ?`, before)
	return err
}

// AgentMsgOutbox is one persisted outbound envelope awaiting (or having
// completed) delivery to another host.
type AgentMsgOutbox struct {
	ID            string
	FromSession   string
	ToHost        string
	ToAgent       string
	Kind          string
	RequestID     string
	Envelope      string
	Status        string // pending | delivered | failed
	Result        string
	Error         string
	Attempts      int
	CreatedAt     int64
	NextAttemptAt int64
	UpdatedAt     int64
}

const agentMsgOutboxColumns = `id, fromSession, toHost, toAgent, kind, requestId, envelope, status, result, error, attempts, createdAt, nextAttemptAt, updatedAt`

func scanAgentMsgOutbox(row interface{ Scan(...any) error }) (AgentMsgOutbox, error) {
	var o AgentMsgOutbox
	err := row.Scan(&o.ID, &o.FromSession, &o.ToHost, &o.ToAgent, &o.Kind, &o.RequestID, &o.Envelope, &o.Status, &o.Result, &o.Error, &o.Attempts, &o.CreatedAt, &o.NextAttemptAt, &o.UpdatedAt)
	return o, err
}

func (s *Store) AddAgentMsgOutbox(o AgentMsgOutbox) error {
	now := s.now().UnixMilli()
	if o.CreatedAt == 0 {
		o.CreatedAt = now
	}
	if o.NextAttemptAt == 0 {
		o.NextAttemptAt = o.CreatedAt
	}
	o.UpdatedAt = now
	if o.Status == "" {
		o.Status = "pending"
	}
	_, err := s.db.Exec(`INSERT INTO agent_msg_outbox (`+agentMsgOutboxColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.FromSession, o.ToHost, o.ToAgent, o.Kind, o.RequestID, o.Envelope, o.Status, o.Result, o.Error, o.Attempts, o.CreatedAt, o.NextAttemptAt, o.UpdatedAt)
	return err
}

func (s *Store) AgentMsgOutbox(id string) (*AgentMsgOutbox, error) {
	o, err := scanAgentMsgOutbox(s.db.QueryRow(`SELECT `+agentMsgOutboxColumns+` FROM agent_msg_outbox WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// PendingAgentMsgOutbox lists undelivered envelopes, oldest first.
func (s *Store) PendingAgentMsgOutbox() ([]AgentMsgOutbox, error) {
	rows, err := s.db.Query(`SELECT ` + agentMsgOutboxColumns + ` FROM agent_msg_outbox WHERE status = 'pending' ORDER BY createdAt, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMsgOutbox{}
	for rows.Next() {
		o, err := scanAgentMsgOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpdateAgentMsgOutbox records the outcome of an attempt.
func (s *Store) UpdateAgentMsgOutbox(id, status, result, errText string, attempts int, nextAttemptAt int64) error {
	_, err := s.db.Exec(`UPDATE agent_msg_outbox SET status = ?, result = ?, error = ?, attempts = ?, nextAttemptAt = ?, updatedAt = ? WHERE id = ?`,
		status, result, errText, attempts, nextAttemptAt, s.now().UnixMilli(), id)
	return err
}

// PruneAgentMsgOutbox drops finished envelopes last touched before before.
func (s *Store) PruneAgentMsgOutbox(before int64) error {
	_, err := s.db.Exec(`DELETE FROM agent_msg_outbox WHERE status != 'pending' AND updatedAt < ?`, before)
	return err
}

// AgentMsgRequest is an open (or recently closed) outbound ask, held by the
// asker's host.
type AgentMsgRequest struct {
	RequestID  string
	Session    string
	ToHost     string
	ToAgent    string
	ToName     string
	ThreadID   string
	MessageID  string
	Status     string // open | replied | declined | timeout | gone | closed
	TimeoutSec int
	CreatedAt  int64
	Deadline   int64
	ClosedAt   int64
}

const agentMsgRequestColumns = `requestId, session, toHost, toAgent, toName, threadId, messageId, status, timeoutSec, createdAt, deadline, COALESCE(closedAt, 0)`

func scanAgentMsgRequest(row interface{ Scan(...any) error }) (AgentMsgRequest, error) {
	var r AgentMsgRequest
	err := row.Scan(&r.RequestID, &r.Session, &r.ToHost, &r.ToAgent, &r.ToName, &r.ThreadID, &r.MessageID, &r.Status, &r.TimeoutSec, &r.CreatedAt, &r.Deadline, &r.ClosedAt)
	return r, err
}

func (s *Store) AddAgentMsgRequest(r AgentMsgRequest) error {
	if r.Status == "" {
		r.Status = "open"
	}
	_, err := s.db.Exec(`INSERT INTO agent_msg_requests (requestId, session, toHost, toAgent, toName, threadId, messageId, status, timeoutSec, createdAt, deadline) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RequestID, r.Session, r.ToHost, r.ToAgent, r.ToName, r.ThreadID, r.MessageID, r.Status, r.TimeoutSec, r.CreatedAt, r.Deadline)
	return err
}

func (s *Store) AgentMsgRequest(requestID string) (*AgentMsgRequest, error) {
	r, err := scanAgentMsgRequest(s.db.QueryRow(`SELECT `+agentMsgRequestColumns+` FROM agent_msg_requests WHERE requestId = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) queryAgentMsgRequests(query string, args ...any) ([]AgentMsgRequest, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMsgRequest{}
	for rows.Next() {
		r, err := scanAgentMsgRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenAgentMsgRequests lists every open outbound ask, oldest first.
func (s *Store) OpenAgentMsgRequests() ([]AgentMsgRequest, error) {
	return s.queryAgentMsgRequests(`SELECT ` + agentMsgRequestColumns + ` FROM agent_msg_requests WHERE status = 'open' ORDER BY createdAt, requestId`)
}

// DueAgentMsgRequests lists open asks whose deadline is at or before now.
func (s *Store) DueAgentMsgRequests(now int64) ([]AgentMsgRequest, error) {
	return s.queryAgentMsgRequests(`SELECT `+agentMsgRequestColumns+` FROM agent_msg_requests WHERE status = 'open' AND deadline <= ? ORDER BY deadline`, now)
}

// CloseAgentMsgRequest moves an open ask to status and reports whether this
// call closed it (false when it was already closed or unknown).
func (s *Store) CloseAgentMsgRequest(requestID, status string, at int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE agent_msg_requests SET status = ?, closedAt = ? WHERE requestId = ? AND status = 'open'`, status, at, requestID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PruneAgentMsgRequests drops closed asks closed before before.
func (s *Store) PruneAgentMsgRequests(before int64) error {
	_, err := s.db.Exec(`DELETE FROM agent_msg_requests WHERE status != 'open' AND COALESCE(closedAt, 0) < ?`, before)
	return err
}

// AgentMsgObligation is an inbound ask the local agent has not yet answered,
// held by the recipient's host.
type AgentMsgObligation struct {
	RequestID      string
	Session        string
	AskerHost      string
	AskerAgent     string
	AskerName      string
	ThreadID       string
	MessageID      string
	Hop            int
	ArmedPrompt    string // prompt-queue ID whose turn carries the ask; "" = the active turn at steer time
	Reminded       bool
	ReminderPrompt string
	Status         string // open | closed
	CreatedAt      int64
}

const agentMsgObligationColumns = `requestId, session, askerHost, askerAgent, askerName, threadId, messageId, hop, armedPrompt, reminded, reminderPrompt, status, createdAt`

func scanAgentMsgObligation(row interface{ Scan(...any) error }) (AgentMsgObligation, error) {
	var o AgentMsgObligation
	var reminded int
	err := row.Scan(&o.RequestID, &o.Session, &o.AskerHost, &o.AskerAgent, &o.AskerName, &o.ThreadID, &o.MessageID, &o.Hop, &o.ArmedPrompt, &reminded, &o.ReminderPrompt, &o.Status, &o.CreatedAt)
	o.Reminded = reminded != 0
	return o, err
}

func (s *Store) AddAgentMsgObligation(o AgentMsgObligation) error {
	if o.Status == "" {
		o.Status = "open"
	}
	reminded := 0
	if o.Reminded {
		reminded = 1
	}
	_, err := s.db.Exec(`INSERT INTO agent_msg_obligations (`+agentMsgObligationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.RequestID, o.Session, o.AskerHost, o.AskerAgent, o.AskerName, o.ThreadID, o.MessageID, o.Hop, o.ArmedPrompt, reminded, o.ReminderPrompt, o.Status, o.CreatedAt)
	return err
}

func (s *Store) AgentMsgObligation(requestID string) (*AgentMsgObligation, error) {
	o, err := scanAgentMsgObligation(s.db.QueryRow(`SELECT `+agentMsgObligationColumns+` FROM agent_msg_obligations WHERE requestId = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// OpenAgentMsgObligations lists a session's unanswered inbound asks (every
// session when session is empty), oldest first.
func (s *Store) OpenAgentMsgObligations(session string) ([]AgentMsgObligation, error) {
	query := `SELECT ` + agentMsgObligationColumns + ` FROM agent_msg_obligations WHERE status = 'open'`
	args := []any{}
	if session != "" {
		query += ` AND session = ?`
		args = append(args, session)
	}
	rows, err := s.db.Query(query+` ORDER BY createdAt, requestId`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMsgObligation{}
	for rows.Next() {
		o, err := scanAgentMsgObligation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RemindAgentMsgObligation marks an obligation as reminded by prompt.
func (s *Store) RemindAgentMsgObligation(requestID, reminderPrompt string) error {
	_, err := s.db.Exec(`UPDATE agent_msg_obligations SET reminded = 1, reminderPrompt = ? WHERE requestId = ?`, reminderPrompt, requestID)
	return err
}

// CloseAgentMsgObligation closes an open obligation and reports whether this
// call closed it.
func (s *Store) CloseAgentMsgObligation(requestID string, at int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE agent_msg_obligations SET status = 'closed', closedAt = ? WHERE requestId = ? AND status = 'open'`, at, requestID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) PruneAgentMsgObligations(before int64) error {
	_, err := s.db.Exec(`DELETE FROM agent_msg_obligations WHERE status = 'closed' AND COALESCE(closedAt, 0) < ?`, before)
	return err
}

// AgentMsgLinkRequest is a pending request for a link. role "out" rows live on
// the requester's host (session is the requesting agent), role "in" rows on
// the recipient's host (session is the agent being asked).
type AgentMsgLinkRequest struct {
	ID        string
	Role      string
	Session   string
	PeerHost  string
	PeerAgent string
	PeerName  string
	Reason    string
	CreatedAt int64
}

const agentMsgLinkRequestColumns = `id, role, session, peerHost, peerAgent, peerName, reason, createdAt`

// AddAgentMsgLinkRequest stores r unless the same (role, session, peer) is
// already pending; it reports whether a row was added.
func (s *Store) AddAgentMsgLinkRequest(r AgentMsgLinkRequest) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO agent_msg_link_requests (`+agentMsgLinkRequestColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Role, r.Session, r.PeerHost, r.PeerAgent, r.PeerName, r.Reason, r.CreatedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) queryAgentMsgLinkRequests(query string, args ...any) ([]AgentMsgLinkRequest, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMsgLinkRequest{}
	for rows.Next() {
		var r AgentMsgLinkRequest
		if err := rows.Scan(&r.ID, &r.Role, &r.Session, &r.PeerHost, &r.PeerAgent, &r.PeerName, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AgentMsgLinkRequests lists pending link requests of one role, oldest first.
func (s *Store) AgentMsgLinkRequests(role string) ([]AgentMsgLinkRequest, error) {
	return s.queryAgentMsgLinkRequests(`SELECT `+agentMsgLinkRequestColumns+` FROM agent_msg_link_requests WHERE role = ? ORDER BY createdAt, id`, role)
}

// AgentMsgLinkRequestFor returns the pending request of role for session and
// peer, or nil.
func (s *Store) AgentMsgLinkRequestFor(role, session, peerHost, peerAgent string) (*AgentMsgLinkRequest, error) {
	rows, err := s.queryAgentMsgLinkRequests(`SELECT `+agentMsgLinkRequestColumns+` FROM agent_msg_link_requests WHERE role = ? AND session = ? AND peerHost = ? AND peerAgent = ?`, role, session, peerHost, peerAgent)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// AgentMsgLinkRequestsForSession lists pending requests of role for session.
func (s *Store) AgentMsgLinkRequestsForSession(role, session string) ([]AgentMsgLinkRequest, error) {
	return s.queryAgentMsgLinkRequests(`SELECT `+agentMsgLinkRequestColumns+` FROM agent_msg_link_requests WHERE role = ? AND session = ? ORDER BY createdAt, id`, role, session)
}

// DeleteAgentMsgLinkRequest removes one pending request and reports whether it
// existed, making it an atomic claim for whoever resolves the request.
func (s *Store) DeleteAgentMsgLinkRequest(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM agent_msg_link_requests WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AgentMsgSettings is a session's directory presence.
type AgentMsgSettings struct {
	Listed bool
	Card   string
}

// AgentMsgSettings returns a session's settings; the default is listed with no
// card.
func (s *Store) AgentMsgSettings(sessionID string) (AgentMsgSettings, error) {
	var listed int
	var card string
	err := s.db.QueryRow(`SELECT listed, card FROM agent_msg_settings WHERE sessionId = ?`, sessionID).Scan(&listed, &card)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentMsgSettings{Listed: true}, nil
	}
	if err != nil {
		return AgentMsgSettings{}, err
	}
	return AgentMsgSettings{Listed: listed != 0, Card: card}, nil
}

// AllAgentMsgSettings returns every non-default settings row keyed by session.
func (s *Store) AllAgentMsgSettings() (map[string]AgentMsgSettings, error) {
	rows, err := s.db.Query(`SELECT sessionId, listed, card FROM agent_msg_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AgentMsgSettings{}
	for rows.Next() {
		var id, card string
		var listed int
		if err := rows.Scan(&id, &listed, &card); err != nil {
			return nil, err
		}
		out[id] = AgentMsgSettings{Listed: listed != 0, Card: card}
	}
	return out, rows.Err()
}

func (s *Store) SetAgentMsgListed(sessionID string, listed bool) error {
	v := 0
	if listed {
		v = 1
	}
	_, err := s.db.Exec(`INSERT INTO agent_msg_settings (sessionId, listed, card) VALUES (?, ?, '')
ON CONFLICT(sessionId) DO UPDATE SET listed = excluded.listed`, sessionID, v)
	return err
}

func (s *Store) SetAgentMsgCard(sessionID, card string) error {
	_, err := s.db.Exec(`INSERT INTO agent_msg_settings (sessionId, listed, card) VALUES (?, 1, ?)
ON CONFLICT(sessionId) DO UPDATE SET card = excluded.card`, sessionID, card)
	return err
}

// AgentMsgThreadHop returns the highest hop this session has seen or sent on
// a thread, and whether the thread is known.
func (s *Store) AgentMsgThreadHop(sessionID, threadID string) (int, bool, error) {
	var hop int
	err := s.db.QueryRow(`SELECT hop FROM agent_msg_threads WHERE sessionId = ? AND threadId = ?`, sessionID, threadID).Scan(&hop)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return hop, err == nil, err
}

// NoteAgentMsgThreadHop raises a thread's recorded hop to at least hop.
func (s *Store) NoteAgentMsgThreadHop(sessionID, threadID string, hop int) error {
	_, err := s.db.Exec(`INSERT INTO agent_msg_threads (sessionId, threadId, hop, updatedAt) VALUES (?, ?, ?, ?)
ON CONFLICT(sessionId, threadId) DO UPDATE SET hop = MAX(hop, excluded.hop), updatedAt = excluded.updatedAt`,
		sessionID, threadID, hop, s.now().UnixMilli())
	return err
}

func (s *Store) PruneAgentMsgThreads(before int64) error {
	_, err := s.db.Exec(`DELETE FROM agent_msg_threads WHERE updatedAt < ?`, before)
	return err
}

// AgentMsgMeta reads a host-wide messaging value ("" when unset).
func (s *Store) AgentMsgMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM agent_msg_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetAgentMsgMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO agent_msg_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// deleteAgentMessagingForSession forgets a deleted session's messaging state.
func deleteAgentMessagingForSession(tx interface {
	Exec(string, ...any) (sql.Result, error)
}, id string) error {
	for _, q := range []string{
		"DELETE FROM agent_msg_links WHERE toSession = ?",
		"DELETE FROM agent_msg_settings WHERE sessionId = ?",
		"DELETE FROM agent_msg_threads WHERE sessionId = ?",
		"DELETE FROM agent_msg_requests WHERE session = ?",
		"DELETE FROM agent_msg_obligations WHERE session = ?",
		"DELETE FROM agent_msg_link_requests WHERE session = ?",
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}
