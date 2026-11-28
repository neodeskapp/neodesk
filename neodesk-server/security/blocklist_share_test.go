package security

import (
	"testing"

	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

// TestABlockOnOneInstanceReachesAnother: a change reports itself, and the
// other instance's Reload picks the entries up from the shared store, and
// drops what was removed.
func TestABlockOnOneInstanceReachesAnother(t *testing.T) {
	store := &memStore{rows: map[[2]string]db.BlocklistEntry{}}
	a, b := NewBlocklist(), NewBlocklist()
	if err := a.UseStore(store); err != nil {
		t.Fatal(err)
	}
	if err := b.UseStore(store); err != nil {
		t.Fatal(err)
	}
	changes := 0
	a.OnChange(func() {
		changes++
		if err := b.Reload(); err != nil {
			t.Fatal(err)
		}
	})

	if err := a.BlockID("123456789", "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.BlockCIDR("10.9.0.0/16", "test"); err != nil {
		t.Fatal(err)
	}
	if !b.IsIDBlocked("123456789") || !b.IsIPBlocked("10.9.1.2") {
		t.Fatal("the other instance does not block what this one blocked")
	}
	a.UnblockID("123456789")
	if b.IsIDBlocked("123456789") {
		t.Fatal("the other instance still blocks what this one unblocked")
	}
	if changes != 3 {
		t.Fatalf("%d changes reported, want 3", changes)
	}
	// Reload itself is no change: no loop between instances
	b.OnChange(func() { t.Fatal("Reload reported a change") })
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
}
