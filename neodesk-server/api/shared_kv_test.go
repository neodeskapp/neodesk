package api

import (
	"errors"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// sharedServers are two API servers, as two instances of one
// installation, whose short-lived stores share one KV (SetSharedKV).
func sharedServers(t *testing.T) (a, b *Server) {
	t.Helper()
	kv := cluster.NewMemKV()
	newServer := func(kv cluster.KV) *Server {
		s := &Server{clientTFASessions: newTFASessionStore()}
		s.oidcProvider = auth.NewOIDCProvider(&auth.OIDCConfig{})
		t.Cleanup(s.oidcProvider.Close)
		s.SetSharedKV(kv)
		return s
	}
	return newServer(kv), newServer(kv)
}

// A pending second step of the client sign-in made on one instance is
// finished on another, once.
func TestSharedTFASession(t *testing.T) {
	a, b := sharedServers(t)
	a.clientTFASessions.put("secret", &tfaSession{username: "alice", userID: 3, clientID: "D1", createdAt: time.Now()})
	got := b.clientTFASessions.take("secret")
	if got == nil || got.username != "alice" || got.userID != 3 || got.clientID != "D1" {
		t.Fatalf("the other instance took %+v", got)
	}
	if a.clientTFASessions.take("secret") != nil {
		t.Error("a session taken on one instance was taken again on another")
	}
}

// A desktop sign-in through the console starts on one instance, is
// decided on another, and its token is handed out once.
func TestSharedConsoleLogin(t *testing.T) {
	a, b := sharedServers(t)
	now := time.Now()
	l, err := a.consoleLogins.start(consoleLogin{deviceID: "D", uuid: "u", deviceName: "pc"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := b.consoleLogins.view(l.request, now); !ok || v.deviceName != "pc" {
		t.Fatalf("the other instance sees %+v %v", v, ok)
	}
	if _, st := b.consoleLogins.poll(l.code, "D", "u", now); st != pollWaiting {
		t.Fatalf("poll before the decision = %v", st)
	}
	if _, err := b.consoleLogins.decide(l.request, true, 9, now, nil); err != nil {
		t.Fatalf("decide on the other instance: %v", err)
	}
	if _, err := a.consoleLogins.decide(l.request, false, 9, now, nil); !errors.Is(err, errConsoleLoginNotPending) {
		t.Errorf("a second decision on another instance: %v", err)
	}
	got, st := a.consoleLogins.poll(l.code, "D", "u", now)
	if st != pollAllowed || got.account != 9 {
		t.Fatalf("poll after the decision = %v %+v", st, got)
	}
	if _, st := b.consoleLogins.poll(l.code, "D", "u", now); st != pollUnknown {
		t.Errorf("a token handed out on one instance was handed out again: %v", st)
	}
}

// Two decisions at once on two instances: one wins.
func TestSharedConsoleLoginDecidedOnce(t *testing.T) {
	a, b := sharedServers(t)
	now := time.Now()
	l, err := a.consoleLogins.start(consoleLogin{deviceID: "D", uuid: "u"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// The approval's check runs while it holds the request: a decision on
	// the other instance meanwhile is refused
	var other error
	_, err = a.consoleLogins.decide(l.request, true, 1, now, func() error {
		_, other = b.consoleLogins.decide(l.request, false, 2, now, nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(other, errConsoleLoginNotPending) {
		t.Errorf("the decision during the check: %v", other)
	}
	if got, st := b.consoleLogins.poll(l.code, "D", "u", now); st != pollAllowed || got.account != 1 {
		t.Errorf("poll = %v %+v", st, got)
	}
}

// A saved sign-in and a quick support caller are used once per session
// on all instances.
func TestSharedSigninUses(t *testing.T) {
	a, b := sharedServers(t)
	now := time.Now()
	if !a.signinUses.claim("s1", now) {
		t.Fatal("the first use was refused")
	}
	if b.signinUses.claim("s1", now) {
		t.Error("a session used its sign-in again on another instance")
	}
	if !b.callerUses.claim("s2", now) {
		t.Fatal("the first caller check was refused")
	}
	if a.callerUses.claim("s2", now) {
		t.Error("a caller was vouched for again on another instance")
	}
	// The two kinds count apart
	if !a.callerUses.claim("s1", now) {
		t.Error("a used sign-in blocked the caller check of the session")
	}
}

// The nonces of signed requests are refused on another instance once
// used: the management requests, the enrollment proofs and the panel
// statements.
func TestSharedNonces(t *testing.T) {
	a, b := sharedServers(t)
	now := time.Now()
	for name, pick := range map[string]func(*Server) *onceKeys{
		"mgmt":       (*Server).mgmtNonceKeys,
		"enrollment": (*Server).enrollmentNonceKeys,
		"panel":      (*Server).panelNonceKeys,
	} {
		if !pick(a).use("dev:n1", now) {
			t.Fatalf("%s: the first use was refused", name)
		}
		if pick(b).use("dev:n1", now) {
			t.Errorf("%s: a nonce used on one instance worked on another", name)
		}
	}
}

// The single sign-on provider of the server gets the KV too: a code
// stored on one instance is exchanged on another, once.
func TestSharedOIDCProvider(t *testing.T) {
	a, b := sharedServers(t)
	code, err := a.oidcProvider.StoreAuthCode("jwt", "alice", "admin", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.oidcProvider.ConsumeAuthCode(code); got == nil {
		t.Fatal("the other instance has no code")
	}
	if a.oidcProvider.ConsumeAuthCode(code) != nil {
		t.Error("a code was exchanged twice")
	}
}
