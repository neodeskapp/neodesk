package peer

import (
	"net"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/config"
)

// Directory is the cluster-wide view of peers registered on other pods.
type Directory interface {
	Publish(entries []*Entry) PublishResult // owner pod writes its entries (copies, no conns)
	Withdraw(ids []string)                  // owner pod removes them
	Lookup(id string) (*Entry, bool)        // entry of a peer registered on another pod
	LookupAddr(addr string) (*Entry, bool)  // by "ip:port" (UDPAddr.String())
	ByIP(ip string) []*Entry                // live peers whose address has this IP
}

// PublishResult says what a Publish found about peers that moved between
// pods: Took names, per id, the pod whose older entry this one replaced;
// Refused lists ids another pod holds a newer registration of, so the
// local entry is stale.
type PublishResult struct {
	Took    map[string]string
	Refused []string
}

// BatchLookup is implemented by a Directory that can look up many ids in
// one round trip. OnlineStates uses it when the directory has it, and
// Lookup per id otherwise.
type BatchLookup interface {
	LookupMany(ids []string) map[string]*Entry
}

// NormalizeAddr is the canonical "ip:port" the map and the directory match
// addresses by: an IPv4-mapped IPv6 address becomes its IPv4 form.
func NormalizeAddr(addr string) string { return normalizePeerAddr(addr) }

// flushEvery is the shortest time between two flushes to the directory.
const flushEvery = 50 * time.Millisecond

// dirState is the map's link to a Directory. Every field but wake and
// done is guarded by Map.mu.
type dirState struct {
	d    Directory
	self string

	// full: ids whose routing data changed. beat: ids whose only change
	// since their last publish is the heartbeat time. removed: ids to
	// withdraw. An id is in at most one of them.
	full    map[string]struct{}
	beat    map[string]struct{}
	removed map[string]struct{}
	lastPub map[string]time.Time // when each id was last published

	beatEvery time.Duration // a heartbeat-only change waits this long after the last publish

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// SetDirectory makes the map publish the peers it holds to d and fall back
// to d for peers it doesn't hold. self is this pod's name. It starts a
// worker that Close stops; call it once, before the map is in use.
func (m *Map) SetDirectory(d Directory, self string) {
	if d == nil {
		return
	}
	ds := &dirState{
		d:         d,
		self:      self,
		full:      make(map[string]struct{}),
		beat:      make(map[string]struct{}),
		removed:   make(map[string]struct{}),
		lastPub:   make(map[string]time.Time),
		beatEvery: config.RegTimeout / 3,
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	m.mu.Lock()
	if m.dir != nil {
		m.mu.Unlock()
		return
	}
	m.dir = ds
	// Entries put before the directory was set are published too.
	for id := range m.entries {
		ds.full[id] = struct{}{}
	}
	m.mu.Unlock()
	go m.runDirectory(ds)
	ds.signal()
}

// Close stops the directory worker after a last flush. It does nothing
// for a map without a directory and is safe to call more than once.
func (m *Map) Close() {
	m.mu.RLock()
	ds := m.dir
	m.mu.RUnlock()
	if ds == nil {
		return
	}
	ds.stopOnce.Do(func() { close(ds.stop) })
	<-ds.done
}

// Leave stops the directory worker and withdraws every peer this pod
// holds, so other pods stop sending to it at once instead of after the
// records expire. Used when a pod shuts down; its peers register on
// another pod with their next heartbeat.
func (m *Map) Leave() {
	m.Close()
	m.mu.RLock()
	ds := m.dir
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	if ds == nil || len(ids) == 0 {
		return
	}
	ds.d.Withdraw(ids)
}

func (ds *dirState) signal() {
	select {
	case ds.wake <- struct{}{}:
	default:
	}
}

// markLocked records that id's routing data changed. Caller holds m.mu
// for writing.
func (m *Map) markLocked(id string) {
	ds := m.dir
	if ds == nil {
		return
	}
	delete(ds.beat, id)
	delete(ds.removed, id)
	ds.full[id] = struct{}{}
	ds.signal()
}

// markBeatLocked records that only id's heartbeat time changed. Caller
// holds m.mu for writing.
func (m *Map) markBeatLocked(id string) {
	ds := m.dir
	if ds == nil {
		return
	}
	if _, ok := ds.full[id]; ok {
		return
	}
	delete(ds.removed, id)
	ds.beat[id] = struct{}{}
	ds.signal()
}

// markRemovedLocked records that id left the map. Caller holds m.mu for
// writing.
func (m *Map) markRemovedLocked(id string) {
	ds := m.dir
	if ds == nil {
		return
	}
	delete(ds.full, id)
	delete(ds.beat, id)
	ds.removed[id] = struct{}{}
	ds.signal()
}

func (m *Map) runDirectory(ds *dirState) {
	defer close(ds.done)
	var last time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ds.stop:
			m.flushDirectory(ds, true)
			return
		case <-ds.wake:
		case <-timer.C:
		}
		if wait := flushEvery - time.Since(last); wait > 0 {
			select {
			case <-ds.stop:
				m.flushDirectory(ds, true)
				return
			case <-time.After(wait):
			}
		}
		last = time.Now()
		next := m.flushDirectory(ds, false)
		timer.Stop()
		if !next.IsZero() {
			timer.Reset(time.Until(next))
		}
	}
}

// flushDirectory publishes and withdraws what changed since the last
// flush. A heartbeat-only change of an id published less than beatEvery
// ago stays pending; the returned time is when the first of those is due
// (zero when none is), and final publishes them anyway.
func (m *Map) flushDirectory(ds *dirState, final bool) (next time.Time) {
	now := time.Now()
	var pub []*Entry
	var gone []string

	m.mu.Lock()
	for id := range ds.removed {
		gone = append(gone, id)
		delete(ds.lastPub, id)
	}
	clear(ds.removed)
	for id := range ds.full {
		if e, ok := m.entries[id]; ok {
			pub = append(pub, publishCopy(e))
			ds.lastPub[id] = now
		}
	}
	clear(ds.full)
	for id := range ds.beat {
		e, ok := m.entries[id]
		if !ok {
			delete(ds.beat, id)
			continue
		}
		if due := ds.lastPub[id].Add(ds.beatEvery); !final && now.Before(due) {
			if next.IsZero() || due.Before(next) {
				next = due
			}
			continue
		}
		pub = append(pub, publishCopy(e))
		ds.lastPub[id] = now
		delete(ds.beat, id)
	}
	m.mu.Unlock()

	if len(gone) > 0 {
		ds.d.Withdraw(gone)
	}
	if len(pub) > 0 {
		res := ds.d.Publish(pub)
		for _, id := range res.Refused {
			// Another pod registered the peer after this one: the local
			// entry is stale, so this pod stops answering for it
			m.dropStale(id)
		}
		if h := m.movedHandler(); h != nil {
			for id, from := range res.Took {
				h(id, from)
			}
		}
	}
	return next
}

// SetMovedHandler sets who hears that a peer this pod holds now was
// registered on pod from before (the signal server tells that pod).
func (m *Map) SetMovedHandler(h func(id, from string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onMoved = h
}

func (m *Map) movedHandler() func(id, from string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onMoved
}

// SetRemoteRemoveHandler sets who asks pod owner to remove peer id when
// Remove is called for a peer another pod holds (the signal server calls
// that pod).
func (m *Map) SetRemoteRemoveHandler(h func(owner, id string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRemoteRemove = h
}

// removeRemoteLocked hands the removal of id to the pod that holds it. It
// runs with m.mu held and asks on its own goroutine.
func (m *Map) removeRemoteLocked(id string) {
	ds, h := m.dir, m.onRemoteRemove
	if ds == nil || h == nil {
		return
	}
	go func() {
		if e, ok := ds.d.Lookup(id); ok && e != nil && e.Owner != "" && e.Owner != ds.self {
			h(e.Owner, id)
		}
	}()
}

// dropStale removes the local entry of id, which another pod holds a newer
// registration of, without withdrawing it (the record is that pod's).
func (m *Map) dropStale(id string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if ok {
		delete(m.entries, id)
		if ds := m.dir; ds != nil {
			delete(ds.full, id)
			delete(ds.beat, id)
			delete(ds.removed, id)
			delete(ds.lastPub, id)
		}
	}
	m.mu.Unlock()
	if ok {
		e.CloseConnections()
	}
}

// publishCopy is the copy of e a directory gets: no connections, own
// slices.
func publishCopy(e *Entry) *Entry {
	c := *e
	c.TCPConn = nil
	c.WSConn = nil
	c.IPHistory = nil
	c.UUID = append([]byte(nil), e.UUID...)
	c.PK = append([]byte(nil), e.PK...)
	if e.UDPAddr != nil {
		a := *e.UDPAddr
		a.IP = append(net.IP(nil), e.UDPAddr.IP...)
		c.UDPAddr = &a
	}
	return &c
}

// directory returns the map's directory, nil without one.
func (m *Map) directory() *dirState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dir
}

// remote looks id up in the directory. It returns nil when the map has no
// directory, the directory doesn't know id, or this map holds id itself
// by now (local entries win).
func (m *Map) remote(ds *dirState, id string) *Entry {
	e, ok := ds.d.Lookup(id)
	if !ok || e == nil {
		return nil
	}
	if m.hasLocal(e.ID) {
		return nil
	}
	return e
}

func (m *Map) hasLocal(id string) bool {
	m.mu.RLock()
	_, ok := m.entries[id]
	m.mu.RUnlock()
	return ok
}

// DropIfMoved removes the local entry of id when the directory holds a
// newer registration of it on another pod: the peer moved there and the
// local entry is stale. It closes the entry's connections like Remove and
// reports whether it dropped it. The peer's directory entry belongs to the
// other pod now, so nothing is withdrawn.
func (m *Map) DropIfMoved(id string) bool {
	ds := m.directory()
	if ds == nil || id == "" {
		return false
	}
	r, ok := ds.d.Lookup(id)
	if !ok || r == nil || r.Owner == "" || r.Owner == ds.self {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok || !r.LastReg.After(e.LastReg) {
		return false
	}
	e.CloseConnections()
	delete(m.entries, id)
	delete(ds.full, id)
	delete(ds.beat, id)
	delete(ds.removed, id)
	delete(ds.lastPub, id)
	return true
}

// remoteByIP returns the directory's live peers at ip that this map doesn't
// hold itself.
func (m *Map) remoteByIP(ds *dirState, ip net.IP) []*Entry {
	all := ds.d.ByIP(ip.String())
	if len(all) == 0 {
		return nil
	}
	out := all[:0]
	m.mu.RLock()
	for _, e := range all {
		if e == nil || !peerEntryMatchesIP(e, ip) {
			continue
		}
		if _, local := m.entries[e.ID]; local {
			continue
		}
		out = append(out, e)
	}
	m.mu.RUnlock()
	return out
}

// lookupMany looks ids up in the directory, in one call when it can.
func lookupMany(d Directory, ids []string) map[string]*Entry {
	if b, ok := d.(BatchLookup); ok {
		return b.LookupMany(ids)
	}
	out := make(map[string]*Entry, len(ids))
	for _, id := range ids {
		if e, ok := d.Lookup(id); ok && e != nil {
			out[id] = e
		}
	}
	return out
}
