// Package security provides IP/ID blocklist management for the NeoDesk server.
// Supports loading from files, runtime additions, and both IP and ID blocking.
// Runtime additions are kept in the database (UseStore), so they survive a
// restart.
package security

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

// BlockType identifies what kind of entry is blocked.
type BlockType string

const (
	BlockIP  BlockType = "ip"
	BlockID  BlockType = "id"
	BlockNet BlockType = "cidr"
)

// BlockEntry represents a single blocklist entry.
type BlockEntry struct {
	Value     string    `json:"value"`      // IP, CIDR, or device ID
	Type      BlockType `json:"type"`       // ip, id, or cidr
	Reason    string    `json:"reason"`     // Why it was blocked
	CreatedAt time.Time `json:"created_at"` // When it was added
}

// Blocklist manages blocked IPs, CIDRs, and device IDs.
// Thread-safe — uses sync.RWMutex for concurrent access.
type Blocklist struct {
	mu       sync.RWMutex
	ips      map[string]*BlockEntry // Exact IP matches, keyed by ipKey
	ids      map[string]*BlockEntry // Device ID matches
	networks []*networkEntry        // CIDR network matches
	filePath string                 // Path to blocklist file (optional)
	store    Store                  // Keeps the runtime additions (optional)
	onChange func()                 // hears runtime changes (OnChange)
}

// Store keeps the entries added at run time (BlockIP, BlockID,
// BlockCIDR) and drops the removed ones; db.Database is one.
type Store interface {
	SaveBlocklistEntry(e *db.BlocklistEntry) error
	DeleteBlocklistEntry(kind, value string) error
	ListBlocklistEntries() ([]*db.BlocklistEntry, error)
}

type networkEntry struct {
	net   *net.IPNet
	entry *BlockEntry
}

// ipKey is the key of an address in the exact matches: the address in its
// one spelling (lower case, zeros shortened, IPv4 written as IPv6 as IPv4),
// so that an entry matches the client however either is written. Text that
// is no address is its own key.
func ipKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	return addr.Unmap().WithZone("").String()
}

// NewBlocklist creates a new empty blocklist.
func NewBlocklist() *Blocklist {
	return &Blocklist{
		ips: make(map[string]*BlockEntry),
		ids: make(map[string]*BlockEntry),
	}
}

// LoadFromFile loads entries from a blocklist file.
// File format: one entry per line, lines starting with # are comments.
// Lines can be: bare IPs, CIDR notation, or device IDs prefixed with "id:".
// Optional reason after a comma: "192.168.1.1,brute force attempt"
func (b *Blocklist) LoadFromFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist — not an error
		}
		return err
	}
	defer f.Close()

	b.mu.Lock()
	defer b.mu.Unlock()

	b.filePath = path
	scanner := bufio.NewScanner(f)
	loaded := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split value and optional reason
		value, reason := line, ""
		if idx := strings.Index(line, ","); idx >= 0 {
			value = strings.TrimSpace(line[:idx])
			reason = strings.TrimSpace(line[idx+1:])
		}

		entry := &BlockEntry{
			Value:     value,
			Reason:    reason,
			CreatedAt: time.Now(),
		}

		if strings.HasPrefix(value, "id:") {
			entry.Value = strings.TrimPrefix(value, "id:")
			entry.Type = BlockID
			b.ids[entry.Value] = entry
		} else if strings.Contains(value, "/") {
			_, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				log.Printf("[blocklist] Invalid CIDR %q: %v", value, err)
				continue
			}
			entry.Type = BlockNet
			b.networks = append(b.networks, &networkEntry{net: ipNet, entry: entry})
		} else {
			entry.Type = BlockIP
			b.ips[ipKey(value)] = entry
		}
		loaded++
	}

	if loaded > 0 {
		log.Printf("[blocklist] Loaded %d entries from %s", loaded, path)
	}
	return scanner.Err()
}

// UseStore adds the entries store keeps and from then on writes every
// entry added or removed at run time to it. A stored entry that is no
// valid network is left out with a log line.
func (b *Blocklist) UseStore(store Store) error {
	entries, err := store.ListBlocklistEntries()
	if err != nil {
		return fmt.Errorf("blocklist: load the stored entries: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.store = store
	for _, e := range entries {
		entry := &BlockEntry{Value: e.Value, Type: BlockType(e.Kind), Reason: e.Reason, CreatedAt: e.CreatedAt}
		switch entry.Type {
		case BlockIP:
			b.ips[ipKey(e.Value)] = entry
		case BlockID:
			b.ids[e.Value] = entry
		case BlockNet:
			_, ipNet, err := net.ParseCIDR(e.Value)
			if err != nil {
				log.Printf("[blocklist] Stored CIDR %q left out: %v", e.Value, err)
				continue
			}
			b.networks = append(b.networks, &networkEntry{net: ipNet, entry: entry})
		}
	}
	return nil
}

// keep writes a runtime addition to the store, when there is one. The
// caller holds b.mu.
func (b *Blocklist) keep(e *BlockEntry) error {
	if b.store == nil {
		return nil
	}
	return b.store.SaveBlocklistEntry(&db.BlocklistEntry{Kind: string(e.Type), Value: e.Value, Reason: e.Reason, CreatedAt: e.CreatedAt})
}

// drop removes an entry from the store, when there is one. A failure is
// logged: the entry is gone until the next start. The caller holds b.mu.
func (b *Blocklist) drop(t BlockType, value string) {
	if b.store == nil {
		return
	}
	if err := b.store.DeleteBlocklistEntry(string(t), value); err != nil {
		log.Printf("[blocklist] %s %s stays in the database: %v", t, value, err)
	}
}

// IsIPBlocked checks if an IP address is blocked (exact or CIDR match).
func (b *Blocklist) IsIPBlocked(ipStr string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Strip port if present
	host, _, err := net.SplitHostPort(ipStr)
	if err != nil {
		host = ipStr
	}

	// Exact match
	if _, ok := b.ips[ipKey(host)]; ok {
		return true
	}

	// CIDR match
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, ne := range b.networks {
		if ne.net.Contains(ip) {
			return true
		}
	}

	return false
}

// IsIDBlocked checks if a device ID is blocked.
func (b *Blocklist) IsIDBlocked(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.ids[id]
	return ok
}

// IDEntry returns a copy of the entry that blocks a device ID.
func (b *Blocklist) IDEntry(id string) (BlockEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e, ok := b.ids[id]
	if !ok {
		return BlockEntry{}, false
	}
	return *e, true
}

// RevokedReason is the start of the reason of an entry a device revoke
// made ("revoked via panel", "revoked via cascade").
const RevokedReason = "revoked"

// BlockIP adds an IP to the blocklist at runtime and to the store. When
// the store fails, the IP is not blocked.
func (b *Blocklist) BlockIP(ip, reason string) error {
	defer b.changed()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := &BlockEntry{
		Value:     ip,
		Type:      BlockIP,
		Reason:    reason,
		CreatedAt: time.Now(),
	}
	if err := b.keep(entry); err != nil {
		return err
	}
	// The same address in another spelling is the same entry
	if old, ok := b.ips[ipKey(ip)]; ok && old.Value != ip {
		b.drop(BlockIP, old.Value)
	}
	b.ips[ipKey(ip)] = entry
	return nil
}

// BlockID adds a device ID to the blocklist at runtime and to the store.
// When the store fails, the ID is not blocked.
func (b *Blocklist) BlockID(id, reason string) error {
	defer b.changed()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := &BlockEntry{
		Value:     id,
		Type:      BlockID,
		Reason:    reason,
		CreatedAt: time.Now(),
	}
	if err := b.keep(entry); err != nil {
		return err
	}
	b.ids[id] = entry
	return nil
}

// BlockCIDR adds a CIDR network to the blocklist at runtime and to the
// store. When the store fails, the network is not blocked.
func (b *Blocklist) BlockCIDR(cidr, reason string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	defer b.changed()
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := &BlockEntry{
		Value:     cidr,
		Type:      BlockNet,
		Reason:    reason,
		CreatedAt: time.Now(),
	}
	if err := b.keep(entry); err != nil {
		return err
	}
	b.networks = append(b.networks, &networkEntry{net: ipNet, entry: entry})
	return nil
}

// UnblockIP removes an IP from the blocklist.
func (b *Blocklist) UnblockIP(ip string) bool {
	defer b.changed()
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.ips[ipKey(ip)]; ok {
		delete(b.ips, ipKey(ip))
		b.drop(BlockIP, e.Value)
		return true
	}
	return false
}

// UnblockID removes a device ID from the blocklist.
func (b *Blocklist) UnblockID(id string) bool {
	defer b.changed()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.ids[id]; ok {
		delete(b.ids, id)
		b.drop(BlockID, id)
		return true
	}
	return false
}

// UnblockCIDR removes a CIDR from the blocklist.
func (b *Blocklist) UnblockCIDR(cidr string) bool {
	defer b.changed()
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, ne := range b.networks {
		if ne.entry.Value == cidr {
			b.networks = append(b.networks[:i], b.networks[i+1:]...)
			b.drop(BlockNet, cidr)
			return true
		}
	}
	return false
}

// List returns all current blocklist entries.
func (b *Blocklist) List() []BlockEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var entries []BlockEntry
	for _, e := range b.ips {
		entries = append(entries, *e)
	}
	for _, e := range b.ids {
		entries = append(entries, *e)
	}
	for _, ne := range b.networks {
		entries = append(entries, *ne.entry)
	}
	return entries
}

// Count returns the total number of blocklist entries.
func (b *Blocklist) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.ips) + len(b.ids) + len(b.networks)
}

// SaveToFile writes the current blocklist to the file.
func (b *Blocklist) SaveToFile(path string) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if path == "" {
		path = b.filePath
	}
	if path == "" {
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	w.WriteString("# NeoDesk Blocklist (auto-generated)\n")
	w.WriteString("# Format: IP, CIDR, or id:DEVICE_ID [,reason]\n\n")

	for _, e := range b.ips {
		line := e.Value
		if e.Reason != "" {
			line += "," + e.Reason
		}
		w.WriteString(line + "\n")
	}
	for _, ne := range b.networks {
		line := ne.entry.Value
		if ne.entry.Reason != "" {
			line += "," + ne.entry.Reason
		}
		w.WriteString(line + "\n")
	}
	for _, e := range b.ids {
		line := "id:" + e.Value
		if e.Reason != "" {
			line += "," + e.Reason
		}
		w.WriteString(line + "\n")
	}

	return w.Flush()
}

// OnChange sets who hears that an entry was added or removed at run time
// (the other instances of the installation reload then).
func (b *Blocklist) OnChange(f func()) {
	b.mu.Lock()
	b.onChange = f
	b.mu.Unlock()
}

func (b *Blocklist) changed() {
	b.mu.RLock()
	f := b.onChange
	b.mu.RUnlock()
	if f != nil {
		f()
	}
}

// Reload reads the blocklist again from its file and its store: another
// instance changed the entries. The new entries replace the old ones at
// once, so no check runs against a half-loaded list.
func (b *Blocklist) Reload() error {
	b.mu.RLock()
	path, store := b.filePath, b.store
	b.mu.RUnlock()
	fresh := NewBlocklist()
	if path != "" {
		if err := fresh.LoadFromFile(path); err != nil {
			return err
		}
	}
	if store != nil {
		if err := fresh.UseStore(store); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.ips, b.ids, b.networks = fresh.ips, fresh.ids, fresh.networks
	b.mu.Unlock()
	return nil
}
