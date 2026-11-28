package integration

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/wire"
)

// controlProxy sits between a node and the hub and sees the control
// channel in clear: toward the node it is central (it holds the central
// certificate the node pins), toward the hub it is the node (it holds the
// node's key). It records every frame the hub sends the node and can drop
// authorize frames, so the node never acks them.
type controlProxy struct {
	dropAuthorize atomic.Bool

	ln      net.Listener
	mu      sync.Mutex
	frames  []wire.Frame // hub -> node, in order
	dropped int
	conns   []net.Conn
}

func (p *controlProxy) addr() string { return p.ln.Addr().String() }

func (p *controlProxy) start(t *testing.T, s *server, nodeKey ed25519.PrivateKey) {
	t.Helper()
	centralCert, pin, err := hub.LoadOrCreateCert(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if pin != s.glue.Pin {
		t.Fatalf("proxy loaded pin %s, hub uses %s", pin, s.glue.Pin)
	}
	nodeCert, err := wire.SelfSignedCert(nodeKey, "neodesk relay node")
	if err != nil {
		t.Fatal(err)
	}
	p.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
	})
	hubAddr := s.controlAddr()
	go func() {
		for {
			c, err := p.ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, hubAddr, centralCert, nodeCert, pin)
		}
	}()
}

func (p *controlProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns = append(p.conns, c)
	p.mu.Unlock()
}

func (p *controlProxy) serve(raw net.Conn, hubAddr string, centralCert, nodeCert tls.Certificate, pin string) {
	p.track(raw)
	down := tls.Server(raw, wire.ServerTLS(centralCert))
	if err := down.Handshake(); err != nil {
		raw.Close()
		return
	}
	upRaw, err := net.DialTimeout("tcp", hubAddr, 3*time.Second)
	if err != nil {
		raw.Close()
		return
	}
	p.track(upRaw)
	up := tls.Client(upRaw, wire.ClientTLS(nodeCert, pin))
	if err := up.Handshake(); err != nil {
		raw.Close()
		upRaw.Close()
		return
	}
	closeBoth := func() {
		raw.Close()
		upRaw.Close()
	}
	// node -> hub as it is
	go func() {
		io.Copy(up, down)
		closeBoth()
	}()
	// hub -> node frame by frame
	defer closeBoth()
	for {
		f, err := wire.ReadFrame(up)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.frames = append(p.frames, f)
		drop := f.Type == wire.TypeAuthorize && p.dropAuthorize.Load()
		if drop {
			p.dropped++
		}
		p.mu.Unlock()
		if drop {
			continue
		}
		buf := make([]byte, 4+len(f.Body))
		binary.BigEndian.PutUint32(buf, uint32(len(f.Body)))
		copy(buf[4:], f.Body)
		if _, err := down.Write(buf); err != nil {
			return
		}
	}
}

func (p *controlProxy) seen() []wire.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]wire.Frame(nil), p.frames...)
}

func (p *controlProxy) droppedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// relayProxy sits in front of a node's relay port and keeps, per peer
// connection, every byte the peer sent the node.
type relayProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []*captured
}

type captured struct {
	mu       sync.Mutex
	fromPeer bytes.Buffer
	done     chan struct{}
}

func (c *captured) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fromPeer.Write(p)
}

func (c *captured) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.fromPeer.Bytes())
}

func startRelayProxy(t *testing.T, target string) *relayProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &relayProxy{ln: ln, target: target}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			cap := &captured{done: make(chan struct{})}
			p.mu.Lock()
			p.conns = append(p.conns, cap)
			p.mu.Unlock()
			go p.serve(c, cap)
		}
	}()
	return p
}

func (p *relayProxy) addr() string { return p.ln.Addr().String() }

func (p *relayProxy) serve(peer net.Conn, cap *captured) {
	defer close(cap.done)
	defer peer.Close()
	up, err := net.DialTimeout("tcp", p.target, 3*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	back := make(chan struct{})
	go func() {
		io.Copy(peer, up)
		peer.Close()
		close(back)
	}()
	io.Copy(up, io.TeeReader(peer, cap))
	up.Close()
	<-back
}

// captures waits until every connection so far has closed and returns
// what each peer sent.
func (p *relayProxy) captures(t *testing.T) [][]byte {
	t.Helper()
	p.mu.Lock()
	conns := append([]*captured(nil), p.conns...)
	p.mu.Unlock()
	var out [][]byte
	for _, c := range conns {
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Fatal("relay proxy connection did not close")
		}
		out = append(out, c.bytes())
	}
	return out
}
