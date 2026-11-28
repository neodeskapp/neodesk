package sites

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/neodeskapp/neodesk/neodesk-server/console/jsbind"
	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/middleware"
	"github.com/neodeskapp/neodesk/neodesk-server/console/sitedata"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

// pinAnswer is the data after a pin changed: the pinned site, and for a
// device the site it is at now (the pin, its folder's pin or its
// address).
type pinAnswer struct {
	Pin  *sitedata.SiteRef `json:"pin"`
	Site *sitedata.SiteRef `json:"site,omitempty"`
}

// pinTarget reads {site_id} of a pin request: "" or null unpins. A site
// the account may not see is "not found"; the site is returned for a pin.
func (h *handlers) pinTarget(w http.ResponseWriter, r *http.Request) (site *db.Site, ok bool) {
	v := kit.Body(r).Get("site_id")
	id, isText := v.Str()
	if k := v.Kind(); k != middleware.KindUndefined && k != middleware.KindNull && !isText {
		fail(w, r, http.StatusBadRequest, "sites.pin_invalid")
		return nil, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, true
	}
	sc, ok := h.scope(w, r)
	if !ok {
		return nil, false
	}
	site = h.site(w, r, sc, id)
	return site, site != nil
}

// pinDevice answers PATCH /api/devices/{id}/site with {site_id}: the
// device is at that site whatever its address says; "" takes the pin
// away. Only a site of the device's organization takes it.
func (h *handlers) pinDevice(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	device := h.d.Backend.GetDeviceByID(ctx, id, false)
	if device == nil {
		fail(w, r, http.StatusNotFound, "devices.not_found")
		return
	}
	if allowed, err := h.d.DeviceGroups.UserCanAccessDevice(ctx, kit.User(r), device); err != nil {
		h.serverError(w, r, "Device scope error:", err)
		return
	} else if !allowed {
		fail(w, r, http.StatusForbidden, "errors.forbidden")
		return
	}
	site, ok := h.pinTarget(w, r)
	if !ok {
		return
	}
	siteID := ""
	if site != nil {
		siteID = site.ID
	}
	if err := h.data.Sites().SetPin(db.SitePinDevice, id, siteID); err != nil {
		h.writeError(w, r, "Pin device error:", err)
		return
	}
	out := pinAnswer{Site: h.data.DeviceSite(id, device.Str("ip"))}
	if site != nil {
		out.Pin = &sitedata.SiteRef{ID: site.ID, Name: site.Name}
		h.audit(r, "device_site_pinned", fmt.Sprintf("Pinned device %s to site %s (%s)", id, site.Name, site.ID))
	} else {
		h.audit(r, "device_site_pinned", fmt.Sprintf("Removed the site pin of device %s", id))
	}
	kit.OK(w, r, out)
}

// pinFolder answers PATCH /api/folders/{id}/site with {site_id}: the
// folder's devices are at that site unless a device has its own pin.
func (h *handlers) pinFolder(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	raw := r.PathValue("id")
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 {
		fail(w, r, http.StatusNotFound, "folders.not_found")
		return
	}
	folder, err := h.d.DeviceGroups.FolderByID(r.Context(), jsbind.Number(float64(n)))
	if err != nil {
		h.serverError(w, r, "Get folder error:", err)
		return
	}
	if folder == nil {
		fail(w, r, http.StatusNotFound, "folders.not_found")
		return
	}
	site, ok := h.pinTarget(w, r)
	if !ok {
		return
	}
	siteID := ""
	if site != nil {
		siteID = site.ID
	}
	ref := strconv.FormatInt(n, 10)
	if err := h.data.Sites().SetPin(db.SitePinFolder, ref, siteID); err != nil {
		h.writeError(w, r, "Pin folder error:", err)
		return
	}
	out := pinAnswer{}
	if site != nil {
		out.Pin = &sitedata.SiteRef{ID: site.ID, Name: site.Name}
		h.audit(r, "folder_site_pinned", fmt.Sprintf("Pinned folder %s to site %s (%s)", ref, site.Name, site.ID))
	} else {
		h.audit(r, "folder_site_pinned", fmt.Sprintf("Removed the site pin of folder %s", ref))
	}
	kit.OK(w, r, out)
}
