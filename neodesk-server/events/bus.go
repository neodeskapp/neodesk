// Package events provides a publish-subscribe event bus for real-time notifications.
// Admin clients can connect via WebSocket to receive live status changes, bans,
// blocklist updates, and other server events as they happen.
package events

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// EventType identifies the kind of event being broadcast.
type EventType string

const (
	EventPeerOnline        EventType = "peer_online"
	EventPeerDegraded      EventType = "peer_degraded"
	EventPeerCritical      EventType = "peer_critical"
	EventPeerOffline       EventType = "peer_offline"
	EventPeerBanned        EventType = "peer_banned"
	EventPeerUnbanned      EventType = "peer_unbanned"
	EventPeerDeleted       EventType = "peer_deleted"
	EventPeerRevoked       EventType = "peer_revoked"
	EventPeerRestored      EventType = "peer_restored"
	EventPeerIDChanged     EventType = "peer_id_changed"
	EventBlocklistAdd      EventType = "blocklist_add"
	EventBlocklistRemove   EventType = "blocklist_remove"
	EventServerStats       EventType = "server_stats"
	EventEnrollmentPending EventType = "enrollment_pending"
	EventHelpRequest       EventType = "help_request"
	// EventDeviceCredentialsEnded: a device's token was revoked or
	// replaced, its key replaced or the device banned; data "id" names the
	// device when it is known. The console closes the device's chat
	// sockets that lost their credential (L539).
	EventDeviceCredentialsEnded EventType = "device_credentials_ended"
	// EventSettingsChanged travels on the settings bus only (cluster
	// mode): data "what" names the settings an instance changed
	// (SettingsConfig and the others), "code" a language for SettingsI18n.
	EventSettingsChanged EventType = "settings_changed"
)

// What an EventSettingsChanged names.
const (
	SettingsConfig    = "config"    // server_config and the environment (reload)
	SettingsLDAP      = "ldap"      // the LDAP configuration
	SettingsOIDC      = "oidc"      // the OIDC configuration
	SettingsBlocklist = "blocklist" // the blocklist entries
	SettingsBranding  = "branding"  // the console's branding settings
	SettingsI18n      = "i18n"      // a language file
)

// SettingsChanged publishes on bus that what changed; a nil bus (one
// instance alone) does nothing.
func SettingsChanged(bus *Bus, what string, data map[string]string) {
	if bus == nil {
		return
	}
	d := map[string]string{"what": what}
	for k, v := range data {
		d[k] = v
	}
	bus.Publish(Event{Type: EventSettingsChanged, Data: d})
}

// OnSettingsChanged calls f for every change another instance published
// on bus, until ctx ends. A nil bus does nothing.
func OnSettingsChanged(ctx context.Context, bus *Bus, f func(what string, data map[string]string)) {
	if bus == nil {
		return
	}
	sub := bus.Subscribe(EventSettingsChanged)
	go func() {
		defer bus.Unsubscribe(sub)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub.Ch:
				if !ok {
					return
				}
				if ev.Remote {
					f(ev.Data["what"], ev.Data)
				}
			}
		}
	}()
}

// Event is a single notification pushed to subscribers.
type Event struct {
	Type      EventType         `json:"type"`
	Timestamp time.Time         `json:"timestamp"`
	Data      map[string]string `json:"data,omitempty"`
	// Remote is set on an event another instance of the installation
	// published (Bus.Join). A subscriber that acts on events, rather than
	// showing them, acts on the instance that holds what the event names.
	Remote bool `json:"-"`
}

// Subscriber is a channel that receives events.
type Subscriber struct {
	Ch     chan Event
	Filter EventType // empty string = all events
	id     int64
}

// Bus is the central event bus for broadcasting events to subscribers.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[int64]*Subscriber
	nextID      int64
	// origin and out are set by Join: events published here also go to
	// the other instances.
	origin string
	out    chan []byte
}

// NewBus creates a new event bus.
func NewBus() *Bus {
	return &Bus{
		subscribers: make(map[int64]*Subscriber),
	}
}

// Subscribe registers a new subscriber. If filter is non-empty, only events
// matching that type will be sent. The returned Subscriber should be closed
// with Unsubscribe when done.
func (b *Bus) Subscribe(filter EventType) *Subscriber {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	sub := &Subscriber{
		Ch:     make(chan Event, 64), // buffered to avoid blocking publishers
		Filter: filter,
		id:     b.nextID,
	}
	b.subscribers[sub.id] = sub
	return sub
}

// Unsubscribe removes a subscriber from the bus and closes its channel.
func (b *Bus) Unsubscribe(sub *Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[sub.id]; ok {
		delete(b.subscribers, sub.id)
		close(sub.Ch)
	}
}

// Publish sends an event to all matching subscribers.
// Non-blocking: if a subscriber's channel is full, the event is dropped for that subscriber.
func (b *Bus) Publish(evt Event) {
	evt.Timestamp = time.Now()
	evt.Remote = false
	b.deliver(evt)
	b.forward(evt)
}

func (b *Bus) deliver(evt Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		if sub.Filter != "" && sub.Filter != evt.Type {
			continue
		}
		select {
		case sub.Ch <- evt:
		default:
			// Subscriber is slow — drop event to avoid blocking
			log.Printf("[events] Dropped event %s for slow subscriber %d", evt.Type, sub.id)
		}
	}
}

// Count returns the number of active subscribers.
func (b *Bus) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// MarshalEvent serializes an event to JSON bytes.
func MarshalEvent(evt Event) []byte {
	data, _ := json.Marshal(evt)
	return data
}
