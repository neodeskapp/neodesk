package sites

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/serverinfo"
	"github.com/neodeskapp/neodesk/neodesk-server/relaynode/wire"
	sitesvc "github.com/neodeskapp/neodesk/neodesk-server/sites"
)

// Where the install commands fetch the installers from.
const (
	installScriptURL = "https://neode.sk/install.sh"
	composeFileURL   = "https://neode.sk/install/docker-compose.relay-node.yml"
	windowsScriptURL = "https://neode.sk/neodesk.ps1"
)

// tokenAnswer is the data of POST /api/sites/{id}/enrollment-tokens. The
// token and the join string carrying it are in this answer only.
type tokenAnswer struct {
	Token string `json:"token"`
	// Join is the enrollment string: nd1. and the control address, the
	// certificate pin and the token.
	Join string `json:"join"`
	// Server is the control address in Join, host:port.
	Server string `json:"server"`
	// Command is the one-line Linux install (Commands.Linux).
	Command   string        `json:"command"`
	Commands  tokenCommands `json:"commands"`
	ExpiresAt time.Time     `json:"expires_at"`
}

type tokenCommands struct {
	Linux   string `json:"linux"`
	Docker  string `json:"docker"`
	Windows string `json:"windows"`
}

// commandsFor returns the install commands of an enrollment string.
func commandsFor(join string) tokenCommands {
	return tokenCommands{
		Linux:   "curl -fsSL " + installScriptURL + " | sudo bash -s -- --node --join " + join,
		Docker:  "curl -fsSLO " + composeFileURL + "\nNODE_JOIN=" + join + " docker compose -f docker-compose.relay-node.yml up -d",
		Windows: "Invoke-WebRequest " + windowsScriptURL + " -OutFile $env:TEMP\\neodesk.ps1 -UseBasicParsing\n& $env:TEMP\\neodesk.ps1 -Node -Join " + join,
	}
}

// controlHost is the host nodes reach this server at: the host clients
// get (the public server ID setting, PANEL_PUBLIC_HOST, RELAY_SERVERS,
// the API address, the request's host), without a port.
func (h *handlers) controlHost(r *http.Request) string {
	host := ""
	if h.d.ServerInfo != nil {
		host = h.d.ServerInfo.ResolveClientFacingHost(serverinfo.RequestHostOf(r), "")
	}
	if host == "" {
		host = serverinfo.StripRequestHost(r.Host)
	}
	if only, _, err := net.SplitHostPort(host); err == nil {
		host = only
	}
	return strings.Trim(host, "[]")
}

// enrollmentToken answers POST /api/sites/{id}/enrollment-tokens: a token
// that enrolls one relay node into the site within 24 hours, as an
// enrollment string with the commands that install a node with it. 503
// while relay nodes are off: no hub takes the node.
func (h *handlers) enrollmentToken(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	sc, ok := h.scope(w, r)
	if !ok {
		return
	}
	site := h.site(w, r, sc, r.PathValue("id"))
	if site == nil {
		return
	}
	glue := h.data.Glue()
	if glue == nil || glue.ControlPort == 0 || glue.Pin == "" {
		fail(w, r, http.StatusServiceUnavailable, "sites.nodes_off")
		return
	}
	user := kit.User(r)
	createdBy := ""
	if user != nil {
		createdBy = user.Username
	}
	token, t, err := h.data.Sites().CreateEnrollmentToken(site.ID, createdBy)
	if err != nil {
		h.writeError(w, r, "Create enrollment token error:", err)
		return
	}
	server := net.JoinHostPort(h.controlHost(r), strconv.Itoa(glue.ControlPort))
	join := wire.Join{Server: server, Pin: glue.Pin, Token: token}.Encode()
	cmds := commandsFor(join)
	h.audit(r, "relay_node_token_created", fmt.Sprintf("Enrollment token for site %s (%s), expires %s", site.Name, site.ID, t.ExpiresAt.Format(time.RFC3339)))
	kit.OK(w, r, tokenAnswer{Token: token, Join: join, Server: server, Command: cmds.Linux, Commands: cmds, ExpiresAt: t.ExpiresAt})
}

// revokeNode answers POST /api/relay-nodes/{id}/revoke: the node gets no
// more sessions and its connection is closed. Revoking a revoked node
// changes nothing.
func (h *handlers) revokeNode(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	sc, ok := h.scope(w, r)
	if !ok {
		return
	}
	n := h.node(w, r, sc)
	if n == nil {
		return
	}
	if n.RevokedAt == nil {
		if err := h.data.Sites().RevokeNode(n.ID); err != nil {
			h.writeError(w, r, "Revoke relay node error:", err)
			return
		}
		h.audit(r, "relay_node_revoked", fmt.Sprintf("Revoked relay node %s (%s)", n.Name, n.ID))
	}
	kit.JSON(w, r, http.StatusOK, done{Success: true})
}

// renameNode answers PATCH /api/relay-nodes/{id} with {name}.
func (h *handlers) renameNode(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	sc, ok := h.scope(w, r)
	if !ok {
		return
	}
	n := h.node(w, r, sc)
	if n == nil {
		return
	}
	name, isText := kit.Body(r).Get("name").Str()
	if !isText {
		fail(w, r, http.StatusBadRequest, "sites.name_invalid", "max", sitesvc.MaxNameLen)
		return
	}
	if err := h.data.Sites().RenameNode(n.ID, name); err != nil {
		h.writeError(w, r, "Rename relay node error:", err)
		return
	}
	renamed, err := h.data.DB().GetRelayNode(n.ID)
	if err != nil || renamed == nil {
		h.serverError(w, r, "Get relay node error:", err)
		return
	}
	h.audit(r, "relay_node_renamed", fmt.Sprintf("Renamed relay node %s (%s) to %s", n.Name, n.ID, renamed.Name))
	kit.OK(w, r, h.nodeOf(*renamed))
}

// reachJSON is one probe result.
type reachJSON struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Via       string    `json:"via"`
	OK        bool      `json:"ok"`
	RTTMs     int       `json:"rtt_ms"`
	CheckedAt time.Time `json:"checked_at"`
}

// reachability answers GET /api/sites/reachability: the last probe
// results between relay nodes, where the account may see both nodes'
// sites.
func (h *handlers) reachability(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w, r) {
		return
	}
	sc, ok := h.scope(w, r)
	if !ok {
		return
	}
	store := h.data.DB()
	results, err := store.ListSiteReachability()
	if err != nil {
		h.serverError(w, r, "List reachability error:", err)
		return
	}
	nodes, err := store.ListRelayNodes("")
	if err != nil {
		h.serverError(w, r, "List relay nodes error:", err)
		return
	}
	allSites, err := store.ListSites("", true)
	if err != nil {
		h.serverError(w, r, "List sites error:", err)
		return
	}
	visibleSite := map[string]bool{}
	for _, s := range allSites {
		visibleSite[s.ID] = sc.Has(s.OrgID)
	}
	visibleNode := map[string]bool{}
	for _, n := range nodes {
		visibleNode[n.ID] = visibleSite[n.SiteID]
	}
	out := []reachJSON{}
	for _, x := range results {
		if visibleNode[x.FromNode] && visibleNode[x.ToNode] {
			out = append(out, reachJSON{From: x.FromNode, To: x.ToNode, Via: x.Via, OK: x.OK, RTTMs: x.RTTMs, CheckedAt: x.CheckedAt})
		}
	}
	kit.OK(w, r, map[string]any{"results": out})
}
