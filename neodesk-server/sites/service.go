// Package sites places devices at the sites of their organization and
// picks the relay nodes that relay a connection between two devices.
package sites

import (
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

// NodeLiveness says which relay nodes have a control connection to this
// server and how many sessions each relays now. The node hub implements it.
type NodeLiveness interface {
	Online(nodeID string) bool
	ActiveSessions(nodeID string) int
}

// deviceCacheTTL is how long the organization and the folder of a device
// are kept. Both change outside this service, so they expire instead of
// being invalidated.
const deviceCacheTTL = 30 * time.Second

// folderLister reads every device's folder (db.PanelGroupStore has it).
type folderLister interface {
	ListPanelDeviceFolderAssignments() (map[string]int64, error)
}

// Service resolves sites and routes, and makes every change to sites,
// networks, pins and nodes, so that its cache stays current.
type Service struct {
	db  db.Database
	now func() time.Time

	mu       sync.Mutex
	snap     *snapshot
	gen      uint64 // counts invalidations, so a slow load can't install old data
	liveness NodeLiveness
	onRevoke func(nodeIDs []string)

	orgs      map[string]orgEntry
	folders   map[string]int64
	foldersAt time.Time

	warned sync.Map // pins of another organization already logged
}

type orgEntry struct {
	org string
	at  time.Time
}

// New returns a Service on the database. No node is online until
// SetLiveness gives it the hub.
func New(database db.Database) *Service {
	return &Service{db: database, now: time.Now, orgs: make(map[string]orgEntry)}
}

// SetLiveness sets where the service learns which nodes are online.
func (s *Service) SetLiveness(l NodeLiveness) {
	s.mu.Lock()
	s.liveness = l
	s.mu.Unlock()
}

// SetRevokeHook sets a function called with the ids of nodes that were
// revoked or whose site was deleted, after the change is stored. The hub
// uses it to close their control connections.
func (s *Service) SetRevokeHook(fn func(nodeIDs []string)) {
	s.mu.Lock()
	s.onRevoke = fn
	s.mu.Unlock()
}

// Invalidate drops the cached sites, networks, pins, nodes and probe
// results; the next call reads them again.
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.snap = nil
	s.gen++
	s.mu.Unlock()
}

// snapshot is everything resolution and routing read, loaded at once.
type snapshot struct {
	sites map[string]*db.Site
	rank  map[string]int // site id -> position, oldest first
	nets  []siteNet
	pins  map[[2]string]string
	nodes map[string][]db.RelayNode // site id -> nodes not revoked
	// failed holds the probes that said node from can't reach node to
	// over via; a pair without a probe result counts as reachable.
	failed map[[3]string]bool
}

type siteNet struct {
	site *db.Site
	net  *net.IPNet
	ones int
	kind string
}

func loadSnapshot(database db.Database) (*snapshot, error) {
	sites, err := database.ListSites("", true)
	if err != nil {
		return nil, err
	}
	snap := &snapshot{
		sites:  make(map[string]*db.Site, len(sites)),
		rank:   make(map[string]int, len(sites)),
		pins:   make(map[[2]string]string),
		nodes:  make(map[string][]db.RelayNode),
		failed: make(map[[3]string]bool),
	}
	sort.SliceStable(sites, func(i, j int) bool {
		if !sites[i].CreatedAt.Equal(sites[j].CreatedAt) {
			return sites[i].CreatedAt.Before(sites[j].CreatedAt)
		}
		return sites[i].ID < sites[j].ID
	})
	for i := range sites {
		site := sites[i]
		snap.sites[site.ID] = &site
		snap.rank[site.ID] = i
	}
	nets, err := database.ListAllSiteNetworks()
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		site := snap.sites[n.SiteID]
		if site == nil {
			continue
		}
		_, ipnet, err := net.ParseCIDR(n.CIDR)
		if err != nil {
			log.Printf("[sites] network %q of site %s does not parse, skipped: %v", n.CIDR, n.SiteID, err)
			continue
		}
		ones, _ := ipnet.Mask.Size()
		snap.nets = append(snap.nets, siteNet{site: site, net: ipnet, ones: ones, kind: n.Kind})
	}
	sort.SliceStable(snap.nets, func(i, j int) bool {
		return snap.rank[snap.nets[i].site.ID] < snap.rank[snap.nets[j].site.ID]
	})
	pins, err := database.ListSitePins()
	if err != nil {
		return nil, err
	}
	for _, p := range pins {
		snap.pins[[2]string{p.Kind, p.Ref}] = p.SiteID
	}
	nodes, err := database.ListRelayNodes("")
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if !n.Revoked() && snap.sites[n.SiteID] != nil {
			snap.nodes[n.SiteID] = append(snap.nodes[n.SiteID], n)
		}
	}
	reach, err := database.ListSiteReachability()
	if err != nil {
		return nil, err
	}
	for _, r := range reach {
		if !r.OK {
			snap.failed[[3]string{r.FromNode, r.ToNode, r.Via}] = true
		}
	}
	return snap, nil
}

// snapshot returns the cached snapshot, loading it when there is none;
// nil when the database fails.
func (s *Service) snapshot() *snapshot {
	s.mu.Lock()
	snap, gen := s.snap, s.gen
	s.mu.Unlock()
	if snap != nil {
		return snap
	}
	snap, err := loadSnapshot(s.db)
	if err != nil {
		log.Printf("[sites] load sites: %v", err)
		return nil
	}
	s.mu.Lock()
	if s.gen == gen {
		s.snap = snap
	}
	s.mu.Unlock()
	return snap
}

// orgOf returns the organization of a device; false when it can't be read.
func (s *Service) orgOf(deviceID string) (string, bool) {
	now := s.now()
	s.mu.Lock()
	e, ok := s.orgs[deviceID]
	s.mu.Unlock()
	if ok && now.Sub(e.at) < deviceCacheTTL {
		return e.org, true
	}
	org, err := s.db.GetDeviceOrgID(deviceID)
	if err != nil {
		log.Printf("[sites] organization of device %s: %v", deviceID, err)
		return "", false
	}
	s.mu.Lock()
	s.orgs[deviceID] = orgEntry{org: org, at: now}
	s.mu.Unlock()
	return org, true
}

// folderOf returns the folder of a device; false when it is in none.
func (s *Service) folderOf(deviceID string) (int64, bool) {
	lister, ok := s.db.(folderLister)
	if !ok {
		return 0, false
	}
	now := s.now()
	s.mu.Lock()
	folders, at := s.folders, s.foldersAt
	s.mu.Unlock()
	if folders == nil || now.Sub(at) >= deviceCacheTTL {
		loaded, err := lister.ListPanelDeviceFolderAssignments()
		if err != nil {
			log.Printf("[sites] device folders: %v", err)
			return 0, false
		}
		if loaded == nil {
			loaded = map[string]int64{}
		}
		s.mu.Lock()
		s.folders, s.foldersAt = loaded, now
		s.mu.Unlock()
		folders = loaded
	}
	id, ok := folders[deviceID]
	return id, ok && id > 0
}

// DeviceSite returns the site of a device that registered from addr, nil
// for none: its own pin, else the pin of its folder, else the site whose
// network holds addr (a private address in private networks only, a public
// one in public networks only; the most specific network wins, ties go to
// the oldest site). Only sites of the device's organization count.
func (s *Service) DeviceSite(deviceID string, addr net.IP) *db.Site {
	snap := s.snapshot()
	if snap == nil {
		return nil
	}
	org, ok := s.orgOf(deviceID)
	if !ok {
		return nil
	}
	site := s.resolve(snap, deviceID, org, addr)
	if site == nil {
		return nil
	}
	out := *site
	return &out
}

func (s *Service) resolve(snap *snapshot, deviceID, org string, addr net.IP) *db.Site {
	if site := s.pinned(snap, db.SitePinDevice, deviceID, org); site != nil {
		return site
	}
	if folder, ok := s.folderOf(deviceID); ok {
		if site := s.pinned(snap, db.SitePinFolder, strconv.FormatInt(folder, 10), org); site != nil {
			return site
		}
	}
	if addr == nil {
		return nil
	}
	kind := db.SiteNetworkPublic
	if IsPrivate(addr) {
		kind = db.SiteNetworkPrivate
	}
	var best *siteNet
	for i := range snap.nets {
		n := &snap.nets[i]
		if n.kind != kind || n.site.OrgID != org || !n.net.Contains(addr) {
			continue
		}
		if best == nil || n.ones > best.ones ||
			(n.ones == best.ones && snap.rank[n.site.ID] < snap.rank[best.site.ID]) {
			best = n
		}
	}
	if best == nil {
		return nil
	}
	return best.site
}

// pinned returns the site of a pin when it is a site of org.
func (s *Service) pinned(snap *snapshot, kind, ref, org string) *db.Site {
	siteID := snap.pins[[2]string{kind, ref}]
	if siteID == "" {
		return nil
	}
	site := snap.sites[siteID]
	if site == nil {
		return nil
	}
	if site.OrgID != org {
		key := kind + "\x00" + ref + "\x00" + siteID + "\x00" + org
		if _, logged := s.warned.LoadOrStore(key, true); !logged {
			log.Printf("[sites] %s %s is pinned to site %s of another organization; pin ignored", kind, ref, siteID)
		}
		return nil
	}
	return site
}

// RouteKind says how a connection is relayed by nodes.
type RouteKind int

const (
	// None: no node relays it; the central relay does, as without sites.
	None RouteKind = iota
	// Same: one node, A, relays it; both peers dial A.
	Same
	// Pair: the initiator dials node A, the target dials node B, and A
	// dials B at Dial.
	Pair
)

// NodeRoute is the relay route Route picks for a connection.
type NodeRoute struct {
	Kind RouteKind
	A    *db.RelayNode
	B    *db.RelayNode // Pair only
	// InitiatorAddr and TargetAddr are the "host:port" each peer dials:
	// the node's LAN address for a peer whose registration address is
	// private, else its public address.
	InitiatorAddr string
	TargetAddr    string
	// Dial is where A reaches B (Pair only): B's LAN address when both
	// sites have the same VPN group, else B's public address.
	Dial string
}

// Route picks the nodes that relay a connection from the initiator to the
// target, given the addresses both registered from.
//
//   - Both at the same site with an online node: Same, the node with the
//     fewest active sessions (ties by id) that has an address for both.
//   - At different sites of the same organization, both with online nodes:
//     Pair. When no pair of nodes works (an address missing, or a probe
//     said A can't reach B), one node both peers reach: a peer reaches a
//     node of its own site by the same rule as above, and a node of the
//     other site at its public address when its own address is public.
//   - Anything else, and always for peers of different organizations: None.
func (s *Service) Route(initiatorID string, initiatorAddr net.IP, targetID string, targetAddr net.IP) NodeRoute {
	if initiatorID == "" || targetID == "" {
		return NodeRoute{}
	}
	snap := s.snapshot()
	if snap == nil {
		return NodeRoute{}
	}
	iOrg, ok1 := s.orgOf(initiatorID)
	tOrg, ok2 := s.orgOf(targetID)
	if !ok1 || !ok2 || iOrg != tOrg {
		return NodeRoute{}
	}
	iSite := s.resolve(snap, initiatorID, iOrg, initiatorAddr)
	tSite := s.resolve(snap, targetID, tOrg, targetAddr)
	if iSite == nil || tSite == nil {
		return NodeRoute{}
	}
	s.mu.Lock()
	live := s.liveness
	s.mu.Unlock()
	if live == nil {
		return NodeRoute{}
	}
	iPrivate, tPrivate := IsPrivate(initiatorAddr), IsPrivate(targetAddr)

	if iSite.ID == tSite.ID {
		for _, n := range onlineNodes(snap, live, iSite.ID) {
			ia, ta := sameSiteAddr(n, iPrivate), sameSiteAddr(n, tPrivate)
			if ia != "" && ta != "" {
				return NodeRoute{Kind: Same, A: n, InitiatorAddr: ia, TargetAddr: ta}
			}
		}
		return NodeRoute{}
	}

	as, bs := onlineNodes(snap, live, iSite.ID), onlineNodes(snap, live, tSite.ID)
	if len(as) == 0 || len(bs) == 0 {
		return NodeRoute{}
	}
	vpn := iSite.VPNGroup != "" && iSite.VPNGroup == tSite.VPNGroup
	via := db.SiteNetworkPublic
	if vpn {
		via = db.SiteNetworkPrivate
	}
	for _, a := range as {
		ia := sameSiteAddr(a, iPrivate)
		if ia == "" {
			continue
		}
		for _, b := range bs {
			ta := sameSiteAddr(b, tPrivate)
			dial := b.PublicAddr
			if vpn {
				dial = b.LANAddr
			}
			if ta == "" || dial == "" || snap.failed[[3]string{a.ID, b.ID, via}] {
				continue
			}
			return NodeRoute{Kind: Pair, A: a, B: b, InitiatorAddr: ia, TargetAddr: ta, Dial: dial}
		}
	}

	both := append(append([]*db.RelayNode{}, as...), bs...)
	sortByLoad(both, live)
	for _, n := range both {
		ia := reachAddr(n, iSite.ID, iPrivate)
		ta := reachAddr(n, tSite.ID, tPrivate)
		if ia != "" && ta != "" {
			return NodeRoute{Kind: Same, A: n, InitiatorAddr: ia, TargetAddr: ta}
		}
	}
	return NodeRoute{}
}

// onlineNodes returns copies of the site's online nodes, fewest active
// sessions first, ties by id.
func onlineNodes(snap *snapshot, live NodeLiveness, siteID string) []*db.RelayNode {
	var out []*db.RelayNode
	for _, n := range snap.nodes[siteID] {
		if live.Online(n.ID) {
			c := n
			out = append(out, &c)
		}
	}
	sortByLoad(out, live)
	return out
}

func sortByLoad(nodes []*db.RelayNode, live NodeLiveness) {
	load := make(map[string]int, len(nodes))
	for _, n := range nodes {
		load[n.ID] = live.ActiveSessions(n.ID)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if load[nodes[i].ID] != load[nodes[j].ID] {
			return load[nodes[i].ID] < load[nodes[j].ID]
		}
		return nodes[i].ID < nodes[j].ID
	})
}

// sameSiteAddr is the address a peer at the node's site dials.
func sameSiteAddr(n *db.RelayNode, peerPrivate bool) string {
	if peerPrivate {
		return n.LANAddr
	}
	return n.PublicAddr
}

// reachAddr is the address a peer at site peerSite dials to reach node n:
// the same-site rule at n's own site, n's public address from another site
// when the peer's address is public, "" otherwise.
func reachAddr(n *db.RelayNode, peerSite string, peerPrivate bool) string {
	if n.SiteID == peerSite {
		return sameSiteAddr(n, peerPrivate)
	}
	if peerPrivate {
		return ""
	}
	return n.PublicAddr
}

// revoked tells the revoke hook about nodes, outside the lock.
func (s *Service) revoked(nodeIDs []string) {
	if len(nodeIDs) == 0 {
		return
	}
	s.mu.Lock()
	fn := s.onRevoke
	s.mu.Unlock()
	if fn != nil {
		fn(nodeIDs)
	}
}

func (s *Service) site(id string) (*db.Site, error) {
	site, err := s.db.GetSite(id)
	if err != nil {
		return nil, err
	}
	if site == nil {
		return nil, fmt.Errorf("%w: site %s", ErrNotFound, id)
	}
	return site, nil
}
