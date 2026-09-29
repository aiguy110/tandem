package store

import (
	"database/sql"
	"errors"
)

// FederationParent is this daemon's durable enrollment with its parent. There
// is at most one. URL is the parent this daemon dials; it is empty when
// Adopted, because an adopting parent dials this daemon instead and proves
// itself with Credential.
type FederationParent struct {
	URL        string
	HostID     string
	Credential string
	Adopted    bool
	UpdatedAt  int64
}

// FederationChild is a peer that was allowed by this daemon's operator to
// offer its local agent host. Credential is a random bearer secret and must
// never be included in browser protocol responses.
type FederationChild struct {
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

func (s *Store) FederationParent() (*FederationParent, error) {
	var m FederationParent
	err := s.db.QueryRow(`SELECT url, hostId, credential, adopted != 0, updatedAt FROM federation_master WHERE singleton = 1`).Scan(&m.URL, &m.HostID, &m.Credential, &m.Adopted, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

func (s *Store) SaveFederationParent(m FederationParent) error {
	if (m.URL == "") != m.Adopted || m.HostID == "" || m.Credential == "" {
		return errors.New("invalid federation parent")
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO federation_master (singleton, url, hostId, credential, adopted, updatedAt) VALUES (1, ?, ?, ?, ?, ?)
ON CONFLICT(singleton) DO UPDATE SET url=excluded.url, hostId=excluded.hostId, credential=excluded.credential, adopted=excluded.adopted, updatedAt=excluded.updatedAt`, m.URL, m.HostID, m.Credential, m.Adopted, m.UpdatedAt)
	return err
}

func (s *Store) ClearFederationParent() error {
	_, err := s.db.Exec(`DELETE FROM federation_master WHERE singleton = 1`)
	return err
}

func (s *Store) UpsertFederationChild(peer FederationChild) error {
	if peer.ID == "" || peer.Status == "" {
		return errors.New("invalid federation child")
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

func (s *Store) FederationChild(id string) (*FederationChild, error) {
	var peer FederationChild
	err := s.db.QueryRow(`SELECT id, name, endpoint, credential, status, requestedAt, COALESCE(acceptedAt,0), COALESCE(lastSeenAt,0), protocolVersion, buildVersion FROM federation_slaves WHERE id = ?`, id).
		Scan(&peer.ID, &peer.Name, &peer.Endpoint, &peer.Credential, &peer.Status, &peer.RequestedAt, &peer.AcceptedAt, &peer.LastSeenAt, &peer.ProtocolVersion, &peer.BuildVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &peer, err
}

func (s *Store) FederationChildren() ([]FederationChild, error) {
	rows, err := s.db.Query(`SELECT id, name, endpoint, credential, status, requestedAt, COALESCE(acceptedAt,0), COALESCE(lastSeenAt,0), protocolVersion, buildVersion FROM federation_slaves ORDER BY requestedAt ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []FederationChild
	for rows.Next() {
		var peer FederationChild
		if err := rows.Scan(&peer.ID, &peer.Name, &peer.Endpoint, &peer.Credential, &peer.Status, &peer.RequestedAt, &peer.AcceptedAt, &peer.LastSeenAt, &peer.ProtocolVersion, &peer.BuildVersion); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

func (s *Store) DeleteFederationChild(id string) error {
	_, err := s.db.Exec(`DELETE FROM federation_slaves WHERE id = ?`, id)
	return err
}

// FederationIdentity is the host ID a daemon without a parent uses to name
// itself to the hosts below it. A daemon with a parent uses the ID that
// parent accepted instead; "" means none has been generated yet.
func (s *Store) FederationIdentity() (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT hostId FROM federation_identity WHERE singleton = 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) SaveFederationIdentity(hostID string) error {
	if hostID == "" {
		return errors.New("invalid federation identity")
	}
	_, err := s.db.Exec(`INSERT INTO federation_identity (singleton, hostId) VALUES (1, ?)
ON CONFLICT(singleton) DO UPDATE SET hostId=excluded.hostId`, hostID)
	return err
}

// FederationAdoption is a child this daemon dials and adopts, keyed by the
// URL it dials. Credential is generated here before the first dial and is
// what the child comes to trust; HostID is the ID the child reported, empty
// until it first connects.
type FederationAdoption struct {
	URL        string
	Credential string
	HostID     string
	UpdatedAt  int64
}

func (s *Store) FederationAdoption(url string) (*FederationAdoption, error) {
	var a FederationAdoption
	err := s.db.QueryRow(`SELECT url, credential, hostId, updatedAt FROM federation_adoptions WHERE url = ?`, url).Scan(&a.URL, &a.Credential, &a.HostID, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) SaveFederationAdoption(a FederationAdoption) error {
	if a.URL == "" || a.Credential == "" {
		return errors.New("invalid federation adoption")
	}
	if a.UpdatedAt == 0 {
		a.UpdatedAt = s.now().UnixMilli()
	}
	_, err := s.db.Exec(`INSERT INTO federation_adoptions (url, credential, hostId, updatedAt) VALUES (?, ?, ?, ?)
ON CONFLICT(url) DO UPDATE SET credential=excluded.credential, hostId=excluded.hostId, updatedAt=excluded.updatedAt`, a.URL, a.Credential, a.HostID, a.UpdatedAt)
	return err
}

// FederationAdoptedHostIDs lists the child host IDs this daemon dials (adopts)
// rather than being dialed by.
func (s *Store) FederationAdoptedHostIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT hostId FROM federation_adoptions WHERE hostId != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// FederationHostNames are operator-chosen display names for hosts, keyed by
// the host ID this daemon addresses them by ("local" for itself). They are a
// per-daemon view preference and are never advertised to other hosts.
func (s *Store) FederationHostNames() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT hostId, name FROM federation_host_names`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// SetFederationHostName saves a host's display name; "" clears it.
func (s *Store) SetFederationHostName(hostID, name string) error {
	if hostID == "" {
		return errors.New("invalid host ID")
	}
	if name == "" {
		_, err := s.db.Exec(`DELETE FROM federation_host_names WHERE hostId = ?`, hostID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO federation_host_names (hostId, name, updatedAt) VALUES (?, ?, ?)
ON CONFLICT(hostId) DO UPDATE SET name=excluded.name, updatedAt=excluded.updatedAt`, hostID, name, s.now().UnixMilli())
	return err
}
