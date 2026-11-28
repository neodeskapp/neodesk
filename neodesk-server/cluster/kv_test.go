package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runKVSuite is the behaviour every KV has, in memory or in Valkey.
func runKVSuite(t *testing.T, newKV func(t *testing.T) KV) {
	ctx := context.Background()

	t.Run("set get del", func(t *testing.T) {
		kv := newKV(t)
		if _, ok, _ := kv.Get(ctx, "a"); ok {
			t.Fatal("empty store has a value")
		}
		if err := kv.Set(ctx, "a", "1", time.Minute); err != nil {
			t.Fatal(err)
		}
		if v, ok, err := kv.Get(ctx, "a"); !ok || v != "1" || err != nil {
			t.Fatalf("get = %q %v %v", v, ok, err)
		}
		_ = kv.Del(ctx, "a")
		if _, ok, _ := kv.Get(ctx, "a"); ok {
			t.Fatal("value outlived Del")
		}
	})

	t.Run("expiry", func(t *testing.T) {
		kv := newKV(t)
		_ = kv.Set(ctx, "short", "x", 150*time.Millisecond)
		if _, ok, _ := kv.Get(ctx, "short"); !ok {
			t.Fatal("value gone at once")
		}
		time.Sleep(400 * time.Millisecond)
		if _, ok, _ := kv.Get(ctx, "short"); ok {
			t.Fatal("value outlived its TTL")
		}
	})

	t.Run("setnx keeps the first and says who holds it", func(t *testing.T) {
		kv := newKV(t)
		cur, set, err := kv.SetNX(ctx, "meet", "pod-a", time.Minute)
		if err != nil || !set || cur != "pod-a" {
			t.Fatalf("first SetNX = %q %v %v", cur, set, err)
		}
		cur, set, _ = kv.SetNX(ctx, "meet", "pod-b", time.Minute)
		if set || cur != "pod-a" {
			t.Fatalf("second SetNX = %q %v, want pod-a held", cur, set)
		}
	})

	t.Run("setnx races have one winner", func(t *testing.T) {
		kv := newKV(t)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, set, _ := kv.SetNX(ctx, "race", string(rune('a'+i)), time.Minute); set {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d winners", wins.Load())
		}
	})

	t.Run("delif removes only the named value", func(t *testing.T) {
		kv := newKV(t)
		_ = kv.Set(ctx, "owner", "pod-b", time.Minute)
		_ = kv.DelIf(ctx, "owner", "pod-a")
		if v, ok, _ := kv.Get(ctx, "owner"); !ok || v != "pod-b" {
			t.Fatal("DelIf removed another pod's value")
		}
		_ = kv.DelIf(ctx, "owner", "pod-b")
		if _, ok, _ := kv.Get(ctx, "owner"); ok {
			t.Fatal("DelIf kept its own value")
		}
	})

	t.Run("take hands a value out once", func(t *testing.T) {
		kv := newKV(t)
		if _, ok, err := kv.Take(ctx, "code"); ok || err != nil {
			t.Fatalf("take of nothing = %v %v", ok, err)
		}
		_ = kv.Set(ctx, "code", "secret", time.Minute)
		if v, ok, err := kv.Take(ctx, "code"); !ok || v != "secret" || err != nil {
			t.Fatalf("take = %q %v %v", v, ok, err)
		}
		if _, ok, _ := kv.Take(ctx, "code"); ok {
			t.Fatal("a value was taken twice")
		}
		if _, ok, _ := kv.Get(ctx, "code"); ok {
			t.Fatal("value outlived Take")
		}
	})

	t.Run("take races have one winner", func(t *testing.T) {
		kv := newKV(t)
		_ = kv.Set(ctx, "once", "v", time.Minute)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ok, _ := kv.Take(ctx, "once"); ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d winners", wins.Load())
		}
	})

	t.Run("take skips an expired value", func(t *testing.T) {
		kv := newKV(t)
		_ = kv.Set(ctx, "old", "x", 100*time.Millisecond)
		time.Sleep(300 * time.Millisecond)
		if _, ok, _ := kv.Take(ctx, "old"); ok {
			t.Fatal("an expired value was taken")
		}
	})

	t.Run("use once", func(t *testing.T) {
		kv := newKV(t)
		at := time.Unix(1_800_000_000, 0)
		if ok, err := UseOnce(ctx, kv, "n", at, time.Minute); !ok || err != nil {
			t.Fatalf("first use = %v %v", ok, err)
		}
		if ok, _ := UseOnce(ctx, kv, "n", at.Add(time.Minute), time.Minute); ok {
			t.Fatal("a use within ttl worked again")
		}
		if ok, _ := UseOnce(ctx, kv, "n", at.Add(time.Minute+time.Millisecond), time.Minute); !ok {
			t.Fatal("a use older than ttl still blocks")
		}
		if ok, _ := UseOnce(ctx, kv, "n", at.Add(time.Minute+2*time.Millisecond), time.Minute); ok {
			t.Fatal("the renewed use does not count")
		}
	})
}

func TestLocalCap(t *testing.T) {
	c := NewLocalCap(2)
	now := time.Unix(1_800_000_000, 0)
	if !c.Add("a", now, now.Add(time.Minute)) || !c.Add("b", now, now.Add(2*time.Minute)) {
		t.Fatal("room below the cap was refused")
	}
	if c.Add("c", now.Add(30*time.Second), now.Add(time.Hour)) {
		t.Fatal("a key over the cap was counted")
	}
	if !c.Add("c", now.Add(time.Minute), now.Add(time.Hour)) {
		t.Fatal("an expired key made no room")
	}
	c.Remove("b")
	if c.Len() != 1 {
		t.Fatalf("len = %d after Remove", c.Len())
	}
}

func TestMemKV(t *testing.T) {
	runKVSuite(t, func(*testing.T) KV { return NewMemKV() })
}

func TestValkeyKV(t *testing.T) {
	runKVSuite(t, func(t *testing.T) KV { return NewValkeyKV(testValkey(t)) })
}
