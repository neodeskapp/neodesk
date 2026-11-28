package i18n

// Routes and the guards the Node routes have. The handlers do no
// authentication themselves.
//
//	Route                                      Handler          Guards in Node
//	GET    /api/i18n/languages                 Languages        none
//	GET    /api/i18n/translations/{code}       Translations     none
//	POST   /api/i18n/set/{code}                Set              none (the CSRF check of every POST)
//	GET    /api/i18n/validate/{code}           Validate         requireAuth
//	POST   /api/i18n/upload                    Upload           requireAuth, requirePermission('server.config')
//	DELETE /api/i18n/{code}                    Delete           requireAuth, requirePermission('server.config')
//	GET    /api/panel/languages                PanelLanguages   fileAccessLimiter, requireAuth, requirePermission('server.config')
//	GET    /api/panel/languages/{code}/missing PanelMissing     fileAccessLimiter, requireAuth, requirePermission('server.config')
//	POST   /api/panel/languages/{code}/fix     PanelFix         uploadLimiter, requireAuth, requirePermission('server.config')
//	GET    /api/console/strings/{code}         Strings          none
//
// Every route under /api/ also passes the limiters and the JSON
// compression that server.js puts in front of all of /api/. A GET pattern
// also serves HEAD, as a GET route does in Express.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/httpx"
)

// Handlers serves the language routes.
type Handlers struct {
	// Bundle holds the loaded languages.
	Bundle *Bundle

	// Production is the console's production mode (NODE_ENV is
	// "production"). The routes answer the same in both modes.
	Production bool

	// SecureCookie says whether the language cookie that Set sends in
	// answer to r is Secure (the console's rule for its cookies,
	// session.Manager.SecureCookie). Nil means never. The cookie Language
	// sets for ?lang= never has it.
	SecureCookie func(r *http.Request) bool

	// PreferredLanguage returns the stored language of the signed-in
	// user (the session's user.preferred_language), or "" when nobody is
	// signed in or nothing is stored. Used by Language. May be nil.
	PreferredLanguage func(r *http.Request) string

	// SetPreferredLanguage stores a language choice for the signed-in
	// user. Set calls it for every accepted choice; it must do nothing
	// when the session has no user id. Otherwise it writes
	// users.preferred_language and sets user.preferred_language in the
	// session, the latter also when the database write fails. Set logs a
	// returned error and still answers success. May be nil.
	SetPreferredLanguage func(r *http.Request, code string) error

	// Audit writes an audit log entry for the signed-in user and the
	// client address of r (db.logAction(userId, action, details, ip)).
	// When it fails the route answers 500 although the change is done.
	// May be nil.
	Audit func(r *http.Request, action, details string) error

	// Now is the clock for cookie expiry. time.Now when nil.
	Now func() time.Time

	// Logf writes error lines. log.Printf when nil.
	Logf func(format string, args ...any)

	// OnChange hears that language code was uploaded or deleted, so the
	// other instances of the installation read it again; nil for none.
	OnChange func(code string)

	stringsMu sync.Mutex
	cache     map[string]*stringsEntry
}

// Reload reads the language files again and drops the strings of code:
// another instance changed them.
func (h *Handlers) Reload(code string) {
	h.Bundle.Load()
	h.forgetStrings(code)
}

func (h *Handlers) changed(code string) {
	if h.OnChange != nil {
		h.OnChange(code)
	}
}

func (h *Handlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handlers) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// obj builds a JSON object from key, value pairs in order.
func obj(pairs ...any) *Object {
	out := NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

func send(w http.ResponseWriter, r *http.Request, status int, v any) {
	httpx.WriteJSONBytes(w, r, status, []byte(stringify(v, "")))
}

func fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	send(w, r, status, obj("success", false, "error", message))
}

// Language picks the language of r and, when ?lang= names a language,
// adds the cookie that remembers it to the answer. The error is always
// nil: every request gets a language.
func (h *Handlers) Language(w http.ResponseWriter, r *http.Request) (string, error) {
	preferred := ""
	if h.PreferredLanguage != nil {
		preferred = h.PreferredLanguage(r)
	}
	sel := h.Bundle.RequestLanguage(r, preferred, h.now())
	if sel.SetCookie != "" {
		w.Header().Add("Set-Cookie", sel.SetCookie)
	}
	return sel.Lang, nil
}

// Languages answers GET /api/i18n/languages: the listed languages keyed by
// code and as a list sorted by name.
func (h *Handlers) Languages(w http.ResponseWriter, r *http.Request) {
	sorted := h.Bundle.sortedList()
	items := make([]any, len(sorted))
	for i, e := range sorted {
		items[i] = e
	}
	send(w, r, http.StatusOK, obj("success", true, "data", h.Bundle.Available(), "list", items))
}

// Translations answers GET /api/i18n/translations/{code}: the texts of a
// language with the default language filling the gaps.
func (h *Handlers) Translations(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !ValidCode(code) {
		fail(w, r, http.StatusBadRequest, "Invalid language code")
		return
	}
	if !h.Bundle.Has(code) {
		fail(w, r, http.StatusNotFound, "Language not found")
		return
	}
	send(w, r, http.StatusOK, obj("success", true, "data", h.Bundle.Merged(code)))
}

// Set answers POST /api/i18n/set/{code}: it sets the language cookie and
// stores the choice for the signed-in user.
func (h *Handlers) Set(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !ValidCode(code) || !h.Bundle.Has(code) {
		fail(w, r, http.StatusBadRequest, "Language not supported")
		return
	}
	w.Header().Add("Set-Cookie", LangCookie(code, h.SecureCookie != nil && h.SecureCookie(r), h.now()))
	if h.SetPreferredLanguage != nil {
		if err := h.SetPreferredLanguage(r, code); err != nil {
			h.logf("[i18n] Failed to persist language: %v", err)
		}
	}
	send(w, r, http.StatusOK, obj("success", true))
}

// Validate answers GET /api/i18n/validate/{code}: which keys of the
// default language a language lacks and which it has on top.
func (h *Handlers) Validate(w http.ResponseWriter, r *http.Request) {
	report := h.Bundle.Validate(r.PathValue("code"))
	send(w, r, http.StatusOK, obj("success", true, "data", report.Object()))
}

// Upload answers POST /api/i18n/upload: a multipart form with one JSON
// file in the field "file". The language code comes from the file's
// _meta.code, else the form's "code" field, else the file name without
// ".json", and must be one of the languages the console loads. The file
// is kept in memory, at most 1 MiB; nothing is written to a temporary
// folder.
func (h *Handlers) Upload(w http.ResponseWriter, r *http.Request) {
	form := readUpload(r)
	if form.failure != "" {
		status := http.StatusBadRequest
		if form.tooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		fail(w, r, status, form.failure)
		return
	}
	if !form.hasFile {
		fail(w, r, http.StatusBadRequest, "No file uploaded")
		return
	}
	translations, err := parseJSON(decodeUTF8(form.data))
	if err != nil {
		fail(w, r, http.StatusBadRequest, "Invalid JSON file")
		return
	}
	crash := func(reason string) {
		h.logf("Upload language error: %s", reason)
		fail(w, r, http.StatusInternalServerError, "Failed to upload language")
	}
	if translations == nil {
		crash("TypeError: Cannot read properties of null (reading '_meta')")
		return
	}
	var code any = undefined
	if meta := property(translations, "_meta"); meta != nil && meta != undefined {
		code = property(meta, "code")
	}
	if !truthy(code) {
		code = form.code
	}
	if !truthy(code) {
		code = strings.Replace(form.originalName, ".json", "", 1)
	}
	// Only a string that names a language of the table is ever loaded
	name, isString := code.(string)
	if _, known := knownLanguageFor(name); !isString || !known {
		fail(w, r, http.StatusBadRequest, "Invalid language code")
		return
	}
	if result := h.Bundle.Save(name, translations); !result.Success {
		status := http.StatusInternalServerError
		if result.BuiltIn {
			status = http.StatusBadRequest
		}
		fail(w, r, status, result.Error)
		return
	}
	h.forgetStrings(name)
	h.changed(name)
	if h.Audit != nil {
		if err := h.Audit(r, "language_uploaded", "Language "+name+" uploaded"); err != nil {
			crash(err.Error())
			return
		}
	}
	var meta any
	if m := h.Bundle.Meta(name); m != nil {
		meta = m
	}
	send(w, r, http.StatusOK, obj("success", true, "data", obj("code", name, "meta", meta)))
}

// Delete answers DELETE /api/i18n/{code}.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if result := h.Bundle.Delete(code); !result.Success {
		status := http.StatusBadRequest
		if result.NotFound {
			status = http.StatusNotFound
		}
		fail(w, r, status, result.Error)
		return
	}
	h.forgetStrings(code)
	h.changed(code)
	if h.Audit != nil {
		if err := h.Audit(r, "language_deleted", "Language "+code+" deleted"); err != nil {
			h.logf("Delete language error: %v", err)
			fail(w, r, http.StatusInternalServerError, "Failed to delete language")
			return
		}
	}
	send(w, r, http.StatusOK, obj("success", true))
}

// --- the strings endpoint ---

var stringsCode = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z]{2,4})?$`)

// stringsEntry is one language's merged texts as sent to the web app. It
// is made on first use and kept until an upload or a delete changes the
// language, or the default language that fills it.
type stringsEntry struct {
	body     []byte
	revision string
	gzip     []byte
}

// forgetStrings drops the strings a change of code makes stale: code's
// own, or every language's when code is the default language.
func (h *Handlers) forgetStrings(code string) {
	h.stringsMu.Lock()
	defer h.stringsMu.Unlock()
	if code == h.Bundle.DefaultLang {
		clear(h.cache)
		return
	}
	delete(h.cache, code)
}

func (h *Handlers) stringsFor(code string) *stringsEntry {
	h.stringsMu.Lock()
	defer h.stringsMu.Unlock()
	if e, ok := h.cache[code]; ok {
		return e
	}
	body := []byte(stringify(h.Bundle.Merged(code), ""))
	sum := sha256.Sum256(body)
	e := &stringsEntry{body: body, revision: hex.EncodeToString(sum[:])[:16]}
	if h.cache == nil {
		h.cache = map[string]*stringsEntry{}
	}
	h.cache[code] = e
	return e
}

func (h *Handlers) gzipped(e *stringsEntry) []byte {
	h.stringsMu.Lock()
	defer h.stringsMu.Unlock()
	if e.gzip == nil {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(e.body)
		_ = zw.Close()
		e.gzip = buf.Bytes()
	}
	return e.gzip
}

// StringsRevision is the revision of a language's strings: the first 16
// hex digits of the SHA-256 of the merged JSON. The web app asks for the
// strings under ?v=<revision> and may then keep them for a year.
func (h *Handlers) StringsRevision(lang string) string {
	return h.stringsFor(lang).revision
}

// StringsURL is the address the web app loads a language's strings from.
func (h *Handlers) StringsURL(lang string) string {
	return "/api/console/strings/" + encodeURIComponent(lang) + "?v=" + h.StringsRevision(lang)
}

// Strings answers GET /api/console/strings/{code}: one language's texts
// merged over the default language, as plain JSON without an envelope.
//
// The ETag is the revision in quotes. With ?v=<revision> the answer may be
// cached for a year, otherwise it must be revalidated. A request that
// names the tag (httpx.Fresh) gets 304 without a body. The body goes out
// gzip-compressed when Accept-Encoding allows gzip. An unknown code is
// 404.
func (h *Handlers) Strings(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !stringsCode.MatchString(code) || !h.Bundle.Has(code) {
		fail(w, r, http.StatusNotFound, "Language not found")
		return
	}
	entry := h.stringsFor(code)
	etag := `"` + entry.revision + `"`
	head := w.Header()
	head.Set("ETag", etag)
	head.Set("Content-Type", "application/json; charset=utf-8")
	if v, _ := qsParam(r.URL.RawQuery, "v").(string); v == entry.revision {
		head.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		head.Set("Cache-Control", "no-cache")
	}
	head.Set("Vary", "Accept-Encoding")
	if httpx.Fresh(r, etag, time.Time{}) {
		head.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := entry.body
	if httpx.AcceptsGzip(strings.Join(r.Header.Values("Accept-Encoding"), ", ")) {
		head.Set("Content-Encoding", "gzip")
		body = h.gzipped(entry)
	}
	head.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
