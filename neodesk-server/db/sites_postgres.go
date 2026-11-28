package db

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const pgSiteColumns = `id, org_id, name, vpn_group, created_at`

func scanPgSite(row pgx.Row) (*Site, error) {
	s := &Site{}
	if err := row.Scan(&s.ID, &s.OrgID, &s.Name, &s.VPNGroup, &s.CreatedAt); err != nil {
		return nil, err
	}
	s.CreatedAt = s.CreatedAt.UTC()
	return s, nil
}

// ListSites returns the sites of orgID, or every site when all is set,
// oldest first.
func (pg *PostgresDB) ListSites(orgID string, all bool) ([]Site, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	query := `SELECT ` + pgSiteColumns + ` FROM sites`
	var args []any
	if !all {
		query += ` WHERE org_id = $1`
		args = append(args, orgID)
	}
	rows, err := pg.pool.Query(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list sites: %w", err)
	}
	defer rows.Close()
	out := []Site{}
	for rows.Next() {
		site, err := scanPgSite(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list sites: %w", err)
		}
		out = append(out, *site)
	}
	return out, rows.Err()
}

// GetSite returns the site, or nil when there is none.
func (pg *PostgresDB) GetSite(id string) (*Site, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	site, err := scanPgSite(pg.pool.QueryRow(ctx, `SELECT `+pgSiteColumns+` FROM sites WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get site: %w", err)
	}
	return site, nil
}

// CreateSite stores a new site; an empty ID gets a new UUID and a zero
// CreatedAt the current time.
func (pg *PostgresDB) CreateSite(site *Site) error {
	if site.ID == "" {
		site.ID = uuid.NewString()
	}
	site.CreatedAt = orNow(site.CreatedAt)
	ctx, cancel := pg.opCtx()
	defer cancel()
	_, err := pg.pool.Exec(ctx, `INSERT INTO sites (id, org_id, name, vpn_group, created_at) VALUES ($1, $2, $3, $4, $5)`,
		site.ID, site.OrgID, site.Name, site.VPNGroup, site.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("db: create site: %w", err)
	}
	return nil
}

// UpdateSite changes the name and VPN group of a site; its organization
// stays. ErrSiteNotFound when there is no such site.
func (pg *PostgresDB) UpdateSite(site *Site) error {
	ctx, cancel := pg.opCtx()
	defer cancel()
	tag, err := pg.pool.Exec(ctx, `UPDATE sites SET name = $1, vpn_group = $2 WHERE id = $3`, site.Name, site.VPNGroup, site.ID)
	if err != nil {
		return fmt.Errorf("db: update site: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSiteNotFound
	}
	return nil
}

// DeleteSite removes a site with its networks, relay nodes, enrollment
// tokens, pins and the reachability rows of its nodes, in one transaction.
func (pg *PostgresDB) DeleteSite(id string) error {
	ctx, cancel := pg.opCtx()
	defer cancel()
	tx, err := pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: delete site: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`DELETE FROM site_reachability WHERE from_node IN (SELECT id FROM relay_nodes WHERE site_id = $1)
		    OR to_node IN (SELECT id FROM relay_nodes WHERE site_id = $1)`,
		`DELETE FROM relay_nodes WHERE site_id = $1`,
		`DELETE FROM site_networks WHERE site_id = $1`,
		`DELETE FROM site_enrollment_tokens WHERE site_id = $1`,
		`DELETE FROM site_pins WHERE site_id = $1`,
		`DELETE FROM sites WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, stmt, id); err != nil {
			return fmt.Errorf("db: delete site: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: delete site: %w", err)
	}
	return nil
}

func (pg *PostgresDB) querySiteNetworks(where string, args ...any) ([]SiteNetwork, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	rows, err := pg.pool.Query(ctx, `SELECT id, site_id, cidr, kind FROM site_networks`+where+` ORDER BY site_id, cidr, id`, args...)
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
func (pg *PostgresDB) ListSiteNetworks(siteID string) ([]SiteNetwork, error) {
	return pg.querySiteNetworks(` WHERE site_id = $1`, siteID)
}

// ListAllSiteNetworks returns the networks of every site.
func (pg *PostgresDB) ListAllSiteNetworks() ([]SiteNetwork, error) {
	return pg.querySiteNetworks(``)
}

// ReplaceSiteNetworks makes nets the networks of the site, in one
// transaction. Each network gets the site's id, and a new UUID when its ID
// is empty.
func (pg *PostgresDB) ReplaceSiteNetworks(siteID string, nets []SiteNetwork) error {
	ctx, cancel := pg.opCtx()
	defer cancel()
	tx, err := pg.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: replace site networks: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM site_networks WHERE site_id = $1`, siteID); err != nil {
		return fmt.Errorf("db: replace site networks: %w", err)
	}
	for i := range nets {
		if nets[i].ID == "" {
			nets[i].ID = uuid.NewString()
		}
		nets[i].SiteID = siteID
		if _, err := tx.Exec(ctx, `INSERT INTO site_networks (id, site_id, cidr, kind) VALUES ($1, $2, $3, $4)`,
			nets[i].ID, siteID, nets[i].CIDR, nets[i].Kind); err != nil {
			return fmt.Errorf("db: replace site networks: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: replace site networks: %w", err)
	}
	return nil
}

const pgRelayNodeColumns = `id, site_id, name, public_key, lan_addr, public_addr, version, last_seen, created_at, revoked_at`

func scanPgRelayNode(row pgx.Row) (*RelayNode, error) {
	n := &RelayNode{}
	if err := row.Scan(&n.ID, &n.SiteID, &n.Name, &n.PublicKey, &n.LANAddr, &n.PublicAddr, &n.Version,
		&n.LastSeen, &n.CreatedAt, &n.RevokedAt); err != nil {
		return nil, err
	}
	n.CreatedAt = n.CreatedAt.UTC()
	n.LastSeen = utcPtr(n.LastSeen)
	n.RevokedAt = utcPtr(n.RevokedAt)
	return n, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ListRelayNodes returns the relay nodes of a site, revoked ones included,
// or of every site when siteID is "".
func (pg *PostgresDB) ListRelayNodes(siteID string) ([]RelayNode, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	query := `SELECT ` + pgRelayNodeColumns + ` FROM relay_nodes`
	var args []any
	if siteID != "" {
		query += ` WHERE site_id = $1`
		args = append(args, siteID)
	}
	rows, err := pg.pool.Query(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list relay nodes: %w", err)
	}
	defer rows.Close()
	out := []RelayNode{}
	for rows.Next() {
		n, err := scanPgRelayNode(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list relay nodes: %w", err)
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (pg *PostgresDB) getRelayNode(where string, arg string) (*RelayNode, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	n, err := scanPgRelayNode(pg.pool.QueryRow(ctx, `SELECT `+pgRelayNodeColumns+` FROM relay_nodes WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get relay node: %w", err)
	}
	return n, nil
}

// GetRelayNode returns the node, or nil when there is none.
func (pg *PostgresDB) GetRelayNode(id string) (*RelayNode, error) {
	return pg.getRelayNode(`id = $1`, id)
}

// GetRelayNodeByKey returns the node with that public key, or nil.
func (pg *PostgresDB) GetRelayNodeByKey(publicKey string) (*RelayNode, error) {
	return pg.getRelayNode(`public_key = $1`, publicKey)
}

// CreateRelayNode stores a new node; an empty ID gets a new UUID and a zero
// CreatedAt the current time. A public key another node has fails.
func (pg *PostgresDB) CreateRelayNode(n *RelayNode) error {
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	n.CreatedAt = orNow(n.CreatedAt)
	ctx, cancel := pg.opCtx()
	defer cancel()
	_, err := pg.pool.Exec(ctx, `INSERT INTO relay_nodes (`+pgRelayNodeColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		n.ID, n.SiteID, n.Name, n.PublicKey, n.LANAddr, n.PublicAddr, n.Version,
		utcPtr(n.LastSeen), n.CreatedAt.UTC(), utcPtr(n.RevokedAt))
	if err != nil {
		return fmt.Errorf("db: create relay node: %w", err)
	}
	return nil
}

func (pg *PostgresDB) execRelayNode(what, stmt string, args ...any) error {
	ctx, cancel := pg.opCtx()
	defer cancel()
	tag, err := pg.pool.Exec(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("db: %s: %w", what, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSiteNotFound
	}
	return nil
}

// UpdateRelayNodeState stores what a connected node reports and when it was
// last seen. ErrSiteNotFound when there is no such node.
func (pg *PostgresDB) UpdateRelayNodeState(id, version, lanAddr, publicAddr string, seen time.Time) error {
	return pg.execRelayNode("update relay node",
		`UPDATE relay_nodes SET version = $1, lan_addr = $2, public_addr = $3, last_seen = $4 WHERE id = $5`,
		version, lanAddr, publicAddr, seen.UTC(), id)
}

// RenameRelayNode changes the name of a node.
func (pg *PostgresDB) RenameRelayNode(id, name string) error {
	return pg.execRelayNode("rename relay node", `UPDATE relay_nodes SET name = $1 WHERE id = $2`, name, id)
}

// RevokeRelayNode marks a node revoked at at; a node revoked before keeps
// its first time.
func (pg *PostgresDB) RevokeRelayNode(id string, at time.Time) error {
	return pg.execRelayNode("revoke relay node",
		`UPDATE relay_nodes SET revoked_at = COALESCE(revoked_at, $1) WHERE id = $2`, at.UTC(), id)
}

// CreateSiteEnrollmentToken stores a new token; a zero CreatedAt gets the
// current time.
func (pg *PostgresDB) CreateSiteEnrollmentToken(t *SiteEnrollmentToken) error {
	t.CreatedAt = orNow(t.CreatedAt)
	ctx, cancel := pg.opCtx()
	defer cancel()
	_, err := pg.pool.Exec(ctx, `INSERT INTO site_enrollment_tokens (token_hash, site_id, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		t.TokenHash, t.SiteID, t.CreatedBy, t.CreatedAt.UTC(), t.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("db: create site enrollment token: %w", err)
	}
	return nil
}

const pgSiteTokenColumns = `token_hash, site_id, created_by, created_at, expires_at, used_at, used_by_node`

func scanPgSiteToken(row pgx.Row) (*SiteEnrollmentToken, error) {
	t := &SiteEnrollmentToken{}
	if err := row.Scan(&t.TokenHash, &t.SiteID, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt, &t.UsedByNode); err != nil {
		return nil, err
	}
	t.CreatedAt = t.CreatedAt.UTC()
	t.ExpiresAt = t.ExpiresAt.UTC()
	t.UsedAt = utcPtr(t.UsedAt)
	return t, nil
}

// ConsumeSiteEnrollmentToken uses the token with that hash for node nodeID
// and returns it. One UPDATE that only matches an unused token that has not
// expired at now does it, so a token is used once however many try at the
// same time; ErrSiteTokenInvalid for the others and for a missing, used or
// expired token.
func (pg *PostgresDB) ConsumeSiteEnrollmentToken(hash, nodeID string, now time.Time) (*SiteEnrollmentToken, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	t, err := scanPgSiteToken(pg.pool.QueryRow(ctx, `UPDATE site_enrollment_tokens SET used_at = $1, used_by_node = $2
		WHERE token_hash = $3 AND used_at IS NULL AND expires_at > $1
		RETURNING `+pgSiteTokenColumns, now.UTC(), nodeID, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSiteTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("db: consume site enrollment token: %w", err)
	}
	return t, nil
}

// ListSiteEnrollmentTokens returns the tokens of a site, used and expired
// ones included, oldest first.
func (pg *PostgresDB) ListSiteEnrollmentTokens(siteID string) ([]SiteEnrollmentToken, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	rows, err := pg.pool.Query(ctx, `SELECT `+pgSiteTokenColumns+` FROM site_enrollment_tokens
		WHERE site_id = $1 ORDER BY created_at, token_hash`, siteID)
	if err != nil {
		return nil, fmt.Errorf("db: list site enrollment tokens: %w", err)
	}
	defer rows.Close()
	out := []SiteEnrollmentToken{}
	for rows.Next() {
		t, err := scanPgSiteToken(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list site enrollment tokens: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// SetSitePin pins a device or folder to a site, replacing its pin; siteID
// "" removes the pin.
func (pg *PostgresDB) SetSitePin(kind, ref, siteID string) error {
	ctx, cancel := pg.opCtx()
	defer cancel()
	var err error
	if siteID == "" {
		_, err = pg.pool.Exec(ctx, `DELETE FROM site_pins WHERE kind = $1 AND ref = $2`, kind, ref)
	} else {
		_, err = pg.pool.Exec(ctx, `INSERT INTO site_pins (kind, ref, site_id) VALUES ($1, $2, $3)
			ON CONFLICT (kind, ref) DO UPDATE SET site_id = EXCLUDED.site_id`, kind, ref, siteID)
	}
	if err != nil {
		return fmt.Errorf("db: set site pin: %w", err)
	}
	return nil
}

// GetSitePin returns the site a device or folder is pinned to, "" for none.
func (pg *PostgresDB) GetSitePin(kind, ref string) (string, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	var siteID string
	err := pg.pool.QueryRow(ctx, `SELECT site_id FROM site_pins WHERE kind = $1 AND ref = $2`, kind, ref).Scan(&siteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: get site pin: %w", err)
	}
	return siteID, nil
}

// ListSitePins returns every pin.
func (pg *PostgresDB) ListSitePins() ([]SitePin, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	rows, err := pg.pool.Query(ctx, `SELECT kind, ref, site_id FROM site_pins ORDER BY kind, ref`)
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
func (pg *PostgresDB) UpsertSiteReachability(r *SiteReachability) error {
	r.CheckedAt = orNow(r.CheckedAt)
	ctx, cancel := pg.opCtx()
	defer cancel()
	_, err := pg.pool.Exec(ctx, `INSERT INTO site_reachability (from_node, to_node, via, ok, rtt_ms, checked_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (from_node, to_node, via) DO UPDATE SET
			ok = EXCLUDED.ok, rtt_ms = EXCLUDED.rtt_ms, checked_at = EXCLUDED.checked_at`,
		r.FromNode, r.ToNode, r.Via, r.OK, r.RTTMs, r.CheckedAt.UTC())
	if err != nil {
		return fmt.Errorf("db: upsert site reachability: %w", err)
	}
	return nil
}

// ListSiteReachability returns every stored probe result.
func (pg *PostgresDB) ListSiteReachability() ([]SiteReachability, error) {
	ctx, cancel := pg.opCtx()
	defer cancel()
	rows, err := pg.pool.Query(ctx, `SELECT from_node, to_node, via, ok, rtt_ms, checked_at FROM site_reachability
		ORDER BY from_node, to_node, via`)
	if err != nil {
		return nil, fmt.Errorf("db: list site reachability: %w", err)
	}
	defer rows.Close()
	out := []SiteReachability{}
	for rows.Next() {
		var r SiteReachability
		if err := rows.Scan(&r.FromNode, &r.ToNode, &r.Via, &r.OK, &r.RTTMs, &r.CheckedAt); err != nil {
			return nil, fmt.Errorf("db: list site reachability: %w", err)
		}
		r.CheckedAt = r.CheckedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetConnectionSessionRelayVia records how the sessions of a relay UUID are
// relayed ("direct", "central", "lan", "node:<id>", "nodes:<a>,<b>").
func (pg *PostgresDB) SetConnectionSessionRelayVia(relayUUID, via string) error {
	relayUUID = strings.TrimSpace(relayUUID)
	if relayUUID == "" {
		return nil
	}
	ctx, cancel := pg.opCtx()
	defer cancel()
	if _, err := pg.pool.Exec(ctx, `UPDATE peer_connection_sessions SET relay_via = $1 WHERE relay_uuid = $2`, via, relayUUID); err != nil {
		return fmt.Errorf("db: set connection session relay via: %w", err)
	}
	return nil
}
