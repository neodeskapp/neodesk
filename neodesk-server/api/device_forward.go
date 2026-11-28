package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	eventsModule "github.com/neodeskapp/neodesk/neodesk-server/events"
)

// When several instances share the installation, a CDAP device or a
// MeshCentral agent keeps its socket on one of them. A console request for
// the device that lands on another instance is passed to the one holding
// it: the whole HTTP request, websocket upgrades included, goes over a
// signed pod-to-pod stream, with the caller this instance authenticated.
// The holding instance runs the same route with that caller, its checks
// included, and answers through the stream.

// DeviceRouting is what the API server needs from the cluster to pass
// device requests on. Nil (the default) keeps every request here.
type DeviceRouting interface {
	// Owner returns the instance that holds device id of kind ("cdap" or
	// "mesh"), "" when this instance does or none does.
	Owner(ctx context.Context, kind, id string) string
	// Stream opens a stream to the forward handler of pod.
	Stream(ctx context.Context, pod, name string, meta map[string]string) (net.Conn, error)
	// ClaimOnce reports whether nonce is new; a replayed stream is refused.
	ClaimOnce(ctx context.Context, nonce string) bool
	// Call runs the device call name (DeviceCalls) on pod.
	Call(ctx context.Context, pod, name string, body []byte) ([]byte, error)
}

// ForwardStream is the name of the cluster stream that carries requests.
const ForwardStream = "api.forward"

type forwardedCtxKey struct{}

// forwardedCaller is the caller of a passed-on request.
type forwardedCaller struct {
	Username string `json:"u"`
	Role     string `json:"r"`
	OrgID    string `json:"o,omitempty"`
	APIKey   bool   `json:"k,omitempty"`
	// Internal marks a request that came through InternalHandler on the
	// first instance, with the Actor and Token of its Identity.
	Internal bool   `json:"i,omitempty"`
	Actor    string `json:"a,omitempty"`
	Token    string `json:"t,omitempty"`
	Remote   string `json:"ra,omitempty"`
}

// SetSettingsBus sets where the server says which settings it changed, so
// the other instances read them again (cluster mode).
func (s *Server) SetSettingsBus(bus *eventsModule.Bus) { s.settingsBus = bus }

// statusRecorder notes the status a handler answered with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// broadcasts tells the other instances that what changed once handler has
// answered with success. Alone it is handler.
func (s *Server) broadcasts(what string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.settingsBus == nil {
			handler(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		handler(rec, r)
		if rec.status != 0 && rec.status < 400 {
			eventsModule.SettingsChanged(s.settingsBus, what, nil)
		}
	}
}

// SetDeviceRouting turns passing device requests on (cluster mode).
func (s *Server) SetDeviceRouting(r DeviceRouting) {
	s.deviceRouting = r
	if s.meshGw != nil && r != nil {
		s.meshGw.SetRelayRouter(func(w http.ResponseWriter, req *http.Request, relayID string) bool {
			if wasForwarded(req) {
				return false
			}
			pod := r.Owner(req.Context(), "meshrelay", relayID)
			if pod == "" {
				return false
			}
			s.forwardRequest(w, req, pod)
			return true
		})
	}
}

// wasForwarded reports whether r was passed on from another instance; it is
// then served here and never passed on again.
func wasForwarded(r *http.Request) bool { return r.Context().Value(forwardedCtxKey{}) != nil }

// atDevice serves a route about one device (the path value "id") on the
// instance that holds the device's socket. kinds are the gateways that may
// hold it, in the order they are asked.
func (s *Server) atDevice(handler http.HandlerFunc, kinds ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pod := s.deviceOwner(r, strings.TrimSpace(r.PathValue("id")), kinds...); pod != "" {
			s.forwardRequest(w, r, pod)
			return
		}
		handler(w, r)
	}
}

// deviceOwner is the other instance that holds device id, "" to serve here.
func (s *Server) deviceOwner(r *http.Request, id string, kinds ...string) string {
	routing := s.deviceRouting
	if routing == nil || id == "" || wasForwarded(r) {
		return ""
	}
	for _, kind := range kinds {
		if s.holdsDevice(kind, id) {
			return ""
		}
	}
	for _, kind := range kinds {
		if pod := routing.Owner(r.Context(), kind, id); pod != "" {
			return pod
		}
	}
	return ""
}

func (s *Server) holdsDevice(kind, id string) bool {
	switch kind {
	case "cdap":
		return s.cdapGw != nil && s.cdapGw.IsConnected(id)
	case "mesh":
		return s.meshGw != nil && s.meshGw.IsConnected(id)
	}
	return false
}

// deviceConnectedAnywhere reports whether device id has a socket of kind on
// this instance or another one.
func (s *Server) deviceConnectedAnywhere(ctx context.Context, kind, id string) bool {
	if s.holdsDevice(kind, id) {
		return true
	}
	return s.deviceRouting != nil && s.deviceRouting.Owner(ctx, kind, id) != ""
}

func callerOf(r *http.Request) forwardedCaller {
	c := forwardedCaller{
		Username: getUsernameFromCtx(r),
		Role:     getRoleFromCtx(r),
		OrgID:    getOrgIDFromCtx(r),
		APIKey:   viaAPIKey(r),
		Remote:   r.RemoteAddr,
	}
	if arrivedInternally(r) {
		c.Internal = true
		if id, ok := IdentityFromContext(r.Context()); ok {
			c.Actor, c.Token = id.Actor, id.Token
		}
	}
	return c
}

// forwardRequest passes r to pod and copies the answer back. A websocket
// upgrade becomes a byte pipe between the client and pod once pod agrees.
func (s *Server) forwardRequest(w http.ResponseWriter, r *http.Request, pod string) {
	caller, _ := json.Marshal(callerOf(r))
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	dctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	conn, err := s.deviceRouting.Stream(dctx, pod, ForwardStream, map[string]string{"caller": string(caller), "nonce": hex.EncodeToString(nonce)})
	cancel()
	if err != nil {
		log.Printf("[api] Could not pass %s %s to instance %s: %v", r.Method, r.URL.Path, pod, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The server instance that holds this device does not answer"})
		return
	}
	defer conn.Close()

	out := r.Clone(context.Background())
	out.RequestURI = ""
	out.Header.Del("X-Forwarded-For") // the caller's address travels in the meta
	if err := out.Write(conn); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The server instance that holds this device does not answer"})
		return
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "The server instance that holds this device does not answer"})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSwitchingProtocols {
		hj, ok := w.(http.Hijacker)
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Upgrade not possible"})
			return
		}
		client, cbuf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		if err := resp.Write(client); err != nil {
			return
		}
		up := net.Conn(conn)
		if br.Buffered() > 0 {
			up = &readerConn{Conn: conn, r: br}
		}
		var down io.Reader = client
		if cbuf != nil && cbuf.Reader.Buffered() > 0 {
			down = io.MultiReader(cbuf.Reader, client)
		}
		splice(client, down, up)
		return
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		// Streamed answers (downloads, server-sent text) reach the client as
		// they come
		buf := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				f.Flush()
			}
			if err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(w, resp.Body)
}

// splice copies both ways until either side ends.
func splice(client net.Conn, down io.Reader, up net.Conn) {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }
	go func() { _, _ = io.Copy(up, down); stop() }()
	go func() { _, _ = io.Copy(client, up); stop() }()
	<-done
}

type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ServeForwarded serves a request another instance passed on (the cluster
// stream ForwardStream): one request with the caller from meta, through
// the same routes as the listener, marked so it is not passed on again.
func (s *Server) ServeForwarded(from string, meta map[string]string, conn net.Conn) {
	defer conn.Close()
	var caller forwardedCaller
	if err := json.Unmarshal([]byte(meta["caller"]), &caller); err != nil {
		log.Printf("[api] Refused a passed-on request from %s: bad caller", from)
		return
	}
	if s.deviceRouting == nil || !s.deviceRouting.ClaimOnce(context.Background(), meta["nonce"]) {
		log.Printf("[api] Refused a passed-on request from %s: replayed", from)
		return
	}
	mux := s.mux.get(s)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), forwardedCtxKey{}, from)
		if caller.Internal {
			ctx = context.WithValue(ctx, internalCtxKey{}, true)
			ctx = WithIdentity(ctx, Identity{Username: caller.Username, Role: caller.Role, OrgID: caller.OrgID, Actor: caller.Actor, Token: caller.Token})
		}
		if caller.Username != "" || caller.Role != "" {
			ctx = context.WithValue(ctx, ctxKeyRole, caller.Role)
			ctx = context.WithValue(ctx, ctxKeyUsername, caller.Username)
			if caller.OrgID != "" {
				ctx = context.WithValue(ctx, ctxKeyOrgID, caller.OrgID)
			}
			if caller.APIKey {
				ctx = context.WithValue(ctx, ctxKeyAPIKey, true)
			}
			if caller.Role != orgTokenRole {
				if user, err := s.db.GetUser(caller.Username); err == nil && user != nil {
					ctx = context.WithValue(ctx, ctxKeyUser, user)
				}
			}
		}
		if caller.Remote != "" {
			r.RemoteAddr = caller.Remote
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
	l := &oneConnListener{conn: conn, done: make(chan struct{})}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	srv.SetKeepAlivesEnabled(false)
	_ = srv.Serve(l)
	<-l.done
}

// oneConnListener hands out one connection, then reports closed; done is
// closed once the server has finished with the connection.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c == nil {
		<-l.done
		return nil, errors.New("listener closed")
	}
	return &notifyConn{Conn: c, closed: func() { l.once.Do(func() { close(l.done) }) }}, nil
}

func (l *oneConnListener) Close() error { return nil }
func (l *oneConnListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

type notifyConn struct {
	net.Conn
	closed func()
	once   sync.Once
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.closed)
	return err
}

// deviceCall is one action on a CDAP device that another instance asks of
// the one holding it.
type deviceCall struct {
	ID       string `json:"id"`
	Reason   string `json:"reason,omitempty"`
	FromID   string `json:"from_id,omitempty"`
	FromName string `json:"from_name,omitempty"`
	Text     string `json:"text,omitempty"`
}

// DeviceCalls are the cluster calls this instance answers for devices it
// holds; main registers them on the mesh.
func (s *Server) DeviceCalls() map[string]func(ctx context.Context, body []byte) ([]byte, error) {
	decode := func(body []byte) (deviceCall, error) {
		var c deviceCall
		if err := json.Unmarshal(body, &c); err != nil || c.ID == "" {
			return c, errors.New("bad device call")
		}
		return c, nil
	}
	return map[string]func(context.Context, []byte) ([]byte, error){
		"api.cdap-revoke": func(ctx context.Context, body []byte) ([]byte, error) {
			c, err := decode(body)
			if err != nil || s.cdapGw == nil {
				return nil, err
			}
			return nil, s.cdapGw.SendRevoke(ctx, c.ID, c.Reason)
		},
		"api.cdap-chat": func(ctx context.Context, body []byte) ([]byte, error) {
			c, err := decode(body)
			if err != nil || s.cdapGw == nil {
				return nil, err
			}
			return nil, s.cdapGw.SendChatToDevice(ctx, c.ID, c.FromID, c.FromName, c.Text)
		},
	}
}

// cdapRevoke revokes CDAP device id on whichever instance holds it.
func (s *Server) cdapRevoke(ctx context.Context, id, reason string) error {
	if s.cdapGw == nil {
		return nil
	}
	if s.cdapGw.IsConnected(id) || s.deviceRouting == nil {
		return s.cdapGw.SendRevoke(ctx, id, reason)
	}
	if pod := s.deviceRouting.Owner(ctx, "cdap", id); pod != "" {
		body, _ := json.Marshal(deviceCall{ID: id, Reason: reason})
		_, err := s.deviceRouting.Call(ctx, pod, "api.cdap-revoke", body)
		return err
	}
	return s.cdapGw.SendRevoke(ctx, id, reason)
}

// cdapChat delivers a chat line to CDAP device id on whichever instance
// holds it; false when no instance does.
func (s *Server) cdapChat(ctx context.Context, id, fromID, fromName, text string) (bool, error) {
	if s.cdapGw == nil {
		return false, nil
	}
	if s.cdapGw.IsConnected(id) {
		return true, s.cdapGw.SendChatToDevice(ctx, id, fromID, fromName, text)
	}
	if s.deviceRouting == nil {
		return false, nil
	}
	pod := s.deviceRouting.Owner(ctx, "cdap", id)
	if pod == "" {
		return false, nil
	}
	body, _ := json.Marshal(deviceCall{ID: id, FromID: fromID, FromName: fromName, Text: text})
	_, err := s.deviceRouting.Call(ctx, pod, "api.cdap-chat", body)
	return true, err
}
