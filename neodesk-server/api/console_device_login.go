package api

// Sign in with NeoDesk: the desktop app signs in through the web console
// in the browser instead of taking a password.
//
//  1. The app sends POST /api/oidc/auth with op "NeoDesk", its device id,
//     uuid and device info. The server keeps a request and answers a poll
//     code and the console's page for the request,
//     <console>/authorize-device/<request id>.
//  2. The app opens the page and polls GET /api/oidc/auth-query with the
//     code, id and uuid. The answer is "No authed oidc is found" while
//     the request waits.
//  3. On the page a person signed in to the console sees which computer
//     asks and clicks Allow or Deny. The console asks the routes below
//     (in-process only) to read and decide the request.
//  4. The next poll after Allow gets the access token of the account that
//     allowed it, once; after Deny it gets "Sign-in was declined in the
//     browser".
//
// The poll code and the request id are separate random secrets: the URL
// in the browser can't poll, and the poll code can't decide. A request is
// bound to the device id and uuid it started with, lives five minutes and
// is decided once.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/audit"
	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

const (
	// consoleLoginOp is the op of the console sign-in in POST
	// /api/oidc/auth and its name in GET /api/login-options. Any other op
	// is the outside identity provider.
	consoleLoginOp = "NeoDesk"
	// consoleLoginLifetime is how long a request can be decided and its
	// token picked up.
	consoleLoginLifetime = 5 * time.Minute
	// maxConsoleLogins caps the requests kept at once. Starts are public
	// and rate-limited per address (oidcStartLimiter).
	maxConsoleLogins = 1024
	// consoleLoginCodePrefix starts every poll code, so auth-query tells
	// them from the states of the outside identity provider.
	consoleLoginCodePrefix = "nd-"
	// consoleLoginMethod names the sign-in in the audit log.
	consoleLoginMethod = "console_device_login"
)

// The answers the desktop app shows as they are.
const (
	consoleLoginDeclinedMsg = "Sign-in was declined in the browser"
	consoleLoginExpiredMsg  = "This sign-in request has expired. Start the sign-in again."
	consoleLoginNoConsole   = "The web console's address is not set. Ask an administrator to set PANEL_PUBLIC_URL, or Console address in Settings, Single sign-on."
	consoleLoginTooMany     = "Too many sign-ins are waiting. Try again in a few minutes."
	consoleLoginNeedsDevice = "The app sent no device id or uuid"
)

// The states of a request.
const (
	consoleLoginPending  = "pending"
	consoleLoginAllowed  = "allowed"
	consoleLoginDeclined = "declined"
)

var (
	errConsoleLoginTooMany    = errors.New("too many console sign-ins waiting")
	errConsoleLoginNotFound   = errors.New("no such console sign-in")
	errConsoleLoginNotPending = errors.New("the console sign-in was decided")
)

// consoleLogin is one desktop sign-in through the console.
type consoleLogin struct {
	code    string // the poll code, only the app has it
	request string // the id in the page's address
	// The device the request is bound to and what it says about itself
	deviceID, uuid           string
	deviceName, os, kind, ip string
	created                  time.Time
	state                    string
	// account allowed or declined it
	account int64
}

func (l *consoleLogin) expired(now time.Time) bool {
	return now.Sub(l.created) >= consoleLoginLifetime
}

// consoleLogins are the requests in progress, in a KV (SetSharedKV): the
// app may start and poll on one instance and the console decide on
// another. A request is kept under its id, and its poll code points to
// the id. Each instance keeps at most maxConsoleLogins of its own at a
// time. The zero value is ready.
type consoleLogins struct {
	kvHolder
	capOnce sync.Once
	started *cluster.LocalCap
}

// consoleLoginStored is a consoleLogin as the KV holds it.
type consoleLoginStored struct {
	Code       string    `json:"code"`
	Request    string    `json:"request"`
	DeviceID   string    `json:"device_id"`
	UUID       string    `json:"uuid"`
	DeviceName string    `json:"device_name"`
	OS         string    `json:"os"`
	Kind       string    `json:"kind"`
	IP         string    `json:"ip"`
	Created    time.Time `json:"created"`
	State      string    `json:"state"`
	Account    int64     `json:"account"`
}

func (l consoleLogin) stored() consoleLoginStored {
	return consoleLoginStored{
		Code: l.code, Request: l.request, DeviceID: l.deviceID, UUID: l.uuid,
		DeviceName: l.deviceName, OS: l.os, Kind: l.kind, IP: l.ip,
		Created: l.created, State: l.state, Account: l.account,
	}
}

func (st consoleLoginStored) login() consoleLogin {
	return consoleLogin{
		code: st.Code, request: st.Request, deviceID: st.DeviceID, uuid: st.UUID,
		deviceName: st.DeviceName, os: st.OS, kind: st.Kind, ip: st.IP,
		created: st.Created, state: st.State, account: st.Account,
	}
}

func consoleLoginRequestKey(request string) string { return "console-login:req:" + request }
func consoleLoginCodeKey(code string) string       { return "console-login:code:" + code }
func consoleLoginDecideKey(request string) string  { return "console-login:decide:" + request }

func (ls *consoleLogins) cap() *cluster.LocalCap {
	ls.capOnce.Do(func() { ls.started = cluster.NewLocalCap(maxConsoleLogins) })
	return ls.started
}

// left is how long l still lives at now, at least a millisecond.
func (l *consoleLogin) left(now time.Time) time.Duration {
	d := consoleLoginLifetime - now.Sub(l.created)
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return d
}

// save writes l under its id for the rest of its life.
func (ls *consoleLogins) save(ctx context.Context, l consoleLogin, now time.Time) error {
	raw, err := json.Marshal(l.stored())
	if err != nil {
		return err
	}
	return ls.kv().Set(ctx, consoleLoginRequestKey(l.request), string(raw), l.left(now))
}

// load reads the request with this id. A request that expired at now is
// dropped and not found.
func (ls *consoleLogins) load(ctx context.Context, request string, now time.Time) (consoleLogin, bool, error) {
	raw, ok, err := ls.kv().Get(ctx, consoleLoginRequestKey(request))
	if err != nil || !ok {
		return consoleLogin{}, false, err
	}
	var st consoleLoginStored
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return consoleLogin{}, false, err
	}
	l := st.login()
	if l.expired(now) {
		ls.drop(ctx, l)
		return consoleLogin{}, false, nil
	}
	return l, true, nil
}

// drop removes a request, its poll code and its decision key.
func (ls *consoleLogins) drop(ctx context.Context, l consoleLogin) {
	kv := ls.kv()
	_ = kv.Del(ctx, consoleLoginRequestKey(l.request))
	_ = kv.Del(ctx, consoleLoginCodeKey(l.code))
	_ = kv.Del(ctx, consoleLoginDecideKey(l.request))
	ls.cap().Remove(l.code)
}

func newConsoleLoginSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// start keeps a new pending request made from l's device fields and
// returns it with its poll code and request id.
func (ls *consoleLogins) start(l consoleLogin, now time.Time) (consoleLogin, error) {
	code, err := newConsoleLoginSecret()
	if err != nil {
		return consoleLogin{}, err
	}
	request, err := newConsoleLoginSecret()
	if err != nil {
		return consoleLogin{}, err
	}
	l.code, l.request = consoleLoginCodePrefix+code, request
	l.created, l.state, l.account = now, consoleLoginPending, 0

	if !ls.cap().Add(l.code, now, now.Add(consoleLoginLifetime)) {
		return consoleLogin{}, errConsoleLoginTooMany
	}
	ctx := context.Background()
	err = ls.save(ctx, l, now)
	if err == nil {
		err = ls.kv().Set(ctx, consoleLoginCodeKey(l.code), l.request, consoleLoginLifetime)
	}
	if err != nil {
		ls.drop(ctx, l)
		return consoleLogin{}, err
	}
	return l, nil
}

// view is the request with this id, while it lives.
func (ls *consoleLogins) view(request string, now time.Time) (consoleLogin, bool) {
	l, ok, err := ls.load(context.Background(), request, now)
	if err != nil {
		log.Printf("[console sign-in] read a request: %v", err)
	}
	return l, ok
}

// decide allows or declines a pending request for account. check, when
// not nil, runs before an approval while the request is held, and its
// error refuses the approval and leaves the request pending. A decision
// holds the request with a key that only one decision on any instance
// gets.
func (ls *consoleLogins) decide(request string, allow bool, account int64, now time.Time, check func() error) (consoleLogin, error) {
	ctx := context.Background()
	l, ok, err := ls.load(ctx, request, now)
	if err != nil {
		return consoleLogin{}, err
	}
	if !ok {
		return consoleLogin{}, errConsoleLoginNotFound
	}
	if l.state != consoleLoginPending {
		return l, errConsoleLoginNotPending
	}
	kv := ls.kv()
	_, held, err := kv.SetNX(ctx, consoleLoginDecideKey(request), "1", l.left(now))
	if err != nil {
		return l, err
	}
	if !held {
		// Another decision holds it: answer with what it made of it
		if cur, found, _ := ls.load(ctx, request, now); found {
			l = cur
		}
		return l, errConsoleLoginNotPending
	}
	if allow && check != nil {
		if err := check(); err != nil {
			_ = kv.Del(ctx, consoleLoginDecideKey(request))
			return l, err
		}
	}
	l.state, l.account = consoleLoginDeclined, account
	if allow {
		l.state = consoleLoginAllowed
	}
	if err := ls.save(ctx, l, now); err != nil {
		_ = kv.Del(ctx, consoleLoginDecideKey(request))
		return consoleLogin{}, err
	}
	return l, nil
}

// What a poll finds.
type consoleLoginPoll int

const (
	pollUnknown  consoleLoginPoll = iota // no such code, or it expired
	pollMismatch                         // another device's id or uuid
	pollWaiting                          // nobody decided yet
	pollDeclined                         // declined, now gone
	pollAllowed                          // allowed, now gone: hand the token out
)

// poll looks up the request of a poll code for the device id and uuid. A
// declined or allowed request is gone after the poll that learns it: the
// poll takes it from the KV, so of two polls on any instances one gets it.
func (ls *consoleLogins) poll(code, deviceID, uuid string, now time.Time) (consoleLogin, consoleLoginPoll) {
	ctx := context.Background()
	kv := ls.kv()
	request, ok, err := kv.Get(ctx, consoleLoginCodeKey(code))
	if err != nil {
		log.Printf("[console sign-in] read a poll code: %v", err)
	}
	if !ok {
		return consoleLogin{}, pollUnknown
	}
	l, ok, err := ls.load(ctx, request, now)
	if err != nil {
		log.Printf("[console sign-in] read a request: %v", err)
	}
	if !ok || l.code != code {
		return consoleLogin{}, pollUnknown
	}
	if deviceID == "" || uuid == "" || deviceID != l.deviceID || uuid != l.uuid {
		return consoleLogin{}, pollMismatch
	}
	if l.state == consoleLoginPending {
		return l, pollWaiting
	}
	raw, took, err := kv.Take(ctx, consoleLoginRequestKey(request))
	if err != nil {
		log.Printf("[console sign-in] take a request: %v", err)
	}
	if !took {
		return consoleLogin{}, pollUnknown
	}
	var st consoleLoginStored
	if json.Unmarshal([]byte(raw), &st) != nil {
		return consoleLogin{}, pollUnknown
	}
	l = st.login()
	ls.drop(ctx, l)
	switch l.state {
	case consoleLoginDeclined:
		return l, pollDeclined
	case consoleLoginAllowed:
		return l, pollAllowed
	}
	return consoleLogin{}, pollUnknown
}

// consoleLoginRoutes are the console's routes to read and decide requests.
func (s *Server) consoleLoginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/console-device-login/{id}", s.inProcessOnly(s.handleConsoleLoginGet))
	mux.HandleFunc("POST /api/console-device-login/{id}/approve", s.inProcessOnly(s.handleConsoleLoginDecide(true)))
	mux.HandleFunc("POST /api/console-device-login/{id}/deny", s.inProcessOnly(s.handleConsoleLoginDecide(false)))
}

func (s *Server) consoleLoginNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// SetConsoleAddress tells the server where people reach the console that
// runs in this process: address(host) is the console's base URL, built
// for the host name the desktop app reached this server at when nothing
// names the console's address. nil when no console runs here.
func (s *Server) SetConsoleAddress(address func(host string) string) {
	s.consoleAddressMu.Lock()
	s.consoleAddress = address
	s.consoleAddressMu.Unlock()
}

// validConsoleBase reports whether base is an http or https URL with a
// host and nothing after its path.
func validConsoleBase(base string) bool {
	if base == "" || strings.ContainsAny(base, " \t\r\n\x00") {
		return false
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// isLoopbackHost reports whether a host name or address is this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// consoleLoginBase is the console's address for the page of a request
// (DECISIONS A276): Console address in the single sign-on settings,
// PANEL_PUBLIC_URL or PUBLIC_URL, as the single sign-on's redirects use
// them; else the console in this process names it (PANEL_PUBLIC_URL,
// PUBLIC_URL, PANEL_PUBLIC_HOST, with the console's scheme and port);
// else the console in this process at the host name the desktop app
// reached this server at, which the console checks is an IP address or a
// host name. A loopback host counts only for an app on this machine. ""
// when no console runs here and nothing names one.
func (s *Server) consoleLoginBase(r *http.Request) string {
	if s.db != nil {
		if v, err := s.db.GetConfig("oidc.panel_url"); err == nil {
			if base := strings.TrimRight(strings.TrimSpace(v), "/"); validConsoleBase(base) {
				return base
			}
		}
	}
	if base := resolvePanelBaseURL(nil); base != "" {
		return base
	}
	s.consoleAddressMu.RLock()
	address := s.consoleAddress
	s.consoleAddressMu.RUnlock()
	if address == nil {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if isLoopbackHost(host) && !isLoopbackHost(s.remoteIP(r)) {
		host = ""
	}
	base := strings.TrimRight(address(host), "/")
	if !validConsoleBase(base) {
		return ""
	}
	return base
}

// startConsoleLogin answers POST /api/oidc/auth with op "NeoDesk".
func (s *Server) startConsoleLogin(w http.ResponseWriter, r *http.Request, deviceID, uuid, name, os, kind string) {
	if deviceID == "" || uuid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": consoleLoginNeedsDevice})
		return
	}
	base := s.consoleLoginBase(r)
	if base == "" {
		log.Printf("[console sign-in] refused a start: the console's address is not known")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": consoleLoginNoConsole})
		return
	}
	l, err := s.consoleLogins.start(consoleLogin{
		deviceID: deviceID, uuid: uuid, deviceName: name, os: os, kind: kind, ip: s.remoteIP(r),
	}, s.consoleLoginNow())
	if errors.Is(err, errConsoleLoginTooMany) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": consoleLoginTooMany})
		return
	}
	if err != nil {
		log.Printf("[console sign-in] start failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to initiate OIDC login"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"code": l.code,
		"url":  base + "/authorize-device/" + l.request,
	})
}

// waitingAnswer is what the stock client keeps polling on.
var waitingAnswer = map[string]string{
	"error":   "No authed oidc is found",
	"message": "Authorization in progress",
}

// pollConsoleLogin answers GET /api/oidc/auth-query for a poll code of
// the console sign-in.
func (s *Server) pollConsoleLogin(w http.ResponseWriter, r *http.Request, code string) {
	q := r.URL.Query()
	l, found := s.consoleLogins.poll(code, q.Get("id"), q.Get("uuid"), s.consoleLoginNow())
	switch found {
	case pollUnknown:
		writeJSON(w, http.StatusOK, map[string]string{"error": consoleLoginExpiredMsg})
		return
	case pollMismatch:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "device id mismatch"})
		return
	case pollWaiting:
		writeJSON(w, http.StatusOK, waitingAnswer)
		return
	case pollDeclined:
		writeJSON(w, http.StatusOK, map[string]string{"error": consoleLoginDeclinedMsg})
		return
	}

	clientIP := s.remoteIP(r)
	user, err := s.db.GetUserByID(l.account)
	if err != nil || user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "User not found"})
		return
	}
	if refuse := s.consoleLoginRefusal(user); refuse != nil {
		if s.auditLog != nil {
			s.auditLog.Log(audit.ActionAuthLoginFailed, clientIP, user.Username, map[string]string{
				"method": consoleLoginMethod, "reason": refuse.code, "client_id": l.deviceID,
			})
		}
		refuse.write(w)
		return
	}
	token, err := s.issueClientSession(user, l.deviceID, l.uuid, clientIP)
	if err != nil {
		log.Printf("[console sign-in] client session issue failed for %q: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Token generation failed"})
		return
	}
	_ = s.db.UpdateUserLogin(user.ID)
	if s.auditLog != nil {
		s.auditLog.Log(audit.ActionAuthLogin, clientIP, user.Username, map[string]string{
			"method": consoleLoginMethod, "client_id": l.deviceID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type":         "access_token",
		"access_token": token,
		"user":         rustdeskUserPayload(user.Username, user.Role),
	})
}

// consoleLoginRefused is why an account may not sign in to the desktop
// app now, with the answer the desktop password sign-in gives for it.
type consoleLoginRefused struct {
	code  string
	write func(http.ResponseWriter)
}

func (e *consoleLoginRefused) Error() string { return e.code }

// consoleLoginRefusal is the refusal the desktop password sign-in makes
// for this account (disabled, two-factor setup due, password change due),
// nil when it may sign in.
func (s *Server) consoleLoginRefusal(user *db.User) *consoleLoginRefused {
	switch {
	case user.Disabled:
		return &consoleLoginRefused{code: "account_disabled", write: writeAccountDisabled}
	case s.twoFactorSetupDue(user):
		return &consoleLoginRefused{code: "totp_setup_required", write: writeTwoFactorSetupDue}
	case s.passwordChangeDue(user):
		return &consoleLoginRefused{code: "password_change_required", write: writePasswordChangeDue}
	}
	return nil
}

// consoleLoginView is a request as the console's page shows it.
type consoleLoginView struct {
	State      string `json:"state"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	DeviceOS   string `json:"device_os"`
	DeviceType string `json:"device_type"`
	IP         string `json:"ip"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}

func viewOf(l consoleLogin) consoleLoginView {
	return consoleLoginView{
		State: l.state, DeviceID: l.deviceID, DeviceName: l.deviceName, DeviceOS: l.os, DeviceType: l.kind, IP: l.ip,
		CreatedAt: l.created.UTC().Format(time.RFC3339),
		ExpiresAt: l.created.Add(consoleLoginLifetime).UTC().Format(time.RFC3339),
	}
}

// validRequestID reports whether an id has the shape of a request id.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// GET /api/console-device-login/{id}: the request, 404 not_found when it
// is unknown or expired.
func (s *Server) handleConsoleLoginGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRequestID(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	l, ok := s.consoleLogins.view(id, s.consoleLoginNow())
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(l))
}

// POST /api/console-device-login/{id}/approve and /deny with {account}:
// the console account decides the pending request. 404 not_found when it
// is unknown or expired, 409 not_pending with its state when it was
// decided, and for an approval 409 account_refused with the code and
// sentence of the desktop password sign-in when the account may not sign
// in (disabled, two-factor setup due, password change due).
func (s *Server) handleConsoleLoginDecide(allow bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Account int64 `json:"account"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Account <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account_required"})
			return
		}
		if !validRequestID(id) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		var check func() error
		if allow {
			check = func() error {
				user, err := s.db.GetUserByID(body.Account)
				if err != nil {
					return err
				}
				if user == nil {
					return &consoleLoginRefused{code: "account_unknown"}
				}
				if refuse := s.consoleLoginRefusal(user); refuse != nil {
					return refuse
				}
				return nil
			}
		}
		l, err := s.consoleLogins.decide(id, allow, body.Account, s.consoleLoginNow(), check)
		var refused *consoleLoginRefused
		switch {
		case errors.Is(err, errConsoleLoginNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		case errors.Is(err, errConsoleLoginNotPending):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "not_pending", "state": l.state})
		case errors.As(err, &refused):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "account_refused", "code": refused.code})
		case err != nil:
			log.Printf("[console sign-in] decide failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		default:
			writeJSON(w, http.StatusOK, viewOf(l))
		}
	}
}
