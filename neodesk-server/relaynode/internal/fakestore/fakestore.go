// Package fakestore is an in-memory hub.Store, hub.Events and hub.Auditor
// for the relaynode tests.
package fakestore

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/hub"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/wire"
)

// Store keeps sites, tokens and nodes in maps.
type Store struct {
	mu       sync.Mutex
	sites    map[string]*hub.Site
	tokens   map[string]string // token -> site id
	used     map[string]bool
	nodes    map[string]*hub.Node
	consumed int
}

// Consumed is the number of successful token uses.
func (s *Store) Consumed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumed
}

// New makes a store with one site "site-1" of org "org-1".
func New() *Store {
	return &Store{
		sites:  map[string]*hub.Site{"site-1": {ID: "site-1", OrgID: "org-1", Name: "Office"}},
		tokens: map[string]string{},
		used:   map[string]bool{},
		nodes:  map[string]*hub.Node{},
	}
}

// AddToken makes token valid once for site.
func (s *Store) AddToken(token, site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = site
}

// Revoke marks the node revoked.
func (s *Store) Revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.nodes[id]; n != nil {
		n.Revoked = true
	}
}

// DeleteSite removes the site.
func (s *Store) DeleteSite(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sites, id)
}

// Node returns a copy of the node.
func (s *Store) Node(id string) (hub.Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil {
		return hub.Node{}, false
	}
	return *n, true
}

// Nodes is the number of nodes.
func (s *Store) Nodes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nodes)
}

func (s *Store) ConsumeEnrollmentToken(token, nodeID string, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	site, ok := s.tokens[token]
	if !ok || s.used[token] {
		return "", errors.New("token unknown or used")
	}
	s.used[token] = true
	s.consumed++
	return site, nil
}

func (s *Store) GetRelayNodeByKey(key string) (*hub.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		if n.PublicKey == key {
			c := *n
			return &c, nil
		}
	}
	return nil, nil
}

func (s *Store) CreateRelayNode(n *hub.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.nodes {
		if o.PublicKey == n.PublicKey {
			return fmt.Errorf("public key in use")
		}
	}
	c := *n
	s.nodes[n.ID] = &c
	return nil
}

func (s *Store) UpdateRelayNodeState(id, version, lan, public string, seen time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.nodes[id]; n != nil {
		n.Version, n.LanAddr, n.PublicAddr = version, lan, public
	}
	return nil
}

func (s *Store) GetSite(id string) (*hub.Site, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if site := s.sites[id]; site != nil {
		c := *site
		return &c, nil
	}
	return nil, nil
}

// Event is one call to Recorder.
type Event struct {
	Kind, Node, UUID string
	Up, Down         int64
	Stats            wire.Stats
	Fields           map[string]string
}

// Recorder is hub.Events and hub.Auditor; every call goes to C.
type Recorder struct{ C chan Event }

// NewRecorder makes a recorder with room for 256 events.
func NewRecorder() *Recorder { return &Recorder{C: make(chan Event, 256)} }

func (r *Recorder) put(e Event) {
	select {
	case r.C <- e:
	default:
	}
}

func (r *Recorder) NodeSessionStarted(node, uuid string) {
	r.put(Event{Kind: "started", Node: node, UUID: uuid})
}

func (r *Recorder) NodeSessionEnded(node, uuid string, up, down int64) {
	r.put(Event{Kind: "ended", Node: node, UUID: uuid, Up: up, Down: down})
}

func (r *Recorder) NodeStats(node string, s wire.Stats) {
	r.put(Event{Kind: "stats", Node: node, Stats: s})
}

func (r *Recorder) Audit(event string, fields map[string]string) {
	r.put(Event{Kind: event, Fields: fields})
}

// Wait returns the next event of kind, failing after timeout.
func (r *Recorder) Wait(kind string, timeout time.Duration) (Event, error) {
	deadline := time.After(timeout)
	for {
		select {
		case e := <-r.C:
			if e.Kind == kind {
				return e, nil
			}
		case <-deadline:
			return Event{}, fmt.Errorf("no %s event within %s", kind, timeout)
		}
	}
}
