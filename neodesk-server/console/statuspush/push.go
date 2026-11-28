// Package statuspush pushes what happens on the server to the browsers:
// the status of devices on /ws/device-status and registrations and help
// requests on /ws/panel-events (services/deviceStatusPush.js of the old
// console). It reads the API server's event bus in this process.
//
// The messages are the bytes the old console sent: the event as the API
// server's event socket writes it, read as JSON.parse read it and written
// as JSON.stringify writes it, with the same keys in the same order.
// Nothing is throttled. A message about a device goes only to the accounts
// whose device scope holds that device (DECISIONS A54, see Scope).
package statuspush

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/access"
	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/session"
	"github.com/neodeskapp/neodesk/neodesk-server/console/ws"
	"github.com/neodeskapp/neodesk/neodesk-server/logging"
)

// PingInterval is how often an open socket is pinged; one that left two
// pings in a row unanswered is dropped (ws.Conn.Heartbeat).
const PingInterval = 30 * time.Second

// scopeTimeout bounds the device scope checks of one message.
const scopeTimeout = 10 * time.Second

// Options configure the service.
type Options struct {
	// Events subscribes to the API server's event bus
	// (goapi.Client.Events). nil leaves the push off: the sockets take
	// browsers and never send anything, as when the old console had no
	// API address or key.
	Events func(ctx context.Context, filter string) (<-chan *goapi.Object, error)
	// Cascade carries a changed device id into the console's tables
	// (devicegroups.Service.CascadePeerIDChange); it logs its own errors.
	Cascade func(ctx context.Context, oldID, newID string)
	// Now stands in for Date.now() where an event has no timestamp.
	// Default time.Now.
	Now func() time.Time
	// PingInterval overrides PingInterval (tests).
	PingInterval time.Duration
	// SessionCheck is how often an open socket reads its session again.
	// Default 10 seconds.
	SessionCheck time.Duration
}

// Scope decides which accounts get a message (DECISIONS A54). A message
// about a device (its status, a new id, a help request, a decided
// registration) goes to the accounts whose device scope holds the device.
// A pending registration, and a message that names no device the console
// knows, go only to accounts whose scope has no limit. The zero Scope
// sends every message to every socket of its kind.
type Scope struct {
	// Device is the device of an id, deleted ones included; nil when
	// there is none (goapi.Backend.GetDeviceByID).
	Device func(ctx context.Context, id string) *goapi.Object
	// Of is the account's scope over a list of devices, nil without a
	// limit (devicegroups.Service.DeviceScopeForUser).
	Of func(ctx context.Context, user *session.User, devices []*goapi.Object) (*access.DeviceScope, error)
}

// Service holds the open sockets with the account of each.
type Service struct {
	opts Options

	mu     sync.Mutex
	scope  Scope
	status map[*ws.Conn]*session.User
	panel  map[*ws.Conn]*session.User
}

// New returns the service. Register adds its sockets; Run reads the
// event bus.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = PingInterval
	}
	if opts.SessionCheck <= 0 {
		opts.SessionCheck = 10 * time.Second
	}
	return &Service{opts: opts, status: map[*ws.Conn]*session.User{}, panel: map[*ws.Conn]*session.User{}}
}

// Register adds /ws/device-status and /ws/panel-events, whose messages
// follow scope.
//
// Both check the Origin first, as every other socket of the console does
// ("HTTP/1.1 403 Forbidden"), then take only a signed-in session ("401
// Unauthorized") whose role has a permission: device.view for the device
// status, enrollment.approve for the panel events ("403 Forbidden"; "503
// Service Unavailable" when the permission lookup fails). Messages the
// browser sends are read and dropped. Every SessionCheck an open socket
// reads its session again (watch).
func (s *Service) Register(reg *ws.Registry, scope Scope) {
	s.mu.Lock()
	s.scope = scope
	s.mu.Unlock()
	reg.Handle("/ws/device-status", func(u *ws.Upgrade) {
		s.serve(u, "device-status /ws/device-status", s.status, "device.view")
	})
	reg.Handle("/ws/panel-events", func(u *ws.Upgrade) {
		s.serve(u, "panel-events /ws/panel-events", s.panel, "enrollment.approve")
	})
}

// serve runs the checks of one socket and keeps it in clients, with its
// account, while it is open.
func (s *Service) serve(u *ws.Upgrade, label string, clients map[*ws.Conn]*session.User, permission string) {
	if !u.CheckOrigin(label) {
		return
	}
	session := u.Session()
	if !session.SignedIn() {
		u.Refuse(http.StatusUnauthorized)
		return
	}
	allowed, err := u.RoleHasPermission(session.UserRole(), permission)
	if err != nil {
		u.Refuse(http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		u.Refuse(http.StatusForbidden)
		return
	}
	conn, ok := u.Accept(ws.AcceptOptions{})
	if !ok {
		return
	}
	defer conn.Wait()
	s.mu.Lock()
	clients[conn] = session.User()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(clients, conn)
		s.mu.Unlock()
	}()
	conn.Heartbeat(s.opts.PingInterval)
	stopWatch := s.watch(u, conn, clients, session.UserID(), permission)
	defer stopWatch()
	_ = conn.Discard()
}

// watch reads the session of an open socket every SessionCheck and closes
// the socket (1008 "Session ended") once the account signed out, another
// account holds the session, or its role lost the permission (a failed
// lookup counts as lost). Otherwise the account of that read replaces the
// socket's, so the messages follow its current role and scope. stop ends
// the watch and waits for it.
func (s *Service) watch(u *ws.Upgrade, conn *ws.Conn, clients map[*ws.Conn]*session.User, userID int64, permission string) (stop func()) {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(s.opts.SessionCheck)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-conn.Context().Done():
				return
			case <-tick.C:
				user, ok := recheck(u, conn.Context(), userID, permission)
				if !ok {
					conn.Close(1008, "Session ended")
					return
				}
				s.mu.Lock()
				if _, open := clients[conn]; open {
					clients[conn] = user
				}
				s.mu.Unlock()
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// recheck reads a socket's session again and runs the upgrade's checks
// on it; user is the account of that read.
func recheck(u *ws.Upgrade, parent context.Context, userID int64, permission string) (user *session.User, ok bool) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	check := *u
	check.R = u.R.WithContext(ctx)
	sn := check.Session()
	if !sn.SignedIn() || sn.UserID() != userID {
		return nil, false
	}
	if allowed, err := check.RoleHasPermission(sn.UserRole(), permission); err != nil || !allowed {
		return nil, false
	}
	return sn.User(), true
}

// Clients counts the open sockets: device status and panel events.
func (s *Service) Clients() (status, panel int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.status), len(s.panel)
}

// recipient is an open socket and its account.
type recipient struct {
	conn *ws.Conn
	user *session.User
}

// broadcast sends text, the message msg written out, to the sockets of a
// kind whose account may see what msg is about; SendText skips one that
// is closing, as the old service skipped a socket that was not open.
func (s *Service) broadcast(ctx context.Context, clients map[*ws.Conn]*session.User, msg *goapi.Object, text string) {
	s.mu.Lock()
	scope := s.scope
	open := make([]recipient, 0, len(clients))
	for c, user := range clients {
		open = append(open, recipient{conn: c, user: user})
	}
	s.mu.Unlock()
	if len(open) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, scopeTimeout)
	defer cancel()
	sees := scope.audience(ctx, subject(msg))
	for _, r := range open {
		if sees(r.user) {
			r.conn.SendText(text)
		}
	}
}

// subject is the id of the device a message is about, "" for a pending
// registration and for a message that names no device.
func subject(msg *goapi.Object) string {
	if msg == nil {
		return ""
	}
	var id any
	switch msg.Get("type") {
	case "registration_pending":
		return ""
	case "registration_changed":
		if registration, ok := msg.Get("registration").(*goapi.Object); ok && registration != nil {
			id = registration.Get("device_id")
		}
	case "device_id_changed":
		id = msg.Get("new_id")
	default:
		id = msg.Get("device_id")
	}
	text, _ := id.(string)
	return text
}

// accountKey tells accounts apart for the checks of one message.
type accountKey struct {
	id             int64
	username, role string
}

// audience returns whether an account may see a message about deviceID
// ("" for none). Each account is checked once per message and the device
// is read once; a failed check sees nothing.
func (sc Scope) audience(ctx context.Context, deviceID string) func(user *session.User) bool {
	if sc.Of == nil {
		return func(*session.User) bool { return true }
	}
	var device *goapi.Object
	loaded := false
	seen := map[accountKey]bool{}
	return func(user *session.User) bool {
		if user == nil {
			return false
		}
		key := accountKey{user.ID, user.Username, user.Role}
		if v, ok := seen[key]; ok {
			return v
		}
		// Only an account with a limit needs the device
		v := sc.sees(ctx, user, deviceID, func() *goapi.Object {
			if !loaded {
				loaded = true
				if deviceID != "" && sc.Device != nil {
					device = sc.Device(ctx, deviceID)
				}
			}
			return device
		})
		seen[key] = v
		return v
	}
}

// sees reports whether the account sees a message about deviceID: always
// without a limit, else when deviceID is a device in its scope.
func (sc Scope) sees(ctx context.Context, user *session.User, deviceID string, device func() *goapi.Object) bool {
	scope, err := sc.Of(ctx, user, nil)
	if err != nil {
		logging.Logf("console", "WARN: [DeviceStatus] device scope of account %d: %v", user.ID, err)
		return false
	}
	if scope == nil {
		return true
	}
	if deviceID == "" {
		return false
	}
	d := device()
	if d == nil {
		return false
	}
	scope, err = sc.Of(ctx, user, []*goapi.Object{d})
	if err != nil {
		logging.Logf("console", "WARN: [DeviceStatus] device scope of account %d: %v", user.ID, err)
		return false
	}
	return scope.Has(deviceID)
}

// PublishPanelText sends a message that is already JSON text to the panel
// sockets whose account may see what it is about, as publishPanelEvent
// did for events the console made itself (a LAN registration request,
// its approval). Text that is no JSON object names no device.
func (s *Service) PublishPanelText(text string) {
	msg, _ := parseObject(text)
	s.broadcast(context.Background(), s.panel, msg, text)
}

// PublishPanelEvent sends v, written as JSON.stringify writes it, to the
// panel sockets as PublishPanelText does.
func (s *Service) PublishPanelEvent(v any) error {
	text, err := stringify(v)
	if err != nil {
		return err
	}
	s.PublishPanelText(text)
	return nil
}

// parseObject reads JSON text as an object, nil for anything else.
func parseObject(text string) (*goapi.Object, bool) {
	v, err := goapi.Parse([]byte(text))
	if err != nil {
		return nil, false
	}
	o, ok := v.(*goapi.Object)
	return o, ok && o != nil
}

// Run reads the event bus until ctx ends and pushes each event.
func (s *Service) Run(ctx context.Context) {
	if s.opts.Events == nil {
		logging.Logf("console", "WARN: [DeviceStatus] Go API URL or API key not configured, device status push disabled")
		return
	}
	frames, err := s.opts.Events(ctx, "")
	if err != nil {
		logging.Logf("console", "WARN: [DeviceStatus] device status push disabled: %v", err)
		return
	}
	for frame := range frames {
		s.Handle(ctx, frame)
	}
}

// Handle pushes one event of the bus: the frame the API server's event
// socket would have sent, parsed.
func (s *Service) Handle(ctx context.Context, frame any) {
	remote := isRemote(frame)
	if remote {
		// The mark stays out of what the sockets get
		frame.(*goapi.Object).Delete("remote")
	}
	out, ok := transform(frame, s.opts.Now)
	if !ok {
		return
	}
	// The instance that published a changed id carries it into the
	// tables; the others only push it to their sockets
	if out.cascade != nil && s.opts.Cascade != nil && !remote {
		s.opts.Cascade(ctx, out.cascade[0], out.cascade[1])
	}
	text, err := stringify(out.message)
	if err != nil {
		return
	}
	if out.panel {
		s.broadcast(ctx, s.panel, out.message, text)
	} else {
		s.broadcast(ctx, s.status, out.message, text)
	}
}

// isRemote reports whether frame came from another instance of the
// installation (goapi marks it "remote").
func isRemote(frame any) bool {
	o, ok := frame.(*goapi.Object)
	if !ok || o == nil {
		return false
	}
	r, _ := o.Get("remote").(bool)
	return r
}
