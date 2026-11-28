package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/db"
	"github.com/neodeskapp/neodesk/neodesk-server/relay"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"
)

// TestPeersAtOneSiteRelayThroughTheSiteNode: two peers at site A are both
// told node A's address, their bytes go through node A, the central relay
// gets neither a ticket nor a connection, and the session is billed and
// recorded as relayed by node A.
func TestPeersAtOneSiteRelayThroughTheSiteNode(t *testing.T) {
	s := newServer(t)
	siteA := s.site("Site A", "10.1.0.0/16")
	siteB := s.site("Site B", "10.2.0.0/16")
	a := s.startNode(siteA, nil)
	b := s.startNode(siteB, nil)
	ini, tgt := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini, tgt)

	ses := s.connect(ini, tgt)
	if ses.initiatorRelay != a.relayAddr || ses.targetRelay != a.relayAddr {
		t.Fatalf("initiator told %q, target told %q; want node A %q (node B %q, central %q)",
			ses.initiatorRelay, ses.targetRelay, a.relayAddr, b.relayAddr, s.relayAddr)
	}
	waitFor(t, "hub counts node A's session", 3*time.Second, func() bool { return s.glue.Hub.ActiveSessions(a.id) == 1 })
	if n := s.glue.Hub.ActiveSessions(b.id); n != 0 {
		t.Fatalf("node B has %d sessions, want 0", n)
	}
	waitFor(t, "billing activated", 3*time.Second, func() bool { return s.billing.activated.has(ses.uuid) })
	if s.relayPaired.has(ses.uuid) {
		t.Fatal("the central relay paired the session too")
	}
	if relay.DefaultTickets().Admitted(ses.uuid, ini.id, tgt.id) {
		t.Fatal("signal issued the session's ticket on the central relay")
	}

	ses.close()
	waitFor(t, "billing ended", 3*time.Second, func() bool { return s.billing.ended.has(ses.uuid) })
	waitFor(t, "hub sees the session end", 3*time.Second, func() bool { return s.glue.Hub.ActiveSessions(a.id) == 0 })
	rec := s.sessionRecord(tgt.id, ses.uuid, func(r *db.PeerConnectionSession) bool { return r.EndedAt != nil })
	if rec.RelayVia != "node:"+a.id || rec.PeerID != ini.id {
		t.Fatalf("session record via %q from %q, want via node:%s from %s", rec.RelayVia, rec.PeerID, a.id, ini.id)
	}
}

// TestAStoppedNodeLeavesSessionsOnTheCentralRelay: once node A's agent is
// gone, the next session at site A goes through the central relay, and
// connects in well under 5 s.
func TestAStoppedNodeLeavesSessionsOnTheCentralRelay(t *testing.T) {
	s := newServer(t)
	siteA := s.site("Site A", "10.1.0.0/16")
	a := s.startNode(siteA, nil)
	ini, tgt := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini, tgt)

	a.stop()
	waitFor(t, "hub sees node A offline", 5*time.Second, func() bool { return !s.glue.Hub.Online(a.id) })

	ses := s.connect(ini, tgt)
	if !s.isCentral(ses.initiatorRelay) || !s.isCentral(ses.targetRelay) {
		t.Fatalf("initiator told %q, target told %q; want the central relay %q", ses.initiatorRelay, ses.targetRelay, s.relayAddr)
	}
	if ses.connected >= 5*time.Second {
		t.Fatalf("connecting took %s, want < 5 s", ses.connected)
	}
	waitFor(t, "central relay pairs the session", 3*time.Second, func() bool { return s.relayPaired.has(ses.uuid) })
	ses.close()
	rec := s.sessionRecord(tgt.id, ses.uuid, func(r *db.PeerConnectionSession) bool { return r.EndedAt != nil })
	if rec.RelayVia != "central" && rec.RelayVia != "lan" {
		t.Fatalf("session record via %q, want central or lan", rec.RelayVia)
	}
}

// TestANodeThatNeverAcksFallsBackWithinTheBudget: node A is connected but
// its acks never come (the proxy drops the authorize frames). The session
// waits out the hub's authorize budget once, then goes to the central
// relay.
func TestANodeThatNeverAcksFallsBackWithinTheBudget(t *testing.T) {
	s := newServer(t)
	siteA := s.site("Site A", "10.1.0.0/16")
	proxy := &controlProxy{}
	a := s.startNode(siteA, &nodeOpts{control: proxy})
	ini, tgt := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini, tgt)
	proxy.dropAuthorize.Store(true)

	ses := s.connect(ini, tgt)
	if proxy.droppedCount() != 1 {
		t.Fatalf("the hub pushed %d tickets to node A, want 1", proxy.droppedCount())
	}
	if !s.isCentral(ses.initiatorRelay) || !s.isCentral(ses.targetRelay) {
		t.Fatalf("initiator told %q, target told %q; want the central relay %q (node A %q)",
			ses.initiatorRelay, ses.targetRelay, s.relayAddr, a.relayAddr)
	}
	// The budget is spent waiting for the ack; the fallback adds little
	if ses.connected < hub.DefaultAuthorizeBudget-100*time.Millisecond || ses.connected > hub.DefaultAuthorizeBudget+1500*time.Millisecond {
		t.Fatalf("connecting took %s, want about the %s budget", ses.connected, hub.DefaultAuthorizeBudget)
	}
	waitFor(t, "central relay pairs the session", 3*time.Second, func() bool { return s.relayPaired.has(ses.uuid) })
	ses.close()
}

// TestPeersAtTwoSitesRelayOverBothNodes is phase 2: the initiator dials
// its site's node, the target its own, and the nodes join the legs.
func TestPeersAtTwoSitesRelayOverBothNodes(t *testing.T) {
	t.Skip("phase 2: sessions between two sites over two nodes are not built yet")
	s := newServer(t)
	siteA := s.site("Site A", "10.1.0.0/16")
	siteB := s.site("Site B", "10.2.0.0/16")
	a := s.startNode(siteA, nil)
	b := s.startNode(siteB, nil)
	ini, tgt := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini)
	s.pin(siteB, tgt)

	ses := s.connect(ini, tgt)
	if ses.initiatorRelay != a.relayAddr || ses.targetRelay != b.relayAddr {
		t.Fatalf("initiator told %q, target told %q; want node A %q and node B %q",
			ses.initiatorRelay, ses.targetRelay, a.relayAddr, b.relayAddr)
	}
	if s.relayPaired.has(ses.uuid) {
		t.Fatal("the central relay paired the session")
	}
	ses.close()
	rec := s.sessionRecord(tgt.id, ses.uuid, func(r *db.PeerConnectionSession) bool { return r.EndedAt != nil })
	if want := "nodes:" + a.id + "," + b.id; rec.RelayVia != want {
		t.Fatalf("session record via %q, want %q", rec.RelayVia, want)
	}
}

// TestARevokedNodeIsCutOffAndRefusedOnReconnect: revoking node A closes
// its control connection, the next session goes to the central relay, and
// the agent's reconnects are refused.
func TestARevokedNodeIsCutOffAndRefusedOnReconnect(t *testing.T) {
	s := newServer(t)
	siteA := s.site("Site A", "10.1.0.0/16")
	a := s.startNode(siteA, nil)
	ini, tgt := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini, tgt)

	// Node A carries sessions before the revocation
	before := s.connect(ini, tgt)
	if before.initiatorRelay != a.relayAddr {
		t.Fatalf("before the revocation the initiator was told %q, want node A %q", before.initiatorRelay, a.relayAddr)
	}
	before.close()

	if err := s.glue.Sites.RevokeNode(a.id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hub drops node A", 3*time.Second, func() bool { return !s.glue.Hub.Online(a.id) })

	ini2, tgt2 := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini2, tgt2)
	after := s.connect(ini2, tgt2)
	if !s.isCentral(after.initiatorRelay) || !s.isCentral(after.targetRelay) {
		t.Fatalf("after the revocation: initiator told %q, target told %q; want the central relay %q",
			after.initiatorRelay, after.targetRelay, s.relayAddr)
	}
	after.close()

	// The agent keeps retrying (50-200 ms backoff); each try is refused
	refused := "Refused node connection from 127.0.0.1: revoked"
	waitFor(t, "hub refuses the agent's reconnects", 5*time.Second, func() bool { return s.hubLogs.count(refused) >= 2 })
	if !strings.Contains(a.logs.String(), "central refused the node: revoked") {
		t.Fatalf("the agent did not hear it was revoked:\n%s", a.logs.String())
	}
	if s.glue.Hub.Online(a.id) {
		t.Fatal("node A is online again after the revocation")
	}
}
