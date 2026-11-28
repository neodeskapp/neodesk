// Package auth — OIDC/OAuth2 authentication provider for NeoDesk server.
//
// Supports OpenID Connect Authorization Code Flow with:
//   - Auto-discovery via .well-known/openid-configuration
//   - Manual endpoint configuration (for non-compliant IdPs)
//   - PKCE (S256) for public/confidential clients
//   - UserInfo endpoint for claim extraction
//   - Claim → role mapping
//   - Auto-provisioning of local user accounts on first OIDC login
//   - State + nonce CSRF protection
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// OIDCConfig holds all OIDC provider settings.
// Stored in server_config table with "oidc." prefix.
type OIDCConfig struct {
	Enabled             bool   `json:"enabled"`
	PreferSSO           bool   `json:"prefer_sso"`            // launch SSO automatically when enabled
	DisplayName         string `json:"display_name"`          // Button label, e.g. "Google", "Azure AD"
	IssuerURL           string `json:"issuer_url"`            // e.g. "https://accounts.google.com"
	ClientID            string `json:"client_id"`             // OAuth2 client ID
	ClientSecret        string `json:"client_secret"`         // OAuth2 client secret
	RedirectURL         string `json:"redirect_url"`          // e.g. "https://neodesk.example.com/api/auth/oidc/callback"
	PanelURL            string `json:"panel_url"`             // e.g. "https://neodesk.example.com" (Node console origin)
	Scopes              string `json:"scopes"`                // space-separated, default "openid profile email"
	UsePKCE             bool   `json:"use_pkce"`              // enable PKCE (S256)
	AutoDiscovery       bool   `json:"auto_discovery"`        // use .well-known/openid-configuration
	AuthorizationURL    string `json:"authorization_url"`     // manual: authorization endpoint
	TokenURL            string `json:"token_url"`             // manual: token endpoint
	UserinfoURL         string `json:"userinfo_url"`          // manual: userinfo endpoint
	ClaimUsername       string `json:"claim_username"`        // claim for username (default: preferred_username)
	ClaimEmail          string `json:"claim_email"`           // claim for email (default: email)
	ClaimName           string `json:"claim_name"`            // claim for display name (default: name)
	ClaimGroups         string `json:"claim_groups"`          // claim for groups (default: groups)
	DefaultRole         string `json:"default_role"`          // role for users without group mapping (default: viewer)
	GroupRoleMap        string `json:"group_role_map"`        // pipe-delimited: "GroupName=role|GroupName=role"
	AllowSignup         bool   `json:"allow_signup"`          // allow auto-creation of new users
	AllowedPrivateCIDRs string `json:"allowed_private_cidrs"` // optional RFC1918/IPv6 ULA OIDC networks
}

// OIDC flow kinds stored in OAuth state.
const (
	OIDCFlowPanel  = "panel"  // web console login
	OIDCFlowClient = "client" // stock RustDesk desktop client
)

// OIDCResult represents the outcome of an OIDC authentication.
type OIDCResult struct {
	Authenticated bool
	Username      string
	DisplayName   string
	Email         string
	Role          string
	RoleFromGroup bool // the group map decided Role (a map is set)
	Groups        []string
	IDToken       string // raw ID token for audit
	ReturnURL     string // post-login relative path from OAuth state
	Flow          string // OIDCFlowPanel or OIDCFlowClient
	ClientID      string
	ClientUUID    string
	State         string // OAuth state (= client poll code for client flow)
	Picture       string // "picture" claim after SanitizeOIDCPicture, "" when absent or refused
}

// oidcDiscovery holds discovered OIDC endpoints.
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	Issuer                string `json:"issuer"`
}

// oidcState holds pending authorization state for CSRF protection.
type oidcState struct {
	Nonce        string
	CodeVerifier string // PKCE
	CreatedAt    time.Time
	ReturnURL    string // where to redirect after login (panel flow)
	Flow         string // OIDCFlowPanel (default) or OIDCFlowClient
	ClientID     string
	ClientUUID   string
	DeviceName   string
	DeviceOS     string
	DeviceType   string
}

// ClientOIDCPending tracks a RustDesk desktop OIDC login until auth-query consumes it.
type ClientOIDCPending struct {
	ClientID   string
	ClientUUID string
	DeviceName string
	DeviceOS   string
	DeviceType string
	UserID     int64
	Username   string
	Role       string
	Authed     bool
	CreatedAt  time.Time
}

// oidcAuthCode is a one-time-use code that the panel exchanges (over a
// server-to-server POST) for the JWT + verified user identity. This avoids
// passing the JWT through the browser URL bar (which would leak it via
// browser history, Referer headers, and access logs).
type oidcAuthCode struct {
	Token     string
	Username  string
	Role      string
	ReturnURL string // already validated as a relative path
	Picture   string // sanitized "picture" claim, "" when none
	// State is the OAuth state of the sign-in. The console compares it
	// with the state its cookie holds, to give the session only to the
	// browser that started the sign-in.
	State     string
	CreatedAt time.Time
}

// oidcDiscoveryRetryIntervalNs is how often discovery is retried while it has
// not yet succeeded (IdP unreachable at boot). Stored as nanoseconds so tests
// can override it without racing discoveryRetryLoop readers (atomic).
var oidcDiscoveryRetryIntervalNs atomic.Int64

func init() {
	oidcDiscoveryRetryIntervalNs.Store(int64(2 * time.Minute))
}

func oidcDiscoveryRetryDuration() time.Duration {
	ns := oidcDiscoveryRetryIntervalNs.Load()
	if ns <= 0 {
		return 2 * time.Minute
	}
	return time.Duration(ns)
}

// setOIDCDiscoveryRetryIntervalForTest overrides the retry interval for tests.
// The returned restore function puts the previous value back.
func setOIDCDiscoveryRetryIntervalForTest(d time.Duration) (restore func()) {
	prev := oidcDiscoveryRetryIntervalNs.Swap(int64(d))
	return func() { oidcDiscoveryRetryIntervalNs.Store(prev) }
}

// maxOIDCPending caps the sign-ins in progress one instance keeps, each of
// the states and of the desktop client starts. The starts are public, so
// without a cap every call would add an entry until it expires.
const maxOIDCPending = 4096

// Longest fields a start may store, in bytes.
const (
	maxOIDCReturnURL   = 2048
	maxOIDCClientID    = 64
	maxOIDCClientUUID  = 128
	maxOIDCDeviceName  = 128
	maxOIDCDeviceOS    = 64
	maxOIDCDeviceType  = 32
	oidcStateLifetime  = 15 * time.Minute
	oidcClientLifetime = 10 * time.Minute
	// oidcCodeLifetime is how long a one-time code for the panel lives.
	oidcCodeLifetime = 60 * time.Second
)

// The keys of the provider's entries in its KV.
const (
	oidcStateKey  = "oidc:state:"
	oidcCodeKey   = "oidc:code:"
	oidcClientKey = "oidc:client:"
)

// ErrOIDCTooManyPending refuses a start while maxOIDCPending sign-ins are in
// progress.
var ErrOIDCTooManyPending = errors.New("too many single sign-ons in progress")

// ErrOIDCFieldTooLong refuses a start with a field over its limit.
var ErrOIDCFieldTooLong = errors.New("single sign-on start field too long")

// OIDCProvider manages OIDC authentication. The sign-ins in progress (the
// states, the one-time codes for the panel and the desktop client starts)
// are in a KV, JSON-encoded under oidcStateKey, oidcCodeKey and
// oidcClientKey, so the callback and the polls can land on another
// instance than the start (SetKV). Each expires with its lifetime.
type OIDCProvider struct {
	mu         sync.RWMutex
	discoverMu sync.Mutex // serializes discover() (boot / UpdateConfig / retry)
	config     *OIDCConfig
	discovery  *oidcDiscovery
	kv         cluster.KV
	// stateCap and clientCap count the states and the client starts this
	// instance keeps (maxOIDCPending)
	stateCap  *cluster.LocalCap
	clientCap *cluster.LocalCap
	// now is the clock of the entries' ages; tests move it
	now      func() time.Time
	client   *http.Client
	stop     chan struct{}
	stopOnce sync.Once
}

// NewOIDCProvider creates a new OIDC provider with the given configuration.
// It keeps its sign-ins in progress in memory until SetKV.
func NewOIDCProvider(cfg *OIDCConfig) *OIDCProvider {
	p := &OIDCProvider{
		config:    cfg,
		kv:        cluster.NewMemKV(),
		stateCap:  cluster.NewLocalCap(maxOIDCPending),
		clientCap: cluster.NewLocalCap(maxOIDCPending),
		now:       time.Now,
		client:    &http.Client{Timeout: 15 * time.Second},
		stop:      make(chan struct{}),
	}
	// Immediate discover + periodic retry while discovery remains nil.
	go p.discoveryRetryLoop()
	return p
}

// SetKV makes the provider keep its sign-ins in progress in kv, which the
// instances of an installation share. Call it before the provider serves.
func (p *OIDCProvider) SetKV(kv cluster.KV) {
	if p == nil || kv == nil {
		return
	}
	p.mu.Lock()
	p.kv = kv
	p.mu.Unlock()
}

func (p *OIDCProvider) store() cluster.KV {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.kv
}

// putJSON stores v under key for ttl.
func (p *OIDCProvider) putJSON(key string, v any, ttl time.Duration) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.store().Set(context.Background(), key, string(raw), ttl)
}

// readJSON reads key into v; take removes it in the same step. It is
// false when there is no such entry or it cannot be read.
func (p *OIDCProvider) readJSON(key string, v any, take bool) bool {
	kv := p.store()
	var raw string
	var ok bool
	var err error
	if take {
		raw, ok, err = kv.Take(context.Background(), key)
	} else {
		raw, ok, err = kv.Get(context.Background(), key)
	}
	if err != nil {
		log.Printf("[OIDC] read a sign-in in progress: %v", err)
		return false
	}
	return ok && json.Unmarshal([]byte(raw), v) == nil
}

// Close stops background discovery/cleanup goroutines. Safe to call multiple times.
func (p *OIDCProvider) Close() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
}

// IsEnabled returns whether OIDC authentication is active.
func (p *OIDCProvider) IsEnabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config != nil && p.config.Enabled && p.config.ClientID != ""
}

// GetConfig returns a copy of the current OIDC config (thread-safe).
func (p *OIDCProvider) GetConfig() OIDCConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.config == nil {
		return OIDCConfig{}
	}
	return *p.config
}

// UpdateConfig replaces the current configuration and triggers re-discovery.
func (p *OIDCProvider) UpdateConfig(cfg *OIDCConfig) {
	p.mu.Lock()
	p.config = cfg
	p.discovery = nil
	p.mu.Unlock()
	if cfg.Enabled && cfg.AutoDiscovery && cfg.IssuerURL != "" {
		go p.discover()
	}
}

// GetDisplayName returns the configured button label or a default.
func (p *OIDCProvider) GetDisplayName() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.config != nil && p.config.DisplayName != "" {
		return p.config.DisplayName
	}
	return "SSO"
}

// needsDiscovery reports whether auto-discovery should still be attempted.
func (p *OIDCProvider) needsDiscovery() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.config != nil &&
		p.config.Enabled &&
		p.config.AutoDiscovery &&
		p.config.IssuerURL != "" &&
		p.discovery == nil
}

// discoveryRetryLoop attempts discovery immediately, then retries on an interval
// until it succeeds (or OIDC / auto-discovery is disabled). Self-heals when the
// IdP is unreachable at process start. Exits when Close() is called.
func (p *OIDCProvider) discoveryRetryLoop() {
	if p.needsDiscovery() {
		p.discover()
	}
	ticker := time.NewTicker(oidcDiscoveryRetryDuration())
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			if p.needsDiscovery() {
				p.discover()
			}
			ticker.Reset(oidcDiscoveryRetryDuration())
		}
	}
}

// discover fetches the OIDC discovery document from issuer_url/.well-known/openid-configuration.
func (p *OIDCProvider) discover() {
	p.discoverMu.Lock()
	defer p.discoverMu.Unlock()

	p.mu.RLock()
	cfg := p.config
	var issuer string
	if cfg != nil {
		issuer = cfg.IssuerURL
	}
	enabled := cfg != nil && cfg.Enabled && cfg.AutoDiscovery && issuer != ""
	p.mu.RUnlock()

	if !enabled {
		return
	}

	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	policy, err := newOIDCFetchPolicy(cfg.AllowedPrivateCIDRs)
	if err != nil {
		log.Printf("[OIDC] Invalid private network allowlist: %v", err)
		return
	}
	resp, err := fetchValidatedHTTPGetWithPolicy(context.Background(), p.client, discoveryURL, policy)
	if err != nil {
		log.Printf("[OIDC] Discovery failed for %s: %v", issuer, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[OIDC] Discovery returned %d for %s", resp.StatusCode, discoveryURL)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		log.Printf("[OIDC] Discovery read error: %v", err)
		return
	}

	var disc oidcDiscovery
	if err := json.Unmarshal(body, &disc); err != nil {
		log.Printf("[OIDC] Discovery parse error: %v", err)
		return
	}

	p.mu.Lock()
	// Apply only if config still matches the issuer we fetched (avoid stale
	// write racing UpdateConfig).
	if p.config != nil && p.config.Enabled && p.config.AutoDiscovery && p.config.IssuerURL == issuer {
		p.discovery = &disc
		log.Printf("[OIDC] Discovery OK: auth=%s, token=%s", disc.AuthorizationEndpoint, disc.TokenEndpoint)
	}
	p.mu.Unlock()
}

// getAuthEndpoint returns the authorization endpoint URL.
func (p *OIDCProvider) getAuthEndpoint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.config.AuthorizationURL != "" {
		return p.config.AuthorizationURL
	}
	if p.discovery != nil {
		return p.discovery.AuthorizationEndpoint
	}
	return ""
}

// getTokenEndpoint returns the token endpoint URL.
func (p *OIDCProvider) getTokenEndpoint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.config.TokenURL != "" {
		return p.config.TokenURL
	}
	if p.discovery != nil {
		return p.discovery.TokenEndpoint
	}
	return ""
}

// getUserinfoEndpoint returns the userinfo endpoint URL.
func (p *OIDCProvider) getUserinfoEndpoint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.config.UserinfoURL != "" {
		return p.config.UserinfoURL
	}
	if p.discovery != nil {
		return p.discovery.UserinfoEndpoint
	}
	return ""
}

// BuildAuthURL constructs the OIDC authorization URL for panel (web console) login.
func (p *OIDCProvider) BuildAuthURL(returnURL string) (string, string, error) {
	return p.buildAuthURL(oidcState{
		ReturnURL: returnURL,
		Flow:      OIDCFlowPanel,
	})
}

// ClientDeviceInfo is device metadata from the RustDesk desktop client.
type ClientDeviceInfo struct {
	Name string
	OS   string
	Type string
}

// BuildClientAuthURL starts OIDC for the stock RustDesk desktop client.
// The returned code is the OAuth state value; the client polls auth-query with it.
func (p *OIDCProvider) BuildClientAuthURL(clientID, clientUUID string, device ClientDeviceInfo) (authURL, code string, err error) {
	if len(clientID) > maxOIDCClientID || len(clientUUID) > maxOIDCClientUUID || len(device.Name) > maxOIDCDeviceName ||
		len(device.OS) > maxOIDCDeviceOS || len(device.Type) > maxOIDCDeviceType {
		return "", "", ErrOIDCFieldTooLong
	}
	authURL, code, err = p.buildAuthURL(oidcState{
		Flow:       OIDCFlowClient,
		ClientID:   clientID,
		ClientUUID: clientUUID,
		DeviceName: device.Name,
		DeviceOS:   device.OS,
		DeviceType: device.Type,
	})
	if err != nil {
		return "", "", err
	}
	now := p.now()
	if !p.clientCap.Add(code, now, now.Add(oidcClientLifetime)) {
		p.dropState(code)
		return "", "", ErrOIDCTooManyPending
	}
	if err := p.putJSON(oidcClientKey+code, &ClientOIDCPending{
		ClientID:   clientID,
		ClientUUID: clientUUID,
		DeviceName: device.Name,
		DeviceOS:   device.OS,
		DeviceType: device.Type,
		CreatedAt:  now,
	}, oidcClientLifetime); err != nil {
		p.clientCap.Remove(code)
		p.dropState(code)
		return "", "", fmt.Errorf("store the sign-in: %w", err)
	}
	return authURL, code, nil
}

func (p *OIDCProvider) buildAuthURL(base oidcState) (string, string, error) {
	if len(base.ReturnURL) > maxOIDCReturnURL {
		return "", "", ErrOIDCFieldTooLong
	}
	authEP := p.getAuthEndpoint()
	if authEP == "" {
		return "", "", fmt.Errorf("authorization endpoint not configured")
	}

	p.mu.RLock()
	cfg := *p.config
	p.mu.RUnlock()

	// Generate state token (CSRF protection)
	state, err := generateRandomString(32)
	if err != nil {
		return "", "", fmt.Errorf("generate state: %w", err)
	}

	// Generate nonce
	nonce, err := generateRandomString(32)
	if err != nil {
		return "", "", fmt.Errorf("generate nonce: %w", err)
	}

	now := p.now()
	stateEntry := &oidcState{
		Nonce:      nonce,
		CreatedAt:  now,
		ReturnURL:  base.ReturnURL,
		Flow:       base.Flow,
		ClientID:   base.ClientID,
		ClientUUID: base.ClientUUID,
		DeviceName: base.DeviceName,
		DeviceOS:   base.DeviceOS,
		DeviceType: base.DeviceType,
	}
	if stateEntry.Flow == "" {
		stateEntry.Flow = OIDCFlowPanel
	}

	params := url.Values{
		"response_type": {"code"},
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {cfg.RedirectURL},
		"state":         {state},
		"nonce":         {nonce},
	}

	scopes := cfg.Scopes
	if scopes == "" {
		scopes = "openid profile email"
	}
	params.Set("scope", scopes)

	// PKCE
	if cfg.UsePKCE {
		verifier, err := generateRandomString(64)
		if err != nil {
			return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
		}
		stateEntry.CodeVerifier = verifier

		h := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(h[:])
		params.Set("code_challenge", challenge)
		params.Set("code_challenge_method", "S256")
	}

	// Store state
	if !p.stateCap.Add(state, now, now.Add(oidcStateLifetime)) {
		return "", "", ErrOIDCTooManyPending
	}
	if err := p.putJSON(oidcStateKey+state, stateEntry, oidcStateLifetime); err != nil {
		p.stateCap.Remove(state)
		return "", "", fmt.Errorf("store state: %w", err)
	}

	return authEP + "?" + params.Encode(), state, nil
}

// ExchangeCode exchanges an authorization code for tokens and user info.
func (p *OIDCProvider) ExchangeCode(ctx context.Context, code, state string) (*OIDCResult, error) {
	// Validate and consume state: of two callbacks with one state, on any
	// instances, one gets it
	stateEntry := &oidcState{}
	ok := state != "" && p.readJSON(oidcStateKey+state, stateEntry, true)
	p.stateCap.Remove(state)

	if !ok {
		return nil, fmt.Errorf("invalid or expired state parameter")
	}

	returnURL := stateEntry.ReturnURL
	flow := stateEntry.Flow
	if flow == "" {
		flow = OIDCFlowPanel
	}
	clientID := stateEntry.ClientID
	clientUUID := stateEntry.ClientUUID
	oauthState := state

	// Check state age (10 minute max)
	if p.now().Sub(stateEntry.CreatedAt) > 10*time.Minute {
		return nil, fmt.Errorf("state parameter expired")
	}

	tokenEP := p.getTokenEndpoint()
	if tokenEP == "" {
		return nil, fmt.Errorf("token endpoint not configured")
	}

	p.mu.RLock()
	cfg := *p.config
	p.mu.RUnlock()
	policy, err := newOIDCFetchPolicy(cfg.AllowedPrivateCIDRs)
	if err != nil {
		return nil, fmt.Errorf("invalid private network allowlist: %w", err)
	}

	// Token exchange
	params := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {cfg.RedirectURL},
		"client_id":    {cfg.ClientID},
	}
	if cfg.ClientSecret != "" {
		params.Set("client_secret", cfg.ClientSecret)
	}
	if cfg.UsePKCE && stateEntry.CodeVerifier != "" {
		params.Set("code_verifier", stateEntry.CodeVerifier)
	}

	reqBody := strings.NewReader(params.Encode())
	resp, err := fetchValidatedHTTPPostWithPolicy(ctx, p.client, tokenEP, reqBody, policy, http.Header{
		"Content-Type": {"application/x-www-form-urlencoded"},
		"Accept":       {"application/json"},
	})
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcTokenResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in token response")
	}

	// Extract claims from ID token (JWT payload, no signature verification —
	// we trust the TLS connection to the IdP's token endpoint). Its nonce,
	// issuer and audience must be the ones of this sign-in.
	claims := make(map[string]interface{})
	if tokenResp.IDToken != "" {
		parsed, err := parseJWTPayload(tokenResp.IDToken)
		if err != nil {
			return nil, fmt.Errorf("parse ID token: %w", err)
		}
		if err := p.checkIDTokenClaims(parsed, stateEntry.Nonce, cfg); err != nil {
			return nil, err
		}
		claims = parsed
	}

	// Fetch userinfo for additional claims
	userinfoClaims, err := p.fetchUserInfo(ctx, tokenResp.AccessToken)
	if err != nil {
		log.Printf("[OIDC] UserInfo fetch failed (non-fatal): %v", err)
	} else {
		// Merge userinfo claims (userinfo takes precedence)
		for k, v := range userinfoClaims {
			claims[k] = v
		}
	}

	// Map claims to result
	result := &OIDCResult{
		Authenticated: true,
		IDToken:       tokenResp.IDToken,
	}

	claimUsername := cfg.ClaimUsername
	if claimUsername == "" {
		claimUsername = "preferred_username"
	}
	claimEmail := cfg.ClaimEmail
	if claimEmail == "" {
		claimEmail = "email"
	}
	claimName := cfg.ClaimName
	if claimName == "" {
		claimName = "name"
	}
	claimGroups := cfg.ClaimGroups
	if claimGroups == "" {
		claimGroups = "groups"
	}

	result.Username = getStringClaim(claims, claimUsername)
	result.Email = getStringClaim(claims, claimEmail)
	result.DisplayName = getStringClaim(claims, claimName)
	result.Groups = getStringSliceClaim(claims, claimGroups)
	if pic, ok := claims["picture"].(string); ok {
		result.Picture = SanitizeOIDCPicture(pic)
	}

	// Without a name claim the part of the e-mail address before the @ is
	// the user name, but only for an address the provider verified: anyone
	// can put any address into an account at some providers.
	if result.Username == "" && result.Email != "" && emailVerified(claims) {
		if idx := strings.Index(result.Email, "@"); idx > 0 {
			result.Username = result.Email[:idx]
		}
	}

	if result.Username == "" {
		// Last resort: use "sub" claim
		result.Username = getStringClaim(claims, "sub")
	}

	if result.Username == "" {
		return nil, fmt.Errorf("could not determine username from OIDC claims")
	}

	// Map groups to role. The source flag lets provisioning distinguish an
	// explicit group mapping from the configured fallback role.
	result.Role, result.RoleFromGroup = p.resolveRoleWithSource(result.Groups)
	result.ReturnURL = returnURL
	result.Flow = flow
	result.ClientID = clientID
	result.ClientUUID = clientUUID
	result.State = oauthState

	return result, nil
}

// checkIDTokenClaims compares the ID token's nonce with the one sent at the
// start of the sign-in, its audience with the client ID, and its issuer
// with the provider's (the discovery document's, else the configured
// issuer URL; not compared when there is neither). A trailing slash does
// not count, and "{tenantid}" in the issuer, as Azure AD's multi-tenant
// endpoints name it, stands for the token's "tid".
func (p *OIDCProvider) checkIDTokenClaims(claims map[string]interface{}, nonce string, cfg OIDCConfig) error {
	if got, _ := claims["nonce"].(string); nonce == "" || subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return fmt.Errorf("ID token nonce does not match")
	}
	if !audienceHas(claims["aud"], cfg.ClientID) {
		return fmt.Errorf("ID token audience does not name the client")
	}
	p.mu.RLock()
	issuer := cfg.IssuerURL
	if p.discovery != nil && p.discovery.Issuer != "" {
		issuer = p.discovery.Issuer
	}
	p.mu.RUnlock()
	if issuer == "" {
		return nil
	}
	got, _ := claims["iss"].(string)
	if tid, _ := claims["tid"].(string); tid != "" {
		issuer = strings.ReplaceAll(issuer, "{tenantid}", tid)
	}
	if got == "" || strings.TrimRight(got, "/") != strings.TrimRight(issuer, "/") {
		return fmt.Errorf("ID token issuer %q is not %q", got, issuer)
	}
	return nil
}

// audienceHas reports whether an "aud" claim, a text or a list of texts,
// names the client.
func audienceHas(aud interface{}, clientID string) bool {
	if clientID == "" {
		return false
	}
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

// GetReturnURL retrieves the return URL stored with a state parameter.
func (p *OIDCProvider) GetReturnURL(state string) string {
	var entry oidcState
	if state != "" && p.readJSON(oidcStateKey+state, &entry, false) {
		return entry.ReturnURL
	}
	return ""
}

// dropState removes a state this instance stored.
func (p *OIDCProvider) dropState(state string) {
	p.stateCap.Remove(state)
	if err := p.store().Del(context.Background(), oidcStateKey+state); err != nil {
		log.Printf("[OIDC] drop a state: %v", err)
	}
}

// fetchUserInfo calls the UserInfo endpoint with an access token.
func (p *OIDCProvider) fetchUserInfo(ctx context.Context, accessToken string) (map[string]interface{}, error) {
	userinfoEP := p.getUserinfoEndpoint()
	if userinfoEP == "" {
		return nil, fmt.Errorf("userinfo endpoint not available")
	}

	p.mu.RLock()
	cfg := *p.config
	p.mu.RUnlock()
	policy, err := newOIDCFetchPolicy(cfg.AllowedPrivateCIDRs)
	if err != nil {
		return nil, fmt.Errorf("invalid private network allowlist: %w", err)
	}

	resp, err := fetchValidatedHTTPGetWithHeaders(ctx, p.client, userinfoEP, policy, http.Header{
		"Authorization": {"Bearer " + accessToken},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcTokenResponseLimit))
	if err != nil {
		return nil, err
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// resolveRole maps OIDC groups to a NeoDesk role using the configured group-role map.
func (p *OIDCProvider) resolveRole(groups []string) string {
	role, _ := p.resolveRoleWithSource(groups)
	return role
}

// resolveRoleWithSource maps groups to a role and reports whether the group
// map decided it. With a map set it always does: the highest role of the
// person's mapped groups, or the default role for a person in none of them,
// so a person who leaves a group at the provider loses its role at the next
// sign-in. Without a map the default role is only the role of a new
// account, and callers keep the role an administrator gave.
func (p *OIDCProvider) resolveRoleWithSource(groups []string) (string, bool) {
	p.mu.RLock()
	cfg := *p.config
	p.mu.RUnlock()

	entries := parseGroupRoleEntries(cfg.GroupRoleMap)
	if len(entries) == 0 {
		return cfg.DefaultRole, false
	}

	bestRole := cfg.DefaultRole
	if bestRole == "" {
		bestRole = RoleViewer
	}
	bestLevel := roleLevel(bestRole)
	member := make(map[string]bool, len(groups))
	for _, group := range groups {
		member[group] = true
	}
	// In the map's order: of two roles on one level the first entry's
	// wins, whatever order the provider lists the groups in
	for _, e := range entries {
		if lvl := roleLevel(e.role); member[e.group] && lvl > bestLevel {
			bestRole = e.role
			bestLevel = lvl
		}
	}
	return bestRole, true
}

// CompleteClientPending marks a RustDesk client OIDC poll code as authenticated.
func (p *OIDCProvider) CompleteClientPending(code string, userID int64, username, role string) bool {
	if code == "" || username == "" {
		return false
	}
	var entry ClientOIDCPending
	if !p.readJSON(oidcClientKey+code, &entry, false) {
		return false
	}
	age := p.now().Sub(entry.CreatedAt)
	if age > 10*time.Minute {
		p.FailClientPending(code)
		return false
	}
	entry.UserID = userID
	entry.Username = username
	entry.Role = role
	entry.Authed = true
	left := oidcClientLifetime - age
	if left < time.Second {
		left = time.Second
	}
	if err := p.putJSON(oidcClientKey+code, &entry, left); err != nil {
		log.Printf("[OIDC] mark a client sign-in done: %v", err)
		return false
	}
	return true
}

// PeekClientPending returns a copy of the pending client OIDC entry without consuming it.
func (p *OIDCProvider) PeekClientPending(code string) *ClientOIDCPending {
	if code == "" {
		return nil
	}
	var entry ClientOIDCPending
	if !p.readJSON(oidcClientKey+code, &entry, false) {
		return nil
	}
	if p.now().Sub(entry.CreatedAt) > 10*time.Minute {
		return nil
	}
	return &entry
}

// ConsumeClientPending atomically retrieves and deletes an authenticated client
// pending entry: of two polls on any instances, one gets it.
func (p *OIDCProvider) ConsumeClientPending(code string) *ClientOIDCPending {
	if code == "" {
		return nil
	}
	var entry ClientOIDCPending
	ok := p.readJSON(oidcClientKey+code, &entry, true)
	p.clientCap.Remove(code)
	if !ok {
		return nil
	}
	if p.now().Sub(entry.CreatedAt) > 10*time.Minute || !entry.Authed {
		return nil
	}
	return &entry
}

// FailClientPending removes a pending client OIDC entry (e.g. after IdP error).
func (p *OIDCProvider) FailClientPending(code string) {
	if code == "" {
		return
	}
	p.clientCap.Remove(code)
	if err := p.store().Del(context.Background(), oidcClientKey+code); err != nil {
		log.Printf("[OIDC] drop a client sign-in: %v", err)
	}
}

// ClientLoginOptionToken returns the login-options string stock RustDesk expects (oidc/<name>).
func (p *OIDCProvider) ClientLoginOptionToken() string {
	name := strings.TrimSpace(p.GetDisplayName())
	if name == "" {
		name = "oidc"
	}
	// Keep spaces — Flutter uses the substring after "oidc/" as the op name.
	return "oidc/" + name
}

// NormalizePanelBaseURL trims whitespace and trailing slashes from a panel origin URL.
func NormalizePanelBaseURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// IsValidPanelBaseURL validates a panel origin (scheme + host only, http/https).
func IsValidPanelBaseURL(u string) bool {
	u = NormalizePanelBaseURL(u)
	if u == "" {
		return false
	}
	if strings.ContainsAny(u, "\r\n\x00") {
		return false
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return false
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return true
}

// BuildOIDCSessionURL builds the browser redirect target after IdP callback.
// When panelBase is empty, returns a relative path (legacy split-port behavior).
func BuildOIDCSessionURL(panelBase, authCode string) string {
	path := "/api/auth/oidc/session?code=" + url.QueryEscape(authCode)
	base := NormalizePanelBaseURL(panelBase)
	if base == "" || !IsValidPanelBaseURL(base) {
		return path
	}
	return base + path
}

// IsRelativeReturnURL validates that a return URL is a safe relative path.
// It rejects:
//   - empty strings
//   - absolute URLs (http://, https://, etc.)
//   - protocol-relative URLs (//evil.com/...)
//   - URLs containing CR/LF (response splitting)
//   - URLs not starting with a single "/"
func IsRelativeReturnURL(u string) bool {
	if u == "" {
		return false
	}
	if strings.ContainsAny(u, "\r\n\x00") {
		return false
	}
	if !strings.HasPrefix(u, "/") {
		return false
	}
	if strings.HasPrefix(u, "//") || strings.HasPrefix(u, "/\\") {
		return false
	}
	// Reject anything that parses as an absolute URL with a host component.
	if parsed, err := url.Parse(u); err == nil {
		if parsed.Scheme != "" || parsed.Host != "" {
			return false
		}
	}
	return true
}

// StoreAuthCode generates a one-time code that maps to a JWT + verified user
// identity. The panel (Node.js) exchanges the code via a server-to-server
// POST to /api/auth/oidc/exchange. Codes expire after 60 seconds.
// picture is the user's "picture" claim; it is sanitized again here.
func (p *OIDCProvider) StoreAuthCode(token, username, role, returnURL, picture string) (string, error) {
	return p.StoreAuthCodeForState(token, username, role, returnURL, picture, "")
}

// StoreAuthCodeForState is StoreAuthCode for the sign-in of an OAuth
// state, which the exchange then names.
func (p *OIDCProvider) StoreAuthCodeForState(token, username, role, returnURL, picture, state string) (string, error) {
	code, err := generateRandomString(32)
	if err != nil {
		return "", err
	}
	if err := p.putJSON(oidcCodeKey+code, &oidcAuthCode{
		Token:     token,
		Username:  username,
		Role:      role,
		ReturnURL: returnURL,
		Picture:   SanitizeOIDCPicture(picture),
		State:     state,
		CreatedAt: p.now(),
	}, oidcCodeLifetime); err != nil {
		return "", fmt.Errorf("store the code: %w", err)
	}
	return code, nil
}

// ConsumeAuthCode atomically retrieves and deletes a one-time auth code,
// on all instances that share the KV. Returns the stored entry or nil if
// the code is unknown, already used, or older than 60 seconds.
func (p *OIDCProvider) ConsumeAuthCode(code string) *oidcAuthCode {
	if code == "" {
		return nil
	}
	entry := &oidcAuthCode{}
	if !p.readJSON(oidcCodeKey+code, entry, true) {
		return nil
	}
	if p.now().Sub(entry.CreatedAt) > oidcCodeLifetime {
		return nil
	}
	return entry
}

// roleLevel returns a numeric priority for role comparison (highest privilege wins).
func roleLevel(role string) int {
	switch strings.ToLower(role) {
	case "super_admin", "admin":
		return 5
	case "server_admin":
		return 4
	case "global_admin":
		return 4
	case "operator":
		return 2
	case "viewer":
		return 1
	case "pro":
		return 0
	default:
		return 0
	}
}

// parseJWTPayload extracts the payload from a JWT without verifying the signature.
// Used only for tokens received directly from the IdP's token endpoint over TLS.
func parseJWTPayload(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}

	// Add padding if needed
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// Try without padding (raw URL encoding)
		decoded, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decode JWT payload: %w", err)
		}
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, fmt.Errorf("parse JWT claims: %w", err)
	}
	return claims, nil
}

// getStringClaim extracts a string value from a claims map.
func getStringClaim(claims map[string]interface{}, key string) string {
	v, ok := claims[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// emailVerified reports whether the claims say the e-mail address is
// verified: email_verified is true, or the text "true" as some providers
// send it.
func emailVerified(claims map[string]interface{}) bool {
	switch v := claims["email_verified"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// getStringSliceClaim extracts a string slice from a claims map.
func getStringSliceClaim(claims map[string]interface{}, key string) []string {
	v, ok := claims[key]
	if !ok {
		return nil
	}
	switch val := v.(type) {
	case []interface{}:
		result := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return val
	case string:
		// Some IdPs return groups as a space/comma-separated string
		return strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == ' ' })
	default:
		return nil
	}
}

// generateRandomString creates a cryptographically secure random string.
func generateRandomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// OIDCDiscoveryResult holds the result of a discovery document test.
type OIDCDiscoveryResult struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// TestOIDCDiscovery fetches and validates an OIDC discovery document using the
// default-deny private-network policy.
func TestOIDCDiscovery(ctx context.Context, issuerURL string) (*OIDCDiscoveryResult, error) {
	return TestOIDCDiscoveryWithPrivateCIDRs(ctx, issuerURL, "")
}

// TestOIDCDiscoveryWithPrivateCIDRs is the operator-facing discovery test with
// the same explicit private-network allowlist used by the live provider.
func TestOIDCDiscoveryWithPrivateCIDRs(ctx context.Context, issuerURL, allowedPrivateCIDRs string) (*OIDCDiscoveryResult, error) {
	policy, err := newOIDCFetchPolicy(allowedPrivateCIDRs)
	if err != nil {
		return nil, fmt.Errorf("private network allowlist: %w", err)
	}
	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := fetchValidatedHTTPGetWithPolicy(ctx, client, discoveryURL, policy)
	if err != nil {
		return nil, fmt.Errorf("discovery URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery endpoint returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var disc OIDCDiscoveryResult
	if err := json.Unmarshal(body, &disc); err != nil {
		return nil, fmt.Errorf("parse discovery document: %w", err)
	}

	if disc.AuthorizationEndpoint == "" {
		return nil, fmt.Errorf("discovery document missing authorization_endpoint")
	}
	if disc.TokenEndpoint == "" {
		return nil, fmt.Errorf("discovery document missing token_endpoint")
	}

	return &disc, nil
}
