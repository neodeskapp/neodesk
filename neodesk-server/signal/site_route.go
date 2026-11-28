package signal

import (
	"errors"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/neodeskapp/neodesk/neodesk-server/relay"
)

// NodeRelay is a relay node route for one session: what each peer dials,
// the node's ticket store and the Via value ("node:<id>", or
// "nodes:<a>,<b>" for two nodes).
type NodeRelay struct {
	Addr       string
	TargetAddr string
	Tickets    relay.Tickets
	Via        string
}

// SiteRouter finds relay node routes. NodeRoute is asked for every
// session; NodeForAddr maps a relay address the target already dials (the
// RelayResponse it sends after a punch hole) back to its node, and only
// to a node that may relay this pair.
type SiteRouter interface {
	NodeRoute(initiatorID string, initiatorIP net.IP, targetID string, targetIP net.IP) (NodeRelay, bool)
	NodeForAddr(addr, initiatorID string, initiatorIP net.IP, targetID string, targetIP net.IP) (NodeRelay, bool)
}

// SetSiteRouter turns relay node routing on; nil turns it off.
func (s *Server) SetSiteRouter(r SiteRouter) {
	s.siteMu.Lock()
	s.siteRouter = r
	s.siteMu.Unlock()
}

func (s *Server) sites() SiteRouter {
	s.siteMu.RLock()
	defer s.siteMu.RUnlock()
	return s.siteRouter
}

// nodeRoute is the relay node route for req, if any. The org network
// policy wins: a node whose address allowed_relay_servers excludes is
// not used.
func (s *Server) nodeRoute(req relayRouteRequest) (NodeRelay, bool) {
	router := s.sites()
	if router == nil || req.noNodes {
		return NodeRelay{}, false
	}
	if req.initiatorID == "" || req.targetID == "" {
		return NodeRelay{}, false
	}
	initiatorAddr, targetAddr := req.a, req.b
	if req.targetIsA {
		initiatorAddr, targetAddr = req.b, req.a
	}
	if initiatorAddr == nil || targetAddr == nil {
		return NodeRelay{}, false
	}
	var route NodeRelay
	var ok bool
	if req.relayHint != "" {
		route, ok = router.NodeForAddr(req.relayHint, req.initiatorID, initiatorAddr.IP, req.targetID, targetAddr.IP)
	} else {
		route, ok = router.NodeRoute(req.initiatorID, initiatorAddr.IP, req.targetID, targetAddr.IP)
	}
	if !ok {
		return NodeRelay{}, false
	}
	peers := []string{req.initiatorID, req.targetID}
	for _, addr := range []string{route.Addr, route.TargetAddr} {
		if s.applyNetworkRelayPolicy(addr, peers...) != addr {
			log.Printf("[signal] relay node %s excluded by allowed_relay_servers for %s -> %s", addr, req.initiatorID, req.targetID)
			return NodeRelay{}, false
		}
	}
	return route, true
}

// admitRoute is admitSession on the route's ticket store. When a relay
// node can't take the ticket in time, the session moves to the central
// relay: route is replaced by the route without nodes and the ticket is
// issued there, so a session never fails only because a node is down.
func (s *Server) admitRoute(route *RelayRoute, req relayRouteRequest, relayUUID, initiatorID, targetID string, account int64, consoleSession string) string {
	refuse := s.admitSessionErr(route.Tickets, relayUUID, initiatorID, targetID, account, consoleSession)
	if refuse.err != nil && errors.Is(refuse.err, relay.ErrTicketsUnavailable) && isNodeVia(route.Via) {
		log.Printf("[signal] relay node unavailable for uuid=%s (%s): %v; falling back to the central relay",
			relayUUIDShort(relayUUID), route.Via, refuse.err)
		req.noNodes = true
		*route = s.chooseRelay(req)
		refuse = s.admitSessionErr(route.Tickets, relayUUID, initiatorID, targetID, account, consoleSession)
	}
	if refuse.reason == "" && relayUUID != "" {
		s.noteRelayVia(relayUUID, route.Via)
	}
	return refuse.reason
}

func isNodeVia(via string) bool {
	return len(via) > 5 && (via[:5] == "node:" || (len(via) > 6 && via[:6] == "nodes:"))
}

// relayVia remembers how each admitted relay session travels, for the
// session record and live sessions.
var relayVia sync.Map // uuid -> via

func (s *Server) noteRelayVia(uuid, via string) {
	relayVia.Store(uuid, via)
	if s.db != nil {
		if err := s.db.SetConnectionSessionRelayVia(uuid, via); err != nil {
			log.Printf("[signal] relay via not recorded for uuid=%s: %v", relayUUIDShort(uuid), err)
		}
	}
}

// RelayVia is how the relay session uuid travels ("central", "lan",
// "node:<id>", ...), "" when signal did not admit it.
func RelayVia(uuid string) string {
	if v, ok := relayVia.Load(uuid); ok {
		return v.(string)
	}
	return ""
}

// ForgetRelayVia drops the entry of an ended session.
func ForgetRelayVia(uuid string) { relayVia.Delete(uuid) }

// IsOwnRelay reports whether addr is one of the relay addresses this
// server hands out.
func (s *Server) IsOwnRelay(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	if strings.EqualFold(addr, s.getRelayServer()) {
		return true
	}
	for _, r := range s.cfg.GetRelayServers() {
		if strings.EqualFold(addr, strings.TrimSpace(r)) {
			return true
		}
	}
	return false
}
