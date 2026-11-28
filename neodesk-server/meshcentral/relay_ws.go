package meshcentral

import (
	"context"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/neodeskapp/neodesk/neodesk-server/audit"
)

// relayWaitDefault is how long a relay waits for its peers: the agent and
// the browser of a session, the agent and the first viewer of a KVM hub.
const relayWaitDefault = 2 * time.Minute

// waitForPeers is the gateway's relayWait.
func (g *Gateway) waitForPeers() time.Duration {
	if g.relayWait > 0 {
		return g.relayWait
	}
	return relayWaitDefault
}

// relayFrame is one message of a relay socket.
type relayFrame struct {
	typ  websocket.MessageType
	data []byte
}

// relayPeer is one socket of a relay. Its messages are read from the
// moment it comes in, so its close is seen while it waits; frames hands
// them on one at a time, and gone is closed when the socket has ended.
type relayPeer struct {
	ws      *websocket.Conn
	ctx     context.Context
	userID  string
	browser bool
	frames  chan relayFrame
	gone    chan struct{}
}

// newRelayPeer starts reading the socket until it ends or ctx ends.
func newRelayPeer(ctx context.Context, ws *websocket.Conn, userID string, browser bool) *relayPeer {
	p := &relayPeer{ws: ws, ctx: ctx, userID: userID, browser: browser,
		frames: make(chan relayFrame), gone: make(chan struct{})}
	go func() {
		defer close(p.gone)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			select {
			case p.frames <- relayFrame{typ, data}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return p
}

// relaySession pairs the two sockets of a relay that is no KVM hub.
type relaySession struct {
	id    string
	mu    sync.Mutex
	peer1 *relayPeer
	// state is relayWaiting, relayPaired or relayAbandoned
	state int
	// paired is closed when the second peer came, done when the piping
	// has ended, abandoned when the session was given up unpaired.
	paired    chan struct{}
	done      chan struct{}
	abandoned chan struct{}
}

const (
	relayWaiting = iota
	relayPaired
	relayAbandoned
)

func newRelaySession(id string) *relaySession {
	return &relaySession{id: id, paired: make(chan struct{}), done: make(chan struct{}), abandoned: make(chan struct{})}
}

// abandon gives the session up when nobody paired with its first peer. It
// reports whether the session is given up, now or before; false when it
// is paired.
func (s *relaySession) abandon() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case relayPaired:
		return false
	case relayWaiting:
		s.state = relayAbandoned
		close(s.abandoned)
	}
	return true
}

// handleRelayWS is /meshrelay.ashx: one socket of a relay the console
// opened with createRelayTunnel. Before the handshake the socket needs a
// valid auth (browser) or rauth (agent) cookie of the node the relay was
// opened to; anything else is 403.
func (g *Gateway) handleRelayWS(w http.ResponseWriter, r *http.Request) {
	if !g.allowIP(w, r) {
		return
	}
	q := r.URL.Query()
	relayID := q.Get("id")
	if relayID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	// A relay another instance set up meets there
	if route := g.relayRouter; route != nil && g.getRelayMeta(relayID) == nil && route(w, r, relayID) {
		return
	}
	cookie, ok := g.relayCookie(relayID, q.Get("auth"), q.Get("rauth"))
	if !ok {
		http.Error(w, "invalid auth", http.StatusForbidden)
		return
	}

	conn, err := websocket.Accept(w, r, g.webSocketAcceptOptions())
	if err != nil {
		return
	}
	conn.SetReadLimit(32 * 1024 * 1024)
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if g.ctx != nil {
		stop := context.AfterFunc(g.ctx, cancel)
		defer stop()
	}

	peer := newRelayPeer(ctx, conn, cookie.RUserID, q.Get("browser") == "1")
	g.pairRelay(ctx, relayID, peer, q.Get("p"))
}

// relayCookie decodes the cookie of a relay socket (rauth wins over auth)
// and checks that it names the node of the relay's agent.
func (g *Gateway) relayCookie(relayID, auth, rauth string) (*RelayCookieData, bool) {
	raw := auth
	if rauth != "" {
		raw = rauth
	}
	if raw == "" {
		return nil, false
	}
	cookie, err := g.cookies.Decode(raw, 240)
	if err != nil || cookie.NodeID == "" {
		return nil, false
	}
	meta := g.getRelayMeta(relayID)
	if meta == nil || meta.PeerID == "" || g.MeshNodeID(meta.PeerID) != cookie.NodeID {
		return nil, false
	}
	return cookie, true
}

func isKvmRelayProto(proto string) bool {
	return proto == "2"
}

// pairRelay runs one socket of a relay until it ends: the first peer waits
// for the second, at most waitForPeers, and the second pipes the two
// together. Either peer leaving ends the session and both sockets; a third
// socket is closed.
func (g *Gateway) pairRelay(ctx context.Context, relayID string, peer *relayPeer, proto string) {
	if isKvmRelayProto(proto) {
		g.runKvmRelayHub(ctx, relayID, peer, proto)
		return
	}
	v, loaded := g.relays.LoadOrStore(relayID, newRelaySession(relayID))
	session := v.(*relaySession)
	if !loaded {
		timer := time.AfterFunc(g.waitForPeers(), func() { session.abandon() })
		defer timer.Stop()
	}

	session.mu.Lock()
	switch {
	case session.state == relayAbandoned:
		session.mu.Unlock()
		peer.ws.Close(websocket.StatusGoingAway, "")
		return
	case session.peer1 == nil:
		session.peer1 = peer
		session.mu.Unlock()
		g.waitAsFirstPeer(ctx, session, peer)
		return
	case session.state == relayPaired:
		session.mu.Unlock()
		peer.ws.Close(websocket.StatusPolicyViolation, "relay in use")
		return
	}
	peer1 := session.peer1
	session.state = relayPaired
	close(session.paired)
	session.mu.Unlock()
	defer close(session.done)
	g.pipeSession(ctx, relayID, proto, peer1, peer)
}

// waitAsFirstPeer waits for the second peer and then for the end of the
// session, or gives the session up when the socket ends, the time is over
// or the gateway stops.
func (g *Gateway) waitAsFirstPeer(ctx context.Context, session *relaySession, peer *relayPeer) {
	select {
	case <-session.paired:
	case <-peer.gone:
	case <-session.abandoned:
	case <-ctx.Done():
	}
	if session.abandon() {
		g.relays.CompareAndDelete(session.id, session)
		g.setRelayMeta(session.id, nil)
		peer.ws.Close(websocket.StatusGoingAway, "")
		return
	}
	// Paired: the second peer's handler pipes until the session ends.
	select {
	case <-session.done:
	case <-ctx.Done():
	}
}

// pipeSession pipes a paired session until one side ends.
func (g *Gateway) pipeSession(ctx context.Context, relayID, proto string, peer1, peer2 *relayPeer) {
	meta := g.getRelayMeta(relayID)
	recording := meta != nil && meta.Record
	sig := RelayConnectSignal(recording)
	peer1.ws.Write(ctx, websocket.MessageText, []byte(sig))
	peer2.ws.Write(ctx, websocket.MessageText, []byte(sig))

	auditUser := peer1.userID
	auditPeer := relayID
	sessionType := proto
	recPath := ""
	if meta != nil {
		if meta.UserID != "" {
			auditUser = meta.UserID
		}
		if meta.PeerID != "" {
			auditPeer = meta.PeerID
		}
		if meta.SessionType != "" {
			sessionType = meta.SessionType
		}
	}

	var recorder *relayRecorder
	if recording && meta != nil && g.cfg != nil {
		dataDir := filepath.Dir(g.cfg.DBPath)
		if dataDir == "" || dataDir == "." {
			dataDir = "."
		}
		if r, path, err := openRelayRecorder(dataDir, meta.PeerID, relayID, meta.SessionType); err == nil {
			recorder = r
			recPath = path
		}
	}

	if g.auditLog != nil {
		g.auditLog.Log(audit.ActionPeerUpdated, auditUser, auditPeer, map[string]string{
			"event": "mesh_relay_paired",
			"proto": proto,
		})
	}

	pipeRelay(ctx, peer1, peer2, recorder)
	if recorder != nil {
		recorder.Close()
	}
	g.relays.Delete(relayID)
	g.setRelayMeta(relayID, nil)

	if g.auditLog != nil {
		fields := map[string]string{
			"event": "mesh_session_end",
			"type":  sessionType,
			"proto": proto,
		}
		if recPath != "" {
			fields["recording"] = recPath
		}
		g.auditLog.Log(audit.ActionPeerUpdated, auditUser, auditPeer, fields)
	}
	log.Printf("[mesh] Relay session %s ended", relayID)
}

// pipeRelay copies the messages of each peer to the other until one side
// ends, then closes both sockets.
func pipeRelay(ctx context.Context, a, b *relayPeer, recorder *relayRecorder) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	copyFrames := func(dst, src *relayPeer) {
		defer wg.Done()
		defer cancel()
		for {
			select {
			case f := <-src.frames:
				if recorder != nil && len(f.data) > 0 {
					recorder.Write(int(f.typ), f.data)
				}
				if err := dst.ws.Write(ctx, f.typ, f.data); err != nil {
					return
				}
			case <-src.gone:
				return
			case <-ctx.Done():
				return
			}
		}
	}
	wg.Add(2)
	go copyFrames(b, a)
	go copyFrames(a, b)
	wg.Wait()
	closeAll(a, b)
}

// closeAll closes the sockets of the peers at once.
func closeAll(peers ...*relayPeer) {
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.ws.Close(websocket.StatusNormalClosure, "")
		}()
	}
	wg.Wait()
}

// KVMRelay handles opaque MNG_KVM binary relay (protocol p=2).
func KVMRelay(ctx context.Context, a, b io.ReadWriter) {
	go func() { io.Copy(b, a) }()
	io.Copy(a, b)
}
