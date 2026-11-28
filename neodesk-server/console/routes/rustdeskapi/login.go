package rustdeskapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/auditlog"
	"github.com/neodeskapp/neodesk/neodesk-server/console/authn"
	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
	"github.com/neodeskapp/neodesk/neodesk-server/console/jsbind"
	"github.com/neodeskapp/neodesk/neodesk-server/console/kit"
	"github.com/neodeskapp/neodesk/neodesk-server/console/middleware"
)

// tokenAnswer is a finished sign-in.
type tokenAnswer struct {
	Type        string      `json:"type"`
	AccessToken string      `json:"access_token"`
	User        userPayload `json:"user"`
}

// secondStepAnswer asks the client for its second factor (RustDesk 1.4.7
// and later).
type secondStepAnswer struct {
	Type    string `json:"type"`
	TFAType string `json:"tfa_type"`
	Secret  string `json:"secret"`
}

// blockedAnswer is a sign-in the lockout refused. retry_after is null when
// the end of the lockout could not be read.
type blockedAnswer struct {
	Error      string `json:"error"`
	RetryAfter *int64 `json:"retry_after"`
}

type collisionAnswer struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// clientText is `${value || 'unknown'}` of an audit entry.
func clientText(values ...any) (string, error) {
	for _, v := range values {
		if truthy(v) {
			return str(v)
		}
	}
	return "unknown", nil
}

// clientRef is the member of a sign-in body that names the client (id,
// uuid). It counts only as text: a member of another type is missing.
func clientRef(body middleware.JSONObject, name string) (text string, given bool) {
	text, given = member(body, name).(string)
	return text, given
}

// clientDevice is the id of a sign-in body, the device the token is
// stored for: text of the form of a device id (authn.IsDeviceID, 3 to 64
// letters, digits, "_" and "-"). Anything else counts as missing, so no
// text of any length or form ends up as a token's client id.
func clientDevice(body middleware.JSONObject) string {
	if id, _ := clientRef(body, "id"); authn.IsDeviceID(id) {
		return id
	}
	return ""
}

// firstText is a || b || ” for texts.
func firstText(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// orUnknown is `${value || 'unknown'}` of an audit entry, for texts.
func orUnknown(values ...string) string {
	if text := firstText(values...); text != "" {
		return text
	}
	return "unknown"
}

// login answers POST /api/login: the password step, or with a code and a
// secret the second step.
func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := kit.ClientIP(r)
	body := kit.Body(r)
	username, _ := member(body, "username").(string)
	username = jsTrim(username)
	password, _ := member(body, "password").(string)
	h.log.Info(fmt.Sprintf("[API:LOGIN] Login attempt from %s user=%s", ip, auditlog.RedactUsername(username)))

	clientID := clientDevice(body)
	clientUUID, uuidGiven := clientRef(body, "uuid")
	code := or(member(body, "tfaCode"), member(body, "verificationCode"))
	if truthy(code) && truthy(member(body, "secret")) {
		h.secondStep(w, r, ip, body)
		return
	}

	if username == "" || password == "" {
		h.log.Info(fmt.Sprintf("[API:LOGIN] Missing credentials - username empty: %t, password empty: %t, IP: %s", username == "", password == "", ip))
		fail(w, r, http.StatusBadRequest, "Missing credentials")
		return
	}

	serverError := func(err error) {
		h.log.Error("RustDesk API login error:", err)
		fail(w, r, http.StatusInternalServerError, msgServerError)
	}

	brute, err := h.d.Authn.CheckBruteForce(ctx, username, ip)
	if err != nil {
		serverError(err)
		return
	}
	if brute.Blocked {
		if err := h.refuseBlocked(w, r, username, ip, brute); err != nil {
			serverError(err)
		}
		return
	}

	outcome, err := h.d.Authn.Authenticate(authn.WithClientAddress(ctx, ip), username, password)
	if err != nil {
		serverError(err)
		return
	}
	if outcome.IsFailure() && outcome.Failure == authn.FailureUsernameCollision {
		_ = h.d.Authn.RecordAttempt(ctx, username, ip, false)
		if err := h.audit(ctx, 0, "api_login_failed", "Username collision: "+username, ip); err != nil {
			serverError(err)
			return
		}
		kit.JSON(w, r, http.StatusConflict, collisionAnswer{Error: "Username collision: a local account with this name already exists", Code: "username_collision"})
		return
	}
	if outcome.Failure == authn.FailureDirectoryTimeout {
		if err := h.audit(ctx, 0, "api_login_failed", "Directory timeout: "+username, ip); err != nil {
			serverError(err)
			return
		}
		kit.JSON(w, r, http.StatusGatewayTimeout, collisionAnswer{Error: "The directory server took too long to answer", Code: authn.FailureDirectoryTimeout})
		return
	}
	if outcome.Failure == authn.FailureAccountDisabled {
		_ = h.d.Authn.RecordAttempt(ctx, username, ip, false)
		if err := h.audit(ctx, 0, "api_login_failed", "Account disabled: "+username, ip); err != nil {
			serverError(err)
			return
		}
		kit.JSON(w, r, http.StatusForbidden, collisionAnswer{Error: "This account is disabled", Code: authn.FailureAccountDisabled})
		return
	}
	user := outcome.User
	if user == nil {
		_ = h.d.Authn.RecordAttempt(ctx, username, ip, false)
		if err := h.audit(ctx, 0, "api_login_failed", "User: "+username, ip); err != nil {
			serverError(err)
			return
		}
		fail(w, r, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	// A banned device may not sign in (the client id is the device id).
	// It is asked only after the password, so the answer tells no one
	// without the password whether a device is banned.
	if banned, err := h.refuseBannedDevice(w, r, clientID, username, ip); err != nil || banned {
		if err != nil {
			serverError(err)
		}
		return
	}

	if user.TOTPSetupRequired {
		_ = h.d.Authn.RecordAttempt(ctx, username, ip, false)
		kit.JSON(w, r, http.StatusForbidden, collisionAnswer{Error: authn.TOTPSetupDueMessage, Code: authn.TOTPSetupDueCode})
		return
	}
	// The new password an admin asked for is chosen in the web console
	if user.PasswordChangeRequired {
		_ = h.d.Authn.RecordAttempt(ctx, username, ip, false)
		kit.JSON(w, r, http.StatusForbidden, collisionAnswer{Error: authn.PasswordChangeDueMessage, Code: authn.PasswordChangeDueCode})
		return
	}
	userID := user.ID.V
	if user.TOTPRequired && !h.totpBypass {
		secret, err := randomSecret()
		if err != nil {
			serverError(err)
			return
		}
		if err := h.tfa.add(secret, tfaSession{
			userID: userID, username: user.Username, role: user.Role.String,
			clientID: clientID, clientUUID: clientUUID, createdAt: h.now(),
		}); err != nil {
			serverError(err)
			return
		}
		if err := h.audit(ctx, userID, "api_login_tfa_required", "Client: "+orUnknown(clientID), ip); err != nil {
			serverError(err)
			return
		}
		kit.JSON(w, r, http.StatusOK, secondStepAnswer{Type: "email_check", TFAType: "tfa_check", Secret: secret})
		return
	}

	// No second factor, or the operator switched it off for this API
	// (RUSTDESK_API_DISABLE_TOTP with its acknowledgement)
	_ = h.d.Authn.RecordAttempt(ctx, username, ip, true)
	token, err := h.issueToken(ctx, userID, clientID, clientUUID, uuidGiven, ip)
	if err == nil {
		err = h.d.Authn.TouchLastLogin(ctx, userID)
	}
	if err != nil {
		serverError(err)
		return
	}
	action, details := "api_login_success", orUnknown(clientID)
	if user.TOTPRequired {
		action, details = "api_login_success_totp_bypassed", details+" (RUSTDESK_API_DISABLE_TOTP=true)"
	}
	if err := h.audit(ctx, userID, action, "Client: "+details, ip); err != nil {
		serverError(err)
		return
	}
	if user.TOTPRequired {
		h.log.Warn(fmt.Sprintf("[API:LOGIN] TOTP bypass active for user '%s' (RUSTDESK_API_DISABLE_TOTP=true)", username))
	}
	kit.JSON(w, r, http.StatusOK, tokenAnswer{Type: "access_token", AccessToken: token, User: buildUserPayload(user.Username, user.Role.String)})
}

// refuseBannedDevice answers 403 "Device is banned" with an audit entry
// when the client id names a banned device; banned reports whether it
// answered. An error is the lookup's or the audit entry's, and nothing is
// answered then.
func (h *handlers) refuseBannedDevice(w http.ResponseWriter, r *http.Request, clientID, username, ip string) (banned bool, err error) {
	device := sanitize(clientID, maxIDLen)
	if device == "" || !isValidDeviceID(device) {
		return false, nil
	}
	row, err := h.store.peerByID(r.Context(), device)
	if err != nil || row == nil || !row.banned {
		return false, err
	}
	h.log.Info("[API:LOGIN] Rejected login from banned device: " + device)
	if err := h.audit(r.Context(), 0, "api_login_banned_device", fmt.Sprintf("Device: %s, User: %s", device, username), ip); err != nil {
		return false, err
	}
	fail(w, r, http.StatusForbidden, "Device is banned")
	return true, nil
}

// refuseBlocked answers a sign-in step the lockout refused: an audit
// entry, then 429 with the seconds to wait. An error is the audit entry's,
// and nothing is answered then.
func (h *handlers) refuseBlocked(w http.ResponseWriter, r *http.Request, username, ip string, brute authn.BruteForce) error {
	if err := h.audit(r.Context(), 0, "api_login_blocked", fmt.Sprintf("User: %s, IP: %s, Reason: %s", username, ip, brute.Reason), ip); err != nil {
		return err
	}
	answer := blockedAnswer{Error: brute.Reason}
	if !brute.RetryUnknown {
		answer.RetryAfter = &brute.RetryAfter
	}
	kit.JSON(w, r, http.StatusTooManyRequests, answer)
	return nil
}

// secondStep is handleTfaVerification: the code of the account's second
// factor for a pending secret. The lockout of the account and of the
// address is checked before the code, as in the password step, so the
// wrong codes that locked the account also end the tries of a secret
// handed out before. A wrong code counts as a failed sign-in and keeps
// the secret until it expires, so the client can ask for the code again;
// the right code uses the secret up.
func (h *handlers) secondStep(w http.ResponseWriter, r *http.Request, ip string, body middleware.JSONObject) {
	ctx := r.Context()
	serverError := func(err error) {
		h.log.Error("RustDesk API TFA error:", err)
		fail(w, r, http.StatusInternalServerError, msgServerError)
	}
	secret := member(body, "secret")
	pending, ok, err := h.tfa.get(secret, h.now())
	if err != nil {
		serverError(err)
		return
	}
	if !ok {
		fail(w, r, http.StatusUnauthorized, "TFA session expired or invalid")
		return
	}
	lockout, err := h.d.Authn.CheckBruteForce(ctx, pending.username, ip)
	if err != nil {
		serverError(err)
		return
	}
	if lockout.Blocked {
		if err := h.refuseBlocked(w, r, pending.username, ip, lockout); err != nil {
			serverError(err)
		}
		return
	}
	code, _ := or(member(body, "tfaCode"), member(body, "verificationCode")).(string)
	verified, err := h.d.Authn.VerifyTOTPCode(ctx, pending.userID, code)
	if err != nil {
		serverError(err)
		return
	}
	if !verified {
		_ = h.d.Authn.RecordAttempt(ctx, pending.username, ip, false)
		if err := h.audit(ctx, pending.userID, "api_tfa_failed", "Client: "+orUnknown(pending.clientID), ip); err != nil {
			serverError(err)
			return
		}
		fail(w, r, http.StatusUnauthorized, "Invalid verification code")
		return
	}
	// The secret is used up here, once on all instances: a second
	// request with it that got this far is refused
	if used, err := h.tfa.remove(secret.(string)); err != nil {
		serverError(err)
		return
	} else if !used {
		fail(w, r, http.StatusUnauthorized, "TFA session expired or invalid")
		return
	}

	_ = h.d.Authn.RecordAttempt(ctx, pending.username, ip, true)
	// The client id and UUID of this step, else those of the first step;
	// the tokens of that UUID are revoked, also of the empty one
	bodyID := clientDevice(body)
	bodyUUID, _ := clientRef(body, "uuid")
	clientID, clientUUID := firstText(bodyID, pending.clientID), firstText(bodyUUID, pending.clientUUID)
	if banned, err := h.refuseBannedDevice(w, r, clientID, pending.username, ip); err != nil || banned {
		if err != nil {
			serverError(err)
		}
		return
	}
	token, err := h.issueToken(ctx, pending.userID, clientID, clientUUID, true, ip)
	if err == nil {
		err = h.d.Authn.TouchLastLogin(ctx, pending.userID)
	}
	if err != nil {
		serverError(err)
		return
	}
	if err := h.audit(ctx, pending.userID, "api_login_success", "Client: "+orUnknown(clientID)+" (2FA: totp)", ip); err != nil {
		serverError(err)
		return
	}
	kit.JSON(w, r, http.StatusOK, tokenAnswer{Type: "access_token", AccessToken: token, User: buildUserPayload(pending.username, pending.role)})
}

// randomSecret is crypto.randomBytes(16).toString('hex').
func randomSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// issueToken is authService.generateAccessToken for a client id and UUID
// given as text: with revoke, the account's tokens of the UUID are
// revoked first, then the token is stored for the client.
func (h *handlers) issueToken(ctx context.Context, userID int64, clientID, clientUUID string, revoke bool, ip string) (string, error) {
	if revoke {
		if err := h.d.Authn.RevokeClientUUIDTokens(ctx, userID, clientUUID); err != nil {
			return "", err
		}
	}
	return h.d.Authn.IssueAccessToken(ctx, userID, clientID, clientUUID, ip)
}

// audit writes an audit entry; userID 0 is no account.
func (h *handlers) audit(ctx context.Context, userID int64, action, details, ip string) error {
	return h.d.Audit.Write(ctx, userID, action, details, ip)
}

// logout answers POST /api/logout: it revokes the tokens of the client
// and always answers {}.
func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := kit.ClientIP(r)
	defer kit.JSON(w, r, http.StatusOK, struct{}{})
	token := bearerToken(r)
	if token == "" {
		return
	}
	user, err := h.d.Authn.ValidateAccessToken(ctx, token)
	if err != nil || user == nil {
		if err != nil {
			h.log.Error("RustDesk API logout error:", err)
		}
		return
	}
	body := kit.Body(r)
	clientID, clientUUID := body.Get("id"), body.Get("uuid")
	if clientID.Truthy() && clientUUID.Truthy() {
		args, err := jsbind.Args(h.d.Store.Dialect(), 2, jsbind.Number(float64(user.ID.V)), jsbind.Of(clientUUID))
		var text string
		if err == nil {
			text, err = h.store.boundText(ctx, args[1])
		}
		if err == nil {
			err = h.d.Authn.RevokeClientUUIDTokens(ctx, user.ID.V, text)
		}
		if err != nil {
			h.log.Error("RustDesk API logout error:", err)
			return
		}
	} else if err := h.d.Authn.RevokeClientTokens(ctx, user.ID.V, "", ""); err != nil {
		h.log.Error("RustDesk API logout error:", err)
		return
	}
	details, err := clientText(valueOf(clientID))
	if err == nil {
		err = h.audit(ctx, user.ID.V, "api_logout", "Client: "+details, ip)
	}
	if err != nil {
		h.log.Error("RustDesk API logout error:", err)
	}
}

// currentUserAnswer is the account behind a token.
type currentUserAnswer struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Note    string `json:"note"`
	Status  int    `json:"status"`
	IsAdmin bool   `json:"is_admin"`
}

// currentUser answers /api/currentUser for every method. is_admin is one
// of the four admin roles, as at sign-in.
func (h *handlers) currentUser(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		fail(w, r, http.StatusUnauthorized, msgAuthRequired)
		return
	}
	user, err := h.d.Authn.ValidateAccessToken(r.Context(), token)
	if err != nil {
		h.log.Error("RustDesk API currentUser error:", err)
		fail(w, r, http.StatusInternalServerError, msgServerError)
		return
	}
	if user == nil {
		fail(w, r, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	kit.JSON(w, r, http.StatusOK, currentUserAnswer{
		Name: user.Username, Status: 1, IsAdmin: adminRoles[user.Role.String],
	})
}

// loginOptions answers GET /api/login-options with the sign-in methods of
// the API server, or [""] (password only) when it does not answer with a
// list within five seconds.
func (h *handlers) loginOptions(w http.ResponseWriter, r *http.Request) {
	resp, err := h.d.GoAPI.Get(r.Context(), "/login-options", goapi.Timeout(5*time.Second))
	if err != nil {
		h.log.Warn("[API] login-options proxy failed:", err.Error())
	} else if list, ok := resp.Data().([]any); ok {
		kit.JSON(w, r, http.StatusOK, list)
		return
	}
	kit.JSON(w, r, http.StatusOK, []string{""})
}
