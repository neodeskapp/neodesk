package access

import (
	"strings"
	"testing"
)

// pagesFor returns the pages a role sees by the built-in role map.
func pagesFor(role string) map[string]bool {
	nav := BuildNav(NavInput{
		Can:          func(p string) bool { return RoleHasPermission(role, p) },
		IsSuperAdmin: IsSuperAdminRole(role),
		Role:         role,
	})
	pages := map[string]bool{}
	for _, section := range nav.Sections {
		for _, page := range section.Pages {
			pages[page.Page] = true
		}
	}
	return pages
}

// Jest: consoleNav.roles.test.js. The navigation shows a page to exactly
// the roles its route lets in.
func TestNavFollowsRouteGuards(t *testing.T) {
	cases := []struct {
		page  string
		roles map[string]bool
	}{
		// requireAdmin routes
		{"policies", map[string]bool{"super_admin": true, "admin": true, "global_admin": true, "server_admin": true, "operator": false, "viewer": false}},
		{"attestation", map[string]bool{"super_admin": true, "admin": true, "global_admin": true, "server_admin": true, "operator": false, "viewer": false}},
		// requireRole('operator')
		{"cdap-studio", map[string]bool{"operator": true, "super_admin": true, "admin": true, "global_admin": false, "server_admin": false, "viewer": false}},
		// Support chat or Team chat, as the chat socket needs (A242)
		{"chat", map[string]bool{"operator": true, "viewer": true, "global_admin": true, "server_admin": false}},
		// a session is enough: Add computers shows each account the tabs it may use
		{"devices-add", map[string]bool{"super_admin": true, "admin": true, "global_admin": true, "server_admin": true, "operator": true, "viewer": true, "guest_issuer": true}},
		{"dashboard", map[string]bool{"viewer": true, "guest_issuer": true, "pro": true}},
		// super admins only
		{"permissions", map[string]bool{"super_admin": true, "admin": true, "server_admin": false, "global_admin": false, "operator": false}},
		// one of several permissions
		{"organizations", map[string]bool{"global_admin": true, "admin": true, "operator": false, "server_admin": false, "viewer": false}},
		{"reports", map[string]bool{"viewer": true, "operator": true, "server_admin": true, "guest_issuer": false}},
		{"devices", map[string]bool{"guest_issuer": true, "viewer": true, "pro": false}},
		{"remote", map[string]bool{"operator": true, "viewer": false, "guest_issuer": false}},
	}
	for _, c := range cases {
		for role, shown := range c.roles {
			if got := pagesFor(role)[c.page]; got != shown {
				t.Errorf("%s sees %s: %v, want %v", role, c.page, got, shown)
			}
		}
	}

	// Without a role, role-guarded pages stay hidden whatever the permission check says.
	nav := BuildNav(NavInput{Can: func(string) bool { return true }})
	for _, section := range nav.Sections {
		for _, page := range section.Pages {
			if page.Page == "agents" || page.Page == "cdap-studio" || page.Page == "permissions" {
				t.Errorf("without a role the navigation shows %s", page.Page)
			}
		}
	}
	// Without a permission check, permission-guarded pages stay hidden.
	nav = BuildNav(NavInput{Role: "viewer"})
	for _, section := range nav.Sections {
		for _, page := range section.Pages {
			if page.Page == "devices" || page.Page == "chat" {
				t.Errorf("without a permission check the navigation shows %s", page.Page)
			}
		}
	}
}

// A194: the console has no server terminal, so no role sees an entry for
// it; Server status keeps its entry (Files and Services are its tabs).
func TestNavHasNoServerTerminal(t *testing.T) {
	for _, role := range []string{"super_admin", "admin", "server_admin"} {
		pages := pagesFor(role)
		if pages["server-management:terminal"] {
			t.Errorf("%s sees the server terminal", role)
		}
		for _, page := range []string{"server-management:overview"} {
			if !pages[page] {
				t.Errorf("%s does not see %s", role, page)
			}
		}
	}
}

// sectionPages lists the pages of one section, in order, for an account
// that may open everything.
func sectionPages(id string) (pages []string, groups []string) {
	nav := BuildNav(NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "super_admin"})
	for _, section := range nav.Sections {
		if section.ID != id {
			continue
		}
		for _, page := range section.Pages {
			pages = append(pages, page.Page)
			groups = append(groups, page.Group)
		}
	}
	return pages, groups
}

// Plan step 7: the rules pages are a section of their own between Activity
// and Settings, each with the check and address it had.
func TestNavRulesSection(t *testing.T) {
	pages, groups := sectionPages("rules")
	if got := strings.Join(pages, ","); got != "policies,automation,resource-control,dataguard,attestation" {
		t.Errorf("rules section has %s", got)
	}
	for i, group := range groups {
		if group != "" {
			t.Errorf("%s still carries the group %q", pages[i], group)
		}
	}
	settings, settingsGroups := sectionPages("settings")
	for i, page := range settings {
		if settingsGroups[i] == "nav.group_rules" || page == "policies" || page == "dataguard" {
			t.Errorf("settings still holds %s", page)
		}
	}
	for _, section := range NavSections {
		if section.ID == "rules" && (section.Label != "nav.section_rules" || section.Icon != "shield") {
			t.Errorf("rules section: label %q, icon %q", section.Label, section.Icon)
		}
	}
	// The checks did not change: operators see no policy page, server
	// admins see them all
	if pagesFor("operator")["policies"] || !pagesFor("server_admin")["dataguard"] {
		t.Error("the rules pages changed who sees them")
	}
}

// Plan step 4: Sign-in splits into Sign-in, Directory (LDAP), Single
// sign-on and Joining; MeshCentral is a tab of its own. The server-wide
// tabs show to server.config, Sign-in to everyone.
func TestNavSettingsTabs(t *testing.T) {
	pages, _ := sectionPages("settings")
	var tabs []string
	for _, page := range pages {
		if strings.HasPrefix(page, "settings:") {
			tabs = append(tabs, strings.TrimPrefix(page, "settings:"))
		}
	}
	if got := strings.Join(tabs, ","); got != "general,auth,ldap,sso,joining,branding,brand,email,time,meshcentral,quicksupport" {
		t.Errorf("settings tabs %s", got)
	}
	hrefs := map[string]NavPage{}
	for _, section := range NavSections {
		for _, page := range section.Pages {
			hrefs[page.Page] = page
		}
	}
	for tab, label := range map[string]string{"ldap": "settings.ldap_title", "sso": "settings.oidc_title", "joining": "settings.enrollment_title", "meshcentral": "nav.meshcentral", "quicksupport": "settings.tab_quicksupport"} {
		p := hrefs["settings:"+tab]
		if p.Href != "/settings?tab="+tab || p.Label != label {
			t.Errorf("settings:%s: %+v", tab, p)
		}
	}
	// Plan step 5: Branding is a tab for everyone (edits need branding.edit),
	// and Client app branding moved into it
	if p := hrefs["settings:brand"]; p.Href != "/settings?tab=brand" || p.Label != "settings.tab_brand" || p.rule != navAlways {
		t.Errorf("settings:brand: %+v", p)
	}
	if _, ok := hrefs["client-branding"]; ok {
		t.Error("Client app branding still has its own entry")
	}
	viewer := map[string]bool{}
	for _, section := range BuildNav(NavInput{Can: func(string) bool { return false }, Role: "viewer"}).Sections {
		for _, page := range section.Pages {
			viewer[page.Page] = true
		}
	}
	// Sign-in holds only what applies to everyone since the own password
	// and two-factor sign-in moved to Your account (plan step 2)
	if viewer["settings:auth"] || viewer["settings:ldap"] || viewer["settings:sso"] || viewer["settings:joining"] || viewer["settings:meshcentral"] {
		t.Errorf("an account without server.config sees %v", viewer)
	}
}

// Plan step 9: Activity splits what happened (Audit log) from what is
// wrong (Security checks), and Billing is one page there with its tabs.
func TestNavActivitySection(t *testing.T) {
	pages, _ := sectionPages("activity")
	if got := strings.Join(pages, ","); got != "security-audit,security-checks,activity,reports,billing" {
		t.Errorf("activity section has %s", got)
	}
	hrefs := map[string]NavPage{}
	for _, section := range NavSections {
		for _, page := range section.Pages {
			hrefs[page.Page] = page
		}
	}
	if p := hrefs["security-audit"]; p.Label != "security_audit.tab_audit_log" || p.Href != "/security-audit" {
		t.Errorf("audit log entry: %+v", p)
	}
	if p := hrefs["security-checks"]; p.Href != "/security-checks" || p.Label != "security_audit.tab_checks" || !p.Core {
		t.Errorf("security checks entry: %+v", p)
	}
	if p := hrefs["billing"]; p.Href != "/billing" || p.Label != "nav.commercialization" {
		t.Errorf("billing entry: %+v", p)
	}
	for _, gone := range []string{"commercialization:sessions", "commercialization:packages"} {
		if _, ok := hrefs[gone]; ok {
			t.Errorf("the navigation still has %s", gone)
		}
	}
	// The checks follow the pages' routes: audit.view and billing.view
	only := func(permission string) map[string]bool {
		shown := map[string]bool{}
		nav := BuildNav(NavInput{Can: func(p string) bool { return p == permission }, Role: "operator"})
		for _, section := range nav.Sections {
			for _, page := range section.Pages {
				shown[page.Page] = true
			}
		}
		return shown
	}
	if a := only("audit.view"); !a["security-checks"] || !a["security-audit"] || a["billing"] {
		t.Errorf("audit.view sees %v", a)
	}
	if b := only("billing.view"); !b["billing"] || b["security-checks"] {
		t.Errorf("billing.view sees %v", b)
	}
	// The new addresses belong to the sections of the old ones
	for path, section := range map[string]string{"/security-checks": "security-audit", "/billing": "commercialization"} {
		if rule, ok := FindRule(path); !ok || rule.Section != section {
			t.Errorf("%s: rule %+v, want section %s", path, rule, section)
		}
	}
	for page, key := range map[string]string{"billing": "billing", "security-checks": "security-checks"} {
		if got := NavPageKey(page, "sessions", false); got != key {
			t.Errorf("NavPageKey(%s) = %q", page, got)
		}
	}
}

// Plan step 6: what a server admin runs is a section of its own, shown only
// to server.config (Capacity test: server.attestation). Server status is one
// entry with Files and Services as its tabs.
func TestNavServerSection(t *testing.T) {
	pages, groups := sectionPages("server")
	want := "server-management:overview,scaling,server-backups,server-updates,server-config-files,server-attestation"
	if got := strings.Join(pages, ","); got != want {
		t.Errorf("server section has %s, want %s", got, want)
	}
	for i, group := range groups {
		if group != "" {
			t.Errorf("%s still carries the group %q", pages[i], group)
		}
	}
	hrefs := map[string]string{}
	for _, section := range NavSections {
		for _, page := range section.Pages {
			hrefs[page.Page] = page.Href
		}
		if section.ID == "server" && (section.Label != "nav.section_server" || section.Icon != "dns") {
			t.Errorf("server section: label %q, icon %q", section.Label, section.Icon)
		}
	}
	for page, href := range map[string]string{
		"server-management:overview": "/server-management", "server-backups": "/server/backups",
		"server-updates": "/server/updates", "server-config-files": "/server/config-files",
	} {
		if hrefs[page] != href {
			t.Errorf("%s leads to %q, want %q", page, hrefs[page], href)
		}
	}
	for _, gone := range []string{"settings:backup", "settings:updates", "settings:advanced", "server-management:files", "server-management:services"} {
		if _, ok := hrefs[gone]; ok {
			t.Errorf("the navigation still has %s", gone)
		}
	}
	// Only server.config sees the section; the capacity test asks its own
	// permission
	only := func(permission string) NavInput {
		return NavInput{Can: func(p string) bool { return p == permission }, Role: "operator"}
	}
	for _, section := range BuildNav(only("device.view")).Sections {
		if section.ID == "server" {
			t.Error("an account without server.config sees the server section")
		}
	}
	var attestationOnly []string
	for _, section := range BuildNav(only("server.attestation")).Sections {
		if section.ID == "server" {
			for _, page := range section.Pages {
				attestationOnly = append(attestationOnly, page.Page)
			}
		}
	}
	if strings.Join(attestationOnly, ",") != "server-attestation" {
		t.Errorf("server.attestation alone sees %v", attestationOnly)
	}
	// Files and Services mark Server status as the current page
	for _, tab := range []string{"", "files", "services"} {
		nav := BuildNav(NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "super_admin", CurrentPage: "server-management", CurrentTab: tab})
		if nav.ActivePage == nil || nav.ActivePage.Page != "server-management:overview" || nav.ActiveSection.ID != "server" {
			t.Errorf("tab %q: active %+v", tab, nav.ActivePage)
		}
	}
}

// Plan step 3: Platform support is reference, reached from Help; it left
// the Devices section.
func TestNavPlatformSupportInHelp(t *testing.T) {
	nav := BuildNav(NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "super_admin"})
	for _, section := range nav.Sections {
		for _, page := range section.Pages {
			if page.Page == "cross-platform" || page.Href == "/cross-platform" {
				t.Errorf("Platform support is still in the %s section", section.ID)
			}
		}
	}
}

// Plan step 8: every way a computer gets in is a tab of Devices › Add
// computers, right after Join requests; the pages it replaced and Enter a
// pairing code left the sidebar.
func TestNavAddComputers(t *testing.T) {
	nav := BuildNav(NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "super_admin"})
	var devices []string
	all := map[string]bool{}
	for _, section := range nav.Sections {
		for _, page := range section.Pages {
			all[page.Page] = true
			if section.ID == "devices" {
				devices = append(devices, page.Page)
			}
		}
	}
	if got := strings.Join(devices, ","); !strings.HasPrefix(got, "dashboard,devices,registrations,devices-add,") {
		t.Errorf("Devices: %s", got)
	}
	for _, gone := range []string{"keys", "tokens", "pairing", "agents", "activate-agent"} {
		if all[gone] {
			t.Errorf("%s still has an entry", gone)
		}
	}
}

func TestNavSectionsAndActivePage(t *testing.T) {
	admin := NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "admin"}
	nav := BuildNav(admin)
	var ids []string
	total := 0
	for _, section := range nav.Sections {
		ids = append(ids, section.ID)
		total += len(section.Pages)
	}
	if strings.Join(ids, ",") != "devices,support,people,activity,rules,server,settings" || total != 47 {
		t.Errorf("admin sees sections %v with %d pages", ids, total)
	}
	if nav.ActivePage != nil || nav.ActiveSection != nil {
		t.Error("nothing is active without a current page")
	}

	// A section without a visible page is left out.
	pro := BuildNav(NavInput{Can: func(p string) bool { return RoleHasPermission("pro", p) }, Role: "pro"})
	ids = nil
	for _, section := range pro.Sections {
		ids = append(ids, section.ID)
	}
	if strings.Join(ids, ",") != "devices,settings" {
		t.Errorf("pro sees sections %v", ids)
	}

	for _, c := range []struct {
		page, tab string
		key       string
		section   string
	}{
		{"devices", "", "devices", "devices"},
		{"settings", "", "settings:general", "settings"},
		{"settings", "branding", "settings:branding", "settings"},
		{"server-management", "", "server-management:overview", "server"},
		{"billing", "", "billing", "activity"},
		{"billing", "reports", "billing", "activity"},
		{"security-checks", "", "security-checks", "activity"},
		{"devices-add", "", "devices-add", "devices"},
		{"devices-add", "pairing", "devices-add", "devices"},
		// The old addresses of its tabs mark it too
		{"generator", "", "devices-add", "devices"},
		{"keys", "", "devices-add", "devices"},
	} {
		in := admin
		in.CurrentPage, in.CurrentTab = c.page, c.tab
		in.T = func(key string) string { return "<" + key + ">" }
		nav := BuildNav(in)
		if nav.ActivePage == nil || nav.ActiveSection == nil {
			t.Errorf("%s/%s: nothing active", c.page, c.tab)
			continue
		}
		wantPage := c.key
		if c.page == "generator" || c.page == "keys" {
			// No entry of their own; Add computers is active for them
			if got := NavPageKey(c.page, c.tab, false); got != c.page {
				t.Errorf("NavPageKey(%s) = %q", c.page, got)
			}
		} else if got := NavPageKey(c.page, c.tab, false); got != c.key {
			t.Errorf("NavPageKey(%s, %s) = %q, want %q", c.page, c.tab, got, c.key)
		}
		if nav.ActivePage.Page != wantPage || nav.ActiveSection.ID != c.section || !nav.ActivePage.Active || !nav.ActiveSection.Active {
			t.Errorf("%s/%s: active page %s in %s, want %s in %s", c.page, c.tab, nav.ActivePage.Page, nav.ActiveSection.ID, wantPage, c.section)
		}
		if !strings.HasPrefix(nav.ActivePage.Text, "<") || !strings.HasPrefix(nav.ActiveSection.Text, "<nav.") {
			t.Errorf("texts are not translated: %q, %q", nav.ActivePage.Text, nav.ActiveSection.Text)
		}
	}
}

// The policy hides the pages whose section the account may not read.
func TestPageAllowed(t *testing.T) {
	policy := &Policy{}
	for _, key := range catalogKeys() {
		policy.Sections.Set(key, SectionAccess{Access: LevelRead})
	}
	policy.Sections.Set("users", SectionAccess{Access: LevelNone})
	policy.Sections.Set("settings", SectionAccess{Access: LevelNone})
	policy.Sections.Set("registrations", SectionAccess{Access: LevelNone})
	policy.Sections.Set("dashboard", SectionAccess{Access: LevelWrite})

	admin := NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "admin", Allows: PageAllowed(policy)}
	shown := map[string]bool{}
	for _, section := range BuildNav(admin).Sections {
		for _, page := range section.Pages {
			shown[page.Page] = true
		}
	}
	for page, want := range map[string]bool{
		"dashboard": true, "devices": true,
		"users": false, // its section is at none
		// no section since F1: their routes and menu rules decide
		"settings:general": true, "server-backups": true, "policies": true, "settings:brand": true,
		"registrations": false,
		// Add computers is below /devices but has no section: its tabs ask their own permissions
		"devices-add":        true,
		"server-attestation": true,
		"billing":            true,
	} {
		if shown[page] != want {
			t.Errorf("page %s shown %v, want %v", page, shown[page], want)
		}
	}

	// Without a policy every page passes.
	if !PageAllowed(nil)(NavPage{Href: "/users"}) {
		t.Error("no policy must hide nothing")
	}
	// A policy without the section hides its pages.
	if PageAllowed(&Policy{})(NavPage{Href: "/users"}) {
		t.Error("a policy without the section must hide its pages")
	}
}

func TestAccessFor(t *testing.T) {
	if AccessFor(nil) != nil {
		t.Fatal("no policy: null")
	}
	if got := jsJSON(t, AccessFor(nil)); got != "null" {
		t.Fatalf("no policy as JSON: %s", got)
	}
	policy := &Policy{}
	policy.Sections.Set("dashboard", SectionAccess{Access: "WRITE"})
	policy.Sections.Set("devices", SectionAccess{Access: "bogus"})
	access := AccessFor(policy)
	body := jsJSON(t, access)
	wantStart := `{"sections":{"dashboard":"write","devices":"none"},"pages":[{"path":"/","section":"dashboard"},{"prefix":"/inventory","section":"devices"},`
	if !strings.HasPrefix(body, wantStart) {
		t.Errorf("access starts with\n %s", body[:len(wantStart)+10])
	}
	if !strings.Contains(body, `"catalog":[{"key":"dashboard","read":"device.view","write":null},{"key":"devices","read":"device.view","write":"device.edit"},`) {
		t.Errorf("catalog: %s", body)
	}
	if strings.Count(body, `{"key":"toolkit",`) != 1 || len(access.Catalog) != len(Catalog) || strings.Contains(body, `"prefix":"/api/`) {
		t.Error("the catalog is sent as it is, toolkit once; the pages hold no API rule")
	}
	if level, ok := access.Sections.Get("dashboard"); !ok || level != LevelWrite {
		t.Errorf("dashboard level: %q", level)
	}
}

func TestBootstrapNavShape(t *testing.T) {
	nav := BuildNav(NavInput{Can: func(p string) bool { return p == "enrollment.approve" }, Role: "viewer"}).Bootstrap()
	body := jsJSON(t, nav[0])
	want := `{"id":"devices","icon":"devices","text":"nav.devices","pages":[` +
		`{"page":"dashboard","href":"/","text":"nav.dashboard","group":"","core":true,"keywords":"home dashboard start","external":false,"countId":null},` +
		`{"page":"registrations","href":"/registrations","text":"nav.registrations","group":"","core":true,"keywords":"enrollment approve new device registration pending","external":false,"countId":"reg-sidebar-badge"},` +
		`{"page":"devices-add","href":"/devices/add","text":"nav.devices_add","group":"","core":true,"keywords":"add computers install join invite install links client setup keys device tokens pairing codes support agents","external":false,"countId":null}]}`
	if body != want {
		t.Errorf("first section:\n got  %s\n want %s", body, want)
	}
	last := nav[len(nav)-1]
	if last.ID != "settings" || last.Pages[0].Group != "nav.group_console" {
		t.Errorf("settings pages carry their group: %+v", last.Pages[0])
	}
}

// Remote sessions opens inside the console (the viewer's sessions live
// there); the standalone /remote stays for guests and Open in new tab.
func TestNavRemoteSessionsOpenInsideTheConsole(t *testing.T) {
	nav := BuildNav(NavInput{Can: func(string) bool { return true }, IsSuperAdmin: true, Role: "admin"})
	for _, section := range nav.Sections {
		for _, p := range section.Pages {
			if p.Page != "remote" {
				continue
			}
			if p.Href != "/remote/sessions" || p.External {
				t.Errorf("remote: href %q, external %v; want /remote/sessions inside the console", p.Href, p.External)
			}
			return
		}
	}
	t.Fatal("no remote page in the navigation")
}
