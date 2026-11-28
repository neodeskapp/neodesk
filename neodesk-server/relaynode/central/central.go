// Package central joins the relay node hub, the sites service, signal and
// billing in the central server: it is the hub's store, the signal
// server's site router and the receiver of what nodes report.
package central

import (
	"errors"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/db"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/wire"
	"github.com/neodeskapp/neodesk/neodesk-server/signal"
	"github.com/neodeskapp/neodesk/neodesk-server/sites"
)

// Billing is the part of the billing service node sessions report to.
type Billing interface {
	ActivateRelay(uuid string)
	EndRelay(uuid string)
}

// Glue is the central side of relay nodes.
type Glue struct {
	DB      db.Database
	Sites   *sites.Service
	Hub     *hub.Hub // set after hub.New; nil until then
	Billing Billing  // optional
	// Pin is the SPKI sha256 of the control certificate and ControlPort
	// the control listener's port, for the enrollment string.
	Pin         string
	ControlPort int
}

var defaultGlue atomic.Pointer[Glue]

// SetDefault makes g the glue the console reaches through Default.
func SetDefault(g *Glue) { defaultGlue.Store(g) }

// Default is the running central glue, nil when relay nodes are off.
func Default() *Glue { return defaultGlue.Load() }

// ---- hub.Store ----

// ConsumeEnrollmentToken uses the token for nodeID and returns its site.
func (g *Glue) ConsumeEnrollmentToken(token, nodeID string, now time.Time) (string, error) {
	t, err := g.DB.ConsumeSiteEnrollmentToken(sites.HashToken(token), nodeID, now)
	if err != nil {
		return "", err
	}
	return t.SiteID, nil
}

// GetRelayNodeByKey returns the node enrolled with the public key.
func (g *Glue) GetRelayNodeByKey(publicKey string) (*hub.Node, error) {
	n, err := g.DB.GetRelayNodeByKey(publicKey)
	if err != nil {
		if errors.Is(err, db.ErrSiteNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if n == nil {
		return nil, nil
	}
	return toHubNode(n), nil
}

// CreateRelayNode stores a node the hub just enrolled.
func (g *Glue) CreateRelayNode(n *hub.Node) error {
	err := g.DB.CreateRelayNode(&db.RelayNode{
		ID: n.ID, SiteID: n.SiteID, Name: n.Name, PublicKey: n.PublicKey,
		LANAddr: n.LanAddr, PublicAddr: n.PublicAddr, Version: n.Version,
		CreatedAt: n.CreatedAt, LastSeen: &n.CreatedAt,
	})
	if err == nil {
		g.Sites.Invalidate()
	}
	return err
}

// UpdateRelayNodeState records what a node said in its hello.
func (g *Glue) UpdateRelayNodeState(id, version, lanAddr, publicAddr string, _ time.Time) error {
	return g.Sites.UpdateNodeState(id, version, lanAddr, publicAddr)
}

// GetSite returns the node's site, nil when the site or its organization
// is gone, so the hub refuses the node.
func (g *Glue) GetSite(id string) (*hub.Site, error) {
	s, err := g.DB.GetSite(id)
	if err != nil {
		if errors.Is(err, db.ErrSiteNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	if s.OrgID != "" {
		if org, err := g.DB.GetOrganization(s.OrgID); err != nil || org == nil {
			return nil, nil
		}
	}
	return &hub.Site{ID: s.ID, OrgID: s.OrgID, Name: s.Name}, nil
}

func toHubNode(n *db.RelayNode) *hub.Node {
	return &hub.Node{
		ID: n.ID, SiteID: n.SiteID, Name: n.Name, PublicKey: n.PublicKey,
		LanAddr: n.LANAddr, PublicAddr: n.PublicAddr, Version: n.Version,
		CreatedAt: n.CreatedAt, Revoked: n.Revoked(),
	}
}

// ---- hub.Events ----

// NodeSessionStarted meters a node session like a central one.
func (g *Glue) NodeSessionStarted(_ string, uuid string) {
	if g.Billing != nil {
		g.Billing.ActivateRelay(uuid)
	}
}

// NodeSessionEnded ends billing and the session record.
func (g *Glue) NodeSessionEnded(nodeID, uuid string, up, down int64) {
	if g.Billing != nil {
		g.Billing.EndRelay(uuid)
	}
	if err := g.DB.EndPeerConnectionSessionByRelay(uuid); err != nil {
		log.Printf("[relay-node] end connection session for relay %s on node %s: %v", uuid, nodeID, err)
	}
	signal.ForgetRelayVia(uuid)
}

// NodeStats needs nothing here: the hub keeps the counts for the console.
func (g *Glue) NodeStats(string, wire.Stats) {}

// ---- signal.SiteRouter ----

// NodeRoute is the relay node route of a session, if the peers' site has
// a connected node.
func (g *Glue) NodeRoute(initiatorID string, initiatorIP net.IP, targetID string, targetIP net.IP) (signal.NodeRelay, bool) {
	if g.Hub == nil {
		return signal.NodeRelay{}, false
	}
	r := g.Sites.Route(initiatorID, initiatorIP, targetID, targetIP)
	switch r.Kind {
	case sites.Same:
		if r.A == nil || !g.Hub.Online(r.A.ID) {
			return signal.NodeRelay{}, false
		}
		return signal.NodeRelay{
			Addr: r.InitiatorAddr, TargetAddr: r.TargetAddr,
			Tickets: g.Hub.Tickets(r.A.ID), Via: "node:" + r.A.ID,
		}, true
	case sites.Pair:
		return g.pairRoute(r)
	}
	return signal.NodeRelay{}, false
}

// NodeForAddr maps a relay address a peer already dials to its node.
func (g *Glue) NodeForAddr(addr string) (signal.NodeRelay, bool) {
	addr = strings.TrimSpace(addr)
	if g.Hub == nil || addr == "" {
		return signal.NodeRelay{}, false
	}
	nodes, err := g.DB.ListRelayNodes("")
	if err != nil {
		return signal.NodeRelay{}, false
	}
	for i := range nodes {
		n := &nodes[i]
		if n.RevokedAt != nil || !g.Hub.Online(n.ID) {
			continue
		}
		if strings.EqualFold(n.LANAddr, addr) || strings.EqualFold(n.PublicAddr, addr) {
			return signal.NodeRelay{Addr: addr, TargetAddr: addr, Tickets: g.Hub.Tickets(n.ID), Via: "node:" + n.ID}, true
		}
	}
	return signal.NodeRelay{}, false
}

// RevokeForPeer drops a banned peer's unclaimed tickets on every node.
func (g *Glue) RevokeForPeer(peer string) {
	if g.Hub != nil {
		g.Hub.RevokeForPeer(peer)
	}
}

// ---- hub.Auditor ----

// Audit logs node security events.
func (g *Glue) Audit(event string, fields map[string]string) {
	var b strings.Builder
	for k, v := range fields {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
	}
	log.Printf("[audit] %s%s", event, b.String())
}

var (
	_ hub.Store         = (*Glue)(nil)
	_ hub.Events        = (*Glue)(nil)
	_ hub.Auditor       = (*Glue)(nil)
	_ signal.SiteRouter = (*Glue)(nil)
)
