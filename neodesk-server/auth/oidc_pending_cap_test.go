package auth

import (
	"strings"
	"testing"
	"time"
)

func pendingCapProvider(t *testing.T) *OIDCProvider {
	t.Helper()
	return newTestOIDCProvider(t, &OIDCConfig{
		Enabled: true, ClientID: "console", RedirectURL: "http://localhost/callback",
		AuthorizationURL: "https://idp.example.com/authorize",
	})
}

// The sign-ins in progress are capped: when the cap is reached a new start
// is refused until entries expire, and expired entries make room (RV186).
func TestOIDCPendingSignInsAreCapped(t *testing.T) {
	p := pendingCapProvider(t)
	for i := 0; i < maxOIDCPending; i++ {
		if _, _, err := p.BuildClientAuthURL("DEV1", "uuid-1", ClientDeviceInfo{Name: "pc"}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, _, err := p.BuildAuthURL("/"); err == nil {
		t.Error("panel start beyond the cap was accepted")
	}
	if _, _, err := p.BuildClientAuthURL("DEV1", "uuid-1", ClientDeviceInfo{}); err == nil {
		t.Error("client start beyond the cap was accepted")
	}
	if p.stateCap.Len() > maxOIDCPending || p.clientCap.Len() > maxOIDCPending {
		t.Errorf("%d states and %d client entries, cap %d", p.stateCap.Len(), p.clientCap.Len(), maxOIDCPending)
	}
	later := time.Now().Add(time.Hour)
	p.now = func() time.Time { return later }
	if _, _, err := p.BuildClientAuthURL("DEV1", "uuid-1", ClientDeviceInfo{}); err != nil {
		t.Errorf("start after the entries expired: %v", err)
	}
}

// A client start with an overlong field is refused and stores nothing; a
// panel start with an overlong return address too (RV186).
func TestOIDCStartRefusesOverlongFields(t *testing.T) {
	p := pendingCapProvider(t)
	long := func(n int) string { return strings.Repeat("x", n) }
	for name, start := range map[string]func() error{
		"id":   func() error { _, _, err := p.BuildClientAuthURL(long(65), "u", ClientDeviceInfo{}); return err },
		"uuid": func() error { _, _, err := p.BuildClientAuthURL("d", long(129), ClientDeviceInfo{}); return err },
		"device name": func() error {
			_, _, err := p.BuildClientAuthURL("d", "u", ClientDeviceInfo{Name: long(129)})
			return err
		},
		"os": func() error { _, _, err := p.BuildClientAuthURL("d", "u", ClientDeviceInfo{OS: long(65)}); return err },
		"type": func() error {
			_, _, err := p.BuildClientAuthURL("d", "u", ClientDeviceInfo{Type: long(33)})
			return err
		},
		"return url": func() error { _, _, err := p.BuildAuthURL("/" + long(2048)); return err },
	} {
		if err := start(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p.stateCap.Len() != 0 || p.clientCap.Len() != 0 {
		t.Errorf("refused starts stored %d states and %d client entries", p.stateCap.Len(), p.clientCap.Len())
	}
}
