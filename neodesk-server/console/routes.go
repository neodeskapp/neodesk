package console

import (
	"github.com/neodeskapp/neodesk/neodesk-server/console/backup"
	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	accessibilityroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/accessibility"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/activity"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/apitokens"
	authroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/automation"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/avatars"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/cdap"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/cdapstudio"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/chat"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/clientbranding"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/commercialization"
	consoleroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/console"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/crossplatform"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/dashboard"
	dashboardroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/dashboards"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/dataguard"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/desktop"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/devicelogin"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/devices"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/filetransfer"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/fleet"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/folders"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/generator"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/guest"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/health"
	i18nroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/i18n"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/impersonate"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/inventory"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/keys"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/languages"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/meshcentral"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/ndapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/network"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/onboard"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/organizations"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/pages"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/pairing"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/panelaccess"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/permissions"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/phase45"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/policies"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/receiverapp"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/registration"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/remote"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/reports"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/resourcecontrol"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/rustdeskapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/scaling"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/securityaudit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/serverattestation"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/servermanagement"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/settings"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/settingsbackup"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/settingsupdates"
	setupguideroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/setupguide"
	sitesroutes "github.com/neodeskapp/neodesk/neodesk-server/console/routes/sites"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/system"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/tenants"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/tickets"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/tokens"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/toolkit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/trends"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/uishell"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/users"
)

// The route groups of the client API port (API_PORT), on a router of its
// own (listen.ClientAPI). clientAPILocalRoutes answer there only with
// RUSTDESK_API_PROXY=false, where the old port answered the client API
// itself; clientAPISharedRoutes answer in both modes.
var (
	clientAPILocalRoutes = []func(d *kit.Deps){
		rustdeskapi.Register, // routes/rustdesk-api.routes.js (server.js startRustDeskApiServer)
	}
	clientAPISharedRoutes = []func(d *kit.Deps){
		registration.ClientAPI, // routes/registration.routes.js at /api/nd (server.js startRustDeskApiServer)
	}
)

// registerRoutes adds the route groups, one line per group and mount, in
// the order of web-nodejs/routes/index.js: the
// tables answer with the first route that fits, so the order of the
// groups decides between two routes for one address.
//
// Groups of the early stage (routes/rustdesk-api.routes.js, then
// routes/bd-api.routes.js at /api/nd) register on d.Early and go first.
// backups is the console's backup service, which the updater uses too.
func registerRoutes(d *kit.Deps, backups *backup.Service) {
	rustdeskapi.Register(d)         // routes/rustdesk-api.routes.js (server.js), early stage
	ndapi.Register(d.At("/api/nd")) // routes/bd-api.routes.js at /api/nd (server.js), early stage
	remote.RegisterAgentToken(d)    // routes/bd-api.routes.js at /api/nd (server.js): POST /remote-agent-token, early stage
	health.Register(d)              // routes/index.js: GET /health
	authroutes.Register(d)          // routes/auth.routes.js
	consoleroutes.Register(d)       // routes/console.routes.js
	dashboard.Register(d)           // routes/dashboard.routes.js
	devices.Register(d)             // routes/devices.routes.js; its scope check (Use) also guards the folder group's /api/devices/{id}/folder
	keys.Register(d)                // routes/keys.routes.js
	settings.Register(d)            // routes/settings.routes.js: the general settings and the branding
	// routes/settings.routes.js: backup, restore and the scheduled backups
	settingsbackup.RegisterWith(d, backups)
	// routes/settings.routes.js: the update and management routes
	settingsupdates.Register(d)
	generator.Register(d)      // routes/generator.routes.js
	clientbranding.Register(d) // routes/client-branding.routes.js
	avatars.Register(d)        // routes/avatars.routes.js
	users.Register(d)          // routes/users.routes.js
	impersonate.Register(d)    // "Sign in as" (read-only), not in the Node console
	panelaccess.Register(d)    // routes/panel-access.routes.js
	uishell.Register(d)        // routes/ui-shell.routes.js
	folders.Register(d)        // routes/folders.routes.js
	remote.Register(d)         // routes/remote.routes.js, with the dev mock of lib/devMockRemote
	guest.Register(d)          // routes/guest.routes.js
	i18nroutes.Register(d)     // routes/i18n.routes.js at /api/i18n
	// routes/dataguard.routes.js at /api/dataguard and at /api/nd
	// routes/inventory.routes.js at /api/nd (the device routes inventory, telemetry and inventory/:id; the read routes step aside there) and at /api/inventory
	inventory.Register(d.At("/api/nd"))
	inventory.Register(d.At("/api/inventory"))
	// routes/tickets.routes.js at /api/tickets
	tickets.Register(d.At("/api/tickets"), chat.HubOf(d).ToDevice)
	// routes/activity.routes.js at /api/nd (the device route activity; the read routes step aside there) and at /api/activity
	activity.Register(d.At("/api/nd"))
	activity.Register(d.At("/api/activity"))
	// routes/automation.routes.js at /api/automation and at /api/nd (the device routes commands and commands/:id/result; the panel routes step aside there)
	automation.Register(d.At("/api/automation"))
	automation.Register(d.At("/api/nd"))
	// routes/fileTransfer.routes.js at /api/files and at /api/nd, with the device routes file-transfer
	filetransfer.Register(d.At("/api/files"))
	filetransfer.Register(d.At("/api/nd"))
	// routes/network.routes.js at /api/network
	network.Register(d.At("/api/network"))
	// routes/dataguard.routes.js at /api/dataguard and at /api/nd, without the device routes dlp-policies and dlp-events
	dataguard.Register(d.At("/api/dataguard"))
	dataguard.Register(d.At("/api/nd"))
	trends.Register(d) // routes/trends.routes.js, in front of the reports group
	// routes/reports.routes.js at /api/reports
	reports.Register(d.At("/api/reports"))
	// routes/tenants.routes.js at /api/tenants
	tenants.Register(d.At("/api/tenants"))
	// routes/registration.routes.js at the root and at /api/nd: one value for
	// both mounts, which share the names of who handled a help request
	registrationRoutes := registration.New()
	registrationRoutes.Register(d)
	registrationRoutes.Register(d.At("/api/nd"))
	pages.Register(d)                         // routes/pages.routes.js
	cdap.Register(d)                          // routes/cdap.routes.js
	meshcentral.Register(d)                   // routes/meshcentral.routes.js
	chat.Register(d)                          // routes/chat.routes.js
	tokens.Register(d)                        // routes/tokens.routes.js
	desktop.Register(d.At("/api/desktop"))    // routes/desktop.routes.js at /api/desktop
	organizations.Register(d)                 // routes/organizations.routes.js
	policies.Register(d)                      // routes/policies.routes.js
	policies.Register(d.At("/api/nd"))        // its second mount: /api/nd/api/panel/policies/...
	fleet.Register(d)                         // routes/fleet.routes.js
	commercialization.Register(d)             // routes/commercialization.routes.js
	fleet.Register(d.At("/api/nd"))           // its second mount: /api/nd/api/panel/fleet/...
	scaling.Register(d)                       // routes/scaling.routes.js
	scaling.Register(d.At("/api/nd"))         // routes/scaling.routes.js at /api/nd
	crossplatform.Register(d)                 // routes/cross-platform.routes.js
	toolkit.Register(d)                       // routes/toolkit.routes.js
	securityaudit.Register(d)                 // routes/security-audit.routes.js
	languages.Register(d)                     // routes/languages.routes.js
	resourcecontrol.Register(d)               // routes/resource-control.routes.js
	resourcecontrol.Register(d.At("/api/nd")) // its second mount: /api/nd/api/panel/resource-control/...
	system.Register(d)                        // routes/system.routes.js
	cdapstudio.Register(d)                    // routes/cdap-studio.routes.js
	permissions.Register(d)                   // routes/permissions.routes.js
	servermanagement.Register(d)              // routes/server-management.routes.js (beta)
	serverattestation.Register(d)             // routes/server-attestation.routes.js, with the job that runs the capacity tests
	phase45.Register(d)                       // routes/phase4_5.routes.js
	receiverapp.Register(d)                   // Devices › Add › Receiver app: the silent install of the NeoDesk app
	pairing.Register(d)                       // routes/pairing.routes.js
	onboard.Register(d)                       // Add a computer and Quick support: invites, sessions, client packages
	apitokens.Register(d)                     // API tokens for scripts, the command line and its MCP server
	setupguideroutes.Register(d)              // the first-install guide on the overview
	accessibilityroutes.Register(d)           // the signed-in account's own accessibility settings
	dashboardroutes.Register(d)               // the overview's boards: each account's own and the shared ones
	devicelogin.Register(d)                   // the desktop app's sign-in through the console (Allow page)
	sitesroutes.Register(d)                   // sites and relay nodes; after the device group, whose scope check guards /api/devices/{id}/site
}
