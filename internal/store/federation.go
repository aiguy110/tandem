package store

import (
	"database/sql"
	"errors"
)

// FederationMaster is this daemon's durable enrollment with an upstream
// Tandem. There can be at most one: chained federation is deliberately not
// supported.
type FederationMaster struct {
	URL        string
	HostID     string
	Credential string
	UpdatedAt  int64
}

// FederationSlave is a peer that was allowed by this daemon's operator to
// offer its local agent host. Credential is a random bearer secret and must
// never be included in browser protocol responses.
type FederationSlave struct {
	ID          string
	Name        string
	Endpoint    string
	Credential  string
	Status      string
	RequestedAt int64
	AcceptedAt  int64
	LastSeenAt  int64
	// ProtocolVersion and BuildVersion are what the host reported on its most
	// recent connection. Zero and "" mean a host too old to report either.
	ProtocolVersion int
	BuildVersion    string
}

func (s *Store) FederationMaster() (*FederationMaster, error) {
	var m FederationMaster
	err := s.db.QueryRow(`SELECT url, hostId, credential, updatedAt FROM federation_master WHERE singleton = 1`).Scan(&m.URL, &m.HostID, &m.Credential, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

func (s *Store) SaveFederationMaster(m FederationMaster) error {
	if m.URL == "" || m.HostID == "" || m.Credential == "" {
		return errors.New("invalid federation master")
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO federation_master (singleton, url, hostId, credential, updatedAt) VALUES (1, ?, ?, ?, ?)
ON CONFLICT(singleton) DO UPDATE SET url=excluded.url, hostId=excluded.hostId, credential=excluded.credential, updatedAt=excluded.updatedAt`, m.URL, m.HostID, m.Credential, m.UpdatedAt)
	return err
}

func (s *Store) ClearFederationMaster() error {
	_, err := s.db.Exec(`DELETE FROM federation_master WHERE singleton = 1`)
	return err
}

func (s *Store) UpsertFederationSlave(peer FederationSlave) error {
	if peer.ID == "" || peer.Status == "" {
		return errors.New("invalid federation slave")
	}
	if peer.RequestedAt == 0 {
		peer.RequestedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO federation_slaves (id, name, endpoint, credential, status, requestedAt, acceptedAt, lastSeenAt, protocolVersion, buildVersion)
VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, endpoint=excluded.endpoint, credential=CASE WHEN excluded.credential = '' THEN federation_slaves.credential ELSE excluded.credential END, status=excluded.status, requestedAt=excluded.requestedAt, acceptedAt=excluded.acceptedAt, lastSeenAt=excluded.lastSeenAt, protocolVersion=excluded.protocolVersion, buildVersion=excluded.buildVersion`,
		peer.ID, peer.Name, peer.Endpoint, peer.Credential, peer.Status, peer.RequestedAt, peer.AcceptedAt, peer.LastSeenAt, peer.ProtocolVersion, peer.BuildVersion)
	return err
}

func (s *Store) FederationSlave(id string) (*FederationSlave, error) {
	var peer FederationSlave
	err := s.db.QueryRow(`SELECT id, name, endpoint, credential, status, requestedAt, COALESCE(acceptedAt,0), COALESCE(lastSeenAt,0), protocolVersion, buildVersion FROM federation_slaves WHERE id = ?`, id).
		Scan(&peer.ID, &peer.Name, &peer.Endpoint, &peer.Credential, &peer.Status, &peer.RequestedAt, &peer.AcceptedAt, &peer.LastSeenAt, &peer.ProtocolVersion, &peer.BuildVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &peer, err
}

func (s *Store) FederationSlaves() ([]FederationSlave, error) {
	rows, err := s.db.Query(`SELECT id, name, endpoint, credential, status, requestedAt, COALESCE(acceptedAt,0), COALESCE(lastSeenAt,0), protocolVersion, buildVersion FROM federation_slaves ORDER BY requestedAt ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []FederationSlave
	for rows.Next() {
		var peer FederationSlave
		if err := rows.Scan(&peer.ID, &peer.Name, &peer.Endpoint, &peer.Credential, &peer.Status, &peer.RequestedAt, &peer.AcceptedAt, &peer.LastSeenAt, &peer.ProtocolVersion, &peer.BuildVersion); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

func (s *Store) DeleteFederationSlave(id string) error {
	_, err := s.db.Exec(`DELETE FROM federation_slaves WHERE id = ?`, id)
	return err
}
