package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const sqliteSiteColumns = `id, org_id, name, vpn_group, created_at`

func scanSQLiteSite(row interface{ Scan(...any) error }) (*Site, error) {
	s := &Site{}
	var created string
	if err := row.Scan(&s.ID, &s.OrgID, &s.Name, &s.VPNGroup, &created); err != nil {
		return nil, err
	}
	s.CreatedAt = parseSQLiteDateTime(created)
	return s, nil
}

// ListSites returns the sites of orgID, or every site when all is set,
// oldest first.
func (s *SQLiteDB) ListSites(orgID string, all bool) ([]Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	query := `SELECT ` + sqliteSiteColumns + ` FROM sites`
	var args []any
	if !all {
		query += ` WHERE org_id = ?`
		args = append(args, orgID)
	}
	rows, err := s.db.Query(query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list sites: %w", err)
	}
	defer rows.Close()
	out := []Site{}
	for rows.Next() {
		site, err := scanSQLiteSite(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list sites: %w", err)
		}
		out = append(out, *site)
	}
	return out, rows.Err()
}

// GetSite returns the site, or nil when there is none.
func (s *SQLiteDB) GetSite(id string) (*Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	site, err := scanSQLiteSite(s.db.QueryRow(`SELECT `+sqliteSiteColumns+` FROM sites WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get site: %w", err)
	}
	return site, nil
}

// CreateSite stores a new site; an empty ID gets a new UUID and a zero
// CreatedAt the current time.
func (s *SQLiteDB) CreateSite(site *Site) error {
	if site.ID == "" {
		site.ID = uuid.NewString()
	}
	site.CreatedAt = orNow(site.CreatedAt)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO sites (id, org_id, name, vpn_group, created_at) VALUES (?, ?, ?, ?, ?)`,
		site.ID, site.OrgID, site.Name, site.VPNGroup, sqliteSiteTime(site.CreatedAt))
	if err != nil {
		return fmt.Errorf("db: create site: %w", err)
	}
	return nil
}

// UpdateSite changes the name and VPN group of a site; its organization
// stays. ErrSiteNotFound when there is no such site.
func (s *SQLiteDB) UpdateSite(site *Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE sites SET name = ?, vpn_group = ? WHERE id = ?`, site.Name, site.VPNGroup, site.ID)
	if err != nil {
		return fmt.Errorf("db: update site: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSiteNotFound
	}
	return nil
}

// DeleteSite removes a site with its networks, relay nodes, enrollment
// tokens, pins and the reachability rows of its nodes, in one transaction.
func (s *SQLiteDB) DeleteSite(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("db: delete site: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`DELETE FROM site_reachability WHERE from_node IN (SELECT id FROM relay_nodes WHERE site_id = ?1)
		    OR to_node IN (SELECT id FROM relay_nodes WHERE site_id = ?1)`,
		`DELETE FROM relay_nodes WHERE site_id = ?1`,
		`DELETE FROM site_networks WHERE site_id = ?1`,
		`DELETE FROM site_enrollment_tokens WHERE site_id = ?1`,
		`DELETE FROM site_pins WHERE site_id = ?1`,
		`DELETE FROM sites WHERE id = ?1`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			return fmt.Errorf("db: delete site: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: delete site: %w", err)
	}
	return nil
}

func (s *SQLiteDB) querySiteNetworks(where string, args ...any) ([]SiteNetwork, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT id, site_id, cidr, kind FROM site_networks`+where+` ORDER BY site_id, cidr, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list site networks: %w", err)
	}
	defer rows.Close()
	out := []SiteNetwork{}
	for rows.Next() {
		var n SiteNetwork
		if err := rows.Scan(&n.ID, &n.SiteID, &n.CIDR, &n.Kind); err != nil {
			return nil, fmt.Errorf("db: list site networks: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListSiteNetworks returns the networks of a site.
func (s *SQLiteDB) ListSiteNetworks(siteID string) ([]SiteNetwork, error) {
	return s.querySiteNetworks(` WHERE site_id = ?`, siteID)
}

// ListAllSiteNetworks returns the networks of every site.
func (s *SQLiteDB) ListAllSiteNetworks() ([]SiteNetwork, error) {
	return s.querySiteNetworks(``)
}

// ReplaceSiteNetworks makes nets the networks of the site, in one
// transaction. Each network gets the site's id, and a new UUID when its ID
// is empty.
func (s *SQLiteDB) ReplaceSiteNetworks(siteID string, nets []SiteNetwork) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("db: replace site networks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM site_networks WHERE site_id = ?`, siteID); err != nil {
		return fmt.Errorf("db: replace site networks: %w", err)
	}
	for i := range nets {
		if nets[i].ID == "" {
			nets[i].ID = uuid.NewString()
		}
		nets[i].SiteID = siteID
		if _, err := tx.Exec(`INSERT INTO site_networks (id, site_id, cidr, kind) VALUES (?, ?, ?, ?)`,
			nets[i].ID, siteID, nets[i].CIDR, nets[i].Kind); err != nil {
			return fmt.Errorf("db: replace site networks: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: replace site networks: %w", err)
	}
	return nil
}

const sqliteRelayNodeColumns = `id, site_id, name, public_key, lan_addr, public_addr, version, last_seen, created_at, revoked_at`

func scanSQLiteRelayNode(row interface{ Scan(...any) error }) (*RelayNode, error) {
	n := &RelayNode{}
	var lastSeen, revoked sql.NullString
	var created string
	if err := row.Scan(&n.ID, &n.SiteID, &n.Name, &n.PublicKey, &n.LANAddr, &n.PublicAddr, &n.Version,
		&lastSeen, &created, &revoked); err != nil {
		return nil, err
	}
	n.CreatedAt = parseSQLiteDateTime(created)
	n.LastSeen = sqliteNullTime(lastSeen)
	n.RevokedAt = sqliteNullTime(revoked)
	return n, nil
}

func sqliteNullTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseSQLiteDateTime(v.String)
	return &t
}

// ListRelayNodes returns the relay nodes of a site, revoked ones included,
// or of every site when siteID is "".
func (s *SQLiteDB) ListRelayNodes(siteID string) ([]RelayNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	query := `SELECT ` + sqliteRelayNodeColumns + ` FROM relay_nodes`
	var args []any
	if siteID != "" {
		query += ` WHERE site_id = ?`
		args = append(args, siteID)
	}
	rows, err := s.db.Query(query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list relay nodes: %w", err)
	}
	defer rows.Close()
	out := []RelayNode{}
	for rows.Next() {
		n, err := scanSQLiteRelayNode(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list relay nodes: %w", err)
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *SQLiteDB) getRelayNode(where string, arg string) (*RelayNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, err := scanSQLiteRelayNode(s.db.QueryRow(`SELECT `+sqliteRelayNodeColumns+` FROM relay_nodes WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get relay node: %w", err)
	}
	return n, nil
}

// GetRelayNode returns the node, or nil when there is none.
func (s *SQLiteDB) GetRelayNode(id string) (*RelayNode, error) {
	return s.getRelayNode(`id = ?`, id)
}

// GetRelayNodeByKey returns the node with that public key, or nil.
func (s *SQLiteDB) GetRelayNodeByKey(publicKey string) (*RelayNode, error) {
	return s.getRelayNode(`public_key = ?`, publicKey)
}

// CreateRelayNode stores a new node; an empty ID gets a new UUID and a zero
// CreatedAt the current time. A public key another node has fails.
func (s *SQLiteDB) CreateRelayNode(n *RelayNode) error {
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	n.CreatedAt = orNow(n.CreatedAt)
	var lastSeen, revoked any
	if n.LastSeen != nil {
		lastSeen = sqliteSiteTime(*n.LastSeen)
	}
	if n.RevokedAt != nil {
		revoked = sqliteSiteTime(*n.RevokedAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO relay_nodes (`+sqliteRelayNodeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.SiteID, n.Name, n.PublicKey, n.LANAddr, n.PublicAddr, n.Version,
		lastSeen, sqliteSiteTime(n.CreatedAt), revoked)
	if err != nil {
		return fmt.Errorf("db: create relay node: %w", err)
	}
	return nil
}

func (s *SQLiteDB) execRelayNode(what, stmt string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(stmt, args...)
	if err != nil {
		return fmt.Errorf("db: %s: %w", what, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSiteNotFound
	}
	return nil
}

// UpdateRelayNodeState stores what a connected node reports and when it was
// last seen. ErrSiteNotFound when there is no such node.
func (s *SQLiteDB) UpdateRelayNodeState(id, version, lanAddr, publicAddr string, seen time.Time) error {
	return s.execRelayNode("update relay node",
		`UPDATE relay_nodes SET version = ?, lan_addr = ?, public_addr = ?, last_seen = ? WHERE id = ?`,
		version, lanAddr, publicAddr, sqliteSiteTime(seen), id)
}

// RenameRelayNode changes the name of a node.
func (s *SQLiteDB) RenameRelayNode(id, name string) error {
	return s.execRelayNode("rename relay node", `UPDATE relay_nodes SET name = ? WHERE id = ?`, name, id)
}

// RevokeRelayNode marks a node revoked at at; a node revoked before keeps
// its first time.
func (s *SQLiteDB) RevokeRelayNode(id string, at time.Time) error {
	return s.execRelayNode("revoke relay node",
		`UPDATE relay_nodes SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, sqliteSiteTime(at), id)
}

// CreateSiteEnrollmentToken stores a new token; a zero CreatedAt gets the
// current time.
func (s *SQLiteDB) CreateSiteEnrollmentToken(t *SiteEnrollmentToken) error {
	t.CreatedAt = orNow(t.CreatedAt)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO site_enrollment_tokens (token_hash, site_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		t.TokenHash, t.SiteID, t.CreatedBy, sqliteSiteTime(t.CreatedAt), sqliteSiteTime(t.ExpiresAt))
	if err != nil {
		return fmt.Errorf("db: create site enrollment token: %w", err)
	}
	return nil
}

const sqliteSiteTokenColumns = `token_hash, site_id, created_by, created_at, expires_at, used_at, used_by_node`

func scanSQLiteSiteToken(row interface{ Scan(...any) error }) (*SiteEnrollmentToken, error) {
	t := &SiteEnrollmentToken{}
	var created, expires string
	var used sql.NullString
	if err := row.Scan(&t.TokenHash, &t.SiteID, &t.CreatedBy, &created, &expires, &used, &t.UsedByNode); err != nil {
		return nil, err
	}
	t.CreatedAt = parseSQLiteDateTime(created)
	t.ExpiresAt = parseSQLiteDateTime(expires)
	t.UsedAt = sqliteNullTime(used)
	return t, nil
}

// ConsumeSiteEnrollmentToken uses the token with that hash for node nodeID
// and returns it. One UPDATE that only matches an unused token that has not
// expired at now does it, so a token is used once however many try at the
// same time; ErrSiteTokenInvalid for the others and for a missing, used or
// expired token.
func (s *SQLiteDB) ConsumeSiteEnrollmentToken(hash, nodeID string, now time.Time) (*SiteEnrollmentToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := sqliteSiteTime(now)
	res, err := s.db.Exec(`UPDATE site_enrollment_tokens SET used_at = ?, used_by_node = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, at, nodeID, hash, at)
	if err != nil {
		return nil, fmt.Errorf("db: consume site enrollment token: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrSiteTokenInvalid
	}
	t, err := scanSQLiteSiteToken(s.db.QueryRow(`SELECT `+sqliteSiteTokenColumns+` FROM site_enrollment_tokens WHERE token_hash = ?`, hash))
	if err != nil {
		return nil, fmt.Errorf("db: consume site enrollment token: %w", err)
	}
	return t, nil
}

// ListSiteEnrollmentTokens returns the tokens of a site, used and expired
// ones included, oldest first.
func (s *SQLiteDB) ListSiteEnrollmentTokens(siteID string) ([]SiteEnrollmentToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT `+sqliteSiteTokenColumns+` FROM site_enrollment_tokens
		WHERE site_id = ? ORDER BY created_at, token_hash`, siteID)
	if err != nil {
		return nil, fmt.Errorf("db: list site enrollment tokens: %w", err)
	}
	defer rows.Close()
	out := []SiteEnrollmentToken{}
	for rows.Next() {
		t, err := scanSQLiteSiteToken(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list site enrollment tokens: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// SetSitePin pins a device or folder to a site, replacing its pin; siteID
// "" removes the pin.
func (s *SQLiteDB) SetSitePin(kind, ref, siteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if siteID == "" {
		_, err = s.db.Exec(`DELETE FROM site_pins WHERE kind = ? AND ref = ?`, kind, ref)
	} else {
		_, err = s.db.Exec(`INSERT INTO site_pins (kind, ref, site_id) VALUES (?, ?, ?)
			ON CONFLICT (kind, ref) DO UPDATE SET site_id = excluded.site_id`, kind, ref, siteID)
	}
	if err != nil {
		return fmt.Errorf("db: set site pin: %w", err)
	}
	return nil
}

// GetSitePin returns the site a device or folder is pinned to, "" for none.
func (s *SQLiteDB) GetSitePin(kind, ref string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var siteID string
	err := s.db.QueryRow(`SELECT site_id FROM site_pins WHERE kind = ? AND ref = ?`, kind, ref).Scan(&siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: get site pin: %w", err)
	}
	return siteID, nil
}

// ListSitePins returns every pin.
func (s *SQLiteDB) ListSitePins() ([]SitePin, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT kind, ref, site_id FROM site_pins ORDER BY kind, ref`)
	if err != nil {
		return nil, fmt.Errorf("db: list site pins: %w", err)
	}
	defer rows.Close()
	out := []SitePin{}
	for rows.Next() {
		var p SitePin
		if err := rows.Scan(&p.Kind, &p.Ref, &p.SiteID); err != nil {
			return nil, fmt.Errorf("db: list site pins: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertSiteReachability stores a probe result, replacing the one of the
// same nodes and way; a zero CheckedAt gets the current time.
func (s *SQLiteDB) UpsertSiteReachability(r *SiteReachability) error {
	r.CheckedAt = orNow(r.CheckedAt)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO site_reachability (from_node, to_node, via, ok, rtt_ms, checked_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (from_node, to_node, via) DO UPDATE SET
			ok = excluded.ok, rtt_ms = excluded.rtt_ms, checked_at = excluded.checked_at`,
		r.FromNode, r.ToNode, r.Via, r.OK, r.RTTMs, sqliteSiteTime(r.CheckedAt))
	if err != nil {
		return fmt.Errorf("db: upsert site reachability: %w", err)
	}
	return nil
}

// ListSiteReachability returns every stored probe result.
func (s *SQLiteDB) ListSiteReachability() ([]SiteReachability, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT from_node, to_node, via, ok, rtt_ms, checked_at FROM site_reachability
		ORDER BY from_node, to_node, via`)
	if err != nil {
		return nil, fmt.Errorf("db: list site reachability: %w", err)
	}
	defer rows.Close()
	out := []SiteReachability{}
	for rows.Next() {
		var r SiteReachability
		var checked string
		if err := rows.Scan(&r.FromNode, &r.ToNode, &r.Via, &r.OK, &r.RTTMs, &checked); err != nil {
			return nil, fmt.Errorf("db: list site reachability: %w", err)
		}
		r.CheckedAt = parseSQLiteDateTime(checked)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetConnectionSessionRelayVia records how the sessions of a relay UUID are
// relayed ("direct", "central", "lan", "node:<id>", "nodes:<a>,<b>").
func (s *SQLiteDB) SetConnectionSessionRelayVia(relayUUID, via string) error {
	relayUUID = strings.TrimSpace(relayUUID)
	if relayUUID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE peer_connection_sessions SET relay_via = ? WHERE relay_uuid = ?`, via, relayUUID); err != nil {
		return fmt.Errorf("db: set connection session relay via: %w", err)
	}
	return nil
}
