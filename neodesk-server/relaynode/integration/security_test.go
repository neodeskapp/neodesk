package integration

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/codec"
	pb "github.com/neodeskapp/neodesk/neodesk-server/proto"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/wire"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"
	"google.golang.org/protobuf/proto"
)

// TestARelayNodeNeverReceivesSessionKeys: everything a relay node gets
// during a session, on its control channel and on its relay port, is
// captured in clear. The peers do an end-to-end key exchange through the
// node, as RustDesk clients do inside the relayed stream. The node gets
// no peer key, no signed key, no server secret and not the session key:
// from central only welcome, the ticket (uuid and the two peer ids),
// pings; from each peer only RequestRelay{uuid, licence_key} (the server's
// public key) followed by the bytes that peer sent.
func TestARelayNodeNeverReceivesSessionKeys(t *testing.T) {
	s := newServer(t)
	siteA := s.site("Site A", "10.1.0.0/16")
	opts := &nodeOpts{control: &controlProxy{}, relayFront: true}
	a := s.startNode(siteA, opts)
	ini, tgt := newDevice("INI"), newDevice("TGT")
	s.pin(siteA, ini, tgt)

	ses := s.connect(ini, tgt)
	if ses.initiatorRelay != a.relayAddr || ses.targetRelay != a.relayAddr {
		t.Fatalf("initiator told %q, target told %q; want node A %q", ses.initiatorRelay, ses.targetRelay, a.relayAddr)
	}
	if len(ses.signedPK) == 0 {
		t.Fatal("RelayResponse carried no signed key; the check below would prove nothing about it")
	}
	sessionKey, privs := endToEndExchange(t, ses.ini, ses.tgt)
	ses.close()
	waitFor(t, "node reports the end", 3*time.Second, func() bool { return s.billing.ended.has(ses.uuid) })

	secrets := map[string][]byte{
		"session key":                   sessionKey[:],
		"initiator's exchange key":      privs[0][:],
		"target's exchange key":         privs[1][:],
		"initiator's device key":        ini.pk,
		"target's device key":           tgt.pk,
		"signed key from RelayResponse": ses.signedPK,
		"server private key":            s.kp.PrivateKey,
	}

	// Control channel: only known message types with known fields, and no
	// secret in any encoding
	allowed := map[string][]string{
		wire.TypeWelcome:   {"t", "node_id", "site_id", "org_id", "name", "server_pk"},
		wire.TypeAuthorize: {"t", "id", "uuid", "initiator", "target", "expires_unix_ms"},
		wire.TypePing:      {"t"},
		wire.TypeRevoke:    {"t", "peer"},
		wire.TypeEnd:       {"t", "uuid"},
	}
	frames := opts.control.seen()
	var sawAuthorize bool
	for _, f := range frames {
		fields, ok := allowed[f.Type]
		if !ok {
			t.Errorf("central sent the node a %q frame: %s", f.Type, f.Body)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(f.Body, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if !slices.Contains(fields, k) {
				t.Errorf("%s frame carries field %q: %s", f.Type, k, f.Body)
			}
		}
		switch f.Type {
		case wire.TypeWelcome:
			var w wire.Welcome
			_ = f.Decode(&w)
			if w.ServerPK != s.kp.PublicKeyBase64() {
				t.Errorf("welcome server_pk %q is not the server's public key", w.ServerPK)
			}
		case wire.TypeAuthorize:
			var au wire.Authorize
			_ = f.Decode(&au)
			if au.UUID == ses.uuid && au.Initiator == ini.id && au.Target == tgt.id {
				sawAuthorize = true
			}
		}
		for name, v := range secrets {
			if enc, ok := contains(f.Body, v); ok {
				t.Errorf("%s frame carries the %s (%s): %s", f.Type, name, enc, f.Body)
			}
		}
	}
	if !sawAuthorize {
		t.Fatalf("the capture holds no authorize for the session; frames: %v", frameTypes(frames))
	}

	// Relay port: per peer connection, exactly RequestRelay{uuid,
	// licence_key} and then what that peer sent
	caps := opts.relayProxy.captures(t)
	if len(caps) != 2 {
		t.Fatalf("node relay got %d connections, want 2", len(caps))
	}
	want := [][]byte{ses.ini.sent(), ses.tgt.sent()}
	sort.Slice(caps, func(i, j int) bool { return bytes.Compare(caps[i], caps[j]) < 0 })
	sort.Slice(want, func(i, j int) bool { return bytes.Compare(want[i], want[j]) < 0 })
	for i, got := range caps {
		if !bytes.Equal(got, want[i]) {
			t.Errorf("node relay got %d bytes on a connection, the peer sent %d different ones", len(got), len(want[i]))
		}
		rr, rest := splitRequestRelay(t, got)
		wantRR := &pb.RequestRelay{Uuid: ses.uuid, LicenceKey: s.kp.PublicKeyBase64()}
		if !proto.Equal(rr, wantRR) {
			t.Errorf("node relay got RequestRelay %v, want only uuid and licence_key %v", rr, wantRR)
		}
		for name, v := range secrets {
			if name == "initiator's device key" || name == "target's device key" || name == "signed key from RelayResponse" {
				// public values the peers may exchange end to end
				continue
			}
			if enc, ok := contains(rest, v); ok {
				t.Errorf("node relay got the %s (%s) from a peer", name, enc)
			}
		}
	}
}

// endToEndExchange runs a key exchange like RustDesk's through the relay:
// the target sends a fresh box public key, the initiator answers with its
// own and a random session key sealed to the target, then each side sends
// a message under the session key and the other opens it.
func endToEndExchange(t *testing.T, ini, tgt net.Conn) (sessionKey [32]byte, privs [2]*[32]byte) {
	t.Helper()
	tPub, tPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iPub, iPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(sessionKey[:]); err != nil {
		t.Fatal(err)
	}
	writeRec(t, tgt, tPub[:])
	var gotTPub [32]byte
	copy(gotTPub[:], readRec(t, ini))

	var nonce [24]byte
	_, _ = rand.Read(nonce[:])
	sealed := box.Seal(nil, sessionKey[:], &nonce, &gotTPub, iPriv)
	writeRec(t, ini, slices.Concat(iPub[:], nonce[:], sealed))

	rec := readRec(t, tgt)
	var gotIPub [32]byte
	copy(gotIPub[:], rec[:32])
	copy(nonce[:], rec[32:56])
	opened, ok := box.Open(nil, rec[56:], &nonce, &gotIPub, tPriv)
	if !ok || !bytes.Equal(opened, sessionKey[:]) {
		t.Fatal("the target could not open the session key")
	}
	var key [32]byte
	copy(key[:], opened)

	say := func(from, to net.Conn, msg string) {
		var n [24]byte
		_, _ = rand.Read(n[:])
		writeRec(t, from, secretbox.Seal(n[:], []byte(msg), &n, &key))
		got := readRec(t, to)
		copy(n[:], got[:24])
		plain, ok := secretbox.Open(nil, got[24:], &n, &key)
		if !ok || string(plain) != msg {
			t.Fatalf("encrypted message did not arrive: %q", plain)
		}
	}
	say(ini, tgt, "keyboard and mouse")
	say(tgt, ini, "screen frames")
	return sessionKey, [2]*[32]byte{iPriv, tPriv}
}

func writeRec(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	buf := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(append(buf, b...)); err != nil {
		t.Fatal(err)
	}
}

func readRec(t *testing.T, c net.Conn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// contains reports whether v appears in b raw, in hex (either case) or in
// any base64 alphabet.
func contains(b, v []byte) (string, bool) {
	if len(v) == 0 {
		return "", false
	}
	if bytes.Contains(b, v) {
		return "raw", true
	}
	if bytes.Contains(bytes.ToLower(b), []byte(hex.EncodeToString(v))) {
		return "hex", true
	}
	for name, enc := range map[string]*base64.Encoding{
		"base64": base64.StdEncoding, "base64 unpadded": base64.RawStdEncoding,
		"base64url": base64.URLEncoding, "base64url unpadded": base64.RawURLEncoding,
	} {
		if bytes.Contains(b, []byte(enc.EncodeToString(v))) {
			return name, true
		}
	}
	return "", false
}

// splitRequestRelay reads the RendezvousMessage frame a peer sends first
// and returns its RequestRelay and the bytes after it.
func splitRequestRelay(t *testing.T, b []byte) (*pb.RequestRelay, []byte) {
	t.Helper()
	c := &bytesConn{r: bytes.NewReader(b)}
	m, err := codec.ReadRawProto(c, time.Second)
	if err != nil {
		t.Fatalf("first frame a peer sent the node: %v", err)
	}
	if m.GetRequestRelay() == nil {
		t.Fatalf("first frame a peer sent the node is %v, want RequestRelay", m)
	}
	rest, _ := io.ReadAll(c.r)
	return m.GetRequestRelay(), rest
}

func frameTypes(fs []wire.Frame) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Type)
	}
	return out
}

// bytesConn is a net.Conn that reads from a byte slice.
type bytesConn struct {
	net.Conn
	r *bytes.Reader
}

func (c *bytesConn) Read(p []byte) (int, error)      { return c.r.Read(p) }
func (c *bytesConn) SetReadDeadline(time.Time) error { return nil }
func (c *bytesConn) SetDeadline(time.Time) error     { return nil }
func (c *bytesConn) RemoteAddr() net.Addr            { return &net.TCPAddr{} }
