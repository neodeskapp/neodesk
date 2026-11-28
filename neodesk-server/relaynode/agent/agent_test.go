package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/internal/fakestore"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/wire"
)

// logBuf collects every log line of a test: the hub's, the agent's and
// the standard logger's.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) Logf(format string, args ...any) { fmt.Fprintf(l, format+"\n", args...) }

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type env struct {
	t        *testing.T
	hub      *hub.Hub
	store    *fakestore.Store
	rec      *fakestore.Recorder
	pin      string
	logs     *logBuf
	certDir  string
	rootCtx  context.Context
	shutdown context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	logs := &logBuf{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	dir := t.TempDir()
	cert, pin, err := hub.LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := fakestore.New()
	rec := fakestore.NewRecorder()
	h, err := hub.New(hub.Config{Addr: "127.0.0.1:0", Cert: cert, ServerPK: "server-pk", Store: st, Events: rec, Audit: rec, Logf: logs.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &env{t: t, hub: h, store: st, rec: rec, pin: pin, logs: logs, certDir: dir, rootCtx: ctx, shutdown: cancel}
}

func (e *env) join(token string) string {
	e.store.AddToken(token, "site-1")
	return wire.Join{Server: e.hub.Addr().String(), Pin: e.pin, Token: token}.Encode()
}

func (e *env) agent(dir, join string) *Agent {
	e.t.Helper()
	a, err := New(Config{Join: join, DataDir: dir, PublicAddr: "203.0.113.5:21117", Version: "test", Logf: e.logs.Logf,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond})
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// run keeps a's control connection until the test ends.
func (e *env) run(a *Agent) {
	ctx, cancel := context.WithCancel(e.rootCtx)
	done := make(chan struct{})
	go func() {
		a.RunControl(ctx)
		close(done)
	}()
	e.t.Cleanup(func() {
		cancel()
		<-done
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 3 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// once runs one control connection of a, at most 3 s, and returns how it ended.
func (e *env) once(a *Agent) error {
	ctx, cancel := context.WithTimeout(e.rootCtx, 3*time.Second)
	defer cancel()
	_, err := a.session(ctx)
	return err
}

func refusal(err error) string {
	var r *RefusedError
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func TestEnrollmentTokenWorksOnceAndTheNodeComesBackWithoutIt(t *testing.T) {
	e := newEnv(t)
	join := e.join("tok-once")
	dir := t.TempDir()
	a := e.agent(dir, join)
	e.run(a)
	waitFor(t, "first node online", func() bool { return a.NodeID() != "" && e.hub.Online(a.NodeID()) })
	id := a.NodeID()
	n, ok := e.store.Node(id)
	if !ok || n.PublicKey != a.PublicKey() || n.SiteID != "site-1" || n.PublicAddr != "203.0.113.5:21117" {
		t.Fatalf("stored node %+v, agent key %s", n, a.PublicKey())
	}
	if ev, err := e.rec.Wait("relay_node.enrolled", time.Second); err != nil || ev.Fields["node_id"] != id {
		t.Fatalf("enrollment audit: %+v %v", ev, err)
	}

	// The same string on another machine (another key): the token is used
	other := e.agent(t.TempDir(), join)
	if err := e.once(other); refusal(err) != wire.CodeBadToken {
		t.Fatalf("second use: got %v, want bad_token", err)
	}

	// A restart with NODE_JOIN still set: no token, same node
	e.shutdown()
	waitFor(t, "first agent offline", func() bool { return !e.hub.Online(id) })
	e.rootCtx, e.shutdown = context.WithCancel(context.Background())
	t.Cleanup(e.shutdown)
	again := e.agent(dir, join)
	if again.join != nil {
		t.Fatal("the restart wants to enroll again")
	}
	welcomed := make(chan error, 1)
	go func() {
		_, err := again.session(e.rootCtx)
		welcomed <- err
	}()
	waitFor(t, "restarted node online", func() bool { return e.hub.Online(id) })
	if again.NodeID() != id || e.store.Consumed() != 1 || e.store.Nodes() != 1 {
		t.Fatalf("restart: node %s (want %s), tokens used %d, nodes %d", again.NodeID(), id, e.store.Consumed(), e.store.Nodes())
	}
}

func TestAWrongPinIsRefusedBeforeTheTokenIsSent(t *testing.T) {
	e := newEnv(t)
	e.store.AddToken("tok-pin", "site-1")
	sum := sha256.Sum256([]byte("another certificate"))
	join := wire.Join{Server: e.hub.Addr().String(), Pin: hex.EncodeToString(sum[:]), Token: "tok-pin"}.Encode()
	a := e.agent(t.TempDir(), join)
	err := e.once(a)
	if !errors.Is(err, wire.ErrPinMismatch) {
		t.Fatalf("got %v, want ErrPinMismatch", err)
	}
	if e.store.Consumed() != 0 || e.store.Nodes() != 0 {
		t.Fatalf("token used %d times, %d nodes", e.store.Consumed(), e.store.Nodes())
	}
}

func TestARevokedNodeIsClosedDropsItsTicketsAndIsRefused(t *testing.T) {
	e := newEnv(t)
	a := e.agent(t.TempDir(), e.join("tok-rev"))
	first := make(chan error, 1)
	go func() {
		err := e.once(a)
		first <- err
	}()
	waitFor(t, "node online", func() bool { return a.NodeID() != "" && e.hub.Online(a.NodeID()) })
	id := a.NodeID()
	if err := e.hub.Tickets(id).AuthorizePair(context.Background(), "u-unclaimed", "111", "222", 0, "", time.Now().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	e.store.Revoke(id)
	e.hub.Disconnect(id)
	select {
	case err := <-first:
		if refusal(err) != wire.CodeRevoked {
			t.Fatalf("connection ended with %v, want revoked", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the revoked connection stays open")
	}
	if a.Registry().Claim("u-unclaimed") {
		t.Fatal("a ticket pushed before the revocation can still be claimed")
	}
	if err := e.once(a); refusal(err) != wire.CodeRevoked {
		t.Fatalf("reconnect: got %v, want revoked", err)
	}
	if ev, err := e.rec.Wait("relay_node.revoked", time.Second); err != nil || ev.Fields["node_id"] != id {
		t.Fatalf("revocation audit: %+v %v", ev, err)
	}
}

func TestANodeWhoseSiteIsGoneIsRefused(t *testing.T) {
	e := newEnv(t)
	a := e.agent(t.TempDir(), e.join("tok-site"))
	ctx, cancel := context.WithCancel(e.rootCtx)
	go a.session(ctx)
	waitFor(t, "node online", func() bool { return a.NodeID() != "" && e.hub.Online(a.NodeID()) })
	cancel()
	waitFor(t, "node offline", func() bool { return !e.hub.Online(a.NodeID()) })

	e.store.DeleteSite("site-1")
	if err := e.once(a); refusal(err) != wire.CodeSiteGone {
		t.Fatalf("got %v, want site_gone", err)
	}
}

func TestAPushedTicketLetsTheNodesRelayPairTheSession(t *testing.T) {
	e := newEnv(t)
	a := e.agent(t.TempDir(), e.join("tok-ticket"))
	e.run(a)
	waitFor(t, "node online", func() bool { return a.NodeID() != "" && e.hub.Online(a.NodeID()) })
	id := a.NodeID()
	store := e.hub.Tickets(id)

	if a.Registry().Claim("u-1") {
		t.Fatal("claimed before any ticket")
	}
	if err := store.AuthorizePair(context.Background(), "u-1", "111", "222", 9, "handle", time.Now().Add(30*time.Second)); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !a.Registry().Admitted("u-1", "111", "222") {
		t.Fatal("the node's registry has no ticket for the pair")
	}
	if !a.Registry().Claim("u-1") || !a.Registry().Claim("u-1") || a.Registry().Claim("u-1") {
		t.Fatal("the ticket does not admit exactly the two sides")
	}
	// The same uuid for another pair: the node says no
	if err := store.AuthorizePair(context.Background(), "u-1", "111", "333", 0, "", time.Now().Add(30*time.Second)); !errors.Is(err, hub.ErrRejected) {
		t.Fatalf("conflicting ticket: got %v, want ErrRejected", err)
	}

	a.SessionStarted("u-1")
	if ev, err := e.rec.Wait("started", 2*time.Second); err != nil || ev.UUID != "u-1" || ev.Node != id {
		t.Fatalf("started: %+v %v", ev, err)
	}
	waitFor(t, "active session", func() bool { return e.hub.ActiveSessions(id) == 1 })
	if live, ok := store.Live("u-1"); !ok || live.Account != 9 || live.ConsoleSession != "handle" {
		t.Fatalf("Live on central: %+v %v", live, ok)
	}
	a.SessionEnded("u-1", 1000, 2000)
	if ev, err := e.rec.Wait("ended", 2*time.Second); err != nil || ev.Up != 1000 || ev.Down != 2000 {
		t.Fatalf("ended: %+v %v", ev, err)
	}
	waitFor(t, "no active session", func() bool { return e.hub.ActiveSessions(id) == 0 })
}

func TestReportsWhileOfflineArriveAfterTheNextWelcome(t *testing.T) {
	e := newEnv(t)
	a := e.agent(t.TempDir(), e.join("tok-queue"))
	a.SessionStarted("u-q")
	a.SessionEnded("u-q", 3, 4)
	e.run(a)
	if ev, err := e.rec.Wait("ended", 3*time.Second); err != nil || ev.UUID != "u-q" || ev.Up != 3 || ev.Down != 4 {
		t.Fatalf("queued ended: %+v %v", ev, err)
	}
}

func TestSecretsStayOutOfTheLogs(t *testing.T) {
	e := newEnv(t)
	const token = "tok-SECRET-4f9a2c"
	join := e.join(token)
	dir := t.TempDir()
	a := e.agent(dir, join)
	e.run(a)
	waitFor(t, "node online", func() bool { return a.NodeID() != "" && e.hub.Online(a.NodeID()) })
	// A used token, a wrong pin and a revocation log refusals too
	other := e.agent(t.TempDir(), join)
	e.once(other)
	bad := wire.Join{Server: e.hub.Addr().String(), Pin: strings.Repeat("ab", 32), Token: token}.Encode()
	if _, err := New(Config{Join: bad, DataDir: t.TempDir(), Logf: e.logs.Logf}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Join: "nd1.broken-" + token, DataDir: t.TempDir(), Logf: e.logs.Logf}); err == nil {
		t.Fatal("a broken join string was accepted")
	} else {
		e.logs.Logf("%v", err)
	}
	e.store.Revoke(a.NodeID())
	e.hub.Disconnect(a.NodeID())
	waitFor(t, "revoked node offline", func() bool { return !e.hub.Online(a.NodeID()) })
	time.Sleep(100 * time.Millisecond) // the agent logs its retry

	secrets := map[string]string{"token": token, "join string": join, "join payload": join[len(wire.JoinPrefix):]}
	for name, file := range map[string]string{
		"node key": filepath.Join(dir, KeyFileName), "central key": filepath.Join(e.certDir, hub.KeyFileName),
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if len(line) > 16 && !strings.HasPrefix(line, "-----") {
				secrets[fmt.Sprintf("%s line %d", name, i)] = line
			}
		}
	}
	out := e.logs.String()
	if !strings.Contains(out, "bad_token") || !strings.Contains(out, "revoked") {
		t.Fatalf("the refusals were not logged, so the check proves nothing:\n%s", out)
	}
	for name, s := range secrets {
		if strings.Contains(out, s) {
			t.Fatalf("the log shows the %s:\n%s", name, out)
		}
	}
}
