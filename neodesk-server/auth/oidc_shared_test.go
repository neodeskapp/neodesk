package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// sharedOIDCPair is two providers, as on two instances of one
// installation, on one KV.
func sharedOIDCPair(t *testing.T) (a, b *OIDCProvider) {
	t.Helper()
	cfg := func() *OIDCConfig {
		return &OIDCConfig{
			Enabled: true, ClientID: "console", RedirectURL: "http://localhost/callback",
			AuthorizationURL: "https://idp.example.com/authorize",
			// Nothing listens there: an exchange that gets past the state
			// fails at the token request
			TokenURL: "http://127.0.0.1:1/token",
		}
	}
	kv := cluster.NewMemKV()
	a, b = newTestOIDCProvider(t, cfg()), newTestOIDCProvider(t, cfg())
	a.SetKV(kv)
	b.SetKV(kv)
	return a, b
}

// The callback of a sign-in started on one instance finds its state on
// another, and the state works once on all of them.
func TestOIDCStateIsSharedAndUsedOnce(t *testing.T) {
	a, b := sharedOIDCPair(t)
	_, state, err := a.BuildAuthURL("/devices")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.GetReturnURL(state); got != "/devices" {
		t.Fatalf("the other instance reads return URL %q", got)
	}
	_, err = b.ExchangeCode(context.Background(), "code", state)
	if err == nil || strings.Contains(err.Error(), "invalid or expired state") {
		t.Fatalf("the callback on the other instance: %v", err)
	}
	_, err = a.ExchangeCode(context.Background(), "code", state)
	if err == nil || !strings.Contains(err.Error(), "invalid or expired state") {
		t.Errorf("a state used on one instance worked again on another: %v", err)
	}
}

// A one-time code for the panel stored on one instance is exchanged on
// another, once.
func TestOIDCAuthCodeIsSharedAndUsedOnce(t *testing.T) {
	a, b := sharedOIDCPair(t)
	code, err := a.StoreAuthCodeForState("jwt", "alice", "admin", "/", "", "st")
	if err != nil {
		t.Fatal(err)
	}
	got := b.ConsumeAuthCode(code)
	if got == nil || got.Username != "alice" || got.Token != "jwt" || got.State != "st" {
		t.Fatalf("the other instance got %+v", got)
	}
	if a.ConsumeAuthCode(code) != nil {
		t.Error("a code taken on one instance worked again on another")
	}
}

// A desktop client starts on one instance, the callback completes it on
// another, and the poll on either hands it out once.
func TestOIDCClientPendingIsSharedAndUsedOnce(t *testing.T) {
	a, b := sharedOIDCPair(t)
	_, code, err := a.BuildClientAuthURL("DEV1", "uuid-1", ClientDeviceInfo{Name: "pc"})
	if err != nil {
		t.Fatal(err)
	}
	if !b.CompleteClientPending(code, 7, "alice", "admin") {
		t.Fatal("the other instance could not complete the start")
	}
	peek := a.PeekClientPending(code)
	if peek == nil || !peek.Authed || peek.Username != "alice" || peek.ClientID != "DEV1" {
		t.Fatalf("the first instance sees %+v", peek)
	}
	if got := b.ConsumeClientPending(code); got == nil || got.UserID != 7 {
		t.Fatalf("consume on the other instance = %+v", got)
	}
	if a.ConsumeClientPending(code) != nil {
		t.Error("a client sign-in handed out on one instance was handed out again on another")
	}
}
