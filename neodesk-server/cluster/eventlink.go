package cluster

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// EventLink carries console events between instances over a Valkey
// pub/sub channel (<prefix>events). It implements events.Link. Pub/sub
// keeps nothing: an instance that is away while an event goes out misses
// it, as a socket that reconnects does today.
type EventLink struct {
	rdb     redis.UniversalClient
	channel string
}

// NewEventLink returns the link on v's channel.
func NewEventLink(v *Valkey) *EventLink { return NewEventLinkOn(v.Client, v.Prefix()) }

// NewSettingsLink returns the link of the settings bus (<prefix>settings):
// which settings another instance changed, so this one reads them again.
// It is a bus of its own, so these signals never reach the console's
// event sockets.
func NewSettingsLink(v *Valkey) *EventLink {
	return &EventLink{rdb: v.Client, channel: v.Prefix() + "settings"}
}

// NewEventLinkOn returns the link on client with key prefix prefix.
func NewEventLinkOn(client redis.UniversalClient, prefix string) *EventLink {
	return &EventLink{rdb: client, channel: prefix + "events"}
}

func (l *EventLink) Publish(ctx context.Context, payload []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return l.rdb.Publish(ctx, l.channel, payload).Err()
}

// Run subscribes and delivers until ctx ends. go-redis reconnects a lost
// subscription by itself.
func (l *EventLink) Run(ctx context.Context, deliver func([]byte)) {
	ps := l.rdb.Subscribe(ctx, l.channel)
	defer ps.Close()
	ch := ps.Channel(redis.WithChannelSize(1024))
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			deliver([]byte(m.Payload))
		}
	}
}

// Ready waits until the subscription is live, so a test publishes only
// after the other side listens.
func (l *EventLink) Ready(ctx context.Context, want int64) error {
	for {
		n, err := l.rdb.PubSubNumSub(ctx, l.channel).Result()
		if err != nil {
			return err
		}
		if n[l.channel] >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
