package access

import "strings"

// The console's navigation: seven sections, each with its pages. A page is
// shown to exactly the accounts its route lets in, so each page carries the
// check its route uses. The account's panel access policy then hides the
// pages whose section it may not read.

// navRule is the check a page's route uses.
type navRule int

const (
	navAlways      navRule = iota // every signed-in account
	navPermission                 // one of the page's permissions
	navAdminRole                  // the four admin roles
	navRoleOrSuper                // the page's role, or a super admin
	navSuperAdmin                 // super admins
)

// NavPage is one page of the navigation.
type NavPage struct {
	Page     string
	Keywords string
	Core     bool
	Href     string
	// Label and Group are translation keys.
	Label string
	Group string
	// Access names the page's panel section where its address alone does
	// not.
	Access        string
	CountID       string
	ID            string
	External      bool
	AlsoActiveFor []string

	rule        navRule
	permissions []string
	role        string
}

// NavSection is a top-level section of the navigation.
type NavSection struct {
	ID    string
	Label string
	Icon  string
	Pages []NavPage
}

func withPermission(permissions ...string) func(*NavPage) {
	return func(p *NavPage) { p.rule, p.permissions = navPermission, permissions }
}

func withRule(rule navRule) func(*NavPage) {
	return func(p *NavPage) { p.rule = rule }
}

func withRole(role string) func(*NavPage) {
	return func(p *NavPage) { p.rule, p.role = navRoleOrSuper, role }
}

func navPage(page, keywords, href, label string, check func(*NavPage), more ...func(*NavPage)) NavPage {
	p := NavPage{Page: page, Keywords: keywords, Href: href, Label: label}
	check(&p)
	for _, set := range more {
		set(&p)
	}
	return p
}

func core(p *NavPage) { p.Core = true }

func inGroup(group string) func(*NavPage) { return func(p *NavPage) { p.Group = group } }
func countID(id string) func(*NavPage)    { return func(p *NavPage) { p.CountID = id } }

const (
	groupConsole   = "nav.group_console"
	groupDeveloper = "nav.group_developer"
)

// NavSections is the whole navigation, before any account's checks.
var NavSections = []NavSection{
	{ID: "devices", Label: "nav.devices", Icon: "devices", Pages: []NavPage{
		navPage("dashboard", "home dashboard start", "/", "nav.dashboard", withRule(navAlways), core),
		navPage("devices", "computers peers machines connect", "/devices", "folders.all_devices", withPermission("device.view"), core),
		navPage("registrations", "enrollment approve new device registration pending", "/registrations", "nav.registrations", withPermission("enrollment.approve"), core, countID("reg-sidebar-badge")),
		// Every way a computer gets in, as tabs; the tabs ask their own
		// permissions, so the page is for every account (Manual setup is)
		navPage("devices-add", "add computers install join invite install links client setup keys device tokens pairing codes support agents", "/devices/add", "nav.devices_add", withRule(navAlways), core,
			func(p *NavPage) { p.AlsoActiveFor = []string{"keys", "tokens", "pairing", "agents", "generator"} }),
		navPage("remote", "web client browser remote control sessions", "/remote/sessions", "nav.remote", withPermission("device.connect"), core),
		navPage("inventory", "hardware software", "/inventory", "nav.inventory", withPermission("device.view")),
		navPage("network", "ping monitor latency targets", "/network", "nav.network", withPermission("device.view")),
		// Sites are per organization, so every account that sees devices
		// reads its organizations' sites (routes/sites)
		navPage("sites", "sites offices locations relay nodes networks", "/sites", "nav.sites", withPermission("device.view")),
		navPage("fleet", "fleet bulk tasks workflows compliance", "/fleet", "nav.fleet", withPermission("org.manage_devices")),
		navPage("cdap", "cdap iot scada sensors gateway", "/cdap", "nav.cdap", withPermission("cdap.view")),
	}},
	{ID: "support", Label: "nav.section_support", Icon: "support_agent", Pages: []NavPage{
		navPage("help-requests", "help support requests", "/help-requests", "nav.help_requests", withPermission("device.view"), core),
		navPage("quick-support-feedback", "feedback quick support rating stars answers survey", "/help-requests/feedback", "nav.quick_support_feedback", withPermission("device.connect"), core),
		navPage("tickets", "issues", "/tickets", "nav.tickets", withPermission("device.view"), core),
		navPage("chat", "messages", "/chat", "nav.chat", withPermission("chat.support", "chat.team"), core),
		navPage("toolkit", "toolkit ping dns port ssl diagnostics", "/toolkit", "nav.toolkit", withPermission("device.connect")),
	}},
	{ID: "people", Label: "nav.section_people", Icon: "group", Pages: []NavPage{
		navPage("users", "accounts people operators", "/users", "nav.users", withPermission("user.view"), core),
		navPage("organizations", "tenants customers", "/organizations", "nav.organizations", withPermission("org.create", "org.edit", "org.manage_users")),
		navPage("permissions", "roles rights access", "/permissions", "nav.permissions", withRule(navSuperAdmin)),
	}},
	{ID: "activity", Label: "nav.activity", Icon: "history", Pages: []NavPage{
		navPage("security-audit", "audit log who banned events server events", "/security-audit", "security_audit.tab_audit_log", withPermission("audit.view"), core),
		navPage("security-checks", "security checks hardening vulnerabilities", "/security-checks", "security_audit.tab_checks", withPermission("audit.view"), core),
		navPage("activity", "app usage activity monitoring", "/activity", "activity.title", withPermission("audit.view")),
		navPage("reports", "reports export trends", "/reports", "nav.reports", withPermission("audit.view"), core),
		navPage("billing", "billing commercialization packages contracts sessions minutes work reports", "/billing", "nav.commercialization", withPermission("billing.view"),
			func(p *NavPage) {
				p.AlsoActiveFor = []string{"commercialization:packages", "commercialization:sessions"}
			}),
	}},
	{ID: "rules", Label: "nav.section_rules", Icon: "shield", Pages: []NavPage{
		navPage("policies", "policies rules", "/policies", "nav.policies", withRule(navAdminRole), core),
		navPage("automation", "automation alerts commands rules", "/automation", "nav.automation", withPermission("server.config"), core),
		navPage("resource-control", "resource control usb drives disks", "/resource-control", "nav.resource_control", withPermission("server.config")),
		navPage("dataguard", "dataguard dlp data loss", "/dataguard", "nav.dataguard", withPermission("server.config")),
		navPage("attestation", "attestation fingerprint hardware identity", "/attestation", "nav.attestation", withRule(navAdminRole)),
	}},
	{ID: "server", Label: "nav.section_server", Icon: "dns", Pages: []NavPage{
		navPage("server-management:overview", "server management status health cpu memory uptime files services systemd", "/server-management", "nav.server_status", withPermission("server.config"), core,
			func(p *NavPage) { p.AlsoActiveFor = []string{"server-management:files", "server-management:services"} }),
		navPage("scaling", "scaling relays relay servers", "/scaling", "nav.scaling", withPermission("server.config"), core),
		navPage("server-backups", "backup backups restore schedule", "/server/backups", "nav.server_backups", withPermission("server.config"), core),
		navPage("server-updates", "updates version upgrade channel", "/server/updates", "updates.tab_title", withPermission("server.config"), core),
		navPage("server-config-files", "advanced configuration config files env restart", "/server/config-files", "nav.server_config_files", withPermission("server.config")),
		navPage("server-attestation", "server attestation capacity benchmark load test badge", "/server-attestation", "nav.server_attestation", withPermission("server.attestation")),
	}},
	{ID: "settings", Label: "nav.settings", Icon: "settings", Pages: []NavPage{
		navPage("settings:general", "settings relay address public endpoints connection strategy", "/settings", "settings.tab_general", withRule(navAlways), core, inGroup(groupConsole)),
		navPage("settings:auth", "sign-in authentication 2fa two-factor require client sessions", "/settings?tab=auth", "settings.tab_auth", withPermission("server.config"), core, inGroup(groupConsole)),
		navPage("settings:ldap", "ldap active directory directory sign-in", "/settings?tab=ldap", "settings.ldap_title", withPermission("server.config"), inGroup(groupConsole)),
		navPage("settings:sso", "sso oidc oauth openid single sign-on", "/settings?tab=sso", "settings.oidc_title", withPermission("server.config"), inGroup(groupConsole)),
		navPage("settings:joining", "join joining enrollment approval visibility availability history", "/settings?tab=joining", "settings.enrollment_title", withPermission("server.config"), inGroup(groupConsole)),
		navPage("settings:branding", "appearance theme colours colors fonts dark mode", "/settings?tab=branding", "settings.tab_branding", withRule(navAlways), inGroup(groupConsole)),
		navPage("settings:brand", "branding brand logo name sign-in page download page client app white label", "/settings?tab=brand", "settings.tab_brand", withRule(navAlways), inGroup(groupConsole)),
		navPage("settings:email", "email smtp mail", "/settings?tab=email", "settings.tab_email", withPermission("server.config"), inGroup(groupConsole)),
		navPage("settings:time", "time zone timezone ntp clock", "/settings?tab=time", "settings.tab_time", withPermission("server.config"), inGroup(groupConsole)),
		navPage("settings:meshcentral", "meshcentral mesh meshagent", "/settings?tab=meshcentral", "nav.meshcentral", withPermission("server.config"), inGroup(groupConsole)),
		navPage("settings:quicksupport", "quick support feedback form questions survey", "/settings?tab=quicksupport", "settings.tab_quicksupport", withPermission("server.config"), inGroup(groupConsole)),
		navPage("languages", "languages translations i18n", "/languages", "nav.languages", withPermission("server.config"), inGroup(groupConsole)),
		navPage("cdap-studio", "sdk studio flows widgets", "/cdap-studio", "nav.sdk_studio", withRole("operator"), inGroup(groupDeveloper)),
	}},
}

// NavInput is the account a navigation is built for.
type NavInput struct {
	// Can is the request's permission check; nil refuses everything.
	Can          func(permission string) bool
	IsSuperAdmin bool
	Role         string
	// T translates a key; nil leaves keys as they are.
	T func(key string) string
	// CurrentPage and CurrentTab mark the active page.
	CurrentPage string
	CurrentTab  string
	// ActivateAgentNav marks the old Pair agent entry of Node's navigation
	// (/?activateAgent=1), which the oracle tests still build; the console's
	// own navigation has no such entry since plan step 8 of L494.
	ActivateAgentNav bool
	// Allows is a further filter, in practice PageAllowed; nil lets every
	// page through.
	Allows func(NavPage) bool
}

// NavItem is a visible page with its translated texts.
type NavItem struct {
	NavPage
	Text      string
	GroupText string
	Active    bool
}

// NavGroup is a visible section with its visible pages.
type NavGroup struct {
	ID     string
	Icon   string
	Text   string
	Pages  []NavItem
	Active bool
}

// Nav is the navigation of one account.
type Nav struct {
	Sections      []NavGroup
	ActiveSection *NavGroup
	ActivePage    *NavItem
}

func (p NavPage) visible(in NavInput) bool {
	switch p.rule {
	case navAlways:
		return true
	case navPermission:
		for _, permission := range p.permissions {
			if in.Can != nil && in.Can(permission) {
				return true
			}
		}
		return false
	case navAdminRole:
		return IsAdminRole(in.Role)
	case navRoleOrSuper:
		return in.Role == p.role || IsSuperAdminRole(in.Role)
	case navSuperAdmin:
		return in.IsSuperAdmin
	}
	return false
}

// NavPageKey turns the current page, and the tab for pages that split by
// tab, into the page key of NavSections.
func NavPageKey(currentPage, currentTab string, activateAgentNav bool) string {
	orDefault := func(tab, fallback string) string {
		if tab == "" {
			return fallback
		}
		return tab
	}
	switch {
	case activateAgentNav:
		return "activate-agent"
	case currentPage == "settings":
		return "settings:" + orDefault(currentTab, "general")
	case currentPage == "server-management":
		return "server-management:" + orDefault(currentTab, "overview")
	// Billing (/billing) is one page with its tabs; the old commercialization
	// keys stay for its old address.
	case currentPage == "commercialization":
		if tab := orDefault(currentTab, "packages"); tab == "sessions" || tab == "reports" {
			return "commercialization:sessions"
		}
		return "commercialization:packages"
	}
	return currentPage
}

// BuildNav returns the pages an account sees. A section without a visible
// page is left out.
func BuildNav(in NavInput) Nav {
	t := in.T
	if t == nil {
		t = func(key string) string { return key }
	}
	key := NavPageKey(in.CurrentPage, in.CurrentTab, in.ActivateAgentNav)
	nav := Nav{Sections: []NavGroup{}}
	for _, section := range NavSections {
		group := NavGroup{ID: section.ID, Icon: section.Icon, Text: t(section.Label), Pages: []NavItem{}}
		for _, page := range section.Pages {
			if !page.visible(in) || (in.Allows != nil && !in.Allows(page)) {
				continue
			}
			item := NavItem{NavPage: page, Text: t(page.Label)}
			if page.Group != "" {
				item.GroupText = t(page.Group)
			}
			item.Active = page.Page == key
			for _, other := range page.AlsoActiveFor {
				item.Active = item.Active || other == key
			}
			group.Active = group.Active || item.Active
			group.Pages = append(group.Pages, item)
		}
		if len(group.Pages) > 0 {
			nav.Sections = append(nav.Sections, group)
		}
	}
	for i := range nav.Sections {
		section := &nav.Sections[i]
		if !section.Active {
			continue
		}
		nav.ActiveSection = section
		for j := range section.Pages {
			if section.Pages[j].Active {
				nav.ActivePage = &section.Pages[j]
			}
		}
	}
	return nav
}

// PageAllowed returns the navigation filter for a policy: a page passes
// when it has no panel section or the policy lets the account read its
// section. Without a policy every page passes.
func PageAllowed(policy *Policy) func(NavPage) bool {
	if policy == nil {
		return func(NavPage) bool { return true }
	}
	return func(p NavPage) bool {
		section := p.Access
		if section == "" {
			path, _, _ := strings.Cut(p.Href, "?")
			path, _, _ = strings.Cut(path, "#")
			if rule, ok := FindRule(path); ok {
				section = rule.Section
			}
		}
		return section == "" || AccessAllows(policy, section, LevelRead)
	}
}

// BootstrapNavPage is a page of the navigation as the bootstrap sends it.
type BootstrapNavPage struct {
	Page     string  `json:"page"`
	Href     string  `json:"href"`
	Text     string  `json:"text"`
	Group    string  `json:"group"`
	Core     bool    `json:"core"`
	Keywords string  `json:"keywords"`
	External bool    `json:"external"`
	CountID  *string `json:"countId"`
}

// BootstrapNavSection is a section of the navigation as the bootstrap
// sends it.
type BootstrapNavSection struct {
	ID    string             `json:"id"`
	Icon  string             `json:"icon"`
	Text  string             `json:"text"`
	Pages []BootstrapNavPage `json:"pages"`
}

// Bootstrap returns the navigation in the shape of the bootstrap's "nav".
func (n Nav) Bootstrap() []BootstrapNavSection {
	out := make([]BootstrapNavSection, 0, len(n.Sections))
	for _, section := range n.Sections {
		s := BootstrapNavSection{ID: section.ID, Icon: section.Icon, Text: section.Text, Pages: make([]BootstrapNavPage, 0, len(section.Pages))}
		for _, page := range section.Pages {
			p := BootstrapNavPage{
				Page: page.Page, Href: page.Href, Text: page.Text, Group: page.GroupText,
				Core: page.Core, Keywords: page.Keywords, External: page.External,
			}
			if page.CountID != "" {
				id := page.CountID
				p.CountID = &id
			}
			s.Pages = append(s.Pages, p)
		}
		out = append(out, s)
	}
	return out
}

// BootstrapCatalogEntry is a catalog section as the bootstrap sends it: the
// permission pair the section stands for.
type BootstrapCatalogEntry struct {
	Key   string     `json:"key"`
	Read  Permission `json:"read"`
	Write Permission `json:"write"`
}

// SectionLevels holds each section's level by key, in the policy's order.
type SectionLevels struct {
	keys   []string
	values map[string]Level
}

// Get returns the level of a section.
func (s SectionLevels) Get(key string) (Level, bool) {
	level, ok := s.values[key]
	return level, ok
}

// MarshalJSON writes the levels as an object in their order.
func (s SectionLevels) MarshalJSON() ([]byte, error) {
	return marshalOrdered(s.keys, func(key string) any { return s.values[key] })
}

// BootstrapAccess is the bootstrap's "access": the account's level in each
// section, which page belongs to which section, and the permission pair of
// each section. The web app hides what the guard would refuse and turns off
// write actions in read-only sections.
type BootstrapAccess struct {
	Sections SectionLevels           `json:"sections"`
	Pages    []Rule                  `json:"pages"`
	Catalog  []BootstrapCatalogEntry `json:"catalog"`
}

// AccessFor returns the bootstrap's "access" for a policy, or nil (JSON
// null) when no policy was loaded for the request.
func AccessFor(policy *Policy) *BootstrapAccess {
	if policy == nil {
		return nil
	}
	out := &BootstrapAccess{Pages: PageRules(), Catalog: make([]BootstrapCatalogEntry, 0, len(Catalog))}
	out.Sections.values = make(map[string]Level, len(policy.Sections.keys))
	for _, key := range policy.Sections.keys {
		entry := policy.Sections.values[key]
		out.Sections.keys = append(out.Sections.keys, key)
		out.Sections.values[key] = NormalizeLevel(string(entry.Access))
	}
	for _, entry := range Catalog {
		out.Catalog = append(out.Catalog, BootstrapCatalogEntry{Key: entry.Key, Read: entry.ReadPermission, Write: entry.WritePermission})
	}
	return out
}
