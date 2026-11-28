package signal

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/neodeskapp/neodesk/neodesk-server/config"
	cryptopkg "github.com/neodeskapp/neodesk/neodesk-server/crypto"
	"github.com/neodeskapp/neodesk/neodesk-server/peer"
	pb "github.com/neodeskapp/neodesk/neodesk-server/proto"
	"github.com/neodeskapp/neodesk/neodesk-server/relay"
)

const (
	nodeInitiatorAddr = "10.20.0.5:21117"
	nodeTargetAddr    = "10.30.0.6:21117"
)

// fakeSiteRouter routes every session between ini and tgt over a relay
// node with its own ticket store, and records the order it was asked in.
type fakeSiteRouter struct {
	ini, tgt string
	store    *recordingTickets
	mu       sync.Mutex
	asked    [][2]string
	revoked  []string
}

func (f *fakeSiteRouter) node() NodeRelay {
	return NodeRelay{Addr: nodeInitiatorAddr, TargetAddr: nodeTargetAddr, Tickets: f.store, Via: "node:n1"}
}

func (f *fakeSiteRouter) NodeRoute(initiatorID string, _ net.IP, targetID string, _ net.IP) (NodeRelay, bool) {
	f.mu.Lock()
	f.asked = append(f.asked, [2]string{initiatorID, targetID})
	f.mu.Unlock()
	if initiatorID == f.ini && targetID == f.tgt {
		return f.node(), true
	}
	return NodeRelay{}, false
}

func (f *fakeSiteRouter) NodeForAddr(addr string) (NodeRelay, bool) {
	if addr == nodeTargetAddr || addr == nodeInitiatorAddr {
		return f.node(), true
	}
	return NodeRelay{}, false
}

func (f *fakeSiteRouter) RevokeForPeer(p string) { f.revoked = append(f.revoked, p) }

// siteFixture is a signal server with a node route between SITEINI1 and
// SITETGT1, both reachable on local UDP sockets.
type siteFixture struct {
	srv        *Server
	router     *fakeSiteRouter
	central    *recordingTickets
	ini, tgt   *net.UDPConn
	iniA, tgtA *net.UDPAddr
}

func newSiteFixture(t *testing.T, nodeErr error) *siteFixture {
	t.Helper()
	srv, _ := newTestSignalServer(t, config.EnrollmentModeOpen)
	srv.cfg.P2PFirst = false
	central := &recordingTickets{}
	srv.SetTickets(central)
	ini, iniA := attachTestUDPPair(t, srv)
	tgt, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tgt.Close() })
	tgtA := tgt.LocalAddr().(*net.UDPAddr)
	putOnlinePeer(srv, "SITETGT1", "127.0.0.1", tgtA.Port, peer.ConnUDP)
	putOnlinePeer(srv, "SITEINI1", "127.0.0.1", iniA.Port, peer.ConnUDP)
	router := &fakeSiteRouter{ini: "SITEINI1", tgt: "SITETGT1", store: &recordingTickets{err: nodeErr}}
	srv.SetSiteRouter(router)
	return &siteFixture{srv: srv, router: router, central: central, ini: ini, tgt: tgt, iniA: iniA, tgtA: tgtA}
}

// expectNodeSession checks that the node, not the central store, got the
// session's one ticket.
func (f *siteFixture) expectNodeTicket(t *testing.T) {
	t.Helper()
	if got := f.router.store.issuedUUIDs(); len(got) != 1 {
		t.Fatalf("node store got tickets %v, want one", got)
	}
	if got := f.central.issuedUUIDs(); len(got) != 0 {
		t.Fatalf("central store got tickets %v, want none", got)
	}
}

// TestEveryRelayCallSiteUsesTheSiteNode: each place that names a relay to
// a peer names the relay node of the peers' site, the initiator's node to
// the initiator and the target's node to the target. A call site that
// skips the node hook names the central relay and fails here.
func TestEveryRelayCallSiteUsesTheSiteNode(t *testing.T) {
	t.Run("udp request relay", func(t *testing.T) {
		f := newSiteFixture(t, nil)
		f.srv.handleRequestRelay(&pb.RequestRelay{Id: "SITETGT1", Uuid: "site-udp-rr"}, f.iniA)
		if got := readUDPRendezvous(t, f.tgt).GetRequestRelay().GetRelayServer(); got != nodeTargetAddr {
			t.Fatalf("target told relay %q, want %q", got, nodeTargetAddr)
		}
		if got := readUDPRendezvous(t, f.ini).GetRelayResponse().GetRelayServer(); got != nodeInitiatorAddr {
			t.Fatalf("initiator told relay %q, want %q", got, nodeInitiatorAddr)
		}
		f.expectNodeTicket(t)
		if via := RelayVia("site-udp-rr"); via != "node:n1" {
			t.Fatalf("relay via %q, want node:n1", via)
		}
	})

	for _, conn := range []peer.ConnType{peer.ConnTCP, peer.ConnWS} {
		t.Run("tcp request relay "+fmt.Sprint(conn), func(t *testing.T) {
			f := newSiteFixture(t, nil)
			resp := f.srv.handleRequestRelayTCP(&pb.RequestRelay{Id: "SITETGT1", Uuid: "site-tcp-rr-" + fmt.Sprint(conn)}, f.iniA, conn)
			if got := resp.GetRelayResponse().GetRelayServer(); got != nodeInitiatorAddr {
				t.Fatalf("initiator told relay %q, want %q", got, nodeInitiatorAddr)
			}
			if got := readUDPRendezvous(t, f.tgt).GetRequestRelay().GetRelayServer(); got != nodeTargetAddr {
				t.Fatalf("target told relay %q, want %q", got, nodeTargetAddr)
			}
			f.expectNodeTicket(t)
		})
	}

	t.Run("udp punch hole, forced relay", func(t *testing.T) {
		f := newSiteFixture(t, nil)
		f.srv.handlePunchHoleRequest(&pb.PunchHoleRequest{Id: "SITETGT1", ForceRelay: true}, f.iniA)
		if got := readUDPRendezvous(t, f.ini).GetRelayResponse().GetRelayServer(); got != nodeInitiatorAddr {
			t.Fatalf("initiator told relay %q, want %q", got, nodeInitiatorAddr)
		}
		if got := readUDPRendezvous(t, f.tgt).GetRequestRelay().GetRelayServer(); got != nodeTargetAddr {
			t.Fatalf("target told relay %q, want %q", got, nodeTargetAddr)
		}
		f.expectNodeTicket(t)
	})

	t.Run("udp punch hole, p2p first on one network", func(t *testing.T) {
		// Both peers are on 127.0.0.1, so signal starts the LAN exchange:
		// the target gets FetchLocalAddr naming the node as the relay to
		// fall back to
		f := newSiteFixture(t, nil)
		f.srv.handlePunchHoleRequest(&pb.PunchHoleRequest{Id: "SITETGT1"}, f.iniA)
		msg := readUDPRendezvous(t, f.tgt)
		got := msg.GetFetchLocalAddr().GetRelayServer()
		if ph := msg.GetPunchHole(); ph != nil {
			got = ph.GetRelayServer()
		}
		if got != nodeInitiatorAddr && got != nodeTargetAddr {
			t.Fatalf("target's message %v names relay %q, want the node", msg, got)
		}
	})

	t.Run("tcp punch hole", func(t *testing.T) {
		f := newSiteFixture(t, nil)
		resp := f.srv.handlePunchHoleRequestTCP(&pb.PunchHoleRequest{Id: "SITETGT1", ForceRelay: true}, f.iniA)
		if got := resp.GetPunchHoleResponse().GetRelayServer(); got != nodeInitiatorAddr {
			t.Fatalf("initiator told relay %q, want %q (response %v)", got, nodeInitiatorAddr, resp)
		}
	})

	t.Run("punch hole sent", func(t *testing.T) {
		f := newSiteFixture(t, nil)
		f.srv.storePendingUUID("SITETGT1", f.iniA, "", "SITEINI1")
		f.srv.handlePunchHoleSent(&pb.PunchHoleSent{
			Id: "SITETGT1", SocketAddr: cryptopkg.EncodeAddr(f.iniA), RelayServer: "relay.example:21117",
		}, f.tgtA, true)
		if got := readUDPRendezvous(t, f.ini).GetPunchHoleResponse().GetRelayServer(); got != nodeInitiatorAddr {
			t.Fatalf("initiator told relay %q, want %q", got, nodeInitiatorAddr)
		}
		f.router.mu.Lock()
		defer f.router.mu.Unlock()
		if len(f.router.asked) == 0 || f.router.asked[0] != [2]string{"SITEINI1", "SITETGT1"} {
			t.Fatalf("router asked %v, want initiator then target", f.router.asked)
		}
	})

	t.Run("relay response forward to a node the target dials", func(t *testing.T) {
		f := newSiteFixture(t, nil)
		f.srv.storePendingUUID("SITETGT1", f.iniA, "site-fwd", "SITEINI1")
		f.srv.handleRelayResponseForward(&pb.RendezvousMessage{Union: &pb.RendezvousMessage_RelayResponse{
			RelayResponse: &pb.RelayResponse{
				SocketAddr: cryptopkg.EncodeAddr(f.iniA), Uuid: "site-fwd",
				RelayServer: nodeTargetAddr, Union: &pb.RelayResponse_Id{Id: "SITETGT1"},
			},
		}}, f.tgtA)
		if got := readUDPRendezvous(t, f.ini).GetRelayResponse().GetRelayServer(); got != nodeInitiatorAddr {
			t.Fatalf("initiator told relay %q, want %q", got, nodeInitiatorAddr)
		}
		f.expectNodeTicket(t)
	})
}

// TestADownNodeFallsBackToTheCentralRelay: when the node can't take the
// ticket, the session gets the central relay and its ticket there.
func TestADownNodeFallsBackToTheCentralRelay(t *testing.T) {
	f := newSiteFixture(t, fmt.Errorf("node gone: %w", relay.ErrTicketsUnavailable))
	resp := f.srv.handleRequestRelayTCP(&pb.RequestRelay{Id: "SITETGT1", Uuid: "site-fallback"}, f.iniA, peer.ConnTCP)
	rr := resp.GetRelayResponse()
	if rr.GetRefuseReason() != "" {
		t.Fatalf("session refused: %q", rr.GetRefuseReason())
	}
	if got := rr.GetRelayServer(); got == nodeInitiatorAddr || got == "" {
		t.Fatalf("initiator told relay %q, want the central relay", got)
	}
	if got := f.central.issuedUUIDs(); len(got) != 1 || got[0] != "site-fallback" {
		t.Fatalf("central store got %v, want the session's ticket", got)
	}
	if via := RelayVia("site-fallback"); via != viaCentral && via != viaLAN {
		t.Fatalf("relay via %q, want central or lan", via)
	}
}

// TestARejectedNodeTicketRefusesTheSession: a node that says no (a reused
// uuid) refuses the session; only an unreachable node falls back.
func TestARejectedNodeTicketRefusesTheSession(t *testing.T) {
	f := newSiteFixture(t, relay.ErrTicketRejected)
	resp := f.srv.handleRequestRelayTCP(&pb.RequestRelay{Id: "SITETGT1", Uuid: "site-rejected"}, f.iniA, peer.ConnTCP)
	if got := resp.GetRelayResponse().GetRefuseReason(); got != refuseRelayTicketRejected {
		t.Fatalf("refuse reason %q, want %q", got, refuseRelayTicketRejected)
	}
	if got := f.central.issuedUUIDs(); len(got) != 0 {
		t.Fatalf("central store got %v, want none", got)
	}
}

// TestPeersWithoutANodeKeepTheCentralRelay: a pair the router has no node
// for is routed as before.
func TestPeersWithoutANodeKeepTheCentralRelay(t *testing.T) {
	f := newSiteFixture(t, nil)
	f.router.ini = "SOMEONEELSE"
	resp := f.srv.handleRequestRelayTCP(&pb.RequestRelay{Id: "SITETGT1", Uuid: "site-none"}, f.iniA, peer.ConnTCP)
	if got := resp.GetRelayResponse().GetRelayServer(); got == nodeInitiatorAddr {
		t.Fatal("a pair without a node got the node")
	}
	if got := f.central.issuedUUIDs(); len(got) != 1 {
		t.Fatalf("central store got %v, want one ticket", got)
	}
}

// TestABanRevokesOnRelayNodesToo: banning a peer drops its tickets on the
// relay nodes, not only on the central store.
func TestABanRevokesOnRelayNodesToo(t *testing.T) {
	f := newSiteFixture(t, nil)
	f.srv.revokeBannedPeerAccess("SITEBAN1", nil)
	if len(f.router.revoked) != 1 || f.router.revoked[0] != "SITEBAN1" {
		t.Fatalf("nodes revoked %v, want [SITEBAN1]", f.router.revoked)
	}
}
