// Package sites holds the routes of the Sites page: sites with their
// networks and relay nodes, enrollment strings for new nodes, revoking and
// renaming nodes, the probe results between nodes, and pinning a device or
// a folder to a site.
//
// Reading needs device.view; changing sites and nodes needs server.config
// or org.edit; pinning needs device.edit, as moving a device into a folder
// does. Accounts of the server-level roles see every site; any other
// account sees the sites of the organizations it belongs to (the sites of
// no organization when it belongs to none). A site or node outside that is
// "not found".
//
// The data is the server's database (kit.Deps.Sites). Reads work while
// relay nodes are off; an enrollment string needs the running hub.
//
// PATCH /api/devices/{id}/site is behind the device scope check of the
// device group (kit.RequireDeviceInScope), which comes first in the table.
package sites

import (
	"errors"
	"net/http"

	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/sitedata"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
	sitesvc "github.com/neodeskapp/neodesk/neodesk-server/sites"
)

type handlers struct {
	d    *kit.Deps
	data *sitedata.Service
	log  *kit.Logger
}

// Register adds the group's routes.
func Register(d *kit.Deps) {
	h := &handlers{d: d, data: d.Sites, log: d.Log.Child("SITES")}
	r := d.Router
	view := []kit.Middleware{d.RequireAuth, d.RequirePermission("device.view")}
	edit := []kit.Middleware{d.RequireAuth, d.RequireAnyPermission("server.config", "org.edit")}
	pin := []kit.Middleware{d.RequireAuth, d.RequirePermission("device.edit")}

	r.Get("/api/sites", h.list, view...)
	r.Get("/api/sites/reachability", h.reachability, view...)
	r.Post("/api/sites", h.create, edit...)
	r.Patch("/api/sites/{id}", h.update, edit...)
	r.Delete("/api/sites/{id}", h.remove, edit...)
	r.Put("/api/sites/{id}/networks", h.networks, edit...)
	r.Post("/api/sites/{id}/enrollment-tokens", h.enrollmentToken, edit...)
	r.Post("/api/relay-nodes/{id}/revoke", h.revokeNode, edit...)
	r.Patch("/api/relay-nodes/{id}", h.renameNode, edit...)
	r.Patch("/api/devices/{id}/site", h.pinDevice, pin...)
	r.Patch("/api/folders/{id}/site", h.pinFolder, pin...)
}

// fail answers {"success":false,"error":<text of key>}.
func fail(w http.ResponseWriter, r *http.Request, status int, key string, vars ...any) {
	kit.Fail(w, r, status, kit.T(r, key, kit.Vars(vars...)))
}

// serverError logs what failed and answers 500.
func (h *handlers) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.log.Error(what, err)
	fail(w, r, http.StatusInternalServerError, "errors.server_error")
}

// ready answers 503 when the console has no server database, and then
// reports false.
func (h *handlers) ready(w http.ResponseWriter, r *http.Request) bool {
	if h.data == nil {
		fail(w, r, http.StatusServiceUnavailable, "sites.unavailable")
		return false
	}
	return true
}

// scope is the organizations whose sites the account may see.
func (h *handlers) scope(w http.ResponseWriter, r *http.Request) (sitedata.Scope, bool) {
	user := kit.User(r)
	if user == nil {
		fail(w, r, http.StatusUnauthorized, "errors.unauthorized")
		return sitedata.Scope{}, false
	}
	sc, err := h.data.ScopeFor(user.ID, user.Role)
	if err != nil {
		h.serverError(w, r, "Sites scope error:", err)
		return sitedata.Scope{}, false
	}
	return sc, true
}

// site reads the site of the path when the account may see it; it
// answers 404 or 500 otherwise and reports nil.
func (h *handlers) site(w http.ResponseWriter, r *http.Request, sc sitedata.Scope, id string) *db.Site {
	if id == "" {
		fail(w, r, http.StatusNotFound, "sites.not_found")
		return nil
	}
	site, err := h.data.DB().GetSite(id)
	if err != nil && !errors.Is(err, db.ErrSiteNotFound) {
		h.serverError(w, r, "Get site error:", err)
		return nil
	}
	if site == nil || !sc.Has(site.OrgID) {
		fail(w, r, http.StatusNotFound, "sites.not_found")
		return nil
	}
	return site
}

// node reads the node of the path when the account may see its site.
func (h *handlers) node(w http.ResponseWriter, r *http.Request, sc sitedata.Scope) *db.RelayNode {
	id := r.PathValue("id")
	var n *db.RelayNode
	var err error
	if id != "" {
		n, err = h.data.DB().GetRelayNode(id)
	}
	if err != nil && !errors.Is(err, db.ErrSiteNotFound) {
		h.serverError(w, r, "Get relay node error:", err)
		return nil
	}
	if n != nil {
		site, err := h.data.DB().GetSite(n.SiteID)
		if err != nil && !errors.Is(err, db.ErrSiteNotFound) {
			h.serverError(w, r, "Get site error:", err)
			return nil
		}
		if site != nil && sc.Has(site.OrgID) {
			return n
		}
	}
	fail(w, r, http.StatusNotFound, "sites.node_not_found")
	return nil
}

// writeError answers an error of the sites service.
func (h *handlers) writeError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, sitesvc.ErrNotFound):
		fail(w, r, http.StatusNotFound, "sites.not_found")
	case errors.Is(err, sitesvc.ErrInvalidName):
		fail(w, r, http.StatusBadRequest, "sites.name_invalid", "max", sitesvc.MaxNameLen)
	case errors.Is(err, sitesvc.ErrOtherOrg):
		fail(w, r, http.StatusBadRequest, "sites.pin_other_org")
	case errors.Is(err, sitesvc.ErrInvalidPin):
		fail(w, r, http.StatusBadRequest, "sites.pin_invalid")
	default:
		h.serverError(w, r, what, err)
	}
}

// audit writes the audit log; a failure is logged only.
func (h *handlers) audit(r *http.Request, action, details string) {
	if h.d.Audit == nil {
		return
	}
	if err := h.d.LogAction(r, action, details); err != nil {
		h.log.Error("Audit log error:", err)
	}
}

type done struct {
	Success bool `json:"success"`
}
