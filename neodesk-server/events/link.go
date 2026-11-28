package events

import (
	"context"
	"encoding/json"
	"log"
	"sync"
)

// Link carries events between the buses of the instances that share one
// installation, so a socket held by one instance hears what happened on
// another. Publish sends one encoded event to every instance; Run hands
// each event that arrives to deliver until ctx ends.
type Link interface {
	Publish(ctx context.Context, payload []byte) error
	Run(ctx context.Context, deliver func(payload []byte))
}

// envelope is an event on a Link, with the instance that published it so
// that instance doesn't deliver it twice.
type envelope struct {
	Origin string `json:"o"`
	Event  Event  `json:"e"`
}

// Join connects the bus to the other instances through link until ctx
// ends: every event published here is also sent there, and events from
// there reach the local subscribers with Remote set. origin names this
// instance. Sending runs on its own goroutine so Publish never waits on
// the network; when the queue is full an event is dropped and logged,
// as for a slow subscriber.
func (b *Bus) Join(ctx context.Context, origin string, link Link) {
	out := make(chan []byte, 1024)
	b.mu.Lock()
	b.origin = origin
	b.out = out
	b.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-out:
				if err := link.Publish(ctx, p); err != nil && ctx.Err() == nil {
					log.Printf("[events] Could not send an event to the other instances: %v", err)
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		link.Run(ctx, func(p []byte) {
			var env envelope
			if err := json.Unmarshal(p, &env); err != nil || env.Origin == origin {
				return
			}
			env.Event.Remote = true
			b.deliver(env.Event)
		})
	}()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		b.out = nil
		b.mu.Unlock()
		wg.Wait()
	}()
}

func (b *Bus) forward(evt Event) {
	b.mu.RLock()
	out, origin := b.out, b.origin
	b.mu.RUnlock()
	if out == nil || evt.Remote {
		return
	}
	p, err := json.Marshal(envelope{Origin: origin, Event: evt})
	if err != nil {
		return
	}
	select {
	case out <- p:
	default:
		log.Printf("[events] Dropped event %s for the other instances, the queue is full", evt.Type)
	}
}

// MemHub is a Link between buses in one process, for tests.
type MemHub struct {
	mu   sync.Mutex
	subs map[int]func([]byte)
	next int
}

func NewMemHub() *MemHub { return &MemHub{subs: map[int]func([]byte){}} }

func (h *MemHub) Publish(_ context.Context, p []byte) error {
	h.mu.Lock()
	subs := make([]func([]byte), 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s(p)
	}
	return nil
}

func (h *MemHub) Run(ctx context.Context, deliver func([]byte)) {
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = deliver
	h.mu.Unlock()
	<-ctx.Done()
	h.mu.Lock()
	delete(h.subs, id)
	h.mu.Unlock()
}
