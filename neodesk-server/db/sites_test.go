package db

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// siteTime is a time both databases store exactly (whole seconds, UTC).
func siteTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestSiteCreateListUpdate(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		older := &Site{OrgID: "org-a", Name: "Berlin", VPNGroup: "wan", CreatedAt: siteTime("2026-01-01T10:00:00Z")}
		newer := &Site{OrgID: "org-a", Name: "Hamburg", CreatedAt: siteTime("2026-01-02T10:00:00Z")}
		noOrg := &Site{Name: "Lab", CreatedAt: siteTime("2026-01-03T10:00:00Z")}
		for _, s := range []*Site{newer, older, noOrg} {
			if err := store.CreateSite(s); err != nil {
				t.Fatal(err)
			}
			if s.ID == "" {
				t.Fatal("CreateSite gave no id")
			}
		}

		got, err := store.ListSites("org-a", false)
		if err != nil || len(got) != 2 || got[0].ID != older.ID || got[1].ID != newer.ID {
			t.Fatalf("org-a sites %+v %v; want Berlin then Hamburg", got, err)
		}
		if got[0].VPNGroup != "wan" || got[0].OrgID != "org-a" || !got[0].CreatedAt.Equal(older.CreatedAt) {
			t.Errorf("Berlin read back as %+v", got[0])
		}
		if got, err := store.ListSites("", false); err != nil || len(got) != 1 || got[0].ID != noOrg.ID {
			t.Errorf("sites without org %+v %v; want only Lab", got, err)
		}
		if got, err := store.ListSites("", true); err != nil || len(got) != 3 {
			t.Errorf("all sites %+v %v; want 3", got, err)
		}

		older.Name, older.VPNGroup, older.OrgID = "Berlin HQ", "", "org-b"
		if err := store.UpdateSite(older); err != nil {
			t.Fatal(err)
		}
		s, err := store.GetSite(older.ID)
		if err != nil || s == nil || s.Name != "Berlin HQ" || s.VPNGroup != "" || s.OrgID != "org-a" {
			t.Errorf("after update %+v %v; want the new name and VPN group, the organization kept", s, err)
		}
		if err := store.UpdateSite(&Site{ID: "missing", Name: "x"}); !errors.Is(err, ErrSiteNotFound) {
			t.Errorf("update of a missing site: %v", err)
		}
		if s, err := store.GetSite("missing"); err != nil || s != nil {
			t.Errorf("missing site: %+v %v", s, err)
		}
	})
}

func TestSiteNetworksReplace(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		a, b := &Site{Name: "A"}, &Site{Name: "B"}
		for _, s := range []*Site{a, b} {
			if err := store.CreateSite(s); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ReplaceSiteNetworks(a.ID, []SiteNetwork{
			{CIDR: "10.1.0.0/16", Kind: SiteNetworkPrivate},
			{CIDR: "203.0.113.0/24", Kind: SiteNetworkPublic},
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReplaceSiteNetworks(b.ID, []SiteNetwork{{CIDR: "10.2.0.0/16", Kind: SiteNetworkPrivate}}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReplaceSiteNetworks(a.ID, []SiteNetwork{{CIDR: "10.9.0.0/16", Kind: SiteNetworkPrivate}}); err != nil {
			t.Fatal(err)
		}
		got, err := store.ListSiteNetworks(a.ID)
		if err != nil || len(got) != 1 || got[0].CIDR != "10.9.0.0/16" || got[0].SiteID != a.ID || got[0].ID == "" {
			t.Errorf("networks of A %+v %v; want only 10.9.0.0/16", got, err)
		}
		all, err := store.ListAllSiteNetworks()
		if err != nil || len(all) != 2 {
			t.Fatalf("all networks %+v %v; want A's and B's", all, err)
		}
		bNets, _ := store.ListSiteNetworks(b.ID)
		if len(bNets) != 1 || bNets[0].CIDR != "10.2.0.0/16" || bNets[0].Kind != SiteNetworkPrivate {
			t.Errorf("networks of B %+v; want them untouched", bNets)
		}
		if err := store.ReplaceSiteNetworks(a.ID, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.ListSiteNetworks(a.ID); len(got) != 0 {
			t.Errorf("networks of A after replacing with none: %+v", got)
		}
	})
}

func TestRelayNodeLifecycle(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		a, b := &Site{Name: "A"}, &Site{Name: "B"}
		for _, s := range []*Site{a, b} {
			if err := store.CreateSite(s); err != nil {
				t.Fatal(err)
			}
		}
		n1 := &RelayNode{SiteID: a.ID, Name: "rack 1", PublicKey: "key-1", LANAddr: "10.1.0.5:21117", CreatedAt: siteTime("2026-02-01T00:00:00Z")}
		n2 := &RelayNode{SiteID: b.ID, Name: "rack 2", PublicKey: "key-2", CreatedAt: siteTime("2026-02-02T00:00:00Z")}
		for _, n := range []*RelayNode{n1, n2} {
			if err := store.CreateRelayNode(n); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CreateRelayNode(&RelayNode{SiteID: a.ID, PublicKey: "key-1"}); err == nil {
			t.Error("a second node with the same public key was stored")
		}

		got, err := store.GetRelayNode(n1.ID)
		if err != nil || got == nil || got.Name != "rack 1" || got.LANAddr != "10.1.0.5:21117" || got.LastSeen != nil || got.Revoked() {
			t.Fatalf("node 1 %+v %v", got, err)
		}
		if got, err := store.GetRelayNodeByKey("key-2"); err != nil || got == nil || got.ID != n2.ID {
			t.Errorf("node by key-2: %+v %v", got, err)
		}
		if got, err := store.GetRelayNodeByKey("key-3"); err != nil || got != nil {
			t.Errorf("node by unknown key: %+v %v", got, err)
		}

		seen := siteTime("2026-02-03T12:00:00Z")
		if err := store.UpdateRelayNodeState(n1.ID, "3.7.0", "10.1.0.6:21117", "198.51.100.7:21117", seen); err != nil {
			t.Fatal(err)
		}
		if err := store.RenameRelayNode(n1.ID, "rack one"); err != nil {
			t.Fatal(err)
		}
		got, _ = store.GetRelayNode(n1.ID)
		if got.Version != "3.7.0" || got.LANAddr != "10.1.0.6:21117" || got.PublicAddr != "198.51.100.7:21117" ||
			got.LastSeen == nil || !got.LastSeen.Equal(seen) || got.Name != "rack one" {
			t.Errorf("node 1 after state and rename: %+v", got)
		}

		first := siteTime("2026-02-04T00:00:00Z")
		if err := store.RevokeRelayNode(n1.ID, first); err != nil {
			t.Fatal(err)
		}
		if err := store.RevokeRelayNode(n1.ID, first.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, _ = store.GetRelayNode(n1.ID)
		if got.RevokedAt == nil || !got.RevokedAt.Equal(first) {
			t.Errorf("revoked at %v; want the first revocation %v", got.RevokedAt, first)
		}

		for _, err := range []error{
			store.UpdateRelayNodeState("missing", "", "", "", seen),
			store.RenameRelayNode("missing", "x"),
			store.RevokeRelayNode("missing", seen),
		} {
			if !errors.Is(err, ErrSiteNotFound) {
				t.Errorf("write to a missing node: %v", err)
			}
		}

		if all, err := store.ListRelayNodes(""); err != nil || len(all) != 2 || all[0].ID != n1.ID || all[1].ID != n2.ID {
			t.Errorf("all nodes %+v %v", all, err)
		}
		if onB, err := store.ListRelayNodes(b.ID); err != nil || len(onB) != 1 || onB[0].ID != n2.ID {
			t.Errorf("nodes of B %+v %v", onB, err)
		}
	})
}

func TestDeleteSiteCascadesToNodesTokensPinsAndReachability(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		type siteRows struct {
			site *Site
			node *RelayNode
		}
		var rows []siteRows
		for _, name := range []string{"gone", "kept"} {
			s := &Site{Name: name}
			if err := store.CreateSite(s); err != nil {
				t.Fatal(err)
			}
			n := &RelayNode{SiteID: s.ID, PublicKey: "key-" + name}
			if err := store.CreateRelayNode(n); err != nil {
				t.Fatal(err)
			}
			if err := store.ReplaceSiteNetworks(s.ID, []SiteNetwork{{CIDR: "10.0.0.0/8", Kind: SiteNetworkPrivate}}); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateSiteEnrollmentToken(&SiteEnrollmentToken{TokenHash: "hash-" + name, SiteID: s.ID,
				ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetSitePin(SitePinDevice, "DEV-"+name, s.ID); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, siteRows{s, n})
		}
		gone, kept := rows[0], rows[1]
		for _, r := range []SiteReachability{
			{FromNode: gone.node.ID, ToNode: kept.node.ID, Via: "public", OK: true},
			{FromNode: kept.node.ID, ToNode: gone.node.ID, Via: "public", OK: true},
			{FromNode: kept.node.ID, ToNode: "other", Via: "public", OK: true},
		} {
			if err := store.UpsertSiteReachability(&r); err != nil {
				t.Fatal(err)
			}
		}

		if err := store.DeleteSite(gone.site.ID); err != nil {
			t.Fatal(err)
		}

		if s, _ := store.GetSite(gone.site.ID); s != nil {
			t.Error("the site is still there")
		}
		if n, _ := store.GetRelayNode(gone.node.ID); n != nil {
			t.Error("its node is still there")
		}
		if nets, _ := store.ListSiteNetworks(gone.site.ID); len(nets) != 0 {
			t.Errorf("its networks are still there: %+v", nets)
		}
		if toks, _ := store.ListSiteEnrollmentTokens(gone.site.ID); len(toks) != 0 {
			t.Errorf("its tokens are still there: %+v", toks)
		}
		if pin, _ := store.GetSitePin(SitePinDevice, "DEV-gone"); pin != "" {
			t.Errorf("its pin is still there: %q", pin)
		}
		reach, _ := store.ListSiteReachability()
		if len(reach) != 1 || reach[0].FromNode != kept.node.ID || reach[0].ToNode != "other" {
			t.Errorf("reachability %+v; want only the row without the deleted node", reach)
		}

		if s, _ := store.GetSite(kept.site.ID); s == nil {
			t.Error("the other site went too")
		}
		if n, _ := store.GetRelayNode(kept.node.ID); n == nil {
			t.Error("the other site's node went too")
		}
		if nets, _ := store.ListSiteNetworks(kept.site.ID); len(nets) != 1 {
			t.Errorf("the other site's networks: %+v", nets)
		}
		if toks, _ := store.ListSiteEnrollmentTokens(kept.site.ID); len(toks) != 1 {
			t.Errorf("the other site's tokens: %+v", toks)
		}
		if pin, _ := store.GetSitePin(SitePinDevice, "DEV-kept"); pin != kept.site.ID {
			t.Errorf("the other site's pin: %q", pin)
		}
	})
}

func TestSiteEnrollmentTokenSingleUse(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		site := &Site{Name: "A"}
		if err := store.CreateSite(site); err != nil {
			t.Fatal(err)
		}
		created := siteTime("2026-03-01T08:00:00Z")
		tok := &SiteEnrollmentToken{TokenHash: "abc123", SiteID: site.ID, CreatedBy: "admin",
			CreatedAt: created, ExpiresAt: created.Add(24 * time.Hour)}
		if err := store.CreateSiteEnrollmentToken(tok); err != nil {
			t.Fatal(err)
		}
		now := created.Add(time.Hour)
		got, err := store.ConsumeSiteEnrollmentToken("abc123", "node-1", now)
		if err != nil || got == nil || got.SiteID != site.ID || got.UsedByNode != "node-1" ||
			got.UsedAt == nil || !got.UsedAt.Equal(now) || got.CreatedBy != "admin" {
			t.Fatalf("consume: %+v %v", got, err)
		}
		if got, err := store.ConsumeSiteEnrollmentToken("abc123", "node-2", now.Add(time.Minute)); !errors.Is(err, ErrSiteTokenInvalid) || got != nil {
			t.Errorf("second use: %+v %v; want ErrSiteTokenInvalid", got, err)
		}
		if _, err := store.ConsumeSiteEnrollmentToken("nope", "node-2", now); !errors.Is(err, ErrSiteTokenInvalid) {
			t.Errorf("unknown token: %v", err)
		}
		list, err := store.ListSiteEnrollmentTokens(site.ID)
		if err != nil || len(list) != 1 || list[0].UsedByNode != "node-1" || !list[0].ExpiresAt.Equal(tok.ExpiresAt) {
			t.Errorf("tokens %+v %v; want the one used by node-1", list, err)
		}
	})
}

func TestSiteEnrollmentTokenExpiry(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		site := &Site{Name: "A"}
		if err := store.CreateSite(site); err != nil {
			t.Fatal(err)
		}
		expires := siteTime("2026-03-02T08:00:00Z")
		for _, h := range []string{"late", "early"} {
			if err := store.CreateSiteEnrollmentToken(&SiteEnrollmentToken{TokenHash: h, SiteID: site.ID, ExpiresAt: expires}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.ConsumeSiteEnrollmentToken("late", "node-1", expires); !errors.Is(err, ErrSiteTokenInvalid) {
			t.Errorf("use at the expiry time: %v; want ErrSiteTokenInvalid", err)
		}
		if _, err := store.ConsumeSiteEnrollmentToken("late", "node-1", expires.Add(time.Hour)); !errors.Is(err, ErrSiteTokenInvalid) {
			t.Errorf("use after expiry: %v; want ErrSiteTokenInvalid", err)
		}
		if _, err := store.ConsumeSiteEnrollmentToken("early", "node-1", expires.Add(-time.Second)); err != nil {
			t.Errorf("use a second before expiry: %v", err)
		}
	})
}

func TestSiteEnrollmentTokenConcurrentConsume(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		site := &Site{Name: "A"}
		if err := store.CreateSite(site); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateSiteEnrollmentToken(&SiteEnrollmentToken{TokenHash: "race", SiteID: site.ID,
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		const tries = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]error, tries)
		for i := 0; i < tries; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, results[i] = store.ConsumeSiteEnrollmentToken("race", "node-"+string(rune('a'+i)), time.Now())
			}(i)
		}
		close(start)
		wg.Wait()
		wins := 0
		for _, err := range results {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, ErrSiteTokenInvalid):
				t.Errorf("a loser got %v; want ErrSiteTokenInvalid", err)
			}
		}
		if wins != 1 {
			t.Errorf("%d of %d tries used the token; want exactly one", wins, tries)
		}
	})
}

func TestSitePinSetGetList(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		if pin, err := store.GetSitePin(SitePinDevice, "DEV1"); err != nil || pin != "" {
			t.Errorf("no pin yet: %q %v", pin, err)
		}
		if err := store.SetSitePin(SitePinDevice, "DEV1", "site-a"); err != nil {
			t.Fatal(err)
		}
		if err := store.SetSitePin(SitePinFolder, "7", "site-b"); err != nil {
			t.Fatal(err)
		}
		if err := store.SetSitePin(SitePinDevice, "DEV1", "site-c"); err != nil {
			t.Fatal(err)
		}
		if pin, err := store.GetSitePin(SitePinDevice, "DEV1"); err != nil || pin != "site-c" {
			t.Errorf("device pin %q %v; want site-c", pin, err)
		}
		if pin, _ := store.GetSitePin(SitePinDevice, "7"); pin != "" {
			t.Errorf("a folder pin answered for a device with the same ref: %q", pin)
		}
		pins, err := store.ListSitePins()
		if err != nil || len(pins) != 2 {
			t.Fatalf("pins %+v %v", pins, err)
		}
		if err := store.SetSitePin(SitePinDevice, "DEV1", ""); err != nil {
			t.Fatal(err)
		}
		if pin, _ := store.GetSitePin(SitePinDevice, "DEV1"); pin != "" {
			t.Errorf("pin after removal: %q", pin)
		}
		if pins, _ := store.ListSitePins(); len(pins) != 1 || pins[0] != (SitePin{Kind: SitePinFolder, Ref: "7", SiteID: "site-b"}) {
			t.Errorf("pins after removal %+v; want the folder pin", pins)
		}
	})
}

func TestSiteReachabilityUpsert(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		at := siteTime("2026-04-01T00:00:00Z")
		if err := store.UpsertSiteReachability(&SiteReachability{FromNode: "a", ToNode: "b", Via: "public", OK: true, RTTMs: 12, CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
		later := at.Add(time.Minute)
		if err := store.UpsertSiteReachability(&SiteReachability{FromNode: "a", ToNode: "b", Via: "public", OK: false, RTTMs: 0, CheckedAt: later}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertSiteReachability(&SiteReachability{FromNode: "a", ToNode: "b", Via: "private", OK: true, RTTMs: 3, CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
		got, err := store.ListSiteReachability()
		if err != nil || len(got) != 2 {
			t.Fatalf("reachability %+v %v; want one row per way", got, err)
		}
		if got[0].Via != "private" || !got[0].OK || got[0].RTTMs != 3 {
			t.Errorf("private row %+v", got[0])
		}
		if got[1].Via != "public" || got[1].OK || got[1].RTTMs != 0 || !got[1].CheckedAt.Equal(later) {
			t.Errorf("public row %+v; want the second probe's result", got[1])
		}
	})
}

func TestSiteConnectionSessionRelayVia(t *testing.T) {
	freshStores(t, func(t *testing.T, store Database) {
		if _, err := store.StartPeerConnectionSession(&PeerConnectionSession{HostID: "HOST1", PeerID: "PEER1", RelayUUID: "uuid-1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.StartPeerConnectionSession(&PeerConnectionSession{HostID: "HOST1", PeerID: "PEER2", RelayUUID: "uuid-2"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetConnectionSessionRelayVia("uuid-1", "node:n1"); err != nil {
			t.Fatal(err)
		}
		sessions, err := store.ListPeerConnectionSessions("HOST1", 10)
		if err != nil || len(sessions) != 2 {
			t.Fatalf("sessions %+v %v", sessions, err)
		}
		for _, s := range sessions {
			want := ""
			if s.RelayUUID == "uuid-1" {
				want = "node:n1"
			}
			if s.RelayVia != want {
				t.Errorf("session %s relayed via %q; want %q", s.RelayUUID, s.RelayVia, want)
			}
		}
	})
}
