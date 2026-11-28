package folders

import (
	"context"
	"math"
	"net/http"
	"strconv"

	"github.com/neodeskapp/neodesk/neodesk-server/console/access"
	"github.com/neodeskapp/neodesk/neodesk-server/console/devicegroups"
	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/jsbind"
	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/middleware"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/internal/jsreq"
)

const (
	defaultColor = "#6366f1"
	defaultIcon  = "folder"
	// folderNote is the note of the device group that carries a folder's
	// access lists.
	folderNote = "NeoDesk folder access scope"
	// maxName is the longest folder name, in UTF-16 code units.
	maxName = 50
)

// maxFolderID is the largest id the folders table's integer column holds.
const maxFolderID = math.MaxInt32

// invalidFolderID is the answer to a folder id that no folder can have.
const invalidFolderID = "Invalid folder id"

// folderIDOf reads a folder id written in digits: a whole number from 0
// to maxFolderID. ok is false for any other text.
func folderIDOf(text string) (id jsbind.Value, ok bool) {
	if text == "" || len(text) > 10 {
		return jsbind.Undefined, false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return jsbind.Undefined, false
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n > maxFolderID {
		return jsbind.Undefined, false
	}
	return jsbind.Number(float64(n)), true
}

// folderID reads the folder id of the address. It answers 400 for one
// that is not written in digits or is beyond maxFolderID, and then
// reports false.
func folderID(w http.ResponseWriter, r *http.Request) (jsbind.Value, bool) {
	id, ok := folderIDOf(r.PathValue("id"))
	if !ok {
		kit.Fail(w, r, http.StatusBadRequest, invalidFolderID)
	}
	return id, ok
}

// bodyFolderID reads a folder id of the body: a whole number from 0 to
// maxFolderID, as a number or as text in digits. ok is false for
// anything else.
func bodyFolderID(v middleware.Value) (jsbind.Value, bool) {
	if text, isText := v.Str(); isText {
		return folderIDOf(text)
	}
	f, isNumber := v.Float()
	if !isNumber || f != math.Trunc(f) || f < 0 || f > maxFolderID {
		return jsbind.Undefined, false
	}
	return jsbind.Number(f), true
}

// groupGUID is `folder_${id}` for a folder id as JavaScript wrote it.
func groupGUID(id any) string {
	return "folder_" + jsreq.String(plainOf(id))
}

// plainOf is a value of a row or a request as a plain JSON value.
func plainOf(v any) any {
	switch x := v.(type) {
	case jsbind.Value:
		parsed, err := goapi.Parse([]byte(x.JSON()))
		if err != nil || x.IsUndefined() {
			return nil
		}
		return parsed
	}
	return devicegroups.Plain(v)
}

// bindable is a plain value as a request value, for a statement.
func bindable(v any) jsbind.Value {
	switch x := plainOf(v).(type) {
	case nil:
		return jsbind.Null
	case bool:
		return jsbind.Bool(x)
	case float64:
		return jsbind.Number(x)
	case string:
		return jsbind.String(x)
	}
	return jsbind.String(jsreq.String(plainOf(v)))
}

// list answers GET /api/folders: every folder with device_count, the
// number of its devices the account may see, and the folder's access
// lists. When the counts cannot be made the folders go out as far as
// they got.
func (h *handlers) list(w http.ResponseWriter, r *http.Request) {
	folders, err := h.groups.AllFolders(r.Context())
	if err != nil {
		h.serverError(w, r, "Get folders error:", err)
		return
	}
	if err := h.countAndLists(r, folders); err != nil {
		h.log.Error("Failed to compute folder device counts:", err.Error())
	}
	// The site each folder is pinned to, on a server that has sites
	if h.d.Sites.Finder() != nil {
		pins := h.d.Sites.FolderPins()
		for _, f := range folders {
			var site any
			if id := pins[jsreq.String(plainOf(f.Get("id")))]; id != "" {
				site = id
			}
			f.Set("site_id", site)
		}
	}
	answer(w, r, http.StatusOK, goapi.NewObject("success", true, "data", goapi.NewObject("folders", objects(folders), "total", float64(len(folders)))))
}

// objects is a list of rows as a JSON list.
func objects(rows []*goapi.Object) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// countAndLists sets device_count, allowed_users and allowed_groups on
// each folder.
func (h *handlers) countAndLists(r *http.Request, folders []*goapi.Object) error {
	ctx := r.Context()
	assignments, err := h.groups.AllFolderAssignments(ctx)
	if err != nil {
		return err
	}
	// A person limited to some devices counts only those
	user := kit.User(r)
	scope, err := h.groups.DeviceScopeForUser(ctx, user, nil)
	if err != nil {
		return err
	}
	if scope != nil {
		if scope, err = h.groups.DeviceScopeForUser(ctx, user, h.backend.GetAllDevices(ctx, goapi.DeviceFilters{})); err != nil {
			return err
		}
	}
	counts := map[float64]float64{}
	for deviceID, raw := range assignments {
		if scope != nil && !scope.Has(deviceID) {
			continue
		}
		folder := jsreq.Number(raw)
		if math.IsNaN(folder) || math.IsInf(folder, 0) {
			continue
		}
		counts[folder]++
	}
	for _, f := range folders {
		f.Set("device_count", counts[jsreq.Number(plainOf(f.Get("id")))])
		h.accessLists(ctx, f)
	}
	return nil
}

// accessLists sets allowed_users and allowed_groups on a folder from its
// device group; both are empty when there is none or it cannot be read.
func (h *handlers) accessLists(ctx context.Context, folder *goapi.Object) {
	users, groups := []any{}, []any{}
	if group, err := h.groups.DeviceGroupByGUID(ctx, groupGUID(folder.Get("id"))); err == nil && group != nil {
		if list, ok := group.Get("allowed_users").([]any); ok {
			users = list
		}
		if list, ok := group.Get("allowed_groups").([]any); ok {
			groups = list
		}
	}
	folder.Set("allowed_users", users)
	folder.Set("allowed_groups", groups)
}

// ensureGroup creates or refreshes the device group that carries a
// folder's access lists and returns its guid.
func (h *handlers) ensureGroup(ctx context.Context, id, name any) (string, error) {
	guid := groupGUID(id)
	data := devicegroups.GroupData{
		GUID: jsbind.String(guid), Name: bindable(name), Note: jsbind.String(folderNote),
		SourceType: jsbind.String("manual"), TagFilter: jsbind.String(""),
	}
	group, err := h.groups.DeviceGroupByGUID(ctx, guid)
	if err != nil {
		return "", err
	}
	if group != nil {
		if _, err := h.groups.UpdateDeviceGroup(ctx, guid, data); err != nil {
			return "", err
		}
		return guid, nil
	}
	if group, err = h.groups.CreateDeviceGroup(ctx, data); err != nil {
		return "", err
	}
	return jsreq.String(plainOf(group.Get("guid"))), nil
}

// setAccess writes the access lists the body names (allowed_users,
// allowed_groups) to the folder's device group. Without allowed_groups
// and with always set, the group is created or refreshed anyway.
func (h *handlers) setAccess(ctx context.Context, body middleware.JSONObject, id, name any, always bool) error {
	if body.Has("allowed_users") {
		guid, err := h.ensureGroup(ctx, id, name)
		if err != nil {
			return err
		}
		if _, err := h.groups.SetDeviceGroupUserAccess(ctx, guid, access.NormalizeUsernames(decoded(body.Get("allowed_users")))); err != nil {
			return err
		}
	}
	if body.Has("allowed_groups") {
		guid, err := h.ensureGroup(ctx, id, name)
		if err != nil {
			return err
		}
		_, err = h.groups.SetDeviceGroupUserGroupAccess(ctx, guid, access.NormalizeGroupGUIDs(decoded(body.Get("allowed_groups"))))
		return err
	}
	if always {
		_, err := h.ensureGroup(ctx, id, name)
		return err
	}
	return nil
}

// folderRights returns the device group that carries the folder's access
// lists (nil when there is none) and what the account may do with it
// (access.GroupRightsForUser).
func (h *handlers) folderRights(ctx context.Context, r *http.Request, id any) (*access.DeviceGroup, access.GroupRights, error) {
	group, err := h.d.Access.DeviceGroupByGUID(ctx, groupGUID(id))
	if err != nil {
		return nil, access.GroupRights{}, err
	}
	rights, err := h.d.Access.GroupRightsForUser(ctx, devicegroups.ScopeUser(kit.User(r)), group)
	return group, rights, err
}

// changesLists reports whether the body's access lists differ from the
// group's (none when group is nil). A list the body leaves out stays.
func changesLists(body middleware.JSONObject, group *access.DeviceGroup) bool {
	var users, groups []string
	if group != nil {
		users, groups = group.AllowedUsers, group.AllowedGroups
	}
	return body.Has("allowed_users") && !sameEntries(access.NormalizeUsernames(decoded(body.Get("allowed_users"))), access.StoredUsernames(users)) ||
		body.Has("allowed_groups") && !sameEntries(access.NormalizeGroupGUIDs(decoded(body.Get("allowed_groups"))), access.StoredGroupGUIDs(groups))
}

// sameEntries reports whether two lists without repeats hold the same
// entries, in any order.
func sameEntries(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	have := make(map[string]bool, len(a))
	for _, entry := range a {
		have[entry] = true
	}
	for _, entry := range b {
		if !have[entry] {
			return false
		}
	}
	return true
}

// decoded is a body value decoded the way the access rules read values:
// nil, bool, float64, string, []any, map[string]any.
func decoded(v middleware.Value) any {
	var out any
	if err := v.Decode(&out); err != nil {
		return nil
	}
	return out
}

// checkName checks a submitted folder name. It returns the trimmed name,
// or answers 400 for a name that is no text, blank or too long, and then
// returns ok false.
func (h *handlers) checkName(w http.ResponseWriter, r *http.Request, name middleware.Value) (string, bool) {
	text, isText := name.Str()
	trimmed := jsbind.Trim(text)
	if !isText || trimmed == "" {
		fail(w, r, http.StatusBadRequest, "folders.name_required")
		return "", false
	}
	if jsreq.Length(text) > maxName {
		fail(w, r, http.StatusBadRequest, "folders.name_too_long")
		return "", false
	}
	return trimmed, true
}

// checkLook checks the color and icon of a body: each may be missing,
// null or text. It answers 400 for anything else and then reports false.
func (h *handlers) checkLook(w http.ResponseWriter, r *http.Request, body middleware.JSONObject) bool {
	for _, field := range []struct{ name, refusal string }{{"color", "Invalid folder color"}, {"icon", "Invalid folder icon"}} {
		switch body.Get(field.name).Kind() {
		case middleware.KindUndefined, middleware.KindNull, middleware.KindString:
		default:
			kit.Fail(w, r, http.StatusBadRequest, field.refusal)
			return false
		}
	}
	return true
}

// checkAccessLists answers 400 for an access list of the body with more
// than devicegroups.MaxAccessListEntries different accounts or user
// groups, which the save would cut, and then reports false.
func (h *handlers) checkAccessLists(w http.ResponseWriter, r *http.Request, body middleware.JSONObject) bool {
	var users, groups any
	if body.Has("allowed_users") {
		users = decoded(body.Get("allowed_users"))
	}
	if body.Has("allowed_groups") {
		groups = decoded(body.Get("allowed_groups"))
	}
	if list := devicegroups.AccessListTooLong(users, groups); list != "" {
		kit.Fail(w, r, http.StatusBadRequest, list+" can hold at most "+strconv.Itoa(devicegroups.MaxAccessListEntries)+" entries")
		return false
	}
	return true
}

// create answers POST /api/folders with {name, color?, icon?,
// allowed_users?, allowed_groups?}.
func (h *handlers) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body := kit.Body(r)
	raw := jsreq.Body(r)
	name := body.Get("name")
	if !name.Truthy() {
		fail(w, r, http.StatusBadRequest, "folders.name_required")
		return
	}
	trimmed, ok := h.checkName(w, r, name)
	if !ok {
		return
	}
	text, _ := name.Str()
	if !h.checkLook(w, r, body) || !h.checkAccessLists(w, r, body) {
		return
	}
	if changesLists(body, nil) {
		rights, err := h.d.Access.GroupRightsForUser(ctx, devicegroups.ScopeUser(kit.User(r)), nil)
		if err != nil {
			h.serverError(w, r, "Create folder error:", err)
			return
		}
		if !rights.ChangeAccess {
			fail(w, r, http.StatusForbidden, "errors.no_permission")
			return
		}
	}
	color, icon := jsbind.Of(body.Get("color")), jsbind.Of(body.Get("icon"))
	id, err := h.groups.CreateFolder(ctx, jsbind.String(trimmed), color.Or(jsbind.String(defaultColor)), icon.Or(jsbind.String(defaultIcon)))
	if err != nil {
		h.serverError(w, r, "Create folder error:", err)
		return
	}
	folder := goapi.NewObject(
		"id", id,
		"name", trimmed,
		"color", orPlain(jsreq.Prop(raw, "color"), defaultColor),
		"icon", orPlain(jsreq.Prop(raw, "icon"), defaultIcon),
	)
	if err := h.setAccess(ctx, body, id, trimmed, false); err != nil {
		h.serverError(w, r, "Create folder error:", err)
		return
	}
	h.accessLists(ctx, folder)
	if err := h.d.LogAction(r, "folder_created", "Created folder: "+text); err != nil {
		h.serverError(w, r, "Create folder error:", err)
		return
	}
	answer(w, r, http.StatusOK, goapi.NewObject("success", true, "data", folder))
}

// orPlain is value || fallback for a plain JSON value.
func orPlain(v any, fallback string) any {
	if goapi.Truthy(v) {
		return v
	}
	return fallback
}

// update answers PATCH /api/folders/{id} with the fields to change.
func (h *handlers) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := folderID(w, r)
	if !ok {
		return
	}
	body := kit.Body(r)
	folder, err := h.groups.FolderByID(ctx, id)
	if err != nil {
		h.serverError(w, r, "Update folder error:", err)
		return
	}
	if folder == nil {
		fail(w, r, http.StatusNotFound, "folders.not_found")
		return
	}
	group, rights, err := h.folderRights(ctx, r, id)
	if err != nil {
		h.serverError(w, r, "Update folder error:", err)
		return
	}
	if !rights.Change || !rights.ChangeAccess && changesLists(body, group) {
		fail(w, r, http.StatusForbidden, "errors.no_permission")
		return
	}
	name := jsbind.Undefined
	newName := folder.Get("name")
	if given := body.Get("name"); given.Exists() {
		trimmed, ok := h.checkName(w, r, given)
		if !ok {
			return
		}
		name, newName = jsbind.String(trimmed), trimmed
	}
	if !h.checkLook(w, r, body) || !h.checkAccessLists(w, r, body) {
		return
	}
	if err := h.groups.UpdateFolder(ctx, id, name, jsbind.Of(body.Get("color")), jsbind.Of(body.Get("icon"))); err != nil {
		h.serverError(w, r, "Update folder error:", err)
		return
	}
	if err := h.setAccess(ctx, body, id, newName, true); err != nil {
		h.serverError(w, r, "Update folder error:", err)
		return
	}
	logged := jsreq.String(plainOf(folder.Get("name")))
	if text, _ := body.Get("name").Str(); text != "" {
		logged = text
	}
	if err := h.d.LogAction(r, "folder_updated", "Updated folder: "+logged); err != nil {
		h.serverError(w, r, "Update folder error:", err)
		return
	}
	answer(w, r, http.StatusOK, done{Success: true})
}

// remove answers DELETE /api/folders/{id}: the folder goes, its devices
// are in no folder any more and its device group goes too. A folder with
// an access list goes only for an account that may change the list
// (access.GroupRights.Delete).
func (h *handlers) remove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := folderID(w, r)
	if !ok {
		return
	}
	folder, err := h.groups.FolderByID(ctx, id)
	if err != nil {
		h.serverError(w, r, "Delete folder error:", err)
		return
	}
	if folder == nil {
		fail(w, r, http.StatusNotFound, "folders.not_found")
		return
	}
	if _, rights, err := h.folderRights(ctx, r, id); err != nil {
		h.serverError(w, r, "Delete folder error:", err)
		return
	} else if !rights.Delete {
		fail(w, r, http.StatusForbidden, "errors.no_permission")
		return
	}
	if err := h.groups.UnassignDevicesFromFolder(ctx, id); err != nil {
		h.serverError(w, r, "Delete folder error:", err)
		return
	}
	if err := h.groups.DeleteFolder(ctx, id); err != nil {
		h.serverError(w, r, "Delete folder error:", err)
		return
	}
	// The device group is left when it cannot be deleted
	_ = h.groups.DeleteDeviceGroup(ctx, groupGUID(id))
	if err := h.d.LogAction(r, "folder_deleted", "Deleted folder: "+jsreq.String(plainOf(folder.Get("name")))); err != nil {
		h.serverError(w, r, "Delete folder error:", err)
		return
	}
	answer(w, r, http.StatusOK, done{Success: true})
}
