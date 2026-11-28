package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/events"
)

// runEventLinkSuite joins two buses through links made by newLink and
// checks an event crosses once, marked remote, and doesn't come back.
func runEventLinkSuite(t *testing.T, newLink func(t *testing.T) events.Link, ready func(ctx context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, b := events.NewBus(), events.NewBus()
	a.Join(ctx, "pod-a", newLink(t))
	b.Join(ctx, "pod-b", newLink(t))
	subA := a.Subscribe("")
	subB := b.Subscribe("")
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	if err := ready(rctx); err != nil {
		t.Fatalf("links not ready: %v", err)
	}

	a.Publish(events.Event{Type: events.EventPeerOnline, Data: map[string]string{"id": "123456789"}})

	got := recv(t, subA.Ch)
	if got.Remote || got.Data["id"] != "123456789" {
		t.Fatalf("local subscriber got %+v", got)
	}
	got = recv(t, subB.Ch)
	if !got.Remote || got.Type != events.EventPeerOnline || got.Data["id"] != "123456789" {
		t.Fatalf("other pod got %+v, want the event marked remote", got)
	}
	// Neither side gets a second copy: no echo back to the publisher and no
	// re-forwarding of a remote event.
	select {
	case e := <-subA.Ch:
		t.Fatalf("publisher heard its own event again: %+v", e)
	case e := <-subB.Ch:
		t.Fatalf("other pod heard the event twice: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}

func recv(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no event arrived")
		return events.Event{}
	}
}

func TestMemEventLink(t *testing.T) {
	hub := events.NewMemHub()
	runEventLinkSuite(t, func(*testing.T) events.Link { return hub }, func(ctx context.Context) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	})
}

func TestValkeyEventLink(t *testing.T) {
	v := testValkey(t)
	l := NewEventLink(v)
	runEventLinkSuite(t, func(*testing.T) events.Link { return NewEventLink(v) }, func(ctx context.Context) error {
		return l.Ready(ctx, 2)
	})
}

// TestSettingsReachOnlyTheOtherInstance: a change published on one
// instance's settings bus is acted on by the other, not by itself.
func TestSettingsReachOnlyTheOtherInstance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := events.NewMemHub()
	a, b := events.NewBus(), events.NewBus()
	a.Join(ctx, "pod-a", hub)
	b.Join(ctx, "pod-b", hub)
	heardA, heardB := make(chan string, 4), make(chan string, 4)
	events.OnSettingsChanged(ctx, a, func(what string, _ map[string]string) { heardA <- what })
	events.OnSettingsChanged(ctx, b, func(what string, d map[string]string) { heardB <- what + ":" + d["code"] })
	time.Sleep(50 * time.Millisecond)

	events.SettingsChanged(a, events.SettingsI18n, map[string]string{"code": "de"})
	select {
	case got := <-heardB:
		if got != "i18n:de" {
			t.Fatalf("pod b heard %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pod b did not hear the change")
	}
	select {
	case got := <-heardA:
		t.Fatalf("pod a acted on its own change: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}
