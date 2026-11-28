// Package clustertest runs several NeoDesk server instances in one test
// binary against one PostgreSQL and one Valkey, the way pods of a cluster
// share an installation.
package clustertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
	"github.com/neodeskapp/neodesk/neodesk-server/cluster/peerdir"
	"github.com/neodeskapp/neodesk/neodesk-server/codec"
	"github.com/neodeskapp/neodesk/neodesk-server/config"
	"github.com/neodeskapp/neodesk/neodesk-server/crypto"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
	"github.com/neodeskapp/neodesk/neodesk-server/events"
	pb "github.com/neodeskapp/neodesk/neodesk-server/proto"
	"github.com/neodeskapp/neodesk/neodesk-server/relay"
	"github.com/neodeskapp/neodesk/neodesk-server/signal"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		if os.Getenv("NEODESK_TEST_REQUIRE_VALKEY") == "1" {
			t.Fatalf("%s not set", name)
		}
		t.Skipf("%s not set", name)
	}
	return v
}

// pod is one server instance: signal and relay with their own ports,
// sharing the database, Valkey and the cluster secret with the others.
type pod struct {
	name   string
	cfg    *config.Config
	sig    *signal.Server
	relay  *relay.Server
	mesh   *cluster.Mesh
	signal string // 127.0.0.1:<signal port>
	relayA string // 127.0.0.1:<relay port>
}

// freeBlock finds a base port whose neighbours the signal and relay
// listeners need (signal-1 .. signal+3) are free.
func freeBlock(t *testing.T) int {
	t.Helper()
	for range 50 {
		var b [2]byte
		_, _ = rand.Read(b[:])
		base := 30000 + (int(b[0])<<8|int(b[1]))%20000
		ok := true
		for p := base - 1; p <= base+3 && ok; p++ {
			ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
			if err != nil {
				ok = false
				continue
			}
			ln.Close()
			u, err := net.ListenUDP("udp", &net.UDPAddr{Port: p})
			if err != nil {
				ok = false
				continue
			}
			u.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no free port block")
	return 0
}

type fixture struct {
	database db.Database
	client   *redis.Client
	prefix   string
	secret   string
	kp       *crypto.KeyPair
	cluster  bool
	// noMeet leaves the relays without the meeting spot: legs pair only
	// on the pod they reached
	noMeet bool
}

func newFixture(t *testing.T, clustered bool) *fixture {
	t.Helper()
	dsn := requireEnv(t, "NEODESK_TEST_POSTGRES_DSN")
	url := requireEnv(t, "NEODESK_TEST_VALKEY_URL")
	database, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { client.Close() })
	kp, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &fixture{database: database, client: client, prefix: "ct" + hex.EncodeToString(b) + ":",
		secret: "clustertest-secret-0123456789abcdef", kp: kp, cluster: clustered}
}

func (f *fixture) startPod(t *testing.T, name string) *pod {
	t.Helper()
	base := freeBlock(t)
	cfg := config.DefaultConfig()
	cfg.SignalPort = base
	cfg.RelayPort = base + 1
	cfg.EnrollmentMode = config.EnrollmentModeOpen
	cfg.P2PFirst = false
	cfg.RelayServers = fmt.Sprintf("127.0.0.1:%d", base+1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &pod{name: name, cfg: cfg, signal: fmt.Sprintf("127.0.0.1:%d", base), relayA: fmt.Sprintf("127.0.0.1:%d", base+1)}
	p.sig = signal.New(cfg, f.kp, f.database)
	p.relay = relay.New(cfg)

	if f.cluster {
		v := cluster.NewForTest(f.client, f.prefix)
		meshPort := freePortTCP(t)
		mesh, err := cluster.NewMesh(cluster.Config{Name: name, PodIP: "127.0.0.1", Port: meshPort, Secret: f.secret}, v)
		if err != nil {
			t.Fatal(err)
		}
		if err := mesh.Start(meshPort); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mesh.Stop)
		p.mesh = mesh
		kv := cluster.NewValkeyKV(v)
		tickets := relay.NewValkeyTickets(f.client, f.prefix)
		t.Cleanup(func() { tickets.Close() })
		p.sig.SetTickets(tickets)
		p.relay.SetTicketClaims(tickets)
		dir := peerdir.New(f.client, f.prefix, name)
		p.sig.PeerMap().SetDirectory(dir, name)
		t.Cleanup(p.sig.PeerMap().Close)
		p.sig.SetCluster(mesh, kv)
		p.sig.EventBus().Join(ctx, name, cluster.NewEventLinkOn(f.client, f.prefix))
		if !f.noMeet {
			p.relay.SetCluster(mesh, kv)
		}
	} else {
		// Two lone servers on one database: each has its own tickets
		own := relay.NewAuthorizationRegistry()
		p.sig.SetTickets(own)
		p.relay.SetTicketClaims(own)
	}
	if err := p.sig.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.sig.Stop)
	if err := p.relay.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.relay.Stop)
	return p
}

func freePortTCP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// fakePeer is a RustDesk client on one UDP socket.
type fakePeer struct {
	id   string
	conn *net.UDPConn
	to   *net.UDPAddr
}

func newPeer(t *testing.T, id, signalAddr string) *fakePeer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	to, _ := net.ResolveUDPAddr("udp", signalAddr)
	return &fakePeer{id: id, conn: conn, to: to}
}

func (p *fakePeer) send(t *testing.T, msg *pb.RendezvousMessage) {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.conn.WriteToUDP(b, p.to); err != nil {
		t.Fatal(err)
	}
}

// await reads until a message match accepts or the time is up.
func (p *fakePeer) await(t *testing.T, what string, match func(*pb.RendezvousMessage) bool) *pb.RendezvousMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 65535)
	for time.Now().Before(deadline) {
		_ = p.conn.SetReadDeadline(deadline)
		n, _, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		msg := &pb.RendezvousMessage{}
		if proto.Unmarshal(buf[:n], msg) == nil && match(msg) {
			return msg
		}
	}
	t.Fatalf("%s never received %s", p.id, what)
	return nil
}

func (p *fakePeer) register(t *testing.T) {
	t.Helper()
	p.send(t, &pb.RendezvousMessage{Union: &pb.RendezvousMessage_RegisterPeer{RegisterPeer: &pb.RegisterPeer{Id: p.id, Serial: 1}}})
	p.await(t, "RegisterPeerResponse", func(m *pb.RendezvousMessage) bool { return m.GetRegisterPeerResponse() != nil })
	p.send(t, &pb.RendezvousMessage{Union: &pb.RendezvousMessage_RegisterPk{RegisterPk: &pb.RegisterPk{
		Id: p.id, Uuid: []byte("uuid-" + p.id), Pk: bytes.Repeat([]byte{0x42}, 32),
	}}})
	resp := p.await(t, "RegisterPkResponse", func(m *pb.RendezvousMessage) bool { return m.GetRegisterPkResponse() != nil })
	if r := resp.GetRegisterPkResponse().GetResult(); r != pb.RegisterPkResponse_OK {
		t.Fatalf("%s RegisterPk = %v", p.id, r)
	}
}

// relayLeg dials a relay and sends the RequestRelay of uuid.
func relayLeg(t *testing.T, addr, uuid string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	req := &pb.RendezvousMessage{Union: &pb.RendezvousMessage_RequestRelay{RequestRelay: &pb.RequestRelay{Uuid: uuid}}}
	if err := codec.WriteRawProto(conn, req); err != nil {
		t.Fatal(err)
	}
	return conn
}

func uniqueID(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + strconv.Itoa(int(b[0])<<16|int(b[1])<<8|int(b[2]))
}

// relayedSession runs the relay path across two pods: the target
// registered on pod A, the initiator asks pod B, each side dials the relay
// of the pod it knows, and bytes must cross both ways. It returns an error
// instead of failing, so the test can check that it fails without the
// cluster.
func relayedSession(t *testing.T, a, b *pod) error {
	target := newPeer(t, uniqueID("CTTGT"), a.signal)
	target.register(t)
	// Pod A publishes a registration to the directory within 50 ms; a
	// real target registered long before anyone connects to it
	waitVisible(b, target.id)
	initiator := newPeer(t, uniqueID("CTINI"), b.signal)
	initiator.register(t)

	uuid := uniqueID("ct-uuid-")
	initiator.send(t, &pb.RendezvousMessage{Union: &pb.RendezvousMessage_RequestRelay{RequestRelay: &pb.RequestRelay{
		Id: target.id, Uuid: uuid,
	}}})
	resp := initiator.await(t, "RelayResponse", func(m *pb.RendezvousMessage) bool { return m.GetRelayResponse() != nil })
	if reason := resp.GetRelayResponse().GetRefuseReason(); reason != "" {
		return fmt.Errorf("pod B refused the relay: %s", reason)
	}

	// The target hears of the session from pod A, the pod it registered on
	req := target.awaitOrNil(func(m *pb.RendezvousMessage) bool { return m.GetRequestRelay() != nil })
	if req == nil {
		return fmt.Errorf("target never got the RequestRelay")
	}
	if req.GetRequestRelay().GetUuid() != uuid {
		return fmt.Errorf("target got uuid %q, want %q", req.GetRequestRelay().GetUuid(), uuid)
	}

	targetLeg := relayLeg(t, a.relayA, uuid)
	time.Sleep(100 * time.Millisecond) // the first leg claims the meeting spot
	initiatorLeg := relayLeg(t, b.relayA, uuid)

	if _, err := initiatorLeg.Write([]byte("hello target")); err != nil {
		return err
	}
	_ = targetLeg.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len("hello target"))
	if _, err := io.ReadFull(targetLeg, got); err != nil {
		return fmt.Errorf("target leg read: %w", err)
	}
	if _, err := targetLeg.Write([]byte("hello back")); err != nil {
		return err
	}
	_ = initiatorLeg.SetReadDeadline(time.Now().Add(5 * time.Second))
	back := make([]byte, len("hello back"))
	if _, err := io.ReadFull(initiatorLeg, back); err != nil {
		return fmt.Errorf("initiator leg read: %w", err)
	}
	if string(got) != "hello target" || string(back) != "hello back" {
		return fmt.Errorf("bytes %q / %q", got, back)
	}
	return nil
}

// awaitOrNil is await that returns nil instead of failing.
func (p *fakePeer) awaitOrNil(match func(*pb.RendezvousMessage) bool) *pb.RendezvousMessage {
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 65535)
	for time.Now().Before(deadline) {
		_ = p.conn.SetReadDeadline(deadline)
		n, _, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			return nil
		}
		msg := &pb.RendezvousMessage{}
		if proto.Unmarshal(buf[:n], msg) == nil && match(msg) {
			return msg
		}
	}
	return nil
}

// TestARelaySessionConnectsAcrossTwoPods: a peer registered on pod A is
// reached through pod B, and the two relay legs meet although each lands
// on another pod.
func TestARelaySessionConnectsAcrossTwoPods(t *testing.T) {
	f := newFixture(t, true)
	a := f.startPod(t, "pod-a")
	b := f.startPod(t, "pod-b")
	if err := relayedSession(t, a, b); err != nil {
		t.Fatal(err)
	}
}

// TestWithoutTheClusterTheSameSessionFails: the same two servers on one
// database without Valkey sharing can't connect the session, so the test
// above proves the forwarding and not the setup.
func TestWithoutTheClusterTheSameSessionFails(t *testing.T) {
	f := newFixture(t, false)
	a := f.startPod(t, "pod-a")
	b := f.startPod(t, "pod-b")
	if err := relayedSession(t, a, b); err == nil {
		t.Fatal("the session connected across two servers that share nothing")
	} else {
		t.Logf("fails as it should: %v", err)
	}
}

// TestWithoutTheMeetingSpotTheLegsNeverPair: signal reaches the target
// across pods, but legs that land on different pods don't meet unless the
// relay forwards the second one.
func TestWithoutTheMeetingSpotTheLegsNeverPair(t *testing.T) {
	f := newFixture(t, true)
	f.noMeet = true
	a := f.startPod(t, "pod-a")
	b := f.startPod(t, "pod-b")
	err := relayedSession(t, a, b)
	if err == nil {
		t.Fatal("legs on two pods paired without the meeting spot")
	}
	t.Logf("fails as it should: %v", err)
}

// TestAPeerThatMovesIsReachedOnItsNewPod: a peer registered on pod A
// registers again on pod B (pod A died, or the load balancer moved it).
// Pod A drops its entry, and a session asked on pod A reaches the peer
// through pod B.
func TestAPeerThatMovesIsReachedOnItsNewPod(t *testing.T) {
	f := newFixture(t, true)
	a := f.startPod(t, "pod-a")
	b := f.startPod(t, "pod-b")

	target := newPeer(t, uniqueID("CTMOV"), a.signal)
	target.register(t)
	target.to, _ = net.ResolveUDPAddr("udp", b.signal)
	target.register(t)

	deadline := time.Now().Add(3 * time.Second)
	for {
		local := a.sig.PeerMap().GetLocal(target.id)
		remote := a.sig.PeerMap().Get(target.id)
		if local == nil && remote != nil && remote.Owner == "pod-b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod A still holds %s (local=%v, remote owner=%v)", target.id, local != nil, remote)
		}
		time.Sleep(50 * time.Millisecond)
	}

	initiator := newPeer(t, uniqueID("CTINI"), a.signal)
	initiator.register(t)
	uuid := uniqueID("ct-move-")
	initiator.send(t, &pb.RendezvousMessage{Union: &pb.RendezvousMessage_RequestRelay{RequestRelay: &pb.RequestRelay{Id: target.id, Uuid: uuid}}})
	resp := initiator.await(t, "RelayResponse", func(m *pb.RendezvousMessage) bool { return m.GetRelayResponse() != nil })
	if r := resp.GetRelayResponse().GetRefuseReason(); r != "" {
		t.Fatalf("refused: %s", r)
	}
	req := target.awaitOrNil(func(m *pb.RendezvousMessage) bool { return m.GetRequestRelay() != nil })
	if req == nil || req.GetRequestRelay().GetUuid() != uuid {
		t.Fatalf("moved target got %v", req)
	}
}

// waitVisible waits up to a second until p sees peer id (locally or
// through the directory). Without the cluster it never does, and the
// session then fails as the control tests expect.
func waitVisible(p *pod, id string) {
	deadline := time.Now().Add(time.Second)
	for p.sig.PeerMap().Get(id) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// TestADeviceOnlineOnOnePodShowsLiveOnAnother: a peer registers on pod A;
// pod B's event bus, which the console's live sockets on B read, hears it
// come online.
func TestADeviceOnlineOnOnePodShowsLiveOnAnother(t *testing.T) {
	f := newFixture(t, true)
	a, b := f.startPod(t, "pod-a"), f.startPod(t, "pod-b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cluster.NewEventLinkOn(f.client, f.prefix).Ready(ctx, 2); err != nil {
		t.Fatal(err)
	}
	sub := b.sig.EventBus().Subscribe(events.EventPeerOnline)
	defer b.sig.EventBus().Unsubscribe(sub)

	peerA := newPeer(t, uniqueID("1"), a.signal)
	peerA.register(t)
	for {
		select {
		case ev := <-sub.Ch:
			if ev.Data["id"] == peerA.id {
				if !ev.Remote {
					t.Fatal("the event is not marked as another pod's")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("pod b never heard the peer on pod a come online")
		}
	}
}
