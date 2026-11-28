package cluster

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// UseOnce marks key as used at now for ttl and reports whether it was
// free: true the first time, false while an earlier use is younger than
// ttl. The value is the time of the use, so a use older than ttl by the
// caller's clock counts as gone even before the store drops it, and the
// key can be used again. Of several callers on any instance only one gets
// true.
func UseOnce(ctx context.Context, kv KV, key string, now time.Time, ttl time.Duration) (bool, error) {
	at := strconv.FormatInt(now.UnixNano(), 10)
	cur, set, err := kv.SetNX(ctx, key, at, ttl)
	if err != nil || set {
		return set, err
	}
	used, perr := strconv.ParseInt(cur, 10, 64)
	if perr != nil || now.Sub(time.Unix(0, used)) <= ttl {
		return false, nil
	}
	// The earlier use is older than ttl: drop exactly it and try again
	if err := kv.DelIf(ctx, key, cur); err != nil {
		return false, err
	}
	_, set, err = kv.SetNX(ctx, key, at, ttl)
	return set, err
}

// LocalCap counts the keys this instance put into a KV that are still
// alive, so a store can cap what one instance adds. A KV cannot count its
// keys, so each instance counts its own: a key another instance takes
// stays counted here until its time is up.
type LocalCap struct {
	mu    sync.Mutex
	max   int
	until map[string]time.Time
}

// NewLocalCap makes a LocalCap for max keys.
func NewLocalCap(max int) *LocalCap {
	return &LocalCap{max: max, until: map[string]time.Time{}}
}

// Add counts key until the time until. When max keys are counted it first
// drops those whose time is up at now, and is false when that makes no
// room.
func (c *LocalCap) Add(key string, now, until time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.until) >= c.max {
		for k, u := range c.until {
			if !now.Before(u) {
				delete(c.until, k)
			}
		}
	}
	if len(c.until) >= c.max {
		return false
	}
	c.until[key] = until
	return true
}

// Remove stops counting key.
func (c *LocalCap) Remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.until, key)
}

// Len is the number of keys counted.
func (c *LocalCap) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.until)
}
