package api

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// kvHolder is the KV of one short-lived store: the one SetSharedKV gave
// it, else a KV in memory of its own, made on first use.
type kvHolder struct {
	once   sync.Once
	shared cluster.KV
}

func (h *kvHolder) kv() cluster.KV {
	h.once.Do(func() {
		if h.shared == nil {
			h.shared = cluster.NewMemKV()
		}
	})
	return h.shared
}

// setKV makes the store use kv. It is called before the server serves.
func (h *kvHolder) setKV(kv cluster.KV) {
	h.once.Do(func() {})
	h.shared = kv
}

// onceKeys remembers keys for a while to refuse their second use: the
// nonces of signed requests and the sessions that used a saved sign-in.
// The zero value needs prefix and ttl set.
type onceKeys struct {
	kvHolder
	prefix string
	ttl    time.Duration
}

func newOnceKeys(prefix string, ttl time.Duration) *onceKeys {
	return &onceKeys{prefix: prefix, ttl: ttl}
}

// use notes key at now and reports whether it was free, on every instance
// that shares the KV. A use older than ttl counts as gone. When the KV
// does not answer, use is false: the request is refused.
func (o *onceKeys) use(key string, now time.Time) bool {
	ok, err := cluster.UseOnce(context.Background(), o.kv(), o.prefix+key, now, o.ttl)
	if err != nil {
		log.Printf("[api] note a one-time key: %v", err)
		return false
	}
	return ok
}

func newMgmtNonces() *onceKeys       { return newOnceKeys("nonce:mgmt:", ndMgmtNonceTTL) }
func newEnrollmentNonces() *onceKeys { return newOnceKeys("nonce:enroll:", ndMgmtNonceTTL) }

// initNonces makes the nonce stores a Server built without New lacks.
func (s *Server) initNonces() {
	if s.panelNonces == nil {
		s.panelNonces = newPanelNonces()
	}
	if s.mgmtNonces == nil {
		s.mgmtNonces = newMgmtNonces()
	}
	if s.enrollmentNonces == nil {
		s.enrollmentNonces = newEnrollmentNonces()
	}
}

func (s *Server) panelNonceKeys() *onceKeys {
	s.nonceInit.Do(s.initNonces)
	return s.panelNonces
}

func (s *Server) mgmtNonceKeys() *onceKeys {
	s.nonceInit.Do(s.initNonces)
	return s.mgmtNonces
}

func (s *Server) enrollmentNonceKeys() *onceKeys {
	s.nonceInit.Do(s.initNonces)
	return s.enrollmentNonces
}

// SetSharedKV makes the short-lived stores of the server use kv, which
// the instances of an installation share: the pending second steps of
// the client sign-in, the single sign-on starts and codes, the desktop
// sign-ins through the console, the uses of saved sign-ins and of quick
// support callers, and the nonces of signed requests. A flow that starts
// on one instance can then finish on another, and a value used once on
// one instance is refused on all. nil keeps each store in memory. Call it
// before Start.
func (s *Server) SetSharedKV(kv cluster.KV) {
	if kv == nil {
		return
	}
	s.sharedKV = kv
	s.nonceInit.Do(s.initNonces)
	if s.clientTFASessions != nil {
		s.clientTFASessions.setKV(kv)
	}
	s.consoleLogins.setKV(kv)
	// The two kinds of use share the KV, so each needs its own prefix
	// also on a Server built without New
	if s.signinUses.prefix == "" {
		s.signinUses.prefix = "signin:use:"
	}
	if s.callerUses.prefix == "" {
		s.callerUses.prefix = "signin:caller:"
	}
	s.signinUses.setKV(kv)
	s.callerUses.setKV(kv)
	s.panelNonces.setKV(kv)
	s.mgmtNonces.setKV(kv)
	s.enrollmentNonces.setKV(kv)
	if s.oidcProvider != nil {
		s.oidcProvider.SetKV(kv)
	}
}
