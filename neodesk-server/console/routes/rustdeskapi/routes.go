// Package rustdeskapi is the RustDesk client API of the console
// (routes/rustdesk-api.routes.js): the HTTP API RustDesk desktop clients
// and the NeoDesk client speak. Clients sign in with POST /api/login
// and send the access token they get as "Authorization: Bearer"; they keep
// their address books here, list devices, groups and strategies, and
// report heartbeats, system information and audit records.
//
// The routes sit in the early stage of the pipeline (Deps.Early): after
// the session, the panel access guard and the rate limiters, before the
// request language, the CSRF gate and compression. Clients in the field
// speak this API and cannot be updated in step with the server, so every
// status, field, type and quirk of the old routes is kept.
//
// Five routes answer only requests with a bearer token and leave the
// others to the console route of the same address (kit.PassOn): GET
// /api/users, /api/peers, /api/strategies, /api/strategies/:guid and
// /api/devices.
package rustdeskapi

import (
	"strings"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
)

type handlers struct {
	d     *kit.Deps
	store *clientStore
	now   func() time.Time
	tfa   *tfaSessions

	// totpBypass skips the second factor for this API:
	// RUSTDESK_API_DISABLE_TOTP=true together with
	// RUSTDESK_API_DISABLE_TOTP_ACKNOWLEDGED=true.
	totpBypass bool
	// auditFromGo reads the client audit lists from the server's API,
	// which holds them when the console has no client API port of its own
	// (API_ENABLED=false).
	auditFromGo bool

	sysinfoLog                               *logThrottle
	metrics                                  *metricLimit
	log, ab, peersLog, groupLog, strategyLog *kit.Logger
}

// Register adds the client API's routes to d.Early, in the order of the
// route file.
//
// Each call has its own pending second steps of the sign-in: a client
// finishes a sign-in on the router it started it on, as the old console
// kept them per Express app. The same routes can be added to another
// router (the client API port) with a Deps whose Early is that router.
func Register(d *kit.Deps) {
	getenv := func(string) string { return "" }
	if d.Config != nil && d.Config.Getenv != nil {
		getenv = d.Config.Getenv
	}
	isTrue := func(name string) bool {
		value := getenv(name)
		if value == "" {
			value = "false"
		}
		return strings.ToLower(value) == "true"
	}
	log := d.Log.Child("API")
	h := &handlers{
		d:           d,
		store:       &clientStore{db: d.Store, log: log, now: time.Now},
		now:         time.Now,
		tfa:         newTFASessions(d.KV),
		totpBypass:  isTrue("RUSTDESK_API_DISABLE_TOTP") && isTrue("RUSTDESK_API_DISABLE_TOTP_ACKNOWLEDGED"),
		auditFromGo: d.Config == nil || !d.Config.APIEnabled,
		sysinfoLog:  &logThrottle{},
		metrics:     &metricLimit{},
		log:         log,
		ab:          d.Log.Child("API:AB"),
		peersLog:    d.Log.Child("API:PEERS"),
		groupLog:    d.Log.Child("API:DEVICE-GROUP"),
		strategyLog: d.Log.Child("API:STRATEGIES"),
	}
	if isTrue("RUSTDESK_API_DISABLE_TOTP") {
		if !h.totpBypass {
			log.Warn("[SECURITY] RUSTDESK_API_DISABLE_TOTP=true but ACK flag is missing. The bypass is IGNORED. Set RUSTDESK_API_DISABLE_TOTP_ACKNOWLEDGED=true to confirm you accept disabling 2FA on the RustDesk client login.")
		} else {
			log.Warn("[SECURITY] TOTP is DISABLED on the RustDesk client API. RustDesk desktop clients can log in with username+password only. The web panel still enforces 2FA independently.")
		}
	}

	r := d.Early
	auth := h.requireAuth
	r.Get("/api/telemetry/key", h.telemetryKey)
	r.Get("/api/login-options", h.loginOptions)
	r.Post("/api/heartbeat", h.heartbeat)
	r.Post("/api/sysinfo", h.sysinfo)
	r.Post("/api/sysinfo_ver", h.sysinfoVersion)
	r.Get("/api/ab", h.readAddressBook("legacy"))
	r.Post("/api/ab", h.saveAddressBook("legacy"))
	r.Get("/api/ab/personal", h.readAddressBook("personal"))
	r.Get("/api/audit", h.summary)
	r.Post("/api/ab/personal", h.saveAddressBook("personal"))
	r.Get("/api/ab/tags", h.addressBookTags)
	r.Get("/api/users", h.users, unlessBearer)
	r.Get("/api/peers", h.peers, unlessBearer)
	r.All("/api/peers/list", h.peersList, auth)
	r.Get("/api/device-group/accessible", h.sendDeviceGroups(true), auth)
	r.Get("/api/group", h.sendDeviceGroups(false), auth)
	r.Get("/api/group/get", h.sendDeviceGroups(false), auth)
	r.Post("/api/group/get", h.sendDeviceGroups(false), auth)
	r.Get("/api/device-group", h.sendDeviceGroups(false), auth)
	r.Post("/api/device-group", h.saveDeviceGroup, auth, requireAdmin)
	r.Get("/api/user/group", h.userGroup, auth)
	r.Get("/api/software/client-download-link", software)
	r.Get("/api/software", software)
	r.Post("/api/login", h.login)
	r.Post("/api/logout", h.logout)
	r.All("/api/currentUser", h.currentUser)
	r.Get("/api/server-key", h.serverKey)
	r.Get("/api/server-key/fingerprint", h.fingerprint)
	r.Post("/api/audit/conn", h.reportConnection)
	r.Get("/api/audit/conn", h.connections)
	r.Post("/api/audit/file", h.reportFile)
	r.Get("/api/audit/file", h.files, auth)
	r.Post("/api/audit/alarm", h.reportAlarm)
	r.Get("/api/audit/alarm", h.alarms, auth)
	r.Get("/api/user-groups", h.userGroups, auth)
	r.Post("/api/user-groups", h.saveUserGroup, auth, requireAdmin)
	r.Get("/api/strategies", h.strategies, unlessBearer, auth)
	r.Post("/api/strategies", h.saveStrategy, auth, requireAdmin)
	r.Delete("/api/strategies/{guid}", h.deleteStrategy, auth, requireAdmin)
	r.Get("/api/strategies/{guid}", h.strategy, unlessBearer, auth)
	r.Post("/api/strategies/assign", h.assignStrategy, auth, requireAdmin)
	r.Put("/api/strategies/{guid}/status", h.strategyStatus, auth, requireAdmin)
	r.Get("/api/devices", h.devices, unlessBearer, auth)
	r.Get("/api/peer-key/{id}", h.peerKey, auth)
}
