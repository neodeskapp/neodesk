package cluster

import (
	"context"
	"log"
	"sync"
	"time"
)

// Devices records which pod holds the socket of a CDAP device or a
// MeshCentral agent (dev:<kind>:<id> = pod name), so a console request for
// the device that lands on another pod is passed to this one. A pod
// refreshes its entries while the sockets stay open; a pod that dies drops
// out after DeviceTTL.
type Devices struct {
	kv   KV
	self string

	mu   sync.Mutex
	held map[string]struct{} // kind:id
}

// DeviceTTL is how long an entry outlives the last refresh.
const DeviceTTL = 90 * time.Second

const deviceRefresh = 30 * time.Second

// NewDevices returns the registry of pod self on kv.
func NewDevices(kv KV, self string) *Devices {
	return &Devices{kv: kv, self: self, held: map[string]struct{}{}}
}

func deviceKey(kind, id string) string { return "dev:" + kind + ":" + id }

// Hold records that this pod holds device id of kind.
func (d *Devices) Hold(kind, id string) {
	d.mu.Lock()
	d.held[kind+":"+id] = struct{}{}
	d.mu.Unlock()
	if err := d.kv.Set(context.Background(), deviceKey(kind, id), d.self, DeviceTTL); err != nil {
		log.Printf("[cluster] Could not record device %s:%s: %v", kind, id, err)
	}
}

// Release drops this pod's entry of device id and reports whether the
// device is still this pod's to mark offline: false when another pod has
// taken it over meanwhile (the device reconnected there).
func (d *Devices) Release(kind, id string) bool {
	d.mu.Lock()
	delete(d.held, kind+":"+id)
	d.mu.Unlock()
	ctx := context.Background()
	key := deviceKey(kind, id)
	_ = d.kv.DelIf(ctx, key, d.self)
	owner, ok, err := d.kv.Get(ctx, key)
	return err != nil || !ok || owner == d.self
}

// Owner returns the pod that holds device id of kind, "" when this pod
// does or none does.
func (d *Devices) Owner(ctx context.Context, kind, id string) string {
	owner, ok, err := d.kv.Get(ctx, deviceKey(kind, id))
	if err != nil || !ok || owner == d.self {
		return ""
	}
	return owner
}

// Run refreshes this pod's entries until ctx ends, then removes them.
func (d *Devices) Run(ctx context.Context) {
	t := time.NewTicker(deviceRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			keys := make([]string, 0, len(d.held))
			for k := range d.held {
				keys = append(keys, k)
			}
			d.mu.Unlock()
			for _, k := range keys {
				_ = d.kv.DelIf(context.Background(), "dev:"+k, d.self)
			}
			return
		case <-t.C:
			d.mu.Lock()
			keys := make([]string, 0, len(d.held))
			for k := range d.held {
				keys = append(keys, k)
			}
			d.mu.Unlock()
			for _, k := range keys {
				_ = d.kv.Set(ctx, "dev:"+k, d.self, DeviceTTL)
			}
		}
	}
}
