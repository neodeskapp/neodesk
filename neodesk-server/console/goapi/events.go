package goapi

import (
	"context"
	"errors"

	"github.com/neodeskapp/neodesk/neodesk-server/events"
)

// ErrNoEventBus is returned by Events when the client has no event bus.
var ErrNoEventBus = errors.New("goapi: no event bus")

// Events subscribes to the server's event bus and returns the events as
// the frames the API server sends on GET /api/ws/events: one object per
// event with "type", "timestamp" (RFC 3339 text) and, when the event has
// any, "data" (a flat object of texts).
//
// A filter other than "" keeps one event type. The channel is closed when
// ctx ends. The bus keeps up to 64 events for a subscriber that reads too
// slowly and drops what comes after; there is no replay.
func (c *Client) Events(ctx context.Context, filter string) (<-chan *Object, error) {
	if c.bus == nil {
		return nil, ErrNoEventBus
	}
	sub := c.bus.Subscribe(events.EventType(filter))
	frames := make(chan *Object)
	go func() {
		defer close(frames)
		defer c.bus.Unsubscribe(sub)
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-sub.Ch:
				if !ok {
					return
				}
				frame, err := Parse(events.MarshalEvent(evt))
				if err != nil {
					continue
				}
				obj, _ := frame.(*Object)
				if obj != nil && evt.Remote {
					// Another instance published it: services that act on
					// events (rather than show them) leave it to that one
					obj.Set("remote", true)
				}
				select {
				case frames <- obj:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return frames, nil
}
