package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/neodeskapp/neodesk/neodesk-server/audit"
	"github.com/neodeskapp/neodesk/neodesk-server/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/cdap"
	"github.com/neodeskapp/neodesk/neodesk-server/config"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
	eventsModule "github.com/neodeskapp/neodesk/neodesk-server/events"
	"github.com/neodeskapp/neodesk/neodesk-server/metrics"
	"github.com/neodeskapp/neodesk/neodesk-server/peer"
	"github.com/neodeskapp/neodesk/neodesk-server/security"
)

// pipeRouting connects two API servers in one process the way the cluster
// mesh connects two pods: the device is on pod "b", streams to it run its
// ServeForwarded.
type pipeRouting struct {
	device string
	pod    *Server
	mu     sync.Mutex
	nonces map[string]bool
	calls  []string
}

func (p *pipeRouting) Owner(_ context.Context, kind, id string) string {
	if kind == "cdap" && id == p.device {
		return "b"
	}
	return ""
}

func (p *pipeRouting) Stream(_ context.Context, pod, name string, meta map[string]string) (net.Conn, error) {
	here, there := net.Pipe()
	go p.pod.ServeForwarded("a", meta, there)
	return here, nil
}

func (p *pipeRouting) ClaimOnce(_ context.Context, nonce string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nonces[nonce] {
		return false
	}
	p.nonces[nonce] = true
	return true
}

func (p *pipeRouting) Call(ctx context.Context, pod, name string, body []byte) ([]byte, error) {
	p.mu.Lock()
	p.calls = append(p.calls, name)
	p.mu.Unlock()
	return p.pod.DeviceCalls()[name](ctx, body)
}

// secondServer is another API server on database, like a second pod of
// the installation, with a CDAP gateway no device is connected to.
func secondServer(t *testing.T, database db.Database) *Server {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.APIPort = freePort(t)
	srv := New(cfg, database, peer.NewMap(), nil, "1.0.0-test")
	srv.SetBlocklist(security.NewBlocklist())
	auditLog := audit.NewLogger("")
	t.Cleanup(func() { auditLog.Close() })
	srv.SetAuditLogger(auditLog)
	srv.SetEventBus(eventsModule.NewBus())
	srv.SetMetrics(metrics.NewCollector())
	gcfg := config.DefaultConfig()
	gcfg.CDAPPort = freePort(t)
	gw := cdap.New(gcfg, database, peer.NewMap(), eventsModule.NewBus())
	gw.SetJWTManager(auth.NewJWTManager("cdap-forward-test-secret", time.Hour))
	if err := gw.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Stop)
	srv.SetCDAPGateway(gw)
	return srv
}

// openTerminal opens the device's terminal on srv as bridge and returns
// the browser's socket and the answer of the first read.
func openTerminal(t *testing.T, srv *Server, deviceID string) (*websocket.Conn, func() (map[string]string, error)) {
	t.Helper()
	bridge := Identity{Username: "apikey:console-bridge", Role: auth.RoleAdmin, Actor: "alice"}
	h := srv.InternalHandler()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), bridge)))
	}))
	t.Cleanup(web.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	browser, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(web.URL, "http")+"/api/cdap/devices/"+deviceID+"/terminal", &websocket.DialOptions{Subprotocols: []string{"cdap-terminal"}})
	if err != nil {
		t.Fatalf("terminal socket: %v", err)
	}
	t.Cleanup(func() { browser.CloseNow() })
	if err := browser.Write(ctx, websocket.MessageText, []byte(`{"cols":80,"rows":24}`)); err != nil {
		t.Fatal(err)
	}
	read := func() (map[string]string, error) {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		_, raw, err := browser.Read(rctx)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		_ = json.Unmarshal(raw, &out)
		return out, nil
	}
	return browser, read
}

// TestATerminalOpenedOnOnePodReachesADeviceOnAnother: the console opens a
// CDAP terminal through pod A while the device's socket is on pod B. The
// request is passed to B with the console's caller, the device gets the
// terminal with the signed-in account as operator, and its output comes
// back to the browser on A.
func TestATerminalOpenedOnOnePodReachesADeviceOnAnother(t *testing.T) {
	podB, database := newInternalServer(t, nil)
	gwB, device := connectCDAPDevice(t, database)
	podB.SetCDAPGateway(gwB)
	podA := secondServer(t, database)
	routing := &pipeRouting{device: device.id, pod: podB, nonces: map[string]bool{}}
	podA.SetDeviceRouting(routing)
	podB.SetDeviceRouting(&pipeRouting{nonces: map[string]bool{}})

	_, read := openTerminal(t, podA, device.id)
	start := device.expect("terminal_start")
	if start["operator_name"] != "alice" {
		t.Errorf("the terminal's operator on the device: %v, want alice", start["operator_name"])
	}
	session, _ := start["session_id"].(string)
	device.send("terminal_output", map[string]string{"session_id": session, "data": "hello from pod b", "stream": "stdout"})
	for {
		msg, err := read()
		if err != nil {
			t.Fatalf("the browser on pod A got no output: %v", err)
		}
		if msg["type"] == "output" {
			if msg["data"] != "hello from pod b" {
				t.Fatalf("output %q", msg["data"])
			}
			break
		}
	}

	// A chat line for the device and its revocation, sent through pod A,
	// are carried out on pod B
	if ok, err := podA.cdapChat(context.Background(), device.id, "op", "Operator", "hi"); !ok || err != nil {
		t.Fatalf("chat through pod A: delivered %v, %v", ok, err)
	}
	if got := device.expect("chat_message"); got["text"] != "hi" {
		t.Errorf("chat on the device: %v", got)
	}
	if err := podA.cdapRevoke(context.Background(), device.id, "test"); err != nil {
		t.Fatalf("revoke through pod A: %v", err)
	}
	device.expect("revoke")
}

// TestWithoutRoutingTheTerminalStaysOnThePodWithoutTheDevice: the same
// request on pod A with no routing finds no device there.
func TestWithoutRoutingTheTerminalStaysOnThePodWithoutTheDevice(t *testing.T) {
	podB, database := newInternalServer(t, nil)
	gwB, device := connectCDAPDevice(t, database)
	podB.SetCDAPGateway(gwB)
	podA := secondServer(t, database)

	_, read := openTerminal(t, podA, device.id)
	for {
		msg, err := read()
		if err != nil {
			break // closed: no session
		}
		if msg["type"] == "output" {
			t.Fatalf("output reached pod A without routing: %v", msg)
		}
		if msg["type"] == "error" {
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, raw, err := device.conn.Read(ctx); err == nil && strings.Contains(string(raw), "terminal_start") {
		t.Fatalf("the device got a terminal without routing: %s", raw)
	}
}

// TestAReplayedForwardIsRefused: a stream whose nonce was used is not
// served.
func TestAReplayedForwardIsRefused(t *testing.T) {
	podB, _ := newInternalServer(t, nil)
	routing := &pipeRouting{nonces: map[string]bool{"0123456789abcdef": true}}
	podB.SetDeviceRouting(routing)
	here, there := net.Pipe()
	done := make(chan struct{})
	go func() {
		podB.ServeForwarded("a", map[string]string{"caller": `{"u":"admin","r":"admin"}`, "nonce": "0123456789abcdef"}, there)
		close(done)
	}()
	req, _ := http.NewRequest(http.MethodGet, "http://x/api/health", nil)
	go func() { _ = req.Write(here) }()
	buf := make([]byte, 1)
	_ = here.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := here.Read(buf); n > 0 {
		t.Fatal("a replayed stream got an answer")
	}
	<-done
}
