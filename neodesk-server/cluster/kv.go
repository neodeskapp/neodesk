package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// KV is a small store of short-lived values with an expiry, shared by the
// pods of an installation (ValkeyKV) or kept in the process (MemKV) when
// the server runs alone. Every value has a TTL. Keys are given without the
// installation's prefix; the store adds it.
type KV interface {
	// Set stores value under key for ttl.
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// SetNX stores value under key for ttl only if key is free, and
	// returns the value key holds afterwards and whether it was this one.
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (current string, set bool, err error)
	// Get returns the value of key; ok is false when there is none.
	Get(ctx context.Context, key string) (value string, ok bool, err error)
	// Del removes key.
	Del(ctx context.Context, key string) error
	// DelIf removes key only while it holds value.
	DelIf(ctx context.Context, key, value string) error
	// Take returns the value of key and removes it in one step, so of
	// several callers only one gets it; ok is false when there is none.
	Take(ctx context.Context, key string) (value string, ok bool, err error)
}

// MemKV is KV in the process.
type MemKV struct {
	mu   sync.Mutex
	now  func() time.Time
	vals map[string]memVal
}

type memVal struct {
	v   string
	exp time.Time
}

// NewMemKV makes an empty MemKV.
func NewMemKV() *MemKV { return &MemKV{now: time.Now, vals: map[string]memVal{}} }

func (m *MemKV) getLocked(key string) (string, bool) {
	v, ok := m.vals[key]
	if !ok {
		return "", false
	}
	if !m.now().Before(v.exp) {
		delete(m.vals, key)
		return "", false
	}
	return v.v, true
}

func (m *MemKV) sweepLocked() {
	if len(m.vals) < 4096 {
		return
	}
	now := m.now()
	for k, v := range m.vals {
		if !now.Before(v.exp) {
			delete(m.vals, k)
		}
	}
}

func (m *MemKV) Set(_ context.Context, key, value string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	m.vals[key] = memVal{v: value, exp: m.now().Add(ttl)}
	return nil
}

func (m *MemKV) SetNX(_ context.Context, key, value string, ttl time.Duration) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.getLocked(key); ok {
		return cur, false, nil
	}
	m.sweepLocked()
	m.vals[key] = memVal{v: value, exp: m.now().Add(ttl)}
	return value, true, nil
}

func (m *MemKV) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.getLocked(key)
	return v, ok, nil
}

func (m *MemKV) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, key)
	return nil
}

func (m *MemKV) DelIf(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.getLocked(key); ok && cur == value {
		delete(m.vals, key)
	}
	return nil
}

func (m *MemKV) Take(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.getLocked(key)
	if ok {
		delete(m.vals, key)
	}
	return v, ok, nil
}

// ValkeyKV is KV in Valkey under the installation's prefix.
type ValkeyKV struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewValkeyKV makes a KV on v.
func NewValkeyKV(v *Valkey) *ValkeyKV { return &ValkeyKV{rdb: v.Client, prefix: v.Prefix()} }

// kvTimeout bounds one call: a pod whose Valkey is gone answers within it.
const kvTimeout = 2 * time.Second

func withKVTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, kvTimeout)
}

func (k *ValkeyKV) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	ctx, cancel := withKVTimeout(ctx)
	defer cancel()
	return k.rdb.Set(ctx, k.prefix+key, value, ttl).Err()
}

var setNXGet = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then return {ARGV[1], 1} end
return {redis.call('GET', KEYS[1]), 0}
`)

func (k *ValkeyKV) SetNX(ctx context.Context, key, value string, ttl time.Duration) (string, bool, error) {
	ctx, cancel := withKVTimeout(ctx)
	defer cancel()
	res, err := setNXGet.Run(ctx, k.rdb, []string{k.prefix + key}, value, ttl.Milliseconds()).Slice()
	if err != nil {
		return "", false, err
	}
	if len(res) != 2 {
		return "", false, errors.New("cluster: unexpected SETNX answer")
	}
	cur, _ := res[0].(string)
	set, _ := res[1].(int64)
	return cur, set == 1, nil
}

func (k *ValkeyKV) Get(ctx context.Context, key string) (string, bool, error) {
	ctx, cancel := withKVTimeout(ctx)
	defer cancel()
	v, err := k.rdb.Get(ctx, k.prefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (k *ValkeyKV) Del(ctx context.Context, key string) error {
	ctx, cancel := withKVTimeout(ctx)
	defer cancel()
	return k.rdb.Del(ctx, k.prefix+key).Err()
}

var delIf = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

func (k *ValkeyKV) DelIf(ctx context.Context, key, value string) error {
	ctx, cancel := withKVTimeout(ctx)
	defer cancel()
	return delIf.Run(ctx, k.rdb, []string{k.prefix + key}, value).Err()
}

func (k *ValkeyKV) Take(ctx context.Context, key string) (string, bool, error) {
	ctx, cancel := withKVTimeout(ctx)
	defer cancel()
	v, err := k.rdb.GetDel(ctx, k.prefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
