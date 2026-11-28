package branding

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/ssrfguard"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store"
	"github.com/neodeskapp/neodesk/neodesk-server/logging"
)

// Options are the settings of a Service. The paths are the places the
// Node console used; "<console>" stands for its folder (web-nodejs).
type Options struct {
	// DB holds branding_config and branding_profiles.
	DB *store.DB

	// OnChange hears that the settings were saved or reset, so the other
	// instances of the installation read them again; nil for none.
	OnChange func()

	// UploadsDir is where logo and background pictures are stored and
	// served from as /uploads/<file>. Node: <DATA_DIR>/uploads, and
	// <console>/data/uploads without DATA_DIR.
	UploadsDir string
	// FontsDir holds one folder per installed font, served as
	// /fonts/<name>/: DATA_DIR/fonts. Node: <console>/public/fonts.
	FontsDir string
	// FontCacheFile is an optional JSON list of further font families for
	// the font search. Node: <console>/data/google-fonts-cache.json.
	FontCacheFile string
	// TempDir takes an uploaded font file before it is installed. Node:
	// <console>/data/tmp.
	TempDir string
	// ThemesDir holds the built-in theme presets (*.json). Node:
	// <console>/themes.
	ThemesDir string
	// PalettesFile is the JSON file with the built-in dark and light
	// palettes. Node: <console>/config/theme-palettes.json. New fails
	// when it cannot be read. Empty, and without Files, means no built-in
	// palettes: both are empty objects.
	PalettesFile string
	// FaviconFile is the default favicon. Node: <console>/public/favicon.svg.
	// A built-in mark is used when it cannot be read.
	FaviconFile string
	// Files is the tree of the console's own files (package static). Each
	// of the three built-in files above whose option is empty is read from
	// it: themes, config/theme-palettes.json and public/favicon.svg.
	Files fs.FS

	// HTTP sends the requests of a font download to fonts.googleapis.com
	// and fonts.gstatic.com. Nil means a direct connection without a
	// proxy and without transparent compression, to addresses the
	// outbound guard lets through (guardedDial, with Lookup and Dial).
	HTTP http.RoundTripper
	// Lookup resolves a host name for the font download. Nil means
	// ssrfguard.SystemLookup. Not used when HTTP is set.
	Lookup ssrfguard.LookupFunc
	// Dial opens the connection to an address that passed the guard. Nil
	// means a net.Dialer. Not used when HTTP is set.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// Now is the clock. time.Now when nil.
	Now func() time.Time
	// Rand gives the random part of upload file names. crypto/rand when
	// nil.
	Rand io.Reader
	// Logf writes the service's warnings. Nil means the server's log.
	Logf func(format string, args ...any)
}

// Service is the console's branding: the appearance settings with their
// cache, the stylesheet made from them, saved profiles, themes, fonts and
// uploaded pictures. It is safe for concurrent use.
//
// The cache is the one copy every reader uses. The writes (save, reset,
// import, a profile or a theme applied, a background applied) reload it
// before they return, so the next read anywhere in the console sees the
// change. A row written into branding_config by anything else (the
// restore of a full backup, a hand edit) shows after the next write or
// the next start, as in the Node console.
type Service struct {
	opts     Options
	palettes *Object

	mu       sync.RWMutex
	cache    *Object
	revision string

	faviconOnce sync.Once
	favicon     string
}

// New creates the service, reads the built-in palettes and creates
// UploadsDir when it is missing. The settings cache is empty until
// LoadBranding has run; until then GetBranding answers the defaults.
func New(opts Options) (*Service, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.HTTP == nil {
		// plain HTTP/1.1 without Accept-Encoding and without a proxy, to
		// addresses the outbound guard lets through
		opts.HTTP = &http.Transport{
			DialContext:        guardedDial(opts.Lookup, opts.Dial),
			DisableCompression: true,
			TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
	}
	if opts.Logf == nil {
		opts.Logf = func(format string, args ...any) { logging.Logf("branding", format, args...) }
	}
	if opts.UploadsDir != "" {
		if err := os.MkdirAll(opts.UploadsDir, 0o777); err != nil {
			return nil, fmt.Errorf("create the uploads folder: %w", err)
		}
	}
	var parsed any = NewObject()
	if opts.PalettesFile != "" || opts.Files != nil {
		raw, where, err := readBuiltin(opts, opts.PalettesFile, builtinPalettes)
		if err != nil {
			return nil, fmt.Errorf("read the theme palettes: %w", err)
		}
		if parsed, err = parseJSON(decodeUTF8(raw)); err != nil {
			return nil, fmt.Errorf("read the theme palettes %s: %w", where, err)
		}
		if parsed == nil {
			return nil, fmt.Errorf("read the theme palettes %s: the file holds null", where)
		}
	}
	palettes := NewObject()
	for _, mode := range []string{"dark", "light"} {
		palettes.Set(mode, spread(NewObject(), property(parsed, mode)))
	}
	return &Service{opts: opts, palettes: palettes, revision: "0"}, nil
}

// bumpRevision sets the revision to the current time in base 36.
func (s *Service) bumpRevision() string {
	return strconv.FormatInt(s.opts.Now().UnixMilli(), 36)
}

// Revision returns the token that changes when the settings change. The
// web console compares it to know when to load /css/branding.css again;
// the route sends it as X-Branding-Revision. It is "0" before the first
// load, then eight hex digits made of the stored settings (rowsRevision),
// so every save that changes a setting changes it, and the current time in
// base 36 when the settings cannot be read.
func (s *Service) Revision() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

// LoadBranding reads the stored settings over the defaults, fills the
// cache and sets the revision. It runs at start and after every change.
// When the database cannot be read the cache holds the defaults.
func (s *Service) LoadBranding(ctx context.Context) *Object {
	var branding *Object
	rows, err := getBrandingConfig(ctx, s.opts.DB)
	if err == nil {
		err = try(func() { branding = brandingFromRows(rows, s.opts.Logf) })
	}
	if err != nil {
		s.opts.Logf("[Branding] Failed to load from DB, using defaults: %v", err)
		fallback := DefaultBranding()
		s.mu.Lock()
		s.cache, s.revision = fallback, s.bumpRevision()
		s.mu.Unlock()
		return fallback
	}

	s.mu.Lock()
	s.cache, s.revision = branding, rowsRevision(rows)
	s.mu.Unlock()
	return branding
}

// brandingFromRows lays stored settings over the defaults. A setting
// without a row keeps its default: the plain sign-in layout and the
// built-in slides among them. The SVG markup of the logo and favicon is
// cleaned (SanitizeSVG) as on a save. A row whose key is no setting is
// ignored, and a row that cannot be read (a slide list with a value that
// cannot be turned into text) is logged and leaves its setting as it was.
func brandingFromRows(rows []configRow, logf func(format string, args ...any)) *Object {
	branding := DefaultBranding()
	for _, row := range rows {
		if err := try(func() { applyRow(branding, row) }); err != nil {
			logf("[Branding] Ignoring the stored setting %s: %v", row.key, err)
		}
	}
	if logoType, _ := branding.Get("logoType"); logoType != "default" && usesDefaultLogo(branding) {
		branding.Set("logoType", "default")
	}
	return branding
}

// applyRow lays one stored setting over branding.
func applyRow(branding *Object, row configRow) {
	switch row.key {
	case "colors":
		saved, err := parseJSON(row.value)
		if err != nil {
			return
		}
		colors, _ := branding.Get("colors")
		assign(colors.(*Object), saved)
	case "loginSlides":
		branding.Set("loginSlides", sanitizeLoginSlides(row.value))
	case "loginPromoMode":
		branding.Set("loginPromoMode", normalizeLoginPromoMode(row.value))
	case "logoSvg", "faviconSvg":
		// a row an older version stored, or one restored from a backup,
		// went through an older cleaner or none
		branding.Set(row.key, SanitizeSVG(row.value))
	default:
		if branding.Has(row.key) {
			branding.Set(row.key, row.value)
		}
	}
}

// assign is Object.assign(dst, src): the own keys of src are set on dst.
// Setting "__proto__" this way never adds a key.
func assign(dst *Object, src any) {
	entries(src, func(key string, value any) {
		if key != "__proto__" {
			dst.Set(key, value)
		}
	})
}

// GetBranding returns the current settings from the cache: every setting
// a string, "colors" an object, "loginSlides" a list. The object is shared
// and must not be changed. Before the first LoadBranding, and after
// InvalidateCache, it answers the defaults.
func (s *Service) GetBranding() *Object {
	s.mu.RLock()
	cache := s.cache
	s.mu.RUnlock()
	if cache != nil {
		return cache
	}
	return DefaultBranding()
}

// InvalidateCache empties the cache. GetBranding answers the defaults
// until the next LoadBranding.
func (s *Service) InvalidateCache() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

var (
	urlSettings   = map[string]bool{"logoUrl": true, "faviconUrl": true, "bgImageUrl": true, "loginBgImageUrl": true, "agentBgImageUrl": true, "rdclientBgImageUrl": true}
	colorSettings = map[string]bool{"bgColor": true, "bgGradient": true, "loginBgColor": true, "loginBgGradient": true, "agentBgColor": true, "agentBgGradient": true, "rdclientBgColor": true, "rdclientBgGradient": true, "glassColor": true}
)

// SaveBranding stores settings and reloads the cache. updates is an
// object in the flat or the nested shape (see FlattenAppearanceInput);
// settings it does not name stay as they are, and names that are no
// setting are ignored. Every value is cleaned on the way in: SVG markup,
// picture addresses (an address that is refused is skipped and the stored
// one stays), colours and gradients, the slide list, the layout and theme
// modes and the custom CSS. Everything else is stored as text.
//
// It fails with a TypeError, before anything is written, when a value is
// an object that cannot be turned into text.
func (s *Service) SaveBranding(ctx context.Context, updates any) (err error) {
	defer catch(&err)
	var rows []configRow
	entries(flattenAppearanceInput(updates), func(key string, value any) {
		var text string
		switch {
		case key == "colors":
			colors := NewObject()
			for _, name := range colorKeys {
				if Truthy(value) && hasOwn(value, name) {
					colors.Set(name, substring(sanitizeCSSColorValue(property(value, name)), 100))
				}
			}
			text = Stringify(colors)
		case !isSetting[key]:
			return
		case key == "logoSvg" || key == "faviconSvg":
			text = SanitizeSVG(toString(value))
		case urlSettings[key]:
			normalized, ok := validateBrandingURL(value)
			if !ok {
				return
			}
			text = normalized
		case key == "loginSlides":
			text = Stringify(sanitizeLoginSlides(value))
		case key == "loginPromoMode":
			text = normalizeLoginPromoMode(value)
		case colorSettings[key]:
			text = sanitizeCSSColorValue(value)
		case key == "bgSize" || key == "rdclientBgSize":
			text = normalizeBackgroundSize(value)
		case key == "themeMode":
			text = normalizeThemeMode(value)
		case key == "customCss":
			text = sanitizeCustomCSS(value)
		default:
			text = toString(value)
		}
		rows = append(rows, configRow{key, text})
	})
	if len(rows) > 0 {
		if err := saveBrandingConfigBatch(ctx, s.opts.DB, rows); err != nil {
			return wrap("save the branding settings", err)
		}
	}
	s.LoadBranding(ctx)
	s.changed()
	return nil
}

func (s *Service) changed() {
	if s.opts.OnChange != nil {
		s.opts.OnChange()
	}
}

// ResetBranding deletes every stored setting, so the defaults apply.
func (s *Service) ResetBranding(ctx context.Context) error {
	if err := resetBrandingConfig(ctx, s.opts.DB); err != nil {
		return wrap("reset the branding settings", err)
	}
	s.InvalidateCache()
	s.LoadBranding(ctx)
	s.changed()
	return nil
}

// defaultFaviconFallback is the NeoDesk mark, used when FaviconFile
// cannot be read.
const defaultFaviconFallback = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="9.5 9.5 45 45"><style>.s{fill:#1f6feb}.c{fill:#4d8f06}.o{fill:#0f9e96}@media (prefers-color-scheme: dark){.s{fill:#58a6ff}.c{fill:#a3e635}.o{fill:#3fd0c4}}</style><defs><clipPath id="bdf"><circle cx="38" cy="26" r="16"/></clipPath></defs><rect class="s" x="10" y="22" width="32" height="32" rx="10"/><circle class="c" cx="38" cy="26" r="16"/><rect class="o" x="10" y="22" width="32" height="32" rx="10" clip-path="url(#bdf)"/></svg>`

// GenerateFavicon returns the SVG for /branding/favicon.svg: the
// administrator's own favicon when one is set, otherwise the NeoDesk
// mark from FaviconFile. The file is read once.
func (s *Service) GenerateFavicon() string {
	if own, _ := s.GetBranding().Get("faviconSvg"); Truthy(own) {
		if text, ok := own.(string); ok && trim(text) != "" {
			return text
		}
	}
	s.faviconOnce.Do(func() {
		raw, _, err := readBuiltin(s.opts, s.opts.FaviconFile, builtinFavicon)
		if err != nil {
			s.favicon = defaultFaviconFallback
			return
		}
		s.favicon = decodeUTF8(raw)
	})
	return s.favicon
}

// ExportPreset returns the current settings as a theme file: the nested
// appearance model and the flat settings, so older consoles can import
// it too. It fails where the appearance model cannot be made of the
// settings.
func (s *Service) ExportPreset() (preset *Object, err error) {
	defer catch(&err)
	return s.exportPreset(), nil
}

func (s *Service) exportPreset() *Object {
	branding := s.GetBranding()
	return object(
		"version", "2.0",
		"type", "neodesk-theme",
		"appearance", s.appearanceModel(branding),
		"branding", branding,
	)
}

// ImportPreset applies a theme file. It answers false when preset is not
// a theme (type "neodesk-theme" with an appearance or a branding
// part). When both are there, the flat settings of the branding part are
// laid over the appearance part: the console's own export writes both,
// and only the flat part holds every setting by its stored name (the
// custom CSS among them). Values are cut to their length limits and then
// stored as SaveBranding stores them.
func (s *Service) ImportPreset(ctx context.Context, preset any) (ok bool, err error) {
	defer catch(&err)
	if !Truthy(preset) || property(preset, "type") != "neodesk-theme" {
		return false, nil
	}
	appearance, branding := property(preset, "appearance"), property(preset, "branding")
	if !Truthy(branding) && !Truthy(appearance) {
		return false, nil
	}
	input := NewObject()
	if Truthy(appearance) {
		input = flattenAppearanceInput(object("appearance", appearance))
	}
	if Truthy(branding) {
		entries(flattenAppearanceInput(branding), func(key string, value any) { input.Set(key, value) })
	}
	sanitized := NewObject()
	for _, pair := range defaultSettings {
		key := pair[0]
		value, has := input.Get(key)
		if !has {
			continue
		}
		switch key {
		case "colors":
			colors := NewObject()
			for _, name := range colorKeys {
				if !Truthy(value) {
					continue
				}
				if !isObjectLike(value) {
					throwType("Cannot use 'in' operator to search for '" + name + "' in " + toString(value))
				}
				if hasOwn(value, name) {
					colors.Set(name, substring(toString(property(value, name)), 100))
				}
			}
			sanitized.Set("colors", colors)
		case "loginSlides":
			sanitized.Set(key, sanitizeLoginSlides(value))
		case "loginPromoMode":
			sanitized.Set(key, normalizeLoginPromoMode(value))
		default:
			limit := 500
			switch key {
			case "logoSvg", "faviconSvg":
				limit = 50000
			case "customCss":
				limit = 20000
			case "bgGradient", "loginBgGradient", "agentBgGradient", "rdclientBgGradient":
				limit = 1000
			}
			sanitized.Set(key, substring(toString(value), limit))
		}
	}
	if err := s.SaveBranding(ctx, sanitized); err != nil {
		return false, err
	}
	return true, nil
}
