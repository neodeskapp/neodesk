package meshcentral

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/audit"
	"github.com/neodeskapp/neodesk/neodesk-server/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/config"
	"github.com/neodeskapp/neodesk/neodesk-server/db"
	"github.com/neodeskapp/neodesk/neodesk-server/events"
	"github.com/neodeskapp/neodesk/neodesk-server/peer"
	"github.com/neodeskapp/neodesk/neodesk-server/ratelimit"
	"github.com/neodeskapp/neodesk/neodesk-server/security"
)

// Gateway implements MeshCentral .ashx WebSocket endpoints on the API listener.
type Gateway struct {
	cfg       *config.Config
	db        db.Database
	peerMap   *peer.Map
	eventBus  *events.Bus
	auditLog  *audit.Logger
	blocklist *security.Blocklist
	jwt       *auth.JWTManager
	perms     PermissionChecker
	limiter   *ratelimit.IPLimiter
	cookies   *CookieCodec
	creds     *AgentCredentials
	assets    *CoreAssets
	webHash   []byte // SHA-384 of TLS web cert (48 bytes)

	agents sync.Map // peerID -> *AgentConn
	// presence tells the other instances which agents this one holds;
	// nil when the server runs alone.
	presence Presence
	// relayRouter passes a relay socket to the instance that set the relay
	// up and reports whether it did (SetRelayRouter).
	relayRouter func(w http.ResponseWriter, r *http.Request, relayID string) bool
	relays      sync.Map // relayID -> *relaySession (non-KVM)
	relayHubs   sync.Map // relayID -> *relayHub (KVM multiplex)
	relayMeta   sync.Map // relayID -> *relayMeta

	// relayWait is how long a relay waits for its peers (relayWaitDefault
	// when 0).
	relayWait time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	activeAgents atomic.Int64
	version      string

	// enroll keeps new agents until they are approved (SetEnrollmentQueue).
	enroll atomic.Pointer[enrollmentQueue]
}

// NewGateway creates a MeshCentral compatibility gateway.
func NewGateway(cfg *config.Config, database db.Database, peerMap *peer.Map, eventBus *events.Bus, jwtSecret string) (*Gateway, error) {
	rate := cfg.MeshRateLimit
	if rate <= 0 {
		rate = 30
	}
	creds, err := LoadOrCreateAgentCredentials(agentCertPath(cfg))
	if err != nil {
		return nil, err
	}
	assets, err := LoadCoreAssets(cfg.MeshCoreVersion, cfg.MeshAssetsDir)
	if err != nil {
		return nil, err
	}
	cookieCodec, err := NewCookieCodec(jwtSecret)
	if err != nil {
		return nil, err
	}
	return &Gateway{
		cfg:      cfg,
		db:       database,
		peerMap:  peerMap,
		eventBus: eventBus,
		limiter:  ratelimit.NewIPLimiter(rate, 1*time.Minute, 5*time.Minute),
		cookies:  cookieCodec,
		creds:    creds,
		assets:   assets,
		version:  cfg.MeshCoreVersion,
	}, nil
}

// SetBlocklist attaches IP blocklist.
func (g *Gateway) SetBlocklist(bl *security.Blocklist) { g.blocklist = bl }

// SetAuditLogger attaches audit logger.
func (g *Gateway) SetAuditLogger(al *audit.Logger) { g.auditLog = al }

// SetJWTManager attaches JWT manager for control channel auth.
func (g *Gateway) SetJWTManager(jm *auth.JWTManager) { g.jwt = jm }

// SetVersion sets build version for logs.
func (g *Gateway) SetVersion(v string) { g.version = v }

// SetWebCertHash sets SHA-384 hash of the public TLS certificate (48 bytes).
func (g *Gateway) SetWebCertHash(hash []byte) {
	if len(hash) == sha384Size {
		g.webHash = append([]byte(nil), hash...)
	}
}

// ServerID returns hex ServerID for .msh files.
func (g *Gateway) ServerID() string { return g.creds.ServerID }

// ActiveAgentCount returns connected mesh agents.
func (g *Gateway) ActiveAgentCount() int64 { return g.activeAgents.Load() }

// agentCertPath is the file of the agent-server certificate:
// MESH_AGENT_CERT_FILE (mesh_agent_server.pem by default), and a bare file
// name in the folder of the server's key file. The installers keep that
// key in KEYS_PATH, where a backup takes the certificate from, so the
// certificate does not depend on the folder the server runs in.
func agentCertPath(cfg *config.Config) string {
	path := cfg.MeshAgentCertFile
	if path == "" {
		path = "mesh_agent_server.pem"
	}
	if filepath.Base(path) == path && cfg.KeyFile != "" {
		path = filepath.Join(filepath.Dir(cfg.KeyFile), path)
	}
	return path
}

// AgentCertInfo returns mesh agent-server certificate path and presence on disk.
func (g *Gateway) AgentCertInfo() (path string, present bool, modified string) {
	path = agentCertPath(g.cfg)
	info, err := os.Stat(path)
	if err != nil {
		return path, false, ""
	}
	return path, true, info.ModTime().Format(time.RFC3339)
}

// IsConnected reports whether peer ID has live mesh agent connection.
func (g *Gateway) IsConnected(peerID string) bool {
	if v, ok := g.agents.Load(peerID); ok {
		ac, _ := v.(*AgentConn)
		return ac != nil && ac.authenticated
	}
	return false
}

// MeshNodeID returns stored mesh node id for peer.
func (g *Gateway) MeshNodeID(peerID string) string {
	if v, ok := g.agents.Load(peerID); ok {
		ac, _ := v.(*AgentConn)
		if ac != nil {
			return ac.meshNodeID
		}
	}
	val, _ := g.db.GetConfig("mesh_node_id_" + peerID)
	return val
}

// Start initializes gateway lifecycle context.
func (g *Gateway) Start(ctx context.Context) error {
	g.ctx, g.cancel = context.WithCancel(ctx)
	log.Printf("[mesh] Compatibility layer enabled (core %s, ServerID %s…)", g.assets.Version, g.creds.ServerID[:16])
	return nil
}

// Stop drains agent connections.
func (g *Gateway) Stop() {
	if g.cancel != nil {
		g.cancel()
	}
	g.agents.Range(func(key, value any) bool {
		if ac, ok := value.(*AgentConn); ok {
			ac.close()
		}
		return true
	})
	log.Printf("[mesh] Gateway stopped")
}

// RegisterRoutes mounts .ashx handlers on the API mux.
func (g *Gateway) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/agent.ashx", g.handleAgentWS)
	mux.HandleFunc("/meshrelay.ashx", g.handleRelayWS)
	mux.HandleFunc("/control.ashx", g.handleControlWS)
	mux.HandleFunc("/neocore.js", g.handleNeoCoreJS)
	mux.HandleFunc("/meshcore.js", g.handleNeoCoreJS) // legacy alias
}

func (g *Gateway) handleNeoCoreJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Write(g.assets.CoreModule)
}

func (g *Gateway) allowIP(w http.ResponseWriter, r *http.Request) bool {
	ip := clientIP(r, g.cfg)
	if g.blocklist != nil && g.blocklist.IsIPBlocked(ip) {
		http.Error(w, "blocked", http.StatusForbidden)
		return false
	}
	if g.limiter != nil && !g.limiter.Allow(ip) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return false
	}
	return true
}

// clientIP is the address the gateway counts a client by, for its limit,
// the blocklist and the logs: the API's rule (config's ClientIP).
func clientIP(r *http.Request, cfg *config.Config) string {
	return cfg.ClientIP(r)
}

// Presence records which instance of the installation holds a device's
// socket (cluster.Devices). Release reports whether the device is still
// this instance's to mark offline: false when it has reconnected to
// another instance meanwhile.
type Presence interface {
	Hold(kind, id string)
	Release(kind, id string) bool
}

// SetRelayRouter sets who passes a relay socket on to the instance that set
// the relay up (cluster mode).
func (g *Gateway) SetRelayRouter(route func(w http.ResponseWriter, r *http.Request, relayID string) bool) {
	g.relayRouter = route
}

// SetPresence shares which agents this instance holds (cluster mode).
func (g *Gateway) SetPresence(p Presence) { g.presence = p }
