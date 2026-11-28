package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/branding"
	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/httpx"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store"
)

// The format names and versions an archive and a settings snapshot carry.
const (
	FormatName        = "neodesk-backup"
	FormatVersion     = 1
	FullFormatName    = "neodesk-full-backup"
	FullFormatVersion = 1
)

// Branding is the part of the branding service a backup reads and writes.
// LoadBranding fills its cache from the database again after a restore
// replaced branding_config.
type Branding interface {
	GetBranding() *branding.Object
	SaveBranding(ctx context.Context, updates any) error
	LoadBranding(ctx context.Context) *branding.Object
}

// Avatars is the part of the avatar service a restore needs: it keeps a
// list of who has a picture, which a restore of the accounts or their
// pictures makes stale.
type Avatars interface {
	ResetCache()
}

// Groups lists folders and groups as the console's device group service
// does.
type Groups interface {
	AllFolders(ctx context.Context) ([]*goapi.Object, error)
	AllUserGroups(ctx context.Context) ([]*goapi.Object, error)
	AllDeviceGroups(ctx context.Context) ([]*goapi.Object, error)
}

// GoServer is what a backup asks the API server for.
type GoServer interface {
	GetAllPeers(ctx context.Context, includeDeleted bool) []*goapi.Object
	GetBlocklist(ctx context.Context) *goapi.Object
	GetAuditEvents(ctx context.Context, limit any) *goapi.Object
	GetHealth(ctx context.Context) *goapi.Object
}

// Options are the settings of a Service. The paths are those of the old
// console's config.js; the lead's wiring fills them from config.Config.
type Options struct {
	Store    *store.DB
	Branding Branding
	Groups   Groups
	GoAPI    GoServer
	Avatars  Avatars

	// DataDir is DATA_DIR: .session_secret and uploads/ are below it.
	DataDir string
	// EnvPath is the old console's .env (web-nodejs/.env).
	EnvPath string
	// KeysPath is KEYS_PATH (RUSTDESK_DIR, RUSTDESK_PATH): id_ed25519 and
	// mesh_agent_server.pem are in it.
	KeysPath string
	// PubKeyPath is PUB_KEY_PATH (default KeysPath/id_ed25519.pub),
	// APIKeyPath is API_KEY_PATH (default KeysPath/.api_key).
	PubKeyPath string
	APIKeyPath string
	// DBPath is DB_PATH (default KeysPath/db_v2.sqlite3): the SQLite file
	// a full backup holds twice, as the console's and as the server's
	// database, on SQLite the one open file copied once with VACUUM INTO.
	// Ignored on PostgreSQL for the console's copy.
	DBPath string
	// ServerDB is the server's database DSN (its -db flag or DB_URL), the
	// file the console's store has open on SQLite: a full backup holds it
	// as the server's database, and a restore leaves the archive's file
	// next to it for the start. Empty means DBPath. A PostgreSQL DSN, like
	// a store on PostgreSQL, names no such file.
	ServerDB string
	// ChatFilesDir is the folder of the chat files
	// (CONSOLE_DIR/data/chat-files) with their links in .links/.
	ChatFilesDir string
	// AttachmentsDir is the folder of the ticket attachments
	// (CONSOLE_DIR/data/attachments).
	AttachmentsDir string
	// FontsDir is the folder of the installed fonts (DATA_DIR/fonts).
	FontsDir string
	// CertFile, KeyFile and CAFile are the console's TLS certificate, its
	// private key and its CA (SSL_CERT_PATH, SSL_KEY_PATH, SSL_CA_PATH).
	CertFile, KeyFile, CAFile string
	// EndpointsFile is the file the console keeps the public client
	// endpoints in (DATA_DIR/public-endpoints.env).
	EndpointsFile string
	// AppVersion is the product version the archive names.
	AppVersion string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Docker says the console runs in the Docker image (config.Docker):
	// its settings come from the container's environment, so a backup
	// holds no environment file and a restore writes none.
	Docker bool
	// SessionSecretFromEnv says SESSION_SECRET is set: the session secret
	// is the environment's and there is no .session_secret to save.
	SessionSecretFromEnv bool
	// Limits cap an archive that is read (a restore and its check):
	// MaxBytes is BACKUP_MAX_DECOMPRESSED_BYTES, MaxEntries
	// BACKUP_MAX_ENTRIES, each read with EnvLimit; 0 is the default (2 GiB,
	// 50,000).
	Limits Limits
}

// Service makes and restores backups (services/backupService.js).
type Service struct {
	o Options
}

// New returns a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}
}

// paths are the files a full backup holds, resolveBackupPaths.
type paths struct {
	env, sessionSecret, uploadsDir, chatFilesDir, attachmentsDir, consoleDB, goPrivKey, goPubKey, goAPIKey, goDB, meshAgentCert, nodeControlCert, nodeControlKey string
	fontsDir, certFile, keyFile, caFile, endpointsFile                                                                                                           string
}

func (s *Service) paths() paths {
	p := paths{
		env:            s.o.EnvPath,
		sessionSecret:  filepath.Join(s.o.DataDir, ".session_secret"),
		uploadsDir:     filepath.Join(s.o.DataDir, "uploads"),
		chatFilesDir:   s.o.ChatFilesDir,
		attachmentsDir: s.o.AttachmentsDir,
		goPrivKey:      filepath.Join(s.o.KeysPath, "id_ed25519"),
		goPubKey:       s.o.PubKeyPath,
		goAPIKey:       s.o.APIKeyPath,
		goDB:           s.serverDBFile(),
		meshAgentCert:  filepath.Join(s.o.KeysPath, "mesh_agent_server.pem"),
		// relay nodes pin this certificate: a restore without it makes
		// every node refuse the server until it joins again
		nodeControlCert: filepath.Join(s.o.KeysPath, "node_control.crt"),
		nodeControlKey:  filepath.Join(s.o.KeysPath, "node_control.key"),
		fontsDir:        s.o.FontsDir,
		certFile:        s.o.CertFile,
		keyFile:         s.o.KeyFile,
		caFile:          s.o.CAFile,
		endpointsFile:   s.o.EndpointsFile,
	}
	p.consoleDB = p.goDB
	return p
}

// serverDBFile is the server's SQLite file: ServerDB, else DBPath; empty
// when the server runs on PostgreSQL, which the console's store then
// shares.
func (s *Service) serverDBFile() string {
	if s.o.Store.IsPostgres() {
		return ""
	}
	if s.o.ServerDB == "" {
		return s.o.DBPath
	}
	lower := strings.ToLower(s.o.ServerDB)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		return ""
	}
	return s.o.ServerDB
}

// dbType is config.dbType.
func (s *Service) dbType() string { return s.o.Store.Dialect().String() }

// isoNow is new Date().toISOString().
func (s *Service) isoNow() string { return store.ISOTime(s.o.Now()) }

// ---- Statistics ----

// Stats are the row counts getBackupStats gave.
type Stats struct {
	Users        int64 `json:"users"`
	Settings     int64 `json:"settings"`
	Folders      int64 `json:"folders"`
	UserGroups   int64 `json:"userGroups"`
	DeviceGroups int64 `json:"deviceGroups"`
	Strategies   int64 `json:"strategies"`
	AddressBooks int64 `json:"addressBooks"`
}

// FullStats is the answer of getFullBackupStats: the counts, the backend,
// the database and which parts a full backup would find. Parts is what a
// backup made now holds, part by part; Components is the older answer,
// kept for the pages that read it.
type FullStats struct {
	Stats
	Backend    string     `json:"backend"`
	DBType     string     `json:"dbType"`
	Components Components `json:"components"`
	Parts      []Part     `json:"parts"`
}

// Part is one part of a full backup: whether a backup made now holds it
// and, when not, why.
type Part struct {
	// ID names the part (the Part* constants).
	ID string `json:"id"`
	// Included says a backup made now holds at least one file of it.
	Included bool `json:"included"`
	// Reason is why it is left out (the Reason* constants), "" when it is
	// included.
	Reason string `json:"reason,omitempty"`
	// Files is how many files of the part the backup holds.
	Files int `json:"files"`
	// Secret says the part holds secrets: keys, passwords or their hashes.
	Secret bool `json:"secret"`
}

// Components says which files exist, and how many uploads there are.
type Components struct {
	Env           bool `json:"env"`
	SessionSecret bool `json:"sessionSecret"`
	ConsoleDB     bool `json:"consoleDb"`
	Uploads       int  `json:"uploads"`
	GoPrivKey     bool `json:"goPrivKey"`
	GoPubKey      bool `json:"goPubKey"`
	GoAPIKey      bool `json:"goApiKey"`
	GoDB          bool `json:"goDb"`
	MeshAgentCert bool `json:"meshAgentCert"`
}

// BackupStats is getBackupStats: the counts and the backend.
func (s *Service) BackupStats(ctx context.Context) (*goapi.Object, error) {
	st := backupStats(ctx, s.o.Store)
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	v, err := goapi.Parse(raw)
	if err != nil {
		return nil, err
	}
	o, _ := v.(*goapi.Object)
	return o.Set("backend", "neodesk"), nil
}

// FullBackupStats is getFullBackupStats. Its Parts come from the plan
// CreateFullBackup follows, so the answer and the archive agree.
func (s *Service) FullBackupStats(ctx context.Context) FullStats {
	p := s.paths()
	exists := func(path string) bool {
		if path == "" {
			return false
		}
		_, err := os.Stat(path)
		return err == nil
	}
	uploads := 0
	if exists(p.uploadsDir) {
		_ = walkFiles(p.uploadsDir, "", func(string, string) { uploads++ })
	}
	return FullStats{
		Stats:   backupStats(ctx, s.o.Store),
		Backend: "neodesk",
		DBType:  s.dbType(),
		Components: Components{
			Env:           !s.o.Docker && exists(p.env),
			SessionSecret: exists(p.sessionSecret),
			ConsoleDB:     exists(p.consoleDB),
			Uploads:       uploads,
			GoPrivKey:     exists(p.goPrivKey),
			GoPubKey:      exists(p.goPubKey),
			GoAPIKey:      exists(p.goAPIKey),
			GoDB:          exists(p.goDB),
			MeshAgentCert: exists(p.meshAgentCert),
		},
		Parts: s.parts(p),
	}
}

// walkFiles visits the files below dir (following links), names sorted
// per folder, with each file's path relative to dir. It stops at the
// first entry that cannot be read, as the old walk did, after visiting
// what came before. A name that is not UTF-8 counts as unreadable: Node
// read it as other text, which then named no file.
func walkFiles(dir, rel string, visit func(rel, abs string)) error {
	abs := filepath.Join(dir, rel)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if nodeUTF8([]byte(name)) != name {
			return fmt.Errorf("unreadable name in %s", abs)
		}
		childRel := name
		if rel != "" {
			childRel = filepath.Join(rel, name)
		}
		childAbs := filepath.Join(dir, childRel)
		info, err := os.Stat(childAbs)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := walkFiles(dir, childRel, visit); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			visit(childRel, childAbs)
		}
	}
	return nil
}

// ---- Full backup ----

// Manifest is manifest.json of an archive.
type Manifest struct {
	Format         string   `json:"_format"`
	Version        int      `json:"_version"`
	Created        string   `json:"_created"`
	ConsoleVersion string   `json:"_console_version"`
	Backend        string   `json:"_backend"`
	DBType         string   `json:"dbType"`
	Components     []string `json:"components"`
}

// CreateFullBackup makes the full disaster-recovery archive and its file
// name, neodesk-backup-YYYY-MM-DD-HH-MM-SS.tar.gz (UTC). It holds, in
// this order:
//
//	manifest.json                    format, version, time, product version, backend, database, components
//	console/data.json                the logical dump of every table (DumpAllTables)
//	goserver/data.json               devices, blocklist, the last 500 audit events and the health of the API server
//	console/.env                     the console's .env, none on Docker
//	console/.session_secret          DATA_DIR/.session_secret
//	console/auth.db                  a copy of the SQLite file (DB_PATH), on SQLite only
//	console/uploads/...              every file below DATA_DIR/uploads
//	console/chat-files/...           the chat files and their .links
//	console/attachments/...          the ticket attachments
//	console/fonts/...                the installed fonts
//	console/public-endpoints.env     the public client endpoints saved in the console
//	console/tls/certificate.pem      the console's TLS certificate (SSL_CERT_PATH)
//	console/tls/private-key.pem      its private key (SSL_KEY_PATH)
//	console/tls/ca.pem               its CA (SSL_CA_PATH)
//	goserver/id_ed25519(.pub)        the server's key pair
//	goserver/.api_key                the API key
//	goserver/db_v2.sqlite3           the same copy as the server's database
//	goserver/mesh_agent_server.pem   the mesh agent certificate
//	goserver/node_control.crt/.key   the certificate relay nodes pin
//	README.txt                       how to restore by hand
//
// A part whose file is missing or unreadable is left out; the dump is
// left out when it fails. Files and the two dumps are mode 0600 in the
// archive, uploads, chat files, attachments and the rest 0644.
func (s *Service) CreateFullBackup(ctx context.Context) ([]byte, string, error) {
	timestamp := s.isoNow()
	p := s.paths()
	var entries []Entry
	var present []string

	if dump, err := DumpAllTables(ctx, s.o.Store); err == nil {
		// The dumps hold password hashes, second-factor secrets and
		// recovery codes: readable by their owner only (L409)
		entries = append(entries, Entry{Name: "console/data.json", Data: dump, Mode: 0o600})
		present = append(present, "console/data.json")
	}
	if goServer := s.goServerData(ctx); goServer != nil {
		raw, err := httpx.Marshal(goServer)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, Entry{Name: "goserver/data.json", Data: raw, Mode: 0o600})
		present = append(present, "goserver/data.json")
	}

	// sqliteCopy is the open SQLite file (DB_PATH) on SQLite, copied once
	// for both its entries; nil when the copy failed, which leaves both out.
	var sqliteCopy []byte
	if p.consoleDB != "" {
		sqliteCopy, _ = copySQLite(ctx, s.o.Store)
	}
	addFile := func(name, path string) {
		if path == "" {
			return
		}
		var data []byte
		if path == p.consoleDB {
			if data = sqliteCopy; data == nil {
				return
			}
		} else {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				return
			}
			if data, err = os.ReadFile(path); err != nil {
				return
			}
		}
		entries = append(entries, Entry{Name: name, Data: data, Mode: 0o600})
		present = append(present, name)
	}
	// addFolder adds the files below dir as prefix/<path below dir>.
	addFolder := func(prefix, dir string) {
		if dir == "" {
			return
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return
		}
		_ = walkFiles(dir, "", func(rel, abs string) {
			data, err := os.ReadFile(abs)
			if err != nil {
				return
			}
			name := prefix + filepath.ToSlash(rel)
			entries = append(entries, Entry{Name: name, Data: data, Mode: 0o644})
			present = append(present, name)
		})
	}
	// the same plan FullBackupStats counts (parts.go)
	for _, e := range s.plan(p) {
		if e.folder {
			addFolder(e.name, e.path)
		} else {
			addFile(e.name, e.path)
		}
	}

	if present == nil {
		present = []string{}
	}
	manifest := Manifest{
		Format:         FullFormatName,
		Version:        FullFormatVersion,
		Created:        timestamp,
		ConsoleVersion: s.o.AppVersion,
		Backend:        "neodesk",
		DBType:         s.dbType(),
		Components:     present,
	}
	manifestJSON, err := indentJSON(manifest)
	if err != nil {
		return nil, "", err
	}
	entries = append([]Entry{{Name: "manifest.json", Data: manifestJSON}}, entries...)
	entries = append(entries, Entry{Name: "README.txt", Data: []byte(recoveryReadme(manifest))})

	archive, err := CreateTarGz(entries, s.o.Now().Unix())
	if err != nil {
		return nil, "", err
	}
	stamp := strings.NewReplacer(":", "-", "T", "-").Replace(timestamp[:19])
	return archive, "neodesk-backup-" + stamp + ".tar.gz", nil
}

// goServerData is fetchGoServerData: what the API server says, each call
// falling back on its own.
func (s *Service) goServerData(ctx context.Context) *goapi.Object {
	if s.o.GoAPI == nil {
		return nil
	}
	peers := s.o.GoAPI.GetAllPeers(ctx, false)
	if peers == nil {
		peers = []*goapi.Object{}
	}
	return goapi.NewObject(
		"peers", peers,
		"blocklist", s.o.GoAPI.GetBlocklist(ctx),
		"auditEvents", s.o.GoAPI.GetAuditEvents(ctx, float64(500)),
		"serverHealth", s.o.GoAPI.GetHealth(ctx),
	)
}

// indentJSON is JSON.stringify(v, null, 2).
func indentJSON(v any) ([]byte, error) {
	raw, err := httpx.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func recoveryReadme(m Manifest) string {
	return strings.Join([]string{
		"NeoDesk full backup",
		"===================",
		"",
		"Created:          " + m.Created,
		"Console version:  " + m.ConsoleVersion,
		"Database engine:  " + m.DBType,
		"Components:        " + strings.Join(m.Components, ", "),
		"",
		"WARNING: This archive holds SECRETS: the server's Ed25519 keys, the API",
		"key, the session secret, the console's .env (database credentials),",
		"password hashes and two-factor secrets. Keep it where only you can read",
		"it and never share it.",
		"",
		"How to restore",
		"--------------",
		"1. Install NeoDesk of the same version: install.sh, or the Docker image.",
		"2. Open Server > Backups (/server/backups) in the web console and",
		"   choose Restore, which puts every part back and asks for the restart.",
		"",
		"To restore by hand instead, stop the server, extract the archive with",
		"umask 077 so the files stay private (umask 077; tar -xzf <backup>.tar.gz)",
		"and copy each part back:",
		"     console/.env            -> the console's .env (none on Docker)",
		"     console/.session_secret -> <dataDir>/.session_secret",
		"     console/auth.db         -> <dataDir>/auth.db   (SQLite only)",
		"     console/uploads/*       -> <dataDir>/uploads/",
		"     console/chat-files/*    -> <consoleDir>/data/chat-files/",
		"     console/attachments/*   -> <consoleDir>/data/attachments/",
		"     console/fonts/*         -> <dataDir>/fonts/",
		"     console/public-endpoints.env -> <dataDir>/public-endpoints.env",
		"     console/tls/*.pem       -> SSL_CERT_PATH, SSL_KEY_PATH, SSL_CA_PATH",
		"     goserver/id_ed25519     -> <keysPath>/id_ed25519",
		"     goserver/id_ed25519.pub -> <keysPath>/id_ed25519.pub",
		"     goserver/.api_key       -> <keysPath>/.api_key",
		"     goserver/mesh_agent_server.pem -> <keysPath>/mesh_agent_server.pem",
		"     goserver/node_control.crt -> <keysPath>/node_control.crt",
		"     goserver/node_control.key -> <keysPath>/node_control.key",
		"     goserver/db_v2.sqlite3  -> DB_PATH (SQLite only)",
		"On Docker <dataDir> and <keysPath> are /app/data and /opt/neodesk/data.",
		"With PostgreSQL the dump in console/data.json goes back only through",
		"the console's Restore.",
		"",
		"Start the server again. Clients keep trusting it because the Ed25519 key",
		"is the same.",
		"",
	}, "\n")
}

// Validation is validateFullBackup's answer.
type Validation struct {
	Valid    bool
	Errors   []string
	Manifest any
	Files    *Files
}

// ValidateFullBackup unpacks an archive and checks its manifest: it must
// be a JSON object with _format "neodesk-full-backup" and a numeric
// _version of at most 1. A manifest that is no object (null, a list, text)
// has neither.
func (s *Service) ValidateFullBackup(buf []byte) Validation {
	files, err := ExtractTarGz(buf, s.o.Limits)
	if err != nil {
		return Validation{Errors: []string{"Not a valid gzip/tar archive: " + err.Error()}}
	}
	raw, ok := files.Get("manifest.json")
	if !ok {
		return Validation{Errors: []string{"Archive is missing manifest.json"}}
	}
	manifest, err := branding.ParseJSON(raw)
	if err != nil {
		return Validation{Errors: []string{"manifest.json is not valid JSON: " + errorText(err)}}
	}
	var errs []string
	if prop(manifest, "_format") != FullFormatName {
		errs = append(errs, "Archive is not a NeoDesk full backup (wrong _format)")
	}
	version := prop(manifest, "_version")
	if n, isNumber := version.(float64); !isNumber || n > FullFormatVersion {
		errs = append(errs, "Unsupported full-backup version: "+jsString(version))
	}
	return Validation{Valid: len(errs) == 0, Errors: errs, Manifest: manifest, Files: files}
}

// RestoreOptions say which parts of an archive Restore puts back.
type RestoreOptions struct {
	Database bool // the logical dump; the route's default is on
	Uploads  bool // the uploaded pictures; default on
	Secrets  bool // session secret, server keys, API key, mesh certificate; default off
	Env      bool // the .env; default off
	GoDB     bool // the SQLite file of the server; default off
}

// RestoreResult is what Restore did.
type RestoreResult struct {
	Restored        []string
	Skipped         []string
	Warnings        []string
	RequiresRestart bool
}

// ErrInvalidArchive is the error of a restore that the check refused
// before anything was written.
type ErrInvalidArchive struct{ Errors []string }

func (e *ErrInvalidArchive) Error() string {
	return "Invalid backup archive: " + strings.Join(e.Errors, "; ")
}

// RestoreFullBackup puts an archive back (restoreFullBackup). The whole
// archive is unpacked and checked first; an archive that fails the check
// changes nothing. Then, in this order and each part on its own, so that
// a part that fails leaves a warning and the next one still runs:
//
//  1. the logical dump, table by table (ImportAllTables): with Database
//     the console's tables, with GoDB the server's own (serverTables), so
//     that one part leaves the other's tables as they are on SQLite and
//     PostgreSQL alike; the branding cache is loaded again when
//     branding_config came back, the list of who has a picture is
//     dropped when users or user_avatars did; the chat files, the ticket
//     attachments and the public endpoints file go with the console's
//     data, when it is chosen,
//  2. the uploads, below DATA_DIR/uploads, and the installed fonts,
//  3. the secrets, each to its own path, with the console's TLS files,
//  4. the .env, not on Docker (a warning there),
//  5. for an archive whose dump has none of the server's tables (an older
//     install's), the server's SQLite file, when the server has one
//     (serverDBFile): checked (SQLite's header, PRAGMA integrity_check)
//     and written next to it as that path + RestoredSuffix, never over
//     the open file: the next start puts it in place
//     (SwapInRestoredDatabase). A file that fails the check is a warning
//     and is not written.
//
// The server's tables and parts 3 to 5 take effect at the next start. A
// file is written over the old one in place; a new file gets the mode of
// its part.
func (s *Service) RestoreFullBackup(ctx context.Context, buf []byte, o RestoreOptions) (RestoreResult, error) {
	res := RestoreResult{Restored: []string{}, Skipped: []string{}, Warnings: []string{}}
	v := s.ValidateFullBackup(buf)
	if !v.Valid {
		return res, &ErrInvalidArchive{Errors: v.Errors}
	}
	files := v.Files
	p := s.paths()

	// The dump holds every table. Console data is its tables but the
	// server's own; the server's own come back with Server device database
	// (BUGS L401). An archive whose dump has none of them (an older
	// install's) brings the server's tables back with its SQLite file.
	dumpRaw, hasDump := files.Get("console/data.json")
	var dump any
	var dumpErr error
	if hasDump && (o.Database || o.GoDB) {
		dump, dumpErr = branding.ParseJSON(dumpRaw)
	}
	serverFromDump := o.GoDB && dumpErr == nil && dumpHasServerTables(dump)
	switch {
	case dumpErr != nil && o.Database:
		res.Warnings = append(res.Warnings, "Database restore failed: "+driverMessage(dumpErr))
	case hasDump && (o.Database || serverFromDump):
		keep := func(table string) bool {
			if serverTables[table] {
				return serverFromDump
			}
			return o.Database
		}
		if err := s.importDump(ctx, dump, keep, &res); err != nil {
			res.Warnings = append(res.Warnings, "Database restore failed: "+driverMessage(err))
		}
	}
	if !o.Database || !hasDump {
		res.Skipped = append(res.Skipped, "database")
	}
	if o.Database {
		s.restoreFolderPart(files, &res, chatFilesPrefix, p.chatFilesDir, "chat files", "Chat files")
		s.restoreFolderPart(files, &res, attachmentsPrefix, p.attachmentsDir, "attachments", "Attachments")
		if data, ok := files.Get(endpointsEntry); ok && p.endpointsFile != "" {
			if err := writeFile(p.endpointsFile, data, 0o600); err != nil {
				res.Warnings = append(res.Warnings, "Public endpoints restore failed: "+err.Error())
			} else {
				res.Restored = append(res.Restored, "public endpoints")
				res.RequiresRestart = true
			}
		}
	}

	if o.Uploads {
		count, err := s.restoreFolder(files, uploadsPrefix, p.uploadsDir)
		switch {
		case err != nil:
			res.Warnings = append(res.Warnings, "Uploads restore failed: "+err.Error())
		case count > 0:
			res.Restored = append(res.Restored, fmt.Sprintf("uploads (%d files)", count))
		default:
			res.Skipped = append(res.Skipped, "uploads")
		}
		s.restoreFolderPart(files, &res, fontsPrefix, p.fontsDir, "fonts", "Fonts")
	} else {
		res.Skipped = append(res.Skipped, "uploads")
	}

	if o.Secrets {
		restoredAny := false
		for _, f := range append([]fileEntry{
			{"console/.session_secret", p.sessionSecret, 0o600},
			{"goserver/id_ed25519", p.goPrivKey, 0o600},
			{"goserver/id_ed25519.pub", p.goPubKey, 0o644},
			{"goserver/.api_key", p.goAPIKey, 0o600},
			{"goserver/mesh_agent_server.pem", p.meshAgentCert, 0o600},
			{"goserver/node_control.crt", p.nodeControlCert, 0o600},
			{"goserver/node_control.key", p.nodeControlKey, 0o600},
		}, tlsEntries(p)...) {
			data, ok := files.Get(f.name)
			if !ok || f.target == "" {
				continue
			}
			if err := writeFile(f.target, data, f.mode); err != nil {
				res.Warnings = append(res.Warnings, f.name+" restore failed: "+err.Error())
				continue
			}
			restoredAny = true
		}
		if restoredAny {
			res.Restored = append(res.Restored, "secrets/keys")
			res.RequiresRestart = true
		} else {
			res.Skipped = append(res.Skipped, "secrets/keys")
		}
	} else {
		res.Skipped = append(res.Skipped, "secrets/keys")
	}

	if data, ok := files.Get("console/.env"); o.Env && ok && s.o.Docker {
		// nothing in the container reads a .env: the settings are the
		// container's environment
		res.Skipped = append(res.Skipped, ".env")
		res.Warnings = append(res.Warnings, ".env not restored: a Docker install takes its settings from the container's environment, set them in the .env next to the compose file")
	} else if o.Env && ok {
		if err := writeFile(p.env, data, 0o600); err != nil {
			res.Warnings = append(res.Warnings, ".env restore failed: "+err.Error())
		} else {
			res.Restored = append(res.Restored, ".env")
			res.RequiresRestart = true
		}
	} else {
		res.Skipped = append(res.Skipped, ".env")
	}

	if serverFromDump {
		// restored with the dump above
	} else if data, ok := files.Get("goserver/db_v2.sqlite3"); o.GoDB && ok && p.goDB != "" {
		if err := stageServerDatabase(ctx, p.goDB+RestoredSuffix, data); err != nil {
			res.Warnings = append(res.Warnings, "Go server DB restore failed: "+err.Error())
		} else {
			res.Restored = append(res.Restored, "goserver-db")
			res.RequiresRestart = true
		}
	} else {
		res.Skipped = append(res.Skipped, "goserver-db")
	}
	return res, nil
}

// importDump puts back the tables of the dump that keep takes. The
// console's tables are listed as "database (<n> tables)", the server's
// own as "goserver-db", which takes effect at the next start.
func (s *Service) importDump(ctx context.Context, dump any, keep func(string) bool, res *RestoreResult) error {
	r, err := importAllTables(ctx, s.o.Store, dump, s.o.Now, keep)
	if err != nil {
		return err
	}
	consoleTables, server := 0, false
	for _, table := range r.Restored {
		if serverTables[table] {
			server = true
		} else {
			consoleTables++
		}
	}
	if s.o.Branding != nil && slices.Contains(r.Restored, "branding_config") {
		s.o.Branding.LoadBranding(ctx)
	}
	if slices.Contains(r.Restored, "users") || slices.Contains(r.Restored, "user_avatars") {
		s.forgetPictures()
	}
	if consoleTables > 0 || !server {
		res.Restored = append(res.Restored, fmt.Sprintf("database (%d tables)", consoleTables))
	}
	if server {
		res.Restored = append(res.Restored, "goserver-db")
		res.RequiresRestart = true
	}
	res.Warnings = append(res.Warnings, r.Warnings...)
	return nil
}

// serverTables are the server's own tables: what the signal server
// creates (package db) and the console's schema does not
// (TestServerTablesAreTheSignalServersOwn). The restore part Server
// device database puts back these, Console data every other table of the
// dump: accounts, folders, groups and the rest the console keeps.
var serverTables = map[string]bool{
	"access_policies": true, "api_keys": true, "billing_contracts": true, "billing_currencies": true,
	"billing_packages": true, "billing_period_totals": true, "billing_session_ledger": true,
	"billing_sessions": true, "billing_work_reports": true, "blocklist_entries": true, "chat_groups": true,
	"chat_messages": true, "client_sessions": true, "device_connection_mode_commands": true,
	"device_connection_policies": true, "device_sealed_signin": true, "device_signin": true, "device_telemetry_commands": true, "device_telemetry_snapshots": true,
	"device_tokens": true, "fleet_compliance": true, "fleet_device_resources": true, "fleet_resources": true,
	"fleet_task_runs": true, "fleet_tasks": true, "fleet_workflow_runs": true, "fleet_workflows": true,
	"help_requests": true, "id_change_history": true, "org_address_books": true,
	"org_devices": true, "org_invitations": true, "org_peer_credentials": true, "org_settings": true,
	"org_users": true, "organizations": true, "peer_availability_events": true, "peer_connection_sessions": true,
	"peers": true, "remote_targets": true, "role_permissions": true, "server_config": true, "server_events": true,
	// server_instances rows of a restored dump are long past their last_seen,
	// so the start check ignores them
	"server_instances": true,
	// Sites and their relay nodes. A node's row holds its public key, the
	// one it proves at every connect, so a restore without it would make
	// each node join again. Enrollment tokens are stored hashed, used once
	// and expire, so a restored one is no use past its expiry.
	"sites": true, "site_networks": true, "relay_nodes": true, "site_enrollment_tokens": true,
	"site_pins": true, "site_reachability": true,
}

// dumpHasServerTables reports whether a dump holds any of the server's own
// tables, as every dump of a 3.6 install does.
func dumpHasServerTables(dump any) bool {
	tables := prop(dump, "tables")
	for _, e := range entriesOf(tables) {
		if serverTables[e.key] {
			return true
		}
	}
	return false
}

// The folders of an archive.
const (
	uploadsPrefix     = "console/uploads/"
	chatFilesPrefix   = "console/chat-files/"
	attachmentsPrefix = "console/attachments/"
	fontsPrefix       = "console/fonts/"
)

// endpointsEntry is the public endpoints file in an archive.
const endpointsEntry = "console/public-endpoints.env"

// fileEntry is a file of an archive, the path it goes back to and the
// mode a new file gets.
type fileEntry struct {
	name, target string
	mode         os.FileMode
}

// tlsEntries are the console's TLS files in an archive: they go with the
// secrets.
func tlsEntries(p paths) []fileEntry {
	return []fileEntry{
		{"console/tls/certificate.pem", p.certFile, 0o644},
		{"console/tls/private-key.pem", p.keyFile, 0o600},
		{"console/tls/ca.pem", p.caFile, 0o644},
	}
}

// forgetPictures drops the avatar service's list of who has a picture
// after a restore replaced the accounts or their pictures.
func (s *Service) forgetPictures() {
	if s.o.Avatars != nil {
		s.o.Avatars.ResetCache()
	}
}

// restoreFolderPart restores the files below prefix into dir as a part of
// the console data: "<label> (N files)" when it wrote any, a warning
// "<Label> restore failed: ..." when a write failed, nothing when the
// archive has none.
func (s *Service) restoreFolderPart(files *Files, res *RestoreResult, prefix, dir, label, warning string) {
	if dir == "" {
		return
	}
	count, err := s.restoreFolder(files, prefix, dir)
	switch {
	case err != nil:
		res.Warnings = append(res.Warnings, warning+" restore failed: "+err.Error())
	case count > 0:
		res.Restored = append(res.Restored, fmt.Sprintf("%s (%d files)", label, count))
	}
}

// restoreFolder writes the archive's files below prefix into dir. An
// entry whose name is empty there, or holds "..", is skipped, as before;
// so is one that safepath refuses (an absolute name, a "." part, a link
// that leads out of the folder). A failed write ends the part.
func (s *Service) restoreFolder(files *Files, prefix, dir string) (int, error) {
	count := 0
	for _, name := range files.Names() {
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok || rel == "" || strings.Contains(rel, "..") {
			continue
		}
		if !uploadNameAllowed(dir, rel) {
			continue
		}
		data, _ := files.Get(name)
		if err := writeFile(filepath.Join(dir, rel), data, 0o644); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// writeFile is the old console's writeFile: the folder is made when
// missing, the file is replaced in place, and mode is used for a new file.
func writeFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return NodeFSError(err, "mkdir", dir)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return NodeFSError(err, "open", path)
	}
	return nil
}
