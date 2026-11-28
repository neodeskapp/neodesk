// Package routetest runs the whole console for the tests of a route
// group: the pipeline, every group that is switched on, a fresh database
// with one account per role, and an API server the test makes up.
//
//	w := routetest.New(t, routetest.Options{API: api})
//	got := w.As("admin").Do("POST", "/api/panel/tokens", `{"name":"x"}`)
//
// A test of package tokens lives in package tokens_test, because the
// console imports the groups.
package routetest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console"
	"github.com/neodeskapp/neodesk/neodesk-server/console/access"
	"github.com/neodeskapp/neodesk/neodesk-server/console/config"
	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/session"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store/storetest"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

// RoleIDs gives every test account its users row id. The account of a
// role is e2e_<role>.
var RoleIDs = map[string]int64{
	"admin": 1, "super_admin": 2, "server_admin": 3, "global_admin": 4,
	"operator": 5, "viewer": 6, "guest_issuer": 7, "pro": 8,
}

// ClientIP is the address the test requests come from.
const ClientIP = "127.0.0.1"

// Options say what console a test wants.
type Options struct {
	// DB is the migrated database to run on; nil means a new SQLite one.
	DB *store.DB
	// API stands in for the API server. nil answers every call with 404.
	API http.Handler
	// Roles holds the role overrides; nil leaves the built-in permissions.
	Roles access.OverrideStore
	// Env adds environment variables the console reads.
	Env map[string]string
	// Config changes the configuration before the console is built.
	Config func(cfg *config.Config)
	// Data is the server's database (console.Options.Data); nil gives the
	// console none.
	Data db.Database
}

// World is a console on a fresh database.
type World struct {
	T      *testing.T
	DB     *store.DB
	Server *console.Server
	// DataDir is the console's data folder, empty at the start.
	DataDir string
	front   *httptest.Server
}

// languages are the language files of the test console: the texts the
// route groups answer with, in English and German.
var languages = map[string]string{
	"en": `{"meta":{"lang":"en","name":"English","native_name":"English","direction":"ltr"},` +
		`"errors":{"server_error":"Server error"},` +
		`"users":{"not_found":"User not found","user_group_not_found":"User group not found"}}`,
	"de": `{"meta":{"lang":"de","name":"German","native_name":"Deutsch","direction":"ltr"},` +
		`"errors":{"server_error":"Serverfehler"},` +
		`"users":{"not_found":"Benutzer nicht gefunden","user_group_not_found":"Benutzergruppe nicht gefunden"}}`,
}

// New builds a console and starts it on a local port.
func New(t *testing.T, o Options) *World {
	t.Helper()
	db := o.DB
	if db == nil {
		db = storetest.Migrated(t)
	}
	ctx := context.Background()
	for role, id := range RoleIDs {
		if _, err := db.Exec(ctx, "INSERT INTO users (id, username, password_hash, role) VALUES (?, ?, 'x', ?)", id, "e2e_"+role, role); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	data := t.TempDir()
	env := map[string]string{"SESSION_SECRET": "test-secret-0123456789abcdef0123456789abcdef"}
	for k, v := range o.Env {
		env[k] = v
	}
	langDir := filepath.Join(t.TempDir(), "lang")
	if err := os.MkdirAll(langDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for code, content := range languages {
		if err := os.WriteFile(filepath.Join(langDir, code+".json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Host:            "127.0.0.1",
		NodeEnv:         "development",
		DataDir:         data,
		UploadsDir:      filepath.Join(data, "uploads"),
		LangDir:         langDir,
		WebUIDir:        filepath.Join(data, "no-build"),
		DefaultLanguage: "en",
		Getenv:          func(key string) string { return env[key] },
	}
	if o.Config != nil {
		o.Config(cfg)
	}
	api := o.API
	if api == nil {
		api = http.NotFoundHandler()
	}
	s, err := console.New(ctx, console.Options{Config: cfg, Store: db, API: api, Roles: o.Roles, Data: o.Data})
	if err != nil {
		t.Fatalf("console.New: %v", err)
	}
	t.Cleanup(s.Stop)

	d := s.Deps()
	s.Router().Get("/__test/signin/{role}", func(rw http.ResponseWriter, r *http.Request) {
		role := r.PathValue("role")
		sess := kit.Session(r)
		if err := sess.Regenerate(); err != nil {
			t.Errorf("regenerate: %v", err)
		}
		user := session.User{ID: RoleIDs[role], Username: "e2e_" + role, Role: role}
		if _, err := sess.Establish(user, d.Sessions.ClientMeta(r, "")); err != nil {
			t.Errorf("establish: %v", err)
		}
		kit.OK(rw, r, nil)
	})
	s.Router().Get("/__test/token", func(rw http.ResponseWriter, r *http.Request) {
		kit.JSON(rw, r, http.StatusOK, map[string]string{"token": kit.CSRFToken(r)})
	})

	w := &World{T: t, DB: db, Server: s, DataDir: data, front: httptest.NewServer(s)}
	t.Cleanup(w.front.Close)
	return w
}

// Answer is what the console answered.
type Answer struct {
	Status int
	Header http.Header
	Body   string
}

// Agent is a browser: it keeps cookies between requests.
type Agent struct {
	w       *World
	cookies map[string]string
	token   string
}

// SignedOut returns a browser without a session.
func (w *World) SignedOut() *Agent { return &Agent{w: w, cookies: map[string]string{}} }

// Addr is the host and port the console listens on, for websockets.
func (w *World) Addr() string { return strings.TrimPrefix(w.front.URL, "http://") }

// As returns a browser signed in as the test account of a role.
func (w *World) As(role string) *Agent {
	w.T.Helper()
	if _, known := RoleIDs[role]; !known {
		w.T.Fatalf("no test account for role %q", role)
	}
	a := w.SignedOut()
	if res := a.Do(http.MethodGet, "/__test/signin/"+role, ""); res.Status != http.StatusOK {
		w.T.Fatalf("sign in as %s: %d %s", role, res.Status, res.Body)
	}
	return a
}

var client = &http.Client{
	Transport:     &http.Transport{DisableCompression: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       30 * time.Second,
}

// Do sends a request. A write carries the browser's CSRF token, and a body
// goes out as JSON. header is a list of name, value pairs that are added.
func (a *Agent) Do(method, target, body string, header ...string) Answer {
	a.w.T.Helper()
	answer, err := a.Try(method, target, body, header...)
	if err != nil {
		a.w.T.Fatalf("%s %s: %v", method, target, err)
	}
	return answer
}

// Try is Do for a request that may get no answer: the error says why.
func (a *Agent) Try(method, target, body string, header ...string) (Answer, error) {
	a.w.T.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, a.w.front.URL+target, reader)
	if err != nil {
		a.w.T.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("X-CSRF-Token", a.csrf())
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	var pairs []string
	for name, value := range a.cookies {
		pairs = append(pairs, name+"="+value)
	}
	if len(pairs) > 0 {
		req.Header.Set("Cookie", strings.Join(pairs, "; "))
	}
	res, err := client.Do(req)
	if err != nil {
		return Answer{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	for _, c := range res.Cookies() {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now())) {
			delete(a.cookies, c.Name)
			continue
		}
		a.cookies[c.Name] = c.Value
	}
	return Answer{Status: res.StatusCode, Header: res.Header, Body: string(raw)}, nil
}

// Cookie is the value of a cookie the browser holds, "" for none.
func (a *Agent) Cookie(name string) string { return a.cookies[name] }

// SetCookie gives the browser a cookie; an empty value drops it.
func (a *Agent) SetCookie(name, value string) {
	if value == "" {
		delete(a.cookies, name)
		return
	}
	a.cookies[name] = value
}

// Copy is another browser with the same cookies and CSRF token.
func (a *Agent) Copy() *Agent {
	b := &Agent{w: a.w, cookies: map[string]string{}, token: a.token}
	for name, value := range a.cookies {
		b.cookies[name] = value
	}
	return b
}

func (a *Agent) csrf() string {
	a.w.T.Helper()
	if a.token == "" {
		var out map[string]string
		_ = json.Unmarshal([]byte(a.Do(http.MethodGet, "/__test/token", "").Body), &out)
		if a.token = out["token"]; a.token == "" {
			a.w.T.Fatal("no CSRF token")
		}
	}
	return a.token
}

// AuditRows lists the audit log as "action|details|ip|user", oldest first.
func (w *World) AuditRows() []string {
	w.T.Helper()
	rows, err := w.DB.Query(context.Background(), "SELECT action, COALESCE(details, ''), COALESCE(ip_address, ''), COALESCE(user_id, 0) FROM audit_log ORDER BY id")
	if err != nil {
		w.T.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, details, ip string
		var user int64
		if err := rows.Scan(&action, &details, &ip, &user); err != nil {
			w.T.Fatal(err)
		}
		out = append(out, strings.Join([]string{action, details, ip, strconv.FormatInt(user, 10)}, "|"))
	}
	return out
}

// Call is one request the made-up API server received.
type Call struct {
	Method string
	// Target is the path with its query, as the console sent it.
	Target string
	Body   string
}

func (c Call) String() string {
	if c.Body == "" {
		return c.Method + " " + c.Target
	}
	return c.Method + " " + c.Target + " " + c.Body
}

// Reply is an answer of the made-up API server.
type Reply struct {
	Status int
	// Type is the Content-Type, application/json when empty.
	Type string
	Body string
}

// API is a made-up API server: it answers by "METHOD /path?query" and
// remembers what it was asked.
type API struct {
	mu      sync.Mutex
	replies map[string]Reply
	calls   []Call
	// Fallback answers a request no reply was set for. nil answers 404
	// {"error":"not found"}.
	Fallback func(c Call) Reply
}

// NewAPI returns an API server without answers.
func NewAPI() *API { return &API{replies: map[string]Reply{}} }

// On sets the answer for a request, "GET /api/tokens?include_revoked=true".
func (a *API) On(request string, status int, body string) *API {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies[request] = Reply{Status: status, Body: body}
	return a
}

// OnText sets a plain text answer, as the API server's http.Error sends.
func (a *API) OnText(request string, status int, body string) *API {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replies[request] = Reply{Status: status, Type: "text/plain; charset=utf-8", Body: body}
	return a
}

// Calls returns the requests received so far, nil for none, and forgets
// them.
func (a *API) Calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, c := range a.calls {
		out = append(out, c.String())
	}
	a.calls = nil
	return out
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	c := Call{Method: r.Method, Target: r.RequestURI, Body: string(raw)}
	a.mu.Lock()
	a.calls = append(a.calls, c)
	reply, ok := a.replies[r.Method+" "+r.RequestURI]
	fallback := a.Fallback
	a.mu.Unlock()
	if !ok {
		reply = Reply{Status: http.StatusNotFound, Body: `{"error":"not found"}`}
		if fallback != nil {
			reply = fallback(c)
		}
	}
	if reply.Status == 0 {
		// A reply without a status stands for an API server that hangs up
		panic(http.ErrAbortHandler)
	}
	contentType := reply.Type
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(reply.Status)
	_, _ = io.WriteString(w, reply.Body)
}

// Overrides is a made-up role_permissions table: role, then permission,
// then whether it is granted. Give it as Options.Roles to take a
// permission away from a role, or to grant one.
type Overrides map[string]map[string]bool

// ListRolePermissions returns the overrides of one role, or of all roles
// when role is empty.
func (o Overrides) ListRolePermissions(role string) ([]*db.RolePermission, error) {
	var out []*db.RolePermission
	for name, permissions := range o {
		if role != "" && name != role {
			continue
		}
		for permission, granted := range permissions {
			out = append(out, &db.RolePermission{Role: name, Permission: permission, Granted: granted})
		}
	}
	return out, nil
}

// SetRolePermission stores an override.
func (o Overrides) SetRolePermission(role, permission string, granted bool) error {
	if o[role] == nil {
		o[role] = map[string]bool{}
	}
	o[role][permission] = granted
	return nil
}

// DeleteRolePermission removes an override.
func (o Overrides) DeleteRolePermission(role, permission string) error {
	delete(o[role], permission)
	return nil
}
