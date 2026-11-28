package statuspush

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/ws"
	"github.com/neodeskapp/neodesk/neodesk-server/console/ws/wstest"
	"github.com/neodeskapp/neodesk/neodesk-server/events"
)

// recording is testdata/node_push.json, recorded from the old console's
// deviceStatusPush.js (testdata/README.md).
type recording struct {
	Now    int64 `json:"now"`
	Events []struct {
		Frame    string      `json:"frame"`
		Status   []string    `json:"status"`
		Panel    []string    `json:"panel"`
		Cascades [][2]string `json:"cascades"`
	} `json:"events"`
	Handshakes []struct {
		Name    string `json:"name"`
		Request string `json:"request"`
		Answer  string `json:"answer"`
	} `json:"handshakes"`
}

func load(t *testing.T) recording {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "node_push.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec recording
	if err := json.Unmarshal(wstest.Expand(raw), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// oraclePermission is the permission stub of the oracle, with device.view,
// which the oracle never asked for, for the roles that see devices.
func oraclePermission(_ context.Context, role, permission string) (bool, error) {
	if role == "broken" {
		return false, context.DeadlineExceeded
	}
	switch permission {
	case "enrollment.approve":
		switch role {
		case "admin", "super_admin", "global_admin", "allowed":
			return true, nil
		}
	case "device.view":
		switch role {
		case "admin", "super_admin", "global_admin", "allowed", "operator", "viewer":
			return true, nil
		}
	}
	return false, nil
}

// helpRequestTypeFix is what WS11 changes in a recorded help request whose
// data has a type: Node's message with the type set back to
// help_request. ok is false for every other event.
func helpRequestTypeFix(t *testing.T, frame *goapi.Object, panel []string) ([]string, bool) {
	t.Helper()
	data, _ := frame.Get("data").(*goapi.Object)
	if frame.Get("type") != "help_request" || data == nil || !data.Has("type") {
		return panel, false
	}
	out := make([]string, len(panel))
	for i, text := range panel {
		msg := parse(t, text)
		msg.Set("type", "help_request")
		var err error
		if out[i], err = stringify(msg); err != nil {
			t.Fatal(err)
		}
	}
	return out, true
}

// TestNodeEvents replays every event frame of the recording: the message
// each kind of socket got from Node, byte for byte, and the device id
// changes carried into the database. A help request whose data has a type
// keeps the type help_request since WS11 (helpRequestTypeFix).
func TestNodeEvents(t *testing.T) {
	rec := load(t)
	now := func() time.Time { return time.UnixMilli(rec.Now) }
	if len(rec.Events) < 1000 {
		t.Fatalf("only %d events recorded", len(rec.Events))
	}
	failed, fixed := 0, 0
	for i, e := range rec.Events {
		frame, err := goapi.Parse([]byte(e.Frame))
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if o, ok := frame.(*goapi.Object); ok && o != nil {
			var changed bool
			if e.Panel, changed = helpRequestTypeFix(t, o, e.Panel); changed {
				fixed++
			}
		}
		var status, panel []string
		var cascades [][2]string
		if out, ok := transform(frame, now); ok {
			text, err := stringify(out.message)
			if err != nil {
				t.Fatalf("event %d: %v", i, err)
			}
			if out.panel {
				panel = append(panel, text)
			} else {
				status = append(status, text)
			}
			if out.cascade != nil {
				cascades = append(cascades, *out.cascade)
			}
		}
		if strings.Join(status, "\n") != strings.Join(e.Status, "\n") || strings.Join(panel, "\n") != strings.Join(e.Panel, "\n") ||
			len(cascades) != len(e.Cascades) || (len(cascades) == 1 && cascades[0] != e.Cascades[0]) {
			if failed++; failed <= 10 {
				t.Errorf("event %d %s\n node: status %q panel %q cascades %q\n   go: status %q panel %q cascades %q",
					i, e.Frame, e.Status, e.Panel, e.Cascades, status, panel, cascades)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d events differ", failed, len(rec.Events))
	}
	if fixed == 0 {
		t.Error("no recorded help request has a type in its data: the WS11 fix changes nothing here")
	}
}

// TestFramesAreTheEventBus checks that the recorded frames are what the
// event bus writes for such an event, so the replay covers what the
// service gets in this process.
func TestFramesAreTheEventBus(t *testing.T) {
	rec := load(t)
	checked := 0
	for i, e := range rec.Events {
		var f struct {
			Type      string            `json:"type"`
			Timestamp string            `json:"timestamp"`
			Data      map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(e.Frame), &f); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		ts, err := time.Parse(time.RFC3339Nano, f.Timestamp)
		if err != nil {
			continue
		}
		got := string(events.MarshalEvent(events.Event{Type: events.EventType(f.Type), Timestamp: ts, Data: f.Data}))
		if got != e.Frame {
			t.Errorf("event %d: the bus writes\n%s\nthe recording has\n%s", i, got, e.Frame)
		}
		checked++
	}
	if checked < 1000 {
		t.Errorf("only %d frames checked", checked)
	}
}

func newServer(t *testing.T, opts Options) (*Service, *ws.Registry, *httptest.Server) {
	t.Helper()
	reg := ws.New(ws.Options{Session: wstest.OracleSession, Permission: oraclePermission})
	svc := New(opts)
	svc.Register(reg, Scope{})
	srv := httptest.NewServer(reg)
	t.Cleanup(func() {
		srv.Close()
		reg.Close()
	})
	return svc, reg, srv
}

// handshakeFix is a recorded upgrade that a bug fix answers differently on
// purpose, with the answer Go gives since the fix.
type handshakeFix struct {
	bug, name, answer string
}

// handshakeFixes are the upgrades the bug fixes changed. The tests of each
// fix pin what it does; here they say only what the replay sees instead.
var handshakeFixes = []handshakeFix{
	// Both sockets check the Origin first, as every other socket does
	{"I6 WS1", "/ws/device-status foreign origin", "HTTP/1.1 403 Forbidden\r\n\r\n"},
	{"I6 WS1", "/ws/panel-events foreign origin", "HTTP/1.1 403 Forbidden\r\n\r\n"},
	// The device status socket asks for device.view of
	// req.session.user.role: a failed lookup is 503, no such role 403
	{"I6", "/ws/device-status as broken", "HTTP/1.1 503 Service Unavailable\r\n\r\n"},
	{"I6", "/ws/device-status as null", "HTTP/1.1 403 Forbidden\r\n\r\n"},
	{"I6", "/ws/device-status role number", "HTTP/1.1 403 Forbidden\r\n\r\n"},
	{"I6", "/ws/device-status session role only", "HTTP/1.1 403 Forbidden\r\n\r\n"},
}

// TestNodeHandshakes sends the recorded upgrade requests and compares the
// raw answers: refusals byte for byte, the handshake by its headers. An
// upgrade a bug fix changed is compared with its fix's answer.
func TestNodeHandshakes(t *testing.T) {
	rec := load(t)
	_, _, srv := newServer(t, Options{})
	addr := strings.TrimPrefix(srv.URL, "http://")
	fixed := map[string]handshakeFix{}
	for _, f := range handshakeFixes {
		fixed[f.name] = f
	}
	for _, h := range rec.Handshakes {
		want := h.Answer
		if f, ok := fixed[h.Name]; ok {
			if wstest.SameAnswer(want, f.answer) {
				t.Errorf("the %s fix of %q changes nothing", f.bug, h.Name)
			}
			want = f.answer
			delete(fixed, h.Name)
		}
		got, err := wstest.Raw(addr, h.Request)
		if err != nil {
			t.Fatalf("%s: %v", h.Name, err)
		}
		if !wstest.SameAnswer(want, got) {
			t.Errorf("%s\n want: %q\n   go: %q", h.Name, want, got)
		}
	}
	for name, f := range fixed {
		t.Errorf("the %s fix names %q, which the recording does not have", f.bug, name)
	}
}

// fakeBus hands frames to the service as the event bus subscription does.
type fakeBus struct {
	mu     sync.Mutex
	frames chan *goapi.Object
	filter string
}

func (b *fakeBus) events(ctx context.Context, filter string) (<-chan *goapi.Object, error) {
	b.mu.Lock()
	b.filter = filter
	b.mu.Unlock()
	out := make(chan *goapi.Object)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-b.frames:
				select {
				case out <- f:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func parse(t *testing.T, text string) *goapi.Object {
	t.Helper()
	v, err := goapi.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*goapi.Object)
}

// TestPushToOpenSockets runs the service with sockets: events reach the
// sockets of their kind, a closed socket is dropped, pings go out, the
// device id change reaches the database before the message, and every
// goroutine ends with the console.
func TestPushToOpenSockets(t *testing.T) {
	check := wstest.Goroutines(t)
	bus := &fakeBus{frames: make(chan *goapi.Object)}
	var cascaded [][2]string
	var mu sync.Mutex
	reg := ws.New(ws.Options{Session: wstest.OracleSession, Permission: oraclePermission})
	svc := New(Options{
		Events:       bus.events,
		PingInterval: 40 * time.Millisecond,
		Cascade: func(_ context.Context, a, b string) {
			mu.Lock()
			cascaded = append(cascaded, [2]string{a, b})
			mu.Unlock()
		},
	})
	svc.Register(reg, Scope{})
	srv := httptest.NewServer(reg)
	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(ran)
	}()

	admin := map[string][]string{"X-Oracle-Session": {`{"userId":1,"user":{"role":"admin"}}`}}
	status, _, err := wstest.Dial(addr, "/ws/device-status", admin)
	if err != nil || status == nil {
		t.Fatalf("device-status: %v", err)
	}
	panel, _, err := wstest.Dial(addr, "/ws/panel-events", admin)
	if err != nil || panel == nil {
		t.Fatalf("panel-events: %v", err)
	}
	waitClients(t, svc, 1, 1)

	bus.frames <- parse(t, `{"type":"peer_id_changed","timestamp":"t1","data":{"new_id":"B","old_id":"A","source":"client"}}`)
	bus.frames <- parse(t, `{"type":"enrollment_pending","timestamp":"t2","data":{"device_id":"D"}}`)
	bus.frames <- parse(t, `{"type":"peer_banned","timestamp":"t3","data":{"id":"X"}}`)
	// The bus's events are handled on their own goroutine: the message
	// published here comes after the bus's only once that one is out.
	if !panel.Wait(2*time.Second, func(f []wstest.Frame, _ bool) bool { return len(f) >= 1 }) {
		t.Fatal("no panel message from the bus")
	}
	svc.PublishPanelText(`{"type":"registration_changed","registration":{"id":1},"timestamp":5}`)

	if !status.Wait(2*time.Second, func(f []wstest.Frame, _ bool) bool { return len(f) >= 1 }) {
		t.Fatal("no status message")
	}
	if got := status.Frames()[0].Text(); got != `{"type":"device_id_changed","old_id":"A","new_id":"B","source":"client","timestamp":"t1"}` {
		t.Errorf("status message %s", got)
	}
	if !panel.Wait(2*time.Second, func(f []wstest.Frame, _ bool) bool { return len(f) >= 2 }) {
		t.Fatal("panel messages missing")
	}
	got := panel.Frames()
	if got[0].Text() != `{"type":"registration_pending","registration":{"device_id":"D"},"timestamp":"t2"}` ||
		got[1].Text() != `{"type":"registration_changed","registration":{"id":1},"timestamp":5}` {
		t.Errorf("panel messages %q %q", got[0].Text(), got[1].Text())
	}
	mu.Lock()
	if len(cascaded) != 1 || cascaded[0] != [2]string{"A", "B"} {
		t.Errorf("cascades %v", cascaded)
	}
	mu.Unlock()
	if bus.filter != "" {
		t.Errorf("subscribed with filter %q", bus.filter)
	}
	if !status.Wait(2*time.Second, func([]wstest.Frame, bool) bool { return status.Pings() >= 2 }) {
		t.Error("no pings")
	}
	// The browser's messages are dropped; the socket stays open.
	_ = status.Text("anything")
	time.Sleep(50 * time.Millisecond)
	if closed, _, _ := status.Closed(); closed {
		t.Error("a browser message closed the socket")
	}
	status.Close(1000)
	waitClients(t, svc, 0, 1)
	svc.PublishPanelText(`{"n":2}`)
	if !panel.Wait(2*time.Second, func(f []wstest.Frame, _ bool) bool { return len(f) == 3 }) {
		t.Error("panel did not get the message after the other socket closed")
	}

	cancel()
	<-ran
	srv.Close()
	reg.Close()
	if !panel.Wait(2*time.Second, func(_ []wstest.Frame, closed bool) bool { return closed }) {
		t.Error("the panel socket stayed open after the console stopped")
	}
	panel.Drop()
	check()
}

// TestMessageSizeLimit: the sockets take what the library took by default
// (100 MiB); one byte more closes with 1009 and no reason.
func TestMessageSizeLimit(t *testing.T) {
	_, _, srv := newServer(t, Options{})
	addr := strings.TrimPrefix(srv.URL, "http://")
	c, _, err := wstest.Dial(addr, "/ws/device-status", map[string][]string{"X-Oracle-Session": {`{"userId":1,"user":{"role":"viewer"}}`}})
	if err != nil || c == nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Drop()
	big := make([]byte, ws.DefaultMaxPayload)
	if err := c.Binary(big); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if closed, _, _ := c.Closed(); closed {
		t.Fatal("a message at the limit closed the socket")
	}
	if err := c.Binary(append(big, 1)); err != nil {
		t.Fatal(err)
	}
	if !c.Wait(90*time.Second, func(_ []wstest.Frame, closed bool) bool { return closed }) {
		t.Fatal("a message over the limit did not close the socket")
	}
	if _, code, reason := c.Closed(); code != 1009 || reason != "" {
		t.Errorf("closed with %d %q, want 1009 and no reason", code, reason)
	}
}

func waitClients(t *testing.T, svc *Service, status, panel int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, p := svc.Clients()
		if s == status && p == panel {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("clients %d %d, want %d %d", s, p, status, panel)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FuzzTransform feeds event frames to the transform: nothing panics, and
// every message is a JSON object of a known kind.
func FuzzTransform(f *testing.F) {
	for _, s := range []string{
		`{"type":"peer_online","timestamp":"t","data":{"id":"a"}}`,
		`{"type":"peer_id_changed","data":{"old_id":"a","new_id":"b"}}`,
		`{"type":"help_request","data":{"1":"x","type":"y","timestamp":"z"}}`,
		`{"type":"device_pending","data":"text"}`, `null`, `[]`, `{"type":"peer_offline","data":[1,{"a":null}]}`,
	} {
		f.Add([]byte(s))
	}
	now := func() time.Time { return time.UnixMilli(1) }
	f.Fuzz(func(t *testing.T, frame []byte) {
		v, err := goapi.Parse(frame)
		if err != nil {
			return
		}
		out, ok := transform(v, now)
		if !ok {
			return
		}
		text, err := stringify(out.message)
		if err != nil {
			return
		}
		parsed, err := goapi.Parse([]byte(text))
		if err != nil {
			t.Fatalf("%s gave %s: %v", frame, text, err)
		}
		switch parsed.(*goapi.Object).Str("type") {
		case "device_id_changed", "device_status", "registration_pending", "registration_changed":
		default:
			if !out.panel {
				t.Fatalf("%s gave %s", frame, text)
			}
		}
	})
}

// TestBrowserThatStopsAnsweringIsDropped: a socket whose pings went
// unanswered twice in a row is dropped and gets no more messages.
func TestBrowserThatStopsAnsweringIsDropped(t *testing.T) {
	check := wstest.Goroutines(t)
	svc, reg, srv := newServer(t, Options{PingInterval: 50 * time.Millisecond})
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, handshake("/ws/device-status", `{"userId":1,"user":{"role":"admin"}}`, ""))
	if head, err := bufio.NewReader(conn).ReadString('\n'); err != nil || !strings.HasPrefix(head, "HTTP/1.1 101") {
		t.Fatalf("%q %v", head, err)
	}
	waitClients(t, svc, 1, 0)
	waitClients(t, svc, 0, 0)
	srv.Close()
	reg.Close()
	check()
}

// TestARemoteIDChangeIsNotCascadedAgain: with several instances every one
// hears a changed id; only the one that published it rewrites the tables.
func TestARemoteIDChangeIsNotCascadedAgain(t *testing.T) {
	var cascaded [][2]string
	svc := New(Options{Cascade: func(_ context.Context, a, b string) { cascaded = append(cascaded, [2]string{a, b}) }})
	frame := func(remote bool) any {
		text := `{"type":"peer_id_changed","timestamp":"2026-11-28T10:00:00Z","data":{"old_id":"111","new_id":"222"}`
		if remote {
			text += `,"remote":true`
		}
		v, err := goapi.Parse([]byte(text + "}"))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	svc.Handle(context.Background(), frame(true))
	if len(cascaded) != 0 {
		t.Fatalf("remote change cascaded: %v", cascaded)
	}
	svc.Handle(context.Background(), frame(false))
	if len(cascaded) != 1 || cascaded[0] != [2]string{"111", "222"} {
		t.Fatalf("local change cascaded %v, want once 111->222", cascaded)
	}
}
