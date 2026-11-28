package authn

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/store"
	"github.com/neodeskapp/neodesk/neodesk-server/internal/envvar"
)

// Logger takes the service's log lines. The arguments of a call are the
// parts of one line; *kit.Logger joins them with a space and shortens the
// user names in them. The lines carry "[AUTH]" themselves, so under a
// logger for the component AUTH they read
// "[AUTH] [AUTH] Login successful for 'a***e'", which is the form tools
// that read the console's log know.
type Logger interface {
	Info(args ...any)
	Warn(args ...any)
	Error(args ...any)
}

type discardLogger struct{}

func (discardLogger) Info(...any)  {}
func (discardLogger) Warn(...any)  {}
func (discardLogger) Error(...any) {}

// FS is the file access the default admin's set-up needs.
type FS interface {
	// Exists reports whether there is something at the path.
	Exists(name string) bool
	ReadFile(name string) ([]byte, error)
	// WriteFile replaces the file's content. A file it creates gets perm
	// (less the process's umask); an existing file keeps its mode.
	WriteFile(name string, data []byte, perm fs.FileMode) error
	// Chmod sets the file's mode, whatever the umask.
	Chmod(name string, mode fs.FileMode) error
	Remove(name string) error
}

type osFS struct{}

// OSFS is the real files.
var OSFS FS = osFS{}

func (osFS) Exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}
func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }
func (osFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(name, data, perm)
}
func (osFS) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }
func (osFS) Remove(name string) error                  { return os.Remove(name) }

// Settings are the values the service took from the environment. Each
// field names its variable and what an unset variable means.
type Settings struct {
	// AutoCreate lets an unknown user sign in through the signal server
	// and get a local account, also when no single sign-on is configured.
	// NEODESK_AUTH_AUTOCREATE, an on value (envvar.Switch); off unset.
	AutoCreate bool

	// TokenExpiryDays is how long an access token lasts.
	// API_TOKEN_EXPIRY_DAYS, 7.
	TokenExpiryDays int
	// MaxFailedAttempts is the number of failed sign-ins within the window
	// that locks an account. API_MAX_FAILED_ATTEMPTS, 10.
	MaxFailedAttempts int
	// LockoutMinutes is how long a lockout lasts. API_LOCKOUT_MINUTES, 15.
	LockoutMinutes int
	// IPRateLimit is the number of failed sign-ins within the window that
	// blocks an address. API_IP_RATE_LIMIT, 30.
	IPRateLimit int
	// AttemptWindowMinutes is the window both counts look back.
	// API_ATTEMPT_WINDOW, 15.
	AttemptWindowMinutes int

	// DefaultAdminUsername is the name of the first account.
	// DEFAULT_ADMIN_USERNAME, "admin".
	DefaultAdminUsername string
	// DefaultAdminPassword is its password. DEFAULT_ADMIN_PASSWORD; when
	// empty the password comes from a .admin_credentials file or is made
	// up.
	DefaultAdminPassword string
	// ForcePasswordUpdate makes the next EnsureDefaultAdmin overwrite the
	// admin's password with the known one. FORCE_PASSWORD_UPDATE, an on
	// value (envvar.Switch). It works once.
	ForcePasswordUpdate bool

	// DataDir is the console's data folder (DATA_DIR): the place of the
	// .force_password_update file and a place for .admin_credentials.
	DataDir string
	// KeysPath is the signal server's key folder (KEYS_PATH, else
	// RUSTDESK_DIR, else RUSTDESK_PATH, else what the console's
	// configuration found): the first place for .admin_credentials.
	KeysPath string
	// AuthDBPath is where an install with a separate user database keeps
	// it. AUTH_DB_PATH, <DataDir>/auth.db.
	AuthDBPath string

	// Docker is the raw DOCKER variable. "true" in any letter case makes
	// EnsureDefaultAdmin wait for the signal server's admin; the exact
	// "true" adds /app/data to the places for .admin_credentials.
	Docker string
	// DBType is the type of the database the server opens: "postgres" or
	// "sqlite" by the scheme of DATABASE_URL (envvar.DatabaseFrom, which
	// reads DB_URL for older installs), else DB_TYPE, else "sqlite".
	DBType string
	// SQLiteAuthDBMode is SQLITE_AUTH_DB_MODE: "legacy", "consolidated" or
	// empty.
	SQLiteAuthDBMode string
	// DockerAdminBootstrapWait is how long EnsureDefaultAdmin waits for
	// the signal server's admin. DOCKER_ADMIN_BOOTSTRAP_WAIT_MS, 30 s, at
	// least 1 s.
	DockerAdminBootstrapWait time.Duration
}

// SettingsFromEnv reads the settings the way the console read them from
// its environment. dataDir and keysPath are the folders the console's
// configuration resolved.
func SettingsFromEnv(getenv func(string) string, dataDir, keysPath string) Settings {
	s := Settings{
		AutoCreate:           envvar.SwitchOr(getenv("NEODESK_AUTH_AUTOCREATE"), false),
		TokenExpiryDays:      intOr(getenv(TokenExpiryEnv), DefaultTokenExpiryDays),
		MaxFailedAttempts:    intOr(getenv("API_MAX_FAILED_ATTEMPTS"), 10),
		LockoutMinutes:       intOr(getenv("API_LOCKOUT_MINUTES"), 15),
		IPRateLimit:          intOr(getenv("API_IP_RATE_LIMIT"), 30),
		AttemptWindowMinutes: intOr(getenv("API_ATTEMPT_WINDOW"), 15),
		DefaultAdminUsername: getenv("DEFAULT_ADMIN_USERNAME"),
		DefaultAdminPassword: getenv("DEFAULT_ADMIN_PASSWORD"),
		ForcePasswordUpdate:  envvar.SwitchOr(getenv("FORCE_PASSWORD_UPDATE"), false),
		DataDir:              dataDir,
		KeysPath:             keysPath,
		AuthDBPath:           getenv("AUTH_DB_PATH"),
		Docker:               getenv("DOCKER"),
		SQLiteAuthDBMode:     getenv("SQLITE_AUTH_DB_MODE"),
	}
	switch database := envvar.DatabaseFrom(getenv); {
	case database.Postgres:
		s.DBType = "postgres"
	case database.DSN != "":
		s.DBType = "sqlite"
	case getenv("DB_TYPE") != "":
		s.DBType = getenv("DB_TYPE")
	default:
		s.DBType = "sqlite"
	}
	wait := getenv("DOCKER_ADMIN_BOOTSTRAP_WAIT_MS")
	if wait == "" {
		wait = "30000"
	}
	s.DockerAdminBootstrapWait = time.Duration(max(1000, intOr(wait, 30000))) * time.Millisecond
	return s
}

// withDefaults fills what a zero Settings leaves open.
func (s Settings) withDefaults() Settings {
	fill := func(v *int, fallback int) {
		if *v == 0 {
			*v = fallback
		}
	}
	fill(&s.TokenExpiryDays, 7)
	fill(&s.MaxFailedAttempts, 10)
	fill(&s.LockoutMinutes, 15)
	fill(&s.IPRateLimit, 30)
	fill(&s.AttemptWindowMinutes, 15)
	if s.DefaultAdminUsername == "" {
		s.DefaultAdminUsername = "admin"
	}
	if s.DBType == "" {
		s.DBType = "sqlite"
	}
	if s.DockerAdminBootstrapWait < time.Second {
		if s.DockerAdminBootstrapWait == 0 {
			s.DockerAdminBootstrapWait = 30 * time.Second
		} else {
			s.DockerAdminBootstrapWait = time.Second
		}
	}
	return s
}

// Options configure a Service.
type Options struct {
	// DB is the console's database, migrated.
	DB *store.DB
	// GoAPI is the client for the signal server's API. Sign-in asks it
	// which single sign-on is on, lets it check directory accounts and
	// falls back to it when the local password check fails.
	GoAPI *goapi.Client
	// Settings are the values from the environment.
	Settings Settings
	// QR draws a text as a PNG data URL, 256 pixels wide with a margin of
	// 2 modules, black on white: qr.PNGDataURL with those options. The
	// two-factor set-up needs it.
	QR func(text string) (string, error)
	// Log takes the service's lines. Nil drops them.
	Log Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Rand is the source of secrets, codes, tokens and salts. Nil means
	// crypto/rand.
	Rand io.Reader
	// FS is the file access of EnsureDefaultAdmin. Nil means the real
	// files.
	FS FS
	// Sleep waits in EnsureDefaultAdmin. Nil means a real wait that ends
	// early when ctx does.
	Sleep func(ctx context.Context, d time.Duration) error
	// KV holds the steps of the codes that signed accounts in, which the
	// instances of an installation share, so a code works once on all of
	// them. Nil keeps them in memory.
	KV cluster.KV
}

// Service is the console's sign-in service. It is safe for concurrent use.
type Service struct {
	db    *store.DB
	goAPI *goapi.Client
	set   Settings
	// tokenDays is how many days a new access token lasts. It starts as
	// Settings.TokenExpiryDays and follows the client session settings.
	tokenDays atomic.Int64
	// onDeviceTokensRevoked is a func(deviceID string) told after
	// RevokeDeviceTokens (OnDeviceTokensRevoked)
	onDeviceTokensRevoked atomic.Value
	qr                    func(text string) (string, error)
	log       Logger
	now       func() time.Time
	rand      io.Reader
	fs        FS
	sleep     func(ctx context.Context, d time.Duration) error

	// hashIterations is the cost of new password hashes, recoveryCost the
	// bcrypt cost of stored recovery codes, and dummy the hash an unknown
	// user's password is compared with. Tests lower all three.
	hashIterations int
	recoveryCost   int
	dummy          string
	// windows says the process runs on Windows, which adds places to look
	// for .admin_credentials.
	windows bool
	// zone is the time zone of the process, which one answer depends on
	// (enrollmentExpiry).
	zone *time.Location

	mu sync.Mutex
	// forceUsed is set once ForcePasswordUpdate has had its one effect.
	forceUsed bool
	// kv holds per account the steps of the codes that signed it in
	// (useTOTPStep)
	kv cluster.KV
}

// New returns a service. DB and GoAPI are required.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, errors.New("authn: Options.DB is required")
	}
	if opts.GoAPI == nil {
		return nil, errors.New("authn: Options.GoAPI is required")
	}
	s := &Service{
		db:             opts.DB,
		goAPI:          opts.GoAPI,
		set:            opts.Settings.withDefaults(),
		qr:             opts.QR,
		log:            opts.Log,
		now:            opts.Now,
		rand:           opts.Rand,
		fs:             opts.FS,
		sleep:          opts.Sleep,
		hashIterations: auth.PasswordIterations,
		recoveryCost:   auth.RecoveryCodeCost,
		dummy:          dummyHash,
		windows:        runtime.GOOS == "windows",
		zone:           time.Local,
		kv:             opts.KV,
	}
	if s.kv == nil {
		s.kv = cluster.NewMemKV()
	}
	s.tokenDays.Store(int64(s.set.TokenExpiryDays))
	if s.log == nil {
		s.log = discardLogger{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.rand == nil {
		s.rand = rand.Reader
	}
	if s.fs == nil {
		s.fs = osFS{}
	}
	if s.sleep == nil {
		s.sleep = sleepContext
	}
	return s, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// randomHex returns n random bytes as lower-case hex.
func (s *Service) randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.rand, b); err != nil {
		return "", err
	}
	const digits = "0123456789abcdef"
	var out strings.Builder
	for _, c := range b {
		out.WriteByte(digits[c>>4])
		out.WriteByte(digits[c&15])
	}
	return out.String(), nil
}
