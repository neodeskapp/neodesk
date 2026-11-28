package db

import (
	"errors"
	"time"
)

// Site is a location of an organization (an office, a data centre) whose
// devices can be relayed by the site's own relay nodes. OrgID "" is a site
// of an installation without organizations; it serves devices without an
// organization only.
type Site struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
	// VPNGroup is "" or a label; sites with the same non-empty label are
	// connected by a VPN, so their nodes reach each other's LAN addresses.
	VPNGroup  string    `json:"vpn_group"`
	CreatedAt time.Time `json:"created_at"`
}

// Kinds of a SiteNetwork.
const (
	SiteNetworkPublic  = "public"
	SiteNetworkPrivate = "private"
)

// SiteNetwork is an address range of a site: a device that registers from
// an address in it is at the site.
type SiteNetwork struct {
	ID     string `json:"id"`
	SiteID string `json:"site_id"`
	CIDR   string `json:"cidr"`
	Kind   string `json:"kind"` // SiteNetworkPublic or SiteNetworkPrivate
}

// RelayNode is a relay server at a site, enrolled with a site enrollment
// token. PublicKey is its base64 Ed25519 key, unique; LANAddr and
// PublicAddr are "host:port" of its TCP relay as clients dial it, "" when
// the node has none of that kind.
type RelayNode struct {
	ID         string     `json:"id"`
	SiteID     string     `json:"site_id"`
	Name       string     `json:"name"`
	PublicKey  string     `json:"public_key"`
	LANAddr    string     `json:"lan_addr"`
	PublicAddr string     `json:"public_addr"`
	Version    string     `json:"version"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Revoked says the node was revoked and gets no more sessions.
func (n *RelayNode) Revoked() bool { return n != nil && n.RevokedAt != nil }

// SiteEnrollmentToken is a one-use token that enrolls a relay node into a
// site. Only the sha256 hex of the token is stored.
type SiteEnrollmentToken struct {
	TokenHash  string     `json:"-"`
	SiteID     string     `json:"site_id"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	UsedByNode string     `json:"used_by_node,omitempty"`
}

// Kinds of a SitePin.
const (
	SitePinDevice = "device"
	SitePinFolder = "folder"
)

// SitePin puts a device (Ref is the device id) or a folder (Ref is the
// folder id in decimal) at a site, whatever its address says.
type SitePin struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	SiteID string `json:"site_id"`
}

// SiteReachability is the last probe result of whether node FromNode
// reaches node ToNode over Via ("private" or "public").
type SiteReachability struct {
	FromNode  string    `json:"from"`
	ToNode    string    `json:"to"`
	Via       string    `json:"via"`
	OK        bool      `json:"ok"`
	RTTMs     int       `json:"rtt_ms"`
	CheckedAt time.Time `json:"checked_at"`
}

// ErrSiteNotFound is a write to a site or relay node that does not exist.
var ErrSiteNotFound = errors.New("site not found")

// ErrSiteTokenInvalid is a site enrollment token that does not exist, is
// used or has expired.
var ErrSiteTokenInvalid = errors.New("site enrollment token is invalid, used or expired")

// The tables of sites and relay nodes. They have no foreign keys: DeleteSite
// removes the rows of a site from every table itself, the same way on both
// databases.
var sqliteSitesSchema = []string{
	`CREATE TABLE IF NOT EXISTS sites (
		id TEXT PRIMARY KEY,
		org_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		vpn_group TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sites_org ON sites(org_id)`,
	`CREATE TABLE IF NOT EXISTS site_networks (
		id TEXT PRIMARY KEY,
		site_id TEXT NOT NULL,
		cidr TEXT NOT NULL,
		kind TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_site_networks_site ON site_networks(site_id)`,
	`CREATE TABLE IF NOT EXISTS relay_nodes (
		id TEXT PRIMARY KEY,
		site_id TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		public_key TEXT NOT NULL UNIQUE,
		lan_addr TEXT NOT NULL DEFAULT '',
		public_addr TEXT NOT NULL DEFAULT '',
		version TEXT NOT NULL DEFAULT '',
		last_seen TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		revoked_at TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_relay_nodes_site ON relay_nodes(site_id)`,
	`CREATE TABLE IF NOT EXISTS site_enrollment_tokens (
		token_hash TEXT PRIMARY KEY,
		site_id TEXT NOT NULL,
		created_by TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		expires_at TEXT NOT NULL,
		used_at TEXT,
		used_by_node TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_site_enrollment_tokens_site ON site_enrollment_tokens(site_id)`,
	`CREATE TABLE IF NOT EXISTS site_pins (
		kind TEXT NOT NULL,
		ref TEXT NOT NULL,
		site_id TEXT NOT NULL,
		PRIMARY KEY (kind, ref)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_site_pins_site ON site_pins(site_id)`,
	`CREATE TABLE IF NOT EXISTS site_reachability (
		from_node TEXT NOT NULL,
		to_node TEXT NOT NULL,
		via TEXT NOT NULL,
		ok INTEGER NOT NULL DEFAULT 0,
		rtt_ms INTEGER NOT NULL DEFAULT 0,
		checked_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (from_node, to_node, via)
	)`,
}

var postgresSitesSchema = []string{
	`CREATE TABLE IF NOT EXISTS sites (
		id         TEXT PRIMARY KEY,
		org_id     TEXT NOT NULL DEFAULT '',
		name       TEXT NOT NULL,
		vpn_group  TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sites_org ON sites(org_id)`,
	`CREATE TABLE IF NOT EXISTS site_networks (
		id      TEXT PRIMARY KEY,
		site_id TEXT NOT NULL,
		cidr    TEXT NOT NULL,
		kind    TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_site_networks_site ON site_networks(site_id)`,
	`CREATE TABLE IF NOT EXISTS relay_nodes (
		id          TEXT PRIMARY KEY,
		site_id     TEXT NOT NULL,
		name        TEXT NOT NULL DEFAULT '',
		public_key  TEXT NOT NULL UNIQUE,
		lan_addr    TEXT NOT NULL DEFAULT '',
		public_addr TEXT NOT NULL DEFAULT '',
		version     TEXT NOT NULL DEFAULT '',
		last_seen   TIMESTAMPTZ,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		revoked_at  TIMESTAMPTZ
	)`,
	`CREATE INDEX IF NOT EXISTS idx_relay_nodes_site ON relay_nodes(site_id)`,
	`CREATE TABLE IF NOT EXISTS site_enrollment_tokens (
		token_hash   TEXT PRIMARY KEY,
		site_id      TEXT NOT NULL,
		created_by   TEXT NOT NULL DEFAULT '',
		created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at   TIMESTAMPTZ NOT NULL,
		used_at      TIMESTAMPTZ,
		used_by_node TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_site_enrollment_tokens_site ON site_enrollment_tokens(site_id)`,
	`CREATE TABLE IF NOT EXISTS site_pins (
		kind    TEXT NOT NULL,
		ref     TEXT NOT NULL,
		site_id TEXT NOT NULL,
		PRIMARY KEY (kind, ref)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_site_pins_site ON site_pins(site_id)`,
	`CREATE TABLE IF NOT EXISTS site_reachability (
		from_node  TEXT NOT NULL,
		to_node    TEXT NOT NULL,
		via        TEXT NOT NULL,
		ok         BOOLEAN NOT NULL DEFAULT FALSE,
		rtt_ms     INTEGER NOT NULL DEFAULT 0,
		checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (from_node, to_node, via)
	)`,
}

// sqliteSiteTime is how the site tables keep a time in SQLite: UTC text
// that sorts and compares in time order.
func sqliteSiteTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// orNow is t, or the current time when t is zero.
func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}
