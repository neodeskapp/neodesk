// Package ndapi is the device API of the console under /api/nd
// (routes/bd-api.routes.js): the routes the NeoDesk agent and the
// desktop client call. Devices register and send heartbeats, ask for
// relay sessions to another device and look at the devices online, raise
// help requests and chat; the desktop client in operator mode lists the
// devices, the help requests and its own sessions; the web app's
// notification bell lists help requests and registrations to approve.
//
// The routes sit in the early stage of the pipeline (Deps.Early): after
// the session, the panel access guard and the rate limiters, before the
// request language and the CSRF gate. Devices prove themselves with an
// access token bound to their id (d.RequireDeviceToken); the operator
// routes take any live access token of an admin or operator account; the
// registration takes a token or the header X-Device-Id and decides by the
// admission rules in register.go. The relay sessions and the agents'
// signal sockets are d.Relay (package ndrelay).
//
// POST /api/nd/remote-agent-token is in the remote group, beside the
// socket that takes its tokens.
package ndapi

import (
	"crypto/rand"
	"io"
	"net/http"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/registrations"
)

type handlers struct {
	d      *kit.Deps
	regs   *registrations.Service
	nonces *nonces
	log    *kit.Logger
	now    func() time.Time
	random io.Reader
	// version is the product version, read once at start.
	version string
}

// configuredAppName is the name the old console's configuration gave
// itself.
const configuredAppName = "NeoDesk Console"

// Register adds the group's routes to d.Early below the mount of d
// (/api/nd), in the order of the old route file.
func Register(d *kit.Deps) {
	h := &handlers{
		d:       d,
		regs:    registrations.New(d.Store, d.GoAPI),
		nonces:  newNonces(d.KV),
		log:     d.Log.Child("BD-API"),
		now:     time.Now,
		random:  rand.Reader,
		version: d.Config.Version,
	}
	r := d.Early
	operator := []kit.Middleware{h.requireDeviceAuth, requireOperatorRole}
	device := []kit.Middleware{d.RequireDeviceToken, d.RequireTokenDeviceMatch}

	r.Get("/server-info", h.serverInfo)
	r.Post("/register", h.register, d.Limits.Register.Middleware, h.identifyDevice)
	r.Post("/heartbeat", h.heartbeat, device...)
	r.Post("/connect", h.connect, h.requireDeviceAuth)
	r.Get("/session/{id}", h.session, d.RequireDeviceToken)
	r.Delete("/session/{id}", h.endSession, d.RequireDeviceToken)
	r.Get("/peers", h.peers, d.RequireDeviceToken)
	r.Get("/peer/{id}", h.peer, d.RequireDeviceToken)
	r.Get("/branding", h.branding)
	r.Get("/appearance", h.appearance)
	r.Post("/help-request", h.helpRequest, device...)
	r.Post("/chat/send", h.chatSend, device...)
	r.Get("/chat/history", h.chatHistory, device...)
	r.Post("/operator/login", h.operatorLogin)
	r.Get("/operator/devices", h.operatorDevices, operator...)
	r.Get("/help-requests", h.helpRequests, operator...)
	r.Post("/help-requests/{id}/accept", h.acceptHelp, operator...)
	r.Post("/help-requests/{id}/resolve", h.resolveHelp, operator...)
	r.Delete("/help-requests/{id}", h.deleteHelp, operator...)
	r.Get("/notifications", h.notifications, d.RequireAuth)
	r.Post("/notifications/{id}/read", h.markRead, h.csrfProtection, d.RequireAuth)
	r.Post("/notifications/read-all", h.markAllRead, h.csrfProtection, d.RequireAuth)
	r.Post("/operator/sessions", h.recordSession, operator...)
	r.Get("/operator/sessions", h.sessionHistory, operator...)
}

// failure is the group's error answer: {"error":"..."}, without success.
type failure struct {
	Error string `json:"error"`
}

func fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	kit.JSON(w, r, status, failure{Error: message})
}

// serverError logs what failed and answers what the route's catch block
// answered.
func (h *handlers) serverError(w http.ResponseWriter, r *http.Request, what string, err error, message string) {
	h.log.Error(what, err)
	fail(w, r, http.StatusInternalServerError, message)
}

// done is {"success":true}.
type done struct {
	Success bool `json:"success"`
}

// csrfProtection is doubleCsrfProtection on a route: a write without the
// cookie's token in X-CSRF-Token (or the body's _csrf) is refused with the
// console's CSRF answer, signed in or not. The CSRF gate of the pipeline
// lies behind this stage, and lets /api/nd/ through for nobody signed in.
func (h *handlers) csrfProtection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.d.CSRF.Check(r); err != nil {
			h.d.WebUI.CSRFError(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
