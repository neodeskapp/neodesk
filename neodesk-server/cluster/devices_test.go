package cluster

import (
	"context"
	"testing"
)

func runDevicesSuite(t *testing.T, kv KV) {
	ctx := context.Background()
	a, b := NewDevices(kv, "pod-a"), NewDevices(kv, "pod-b")

	a.Hold("cdap", "DEV1")
	if got := b.Owner(ctx, "cdap", "DEV1"); got != "pod-a" {
		t.Fatalf("pod b sees owner %q, want pod-a", got)
	}
	if got := a.Owner(ctx, "cdap", "DEV1"); got != "" {
		t.Fatalf("pod a sees owner %q of its own device, want none", got)
	}
	if got := b.Owner(ctx, "mesh", "DEV1"); got != "" {
		t.Fatalf("another kind has owner %q", got)
	}

	// The device reconnects to pod b before pod a notices its socket is
	// gone: pod a must not mark it offline, and must not drop b's entry
	b.Hold("cdap", "DEV1")
	if a.Release("cdap", "DEV1") {
		t.Fatal("pod a may mark offline a device pod b holds now")
	}
	if got := a.Owner(ctx, "cdap", "DEV1"); got != "pod-b" {
		t.Fatalf("after pod a let go the owner is %q, want pod-b", got)
	}
	if !b.Release("cdap", "DEV1") {
		t.Fatal("pod b's own device should be its to mark offline")
	}
	if got := a.Owner(ctx, "cdap", "DEV1"); got != "" {
		t.Fatalf("released device still has owner %q", got)
	}
}

func TestMemDevices(t *testing.T) { runDevicesSuite(t, NewMemKV()) }

func TestValkeyDevices(t *testing.T) {
	v := testValkey(t)
	runDevicesSuite(t, NewValkeyKV(v))
}
