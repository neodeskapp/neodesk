package meshcentral

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/neodeskapp/neodesk/neodesk-server/audit"
)

// relayHub multiplexes one mesh agent tunnel to multiple browser viewers
// (KVM p=2). It ends when its agent leaves, when the last socket leaves,
// or when no agent and viewer met within waitForPeers.
type relayHub struct {
	gateway     *Gateway
	relayID     string
	proto       string
	mu          sync.Mutex
	agent       *relayPeer
	viewers     []*relayPeer
	recorder    *relayRecorder
	recPath     string
	auditUser   string
	auditPeer   string
	sessionType string
	started     bool
	idle        *time.Timer
	// ended is closed when the hub has ended
	ended chan struct{}
	done  atomic.Bool
}

// runKvmRelayHub runs one socket of a KVM hub until the socket or the hub
// ends.
func (g *Gateway) runKvmRelayHub(ctx context.Context, relayID string, peer *relayPeer, proto string) {
	hub := &relayHub{gateway: g, relayID: relayID, proto: proto, ended: make(chan struct{})}
	if v, loaded := g.relayHubs.LoadOrStore(relayID, hub); loaded {
		hub = v.(*relayHub)
	} else {
		hub.mu.Lock()
		hub.idle = time.AfterFunc(g.waitForPeers(), hub.endIfNotStarted)
		hub.mu.Unlock()
	}
	if !hub.attach(peer) {
		peer.ws.Close(websocket.StatusPolicyViolation, "relay in use")
		return
	}
	select {
	case <-peer.gone:
	case <-hub.ended:
	case <-ctx.Done():
	}
	hub.detach(peer)
	peer.ws.Close(websocket.StatusNormalClosure, "")
}

// endIfNotStarted ends a hub whose agent and viewers never met.
func (h *relayHub) endIfNotStarted() {
	h.mu.Lock()
	started := h.started
	h.mu.Unlock()
	if !started {
		h.finish()
	}
}

// attach adds a viewer, or the agent, and starts the piping when both are
// there. It reports false for a hub that has ended or has an agent already.
func (h *relayHub) attach(peer *relayPeer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done.Load() {
		return false
	}
	if peer.browser {
		h.viewers = append(h.viewers, peer)
		sig := RelayConnectSignal(h.recorder != nil)
		peer.ws.Write(peer.ctx, websocket.MessageText, []byte(sig))
		if !h.started && h.agent != nil {
			h.startPipingLocked()
		}
		go h.readViewer(peer)
		return true
	}
	if h.agent != nil {
		return false
	}
	h.agent = peer
	if !h.started && len(h.viewers) > 0 {
		h.startPipingLocked()
	}
	return true
}

func (h *relayHub) detach(peer *relayPeer) {
	h.mu.Lock()
	if peer.browser {
		for i, v := range h.viewers {
			if v == peer {
				h.viewers = append(h.viewers[:i], h.viewers[i+1:]...)
				break
			}
		}
	} else if h.agent == peer {
		h.agent = nil
	}
	empty := h.agent == nil && len(h.viewers) == 0
	h.mu.Unlock()
	if empty {
		h.finish()
	}
}

func (h *relayHub) startPipingLocked() {
	if h.started {
		return
	}
	h.started = true

	meta := h.gateway.getRelayMeta(h.relayID)
	recording := meta != nil && meta.Record
	h.auditUser = h.relayID
	h.auditPeer = h.relayID
	h.sessionType = h.proto
	if meta != nil {
		if meta.UserID != "" {
			h.auditUser = meta.UserID
		}
		if meta.PeerID != "" {
			h.auditPeer = meta.PeerID
		}
		if meta.SessionType != "" {
			h.sessionType = meta.SessionType
		}
		if recording && h.gateway.cfg != nil {
			dataDir := filepath.Dir(h.gateway.cfg.DBPath)
			if dataDir == "" || dataDir == "." {
				dataDir = "."
			}
			if r, path, err := openRelayRecorder(dataDir, meta.PeerID, h.relayID, meta.SessionType); err == nil {
				h.recorder = r
				h.recPath = path
			}
		}
	}

	sig := RelayConnectSignal(recording)
	if h.agent != nil {
		h.agent.ws.Write(h.agent.ctx, websocket.MessageText, []byte(sig))
	}
	for _, v := range h.viewers {
		v.ws.Write(v.ctx, websocket.MessageText, []byte(sig))
	}

	if h.gateway.auditLog != nil {
		h.gateway.auditLog.Log(audit.ActionPeerUpdated, h.auditUser, h.auditPeer, map[string]string{
			"event": "mesh_relay_paired",
			"proto": h.proto,
			"mode":  "multiplex",
		})
	}

	if h.agent != nil {
		go h.readAgent(h.agent, h.recorder)
	}
}

// readAgent sends the agent's messages to every viewer until the agent
// leaves, which ends the hub.
func (h *relayHub) readAgent(agent *relayPeer, recorder *relayRecorder) {
	defer func() {
		h.mu.Lock()
		if h.agent == agent {
			h.agent = nil
		}
		h.mu.Unlock()
		h.finish()
	}()
	for {
		select {
		case f := <-agent.frames:
			if recorder != nil && len(f.data) > 0 {
				recorder.Write(int(f.typ), f.data)
			}
			h.mu.Lock()
			viewers := append([]*relayPeer{}, h.viewers...)
			h.mu.Unlock()
			for _, v := range viewers {
				if err := v.ws.Write(v.ctx, f.typ, f.data); err != nil {
					v.ws.Close(websocket.StatusGoingAway, "")
				}
			}
		case <-agent.gone:
			return
		case <-h.ended:
			return
		}
	}
}

// readViewer sends a viewer's messages to the agent until the viewer
// leaves or the hub ends.
func (h *relayHub) readViewer(viewer *relayPeer) {
	for {
		select {
		case f := <-viewer.frames:
			h.mu.Lock()
			agent, recorder := h.agent, h.recorder
			h.mu.Unlock()
			if agent == nil {
				continue
			}
			if recorder != nil && len(f.data) > 0 {
				recorder.Write(int(f.typ), f.data)
			}
			if err := agent.ws.Write(agent.ctx, f.typ, f.data); err != nil {
				return
			}
		case <-viewer.gone:
			return
		case <-h.ended:
			return
		}
	}
}

func (h *relayHub) finish() {
	if h.done.Swap(true) {
		return
	}
	h.mu.Lock()
	if h.idle != nil {
		h.idle.Stop()
	}
	recorder, recPath := h.recorder, h.recPath
	auditUser, auditPeer, sessionType := h.auditUser, h.auditPeer, h.sessionType
	h.mu.Unlock()
	close(h.ended)
	if recorder != nil {
		recorder.Close()
	}
	h.gateway.relayHubs.CompareAndDelete(h.relayID, h)
	h.gateway.setRelayMeta(h.relayID, nil)
	if h.gateway.auditLog != nil {
		fields := map[string]string{
			"event": "mesh_session_end",
			"type":  sessionType,
			"proto": h.proto,
		}
		if recPath != "" {
			fields["recording"] = recPath
		}
		h.gateway.auditLog.Log(audit.ActionPeerUpdated, auditUser, auditPeer, fields)
	}
	log.Printf("[mesh] Relay hub %s ended", h.relayID)
}
