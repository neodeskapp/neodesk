package api

// Join and support codes: pairing codes with a kind, made, listed, revoked
// and redeemed by the console in the same process.
//
//   - A join code admits a computer for good: its redeem makes the device
//     approved, whatever the enrollment mode, with the code's name. The
//     console then puts it into the code's folder and issues its device
//     token.
//   - A support code opens one quick support session. Its redeem spends the
//     code; the console keeps the session.
//
// The codes live where pairing codes live (server_config, under
// pairing_code_<sha256 of the code>), with their kind, so the pairing
// routes neither list nor redeem them. A code has 8 symbols of the
// pairing alphabet (about 39 bits). The routes below answer only callers
// in the same process (InternalHandler); on the listener they answer 404.
// Every code that cannot be used answers the same 404, so nobody can tell
// a wrong code from an expired, used or revoked one.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/neodeskapp/neodesk/neodesk-server/audit"
	"github.com/neodeskapp/neodesk/neodesk-server/security"
)

const (
	onboardKindJoin    = "join"
	onboardKindSupport = "support"
	onboardCodeLen     = 8
	// onboardMaxLive is how many join and support codes can be open at once.
	onboardMaxLive = 500
	onboardMinTTL  = 5 * time.Minute
	onboardMaxTTL  = 30 * 24 * time.Hour
	onboardMaxUses = 1000
	onboardNameMax = 64
	onboardMailMax = 254
	// onboardDeviceType marks a quick support session's temporary device.
	onboardDeviceType = "quick_support"
)

// errOnboardRefused is a device the enrollment rules keep out.
var errOnboardRefused = errors.New("device refused")

// refusedJoin is a device the enrollment rules keep out (errOnboardRefused).
// For a join under a known ID refused for its proof it names why
// (unproved), and audit says an audit entry is due for it now
// (auditJoinUnproved), standing for repeats refusals since the last one.
type refusedJoin struct {
	reason string
	// code is the machine-readable reason (refusal codes below)
	code     string
	unproved string
	audit    bool
	repeats  int
}

func (e *refusedJoin) Error() string { return errOnboardRefused.Error() + ": " + e.reason }
func (e *refusedJoin) Unwrap() error { return errOnboardRefused }

// refusalAnswer is the 403 answer of a refused device: for a refused join
// proof also why, and whether the console writes the audit entry now (its
// audit list is the one admins read, L522).
func refusalAnswer(err error) map[string]any {
	answer := map[string]any{
		"success": false, "error": "device_refused", "message": strings.TrimPrefix(err.Error(), errOnboardRefused.Error()+": "),
	}
	var refused *refusedJoin
	if errors.As(err, &refused) && refused.code != "" {
		answer["reason"] = refused.code
	}
	if errors.As(err, &refused) && refused.unproved != "" {
		answer["unproved"] = refused.unproved
		answer["audit"] = refused.audit
		answer["repeats"] = refused.repeats
	}
	return answer
}

// joinProofRefusal is the refusal of a join for an enrolled ID that does
// not prove the ID's key: an older client, or someone else using the ID.
// The client shows it after "did not accept this computer:".
const joinProofRefusal = "This computer's ID is already enrolled on this server. Update NeoDesk on this computer and open the invite again, or ask your administrator to delete the device in the console."

// revokedRefusal and blockedRefusal refuse an ID on the blocklist: one a
// device revoke put there, and one blocked for another reason.
const (
	revokedRefusal = "This computer's ID was revoked on this server. An administrator can unblock it in the console."
	blockedRefusal = "This computer's ID is blocked on this server. An administrator can unblock it in the console."
)

// refusalCode is the machine-readable reason of a refusal, which the
// console and the desktop client turn into their own words
// (console/reasons): device_revoked, device_blocked, device_banned,
// device_removed, device_rejected, other_machine, key_changed and
// join_proof.
func refusalCode(refusal string) string {
	switch refusal {
	case revokedRefusal:
		return "device_revoked"
	case blockedRefusal:
		return "device_blocked"
	case "Device is banned":
		return "device_banned"
	case "Device has been removed":
		return "device_removed"
	case "Device enrollment was rejected":
		return "device_rejected"
	case "Device ID is already registered to a different machine":
		return "other_machine"
	case keyChangedRefusal:
		return "key_changed"
	case joinProofRefusal:
		return "join_proof"
	}
	return ""
}

// onboardRoutes adds the routes of join and support codes.
func (s *Server) onboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/onboard/codes", s.inProcessOnly(s.handleOnboardCreate))
	mux.HandleFunc("GET /api/onboard/codes", s.inProcessOnly(s.handleOnboardList))
	mux.HandleFunc("GET /api/onboard/codes/{id}", s.inProcessOnly(s.handleOnboardCode))
	mux.HandleFunc("DELETE /api/onboard/codes/{id}", s.inProcessOnly(s.handleOnboardRevoke))
	mux.HandleFunc("POST /api/onboard/codes/lookup", s.inProcessOnly(s.handleOnboardLookup))
	mux.HandleFunc("POST /api/onboard/codes/redeem", s.inProcessOnly(s.handleOnboardRedeem))
	mux.HandleFunc("POST /api/onboard/codes/prune", s.inProcessOnly(s.handleOnboardPrune))
	mux.HandleFunc("POST /api/onboard/devices", s.inProcessOnly(s.handleOnboardAdmit))
	mux.HandleFunc("DELETE /api/onboard/devices/{id}", s.inProcessOnly(s.handleOnboardRemoveDevice))
	mux.HandleFunc("GET /api/onboard/devices/{id}/key-request", s.inProcessOnly(s.handleDeviceKeyRequest))
	mux.HandleFunc("POST /api/onboard/devices/{id}/key-request/approve", s.inProcessOnly(s.handleApproveDeviceKey))
	mux.HandleFunc("POST /api/onboard/admissions", s.inProcessOnly(s.handleReceiverAdmissionCreate))
	s.onboardSessionRoutes(mux)
	s.onboardFeedbackRoutes(mux)
}

// inProcessOnly answers 404 to a request that did not come through
// InternalHandler.
func (s *Server) inProcessOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !arrivedInternally(r) {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// onboardCode is a join or support code as the console sees it, without
// the code itself.
type onboardCode struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	FolderID    int64  `json:"folder_id"`
	Email       string `json:"email"`
	CreatedBy   string `json:"created_by"`
	CreatedByID int64  `json:"created_by_id"`
	// CreatedByToken is the console API token the code was made with, ""
	// for none.
	CreatedByToken string `json:"created_by_token,omitempty"`
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at"`
	MaxUses        int    `json:"max_uses"`
	UsesLeft       int    `json:"uses_left"`
}

func onboardView(r *pairingRecord) onboardCode {
	return onboardCode{
		ID: r.ID, Kind: r.Kind, Name: r.Name, FolderID: r.FolderID, Email: r.Email,
		CreatedBy: r.CreatedBy, CreatedByID: r.CreatedByID, CreatedByToken: r.CreatedByToken,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, MaxUses: r.MaxUses, UsesLeft: r.UsesLeft,
	}
}

func validOnboardKind(kind string) bool {
	return kind == onboardKindJoin || kind == onboardKindSupport
}

// onboardFail answers {success:false, error}.
func onboardFail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"success": false, "error": code})
}

// onboardUnusable is the one answer for a code that is unknown, expired,
// used up, revoked or of another kind.
func onboardUnusable(w http.ResponseWriter) {
	onboardFail(w, http.StatusNotFound, "invalid_or_expired")
}

// validOnboardCodeFormat reports whether code (separators and case
// ignored) has the form of a join or support code.
func validOnboardCodeFormat(code string) bool {
	normalized := normalizePairingCode(code)
	if len(normalized) != onboardCodeLen {
		return false
	}
	for _, r := range normalized {
		if !strings.ContainsRune(pairingAlphabet, r) {
			return false
		}
	}
	return true
}

// liveOnboardRecord is the record of a usable join or support code, nil
// for any code that is not one. An expired record is deleted.
func (s *Server) liveOnboardRecord(code string) (string, *pairingRecord) {
	if !validOnboardCodeFormat(code) {
		return "", nil
	}
	hash := pairingCodeHash(code)
	record := s.loadPairingRecord(hash)
	if record == nil || !validOnboardKind(record.Kind) {
		return "", nil
	}
	expires, err := time.Parse(time.RFC3339, record.ExpiresAt)
	if err != nil || !s.now().UTC().Before(expires) {
		s.deletePairingRecord(hash)
		return "", nil
	}
	if record.UsesLeft <= 0 {
		return "", nil
	}
	return hash, record
}

// liveOnboardCount counts the open join and support codes.
func (s *Server) liveOnboardCount() int {
	n := 0
	for _, entry := range s.pairingRecords() {
		if validOnboardKind(entry.Record.Kind) {
			n++
		}
	}
	return n
}

// handleOnboardCreate makes a code.
// POST /api/onboard/codes {kind, name, folder_id, email, ttl_seconds,
// max_uses, created_by_id}
func (s *Server) handleOnboardCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		FolderID    int64  `json:"folder_id"`
		Email       string `json:"email"`
		TTLSeconds  int64  `json:"ttl_seconds"`
		MaxUses     int    `json:"max_uses"`
		CreatedByID int64  `json:"created_by_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		onboardFail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	switch {
	case !validOnboardKind(body.Kind):
		onboardFail(w, http.StatusBadRequest, "invalid_kind")
		return
	case ttl < onboardMinTTL || ttl > onboardMaxTTL:
		onboardFail(w, http.StatusBadRequest, "invalid_lifetime")
		return
	case body.MaxUses < 1 || body.MaxUses > onboardMaxUses:
		onboardFail(w, http.StatusBadRequest, "invalid_uses")
		return
	case body.FolderID < 0:
		onboardFail(w, http.StatusBadRequest, "invalid_folder")
		return
	case utf8.RuneCountInString(body.Email) > onboardMailMax:
		onboardFail(w, http.StatusBadRequest, "invalid_email")
		return
	}

	s.onboardMu.Lock()
	defer s.onboardMu.Unlock()
	s.prunePairingRecords()
	if s.liveOnboardCount() >= onboardMaxLive {
		onboardFail(w, http.StatusConflict, "too_many_codes")
		return
	}
	var code, hash string
	for attempt := 0; attempt < 5; attempt++ {
		generated, err := generateCodeOfLength(onboardCodeLen)
		if err != nil {
			onboardFail(w, http.StatusInternalServerError, "code_generation_failed")
			return
		}
		if candidate := pairingCodeHash(generated); s.loadPairingRecord(candidate) == nil {
			code, hash = generated, candidate
			break
		}
	}
	if code == "" {
		onboardFail(w, http.StatusInternalServerError, "code_generation_failed")
		return
	}
	now := s.now().UTC()
	record := &pairingRecord{
		ID:          hash[:16],
		Kind:        body.Kind,
		Name:        clipBrandingField(strings.TrimSpace(body.Name), onboardNameMax),
		FolderID:    body.FolderID,
		Email:       strings.TrimSpace(body.Email),
		CreatedAt:   now.Format(time.RFC3339),
		ExpiresAt:   now.Add(ttl).Format(time.RFC3339),
		CreatedBy:   actorFromCtx(r),
		CreatedByID: body.CreatedByID,
		MaxUses:     body.MaxUses,
		UsesLeft:    body.MaxUses,

		CreatedByToken: tokenOf(r),
	}
	if err := s.storePairingRecord(hash, record); err != nil {
		onboardFail(w, http.StatusInternalServerError, "store_failed")
		return
	}
	if s.auditLog != nil {
		// The code itself is never logged.
		s.auditLog.Log("onboard_code_created", s.remoteIP(r), record.CreatedBy, map[string]string{
			"id": record.ID, "kind": record.Kind,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"success": true, "code": code, "record": onboardView(record)})
}

// handleOnboardList lists the open codes of a kind, newest first, after
// deleting expired and used ones.
// GET /api/onboard/codes?kind=join|support
func (s *Server) handleOnboardList(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if !validOnboardKind(kind) {
		onboardFail(w, http.StatusBadRequest, "invalid_kind")
		return
	}
	s.onboardMu.Lock()
	s.prunePairingRecords()
	entries := s.pairingRecords()
	s.onboardMu.Unlock()
	codes := make([]onboardCode, 0)
	for _, entry := range entries {
		if entry.Record.Kind == kind {
			codes = append(codes, onboardView(&entry.Record))
		}
	}
	sort.SliceStable(codes, func(i, j int) bool { return codes[i].CreatedAt > codes[j].CreatedAt })
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "codes": codes})
}

// handleOnboardRevoke deletes an open code and answers its record.
// DELETE /api/onboard/codes/{id}
func (s *Server) handleOnboardRevoke(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	s.onboardMu.Lock()
	defer s.onboardMu.Unlock()
	for _, entry := range s.pairingRecords() {
		if entry.Record.ID == id && validOnboardKind(entry.Record.Kind) {
			s.deletePairingRecord(entry.Hash)
			_ = s.db.DeleteConfig(onboardJoinsPrefix + id)
			if s.auditLog != nil {
				s.auditLog.Log("onboard_code_revoked", s.remoteIP(r), actorFromCtx(r), map[string]string{
					"id": id, "kind": entry.Record.Kind,
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "record": onboardView(&entry.Record)})
			return
		}
	}
	onboardFail(w, http.StatusNotFound, "not_found")
}

// handleOnboardLookup answers the record of a usable code without
// spending it.
// POST /api/onboard/codes/lookup {code}
func (s *Server) handleOnboardLookup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		onboardFail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	s.onboardMu.Lock()
	_, record := s.liveOnboardRecord(body.Code)
	s.onboardMu.Unlock()
	if record == nil {
		onboardUnusable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "record": onboardView(record)})
}

// onboardDevice is a device the console admits: the client's own fields
// and the name the console gives it.
type onboardDevice struct {
	DeviceID    string `json:"device_id"`
	UUID        string `json:"uuid"`
	Hostname    string `json:"hostname"`
	Platform    string `json:"platform"`
	Version     string `json:"version"`
	PublicKey   string `json:"public_key"`
	DisplayName string `json:"display_name"`
	// Temporary marks a quick support session's device, which the console
	// removes after the session.
	Temporary bool `json:"temporary"`
	// Proof is the key proof the client sent with its join (the headers
	// X-ND-Enrollment-* of POST /api/onboard/join, handed on by the
	// console), nil without one.
	Proof *joinProof `json:"proof,omitempty"`
}

// joinProof is a client's key proof of a join: the nd-enrollment-v1
// signature (enrollment_proof.go) over POST, the path the client sent to,
// its device ID and public key, the timestamp and the nonce.
type joinProof struct {
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

// joinPath is the console's join route, the only path a join proof is
// made for.
const joinPath = "/api/onboard/join"

// Why a join under a known ID was refused, as the audit names it.
var (
	errNoJoinProof       = errors.New("no key proof")
	errJoinProofReplayed = errors.New("the proof was used before")
)

// verifyJoinProof checks a join's key proof against the key the server
// bound to the device ID: the route, the time (five minutes either side),
// the signature and that the nonce was not used before.
func (s *Server) verifyJoinProof(deviceID string, bound []byte, p *joinProof) error {
	if p == nil {
		return errNoJoinProof
	}
	timestamp, nonce := strings.TrimSpace(p.Timestamp), strings.TrimSpace(p.Nonce)
	if !strings.HasSuffix(p.Path, joinPath) {
		return errors.New("the proof is not for the join route")
	}
	if timestamp == "" || nonce == "" || len(nonce) > enrollmentProofNonceMax {
		return errors.New("the proof has no usable timestamp or nonce")
	}
	signedAt, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return fmt.Errorf("the proof's timestamp: %w", err)
	}
	now := s.now().UTC()
	if delta := now.Sub(signedAt.UTC()); delta > enrollmentProofClockSkew || delta < -enrollmentProofClockSkew {
		return errors.New("the proof's timestamp is outside the allowed skew")
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Signature))
	if err != nil || len(signature) != ed25519.SignatureSize || len(bound) != ed25519.PublicKeySize {
		return errors.New("the proof's signature cannot be read")
	}
	payload := enrollmentProofPayload(http.MethodPost, p.Path, deviceID, base64.StdEncoding.EncodeToString(bound), timestamp, nonce)
	if !ed25519.Verify(ed25519.PublicKey(bound), payload, signature) {
		return errors.New("the proof's signature is not the device's")
	}
	if !s.enrollmentNonceKeys().use(deviceID+":"+nonce, now) {
		return errJoinProofReplayed
	}
	return nil
}

func (d *onboardDevice) enrollment() *EnrollmentRequest {
	req := &EnrollmentRequest{
		DeviceID: strings.TrimSpace(d.DeviceID), UUID: d.UUID, Hostname: d.Hostname,
		Platform: d.Platform, Version: d.Version, PublicKey: d.PublicKey,
	}
	if d.Temporary {
		req.DeviceType = onboardDeviceType
		req.Tags = "quick-support"
	}
	return req
}

// admissionRefusal answers why the enrollment rules keep a device out, ""
// when they let it in. A device the server knows must have the same uuid
// and, when it bound a key, the same key (keyChangedRefusal, device_key.go).
//
// A join (proved true) is stricter for a device the server knows: the ID is
// no secret, so a join under it must bring the machine's uuid and the key
// the server bound to the ID, and prove that key (dev.Proof). Without a
// bound key there is nothing to prove; the key it brings waits for an
// administrator as a changed key does. A device the server does not know
// joins without a proof, as older clients do.
//
// unproved names why a join under a known ID was refused for its proof
// (no_proof, bad_proof, replayed, empty_key, empty_uuid), "" otherwise.
func (s *Server) admissionRefusal(dev *onboardDevice, proved bool) (refusal, unproved string) {
	req := dev.enrollment()
	// The signal server refuses a blocked ID's registration: a device
	// admitted under it could never be reached (L550)
	switch s.blockedReason(req.DeviceID) {
	case "device_revoked":
		return revokedRefusal, ""
	case "device_blocked":
		return blockedRefusal, ""
	}
	if banned, _ := s.db.IsPeerBanned(req.DeviceID); banned {
		return "Device is banned", ""
	}
	if removed, _ := s.db.IsPeerSoftDeleted(req.DeviceID); removed {
		return "Device has been removed", ""
	}
	if rejected, _ := s.db.GetConfig(rejectedDevicePrefix + req.DeviceID); rejected != "" {
		return "Device enrollment was rejected", ""
	}
	existing, _ := s.db.GetPeer(req.DeviceID)
	if existing == nil {
		return "", ""
	}
	if proved && existing.DeviceType == onboardDeviceType {
		// A quick support session's device came in without a key the
		// server keeps (appDevice), so there is nothing to prove; the
		// computer that had quick support joins for good with the key it
		// brings now
		if bound, err := s.loadNdMgmtPublicKey(req.DeviceID); err != nil || len(bound) == 0 {
			proved = false
		}
	}
	if proved && strings.TrimSpace(req.PublicKey) == "" {
		return joinProofRefusal, "empty_key"
	}
	if proved && strings.TrimSpace(req.UUID) == "" {
		return joinProofRefusal, "empty_uuid"
	}
	if req.UUID != "" && existing.UUID != "" && !sameDeviceUUID(existing.UUID, req.UUID) {
		return "Device ID is already registered to a different machine", ""
	}
	if req.PublicKey == "" {
		return "", ""
	}
	incoming, err := canonicalizeDevicePublicKey(req.PublicKey)
	if err != nil {
		if proved {
			return joinProofRefusal, "bad_proof"
		}
		return "", ""
	}
	bound, err := s.loadNdMgmtPublicKey(req.DeviceID)
	switch {
	case err == nil && base64.StdEncoding.EncodeToString(bound) != incoming:
		return keyChangedRefusal, ""
	case !proved:
		return "", ""
	case err != nil:
		return keyChangedRefusal, ""
	}
	if err := s.verifyJoinProof(req.DeviceID, bound, dev.Proof); err != nil {
		log.Printf("[API] Join of enrolled device %s refused: %v", req.DeviceID, err)
		switch {
		case errors.Is(err, errNoJoinProof):
			return joinProofRefusal, "no_proof"
		case errors.Is(err, errJoinProofReplayed):
			return joinProofRefusal, "replayed"
		}
		return joinProofRefusal, "bad_proof"
	}
	return "", ""
}

// admitOnboardDevice makes the device approved: a device the server does
// not know becomes a peer (also in managed and locked mode: the code or
// the session is the operator's approval). It takes the device out of the
// queue of pending enrollments and gives it the display name. created
// reports a new peer. A known device keeps its name when the console
// gives none and always for a temporary device. proved asks a known device
// for the proof of its key (a join, admissionRefusal).
func (s *Server) admitOnboardDevice(r *http.Request, dev *onboardDevice, proved bool) (created bool, err error) {
	req := dev.enrollment()
	if reason, unproved := s.admissionRefusal(dev, proved); reason != "" {
		refused := &refusedJoin{reason: reason, code: refusalCode(reason), unproved: unproved}
		if unproved != "" {
			refused.audit, refused.repeats = s.auditJoinUnproved(req.DeviceID, s.remoteIP(r), unproved)
		}
		if reason == keyChangedRefusal {
			s.storeDeviceKeyRequest(req, s.signInIP(r))
		}
		return false, refused
	}
	existing, _ := s.db.GetPeer(req.DeviceID)
	switch {
	case existing == nil:
		if err := s.createPeerFromEnrollment(req, s.remoteIP(r), true); err != nil {
			return false, fmt.Errorf("create device: %w", err)
		}
		created = true
	case existing.DeviceType == onboardDeviceType && dev.Temporary:
		// The temporary device of an earlier session: this session owns
		// it now and removes it after itself.
		created = true
	case existing.DeviceType == onboardDeviceType:
		// A computer that had quick support joins for good: it stops
		// being temporary.
		if err := s.db.UpdatePeerFields(req.DeviceID, map[string]string{"device_type": "neodesk", "tags": ""}); err != nil {
			return false, fmt.Errorf("keep device: %w", err)
		}
	}
	name := strings.TrimSpace(dev.DisplayName)
	if name != "" && (created || !dev.Temporary) {
		_ = s.db.SetConfig("device_display_name_"+req.DeviceID, name)
		_ = s.db.UpdatePeerFields(req.DeviceID, map[string]string{"display_name": name, "note": name})
	}
	_ = s.db.DeleteConfig(pendingDevicePrefix + req.DeviceID)
	// It came in with the key the server knows
	_ = s.db.DeleteConfig(deviceKeyRequestPrefix + req.DeviceID)
	if s.auditLog != nil && created {
		mode := "join_code"
		if dev.Temporary {
			mode = "quick_support"
		}
		s.auditLog.Log("device_enrolled", s.remoteIP(r), req.DeviceID, map[string]string{
			"mode": mode, "hostname": req.Hostname,
		})
	}
	return created, nil
}

// handleOnboardRedeem spends one use of a code of the kind. With a
// device (join codes) the device is admitted first: a device the
// enrollment rules keep out answers 403 device_refused and spends
// nothing.
// POST /api/onboard/codes/redeem {code, kind, device}
func (s *Server) handleOnboardRedeem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code   string         `json:"code"`
		Kind   string         `json:"kind"`
		Device *onboardDevice `json:"device"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		onboardFail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if body.Device != nil {
		if reason := checkEnrollmentFields(body.Device.enrollment()); reason != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid_device", "message": reason})
			return
		}
	}

	s.onboardMu.Lock()
	defer s.onboardMu.Unlock()
	hash, record := s.liveOnboardRecord(body.Code)
	if record == nil || record.Kind != body.Kind {
		onboardUnusable(w)
		return
	}
	created := false
	if body.Device != nil {
		if body.Device.DisplayName == "" && record.MaxUses == 1 {
			body.Device.DisplayName = record.Name
		}
		var err error
		created, err = s.admitOnboardDevice(r, body.Device, record.Kind == onboardKindJoin)
		if errors.Is(err, errOnboardRefused) {
			if record.Kind == onboardKindJoin {
				s.noteOnboardJoin(record, strings.TrimSpace(body.Device.DeviceID), strings.TrimPrefix(err.Error(), errOnboardRefused.Error()+": "))
			}
			writeJSON(w, http.StatusForbidden, refusalAnswer(err))
			return
		}
		if err != nil {
			onboardFail(w, http.StatusInternalServerError, "admit_failed")
			return
		}
		if record.Kind == onboardKindJoin {
			s.noteTokenAdmission(record.CreatedByToken, strings.TrimSpace(body.Device.DeviceID), admittedByInvite)
		}
	}
	record.UsesLeft--
	if record.Kind == onboardKindJoin && body.Device != nil {
		s.noteOnboardJoin(record, strings.TrimSpace(body.Device.DeviceID), "")
	}
	if record.UsesLeft <= 0 {
		s.deletePairingRecord(hash)
	} else if err := s.storePairingRecord(hash, record); err != nil {
		onboardFail(w, http.StatusInternalServerError, "store_failed")
		return
	}
	if s.auditLog != nil {
		actor := record.Kind
		if body.Device != nil {
			actor = body.Device.DeviceID
		}
		s.auditLog.Log("onboard_code_redeemed", s.remoteIP(r), actor, map[string]string{"id": record.ID, "kind": record.Kind})
	}
	answer := map[string]any{"success": true, "record": onboardView(record)}
	if body.Device != nil {
		answer["device"] = map[string]any{"id": strings.TrimSpace(body.Device.DeviceID), "created": created}
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleOnboardPrune deletes expired and used codes.
// POST /api/onboard/codes/prune
func (s *Server) handleOnboardPrune(w http.ResponseWriter, _ *http.Request) {
	s.onboardMu.Lock()
	s.prunePairingRecords()
	s.pruneOnboardJoins()
	s.onboardMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleOnboardAdmit admits a device without a code: a quick support
// session's device, which the console's session vouches for.
// POST /api/onboard/devices {device_id, uuid, hostname, platform, version,
// public_key, display_name, temporary}
func (s *Server) handleOnboardAdmit(w http.ResponseWriter, r *http.Request) {
	var dev onboardDevice
	if err := json.NewDecoder(r.Body).Decode(&dev); err != nil {
		onboardFail(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if reason := checkEnrollmentFields(dev.enrollment()); reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid_device", "message": reason})
		return
	}
	s.onboardMu.Lock()
	created, err := s.admitOnboardDevice(r, &dev, false)
	s.onboardMu.Unlock()
	if errors.Is(err, errOnboardRefused) {
		writeJSON(w, http.StatusForbidden, refusalAnswer(err))
		return
	}
	if err != nil {
		onboardFail(w, http.StatusInternalServerError, "admit_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "created": created})
}

// handleOnboardRemoveDevice deletes a quick support session's temporary
// device for good. A device of another type stays; the answer then says
// removed false. The device's history in the console's tables stays.
// DELETE /api/onboard/devices/{id}
func (s *Server) handleOnboardRemoveDevice(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	peerRow, err := s.db.GetPeer(id)
	if err != nil {
		onboardFail(w, http.StatusInternalServerError, "lookup_failed")
		return
	}
	if peerRow == nil || peerRow.DeviceType != onboardDeviceType {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "removed": false})
		return
	}
	if err := s.db.HardDeletePeer(id); err != nil {
		onboardFail(w, http.StatusInternalServerError, "delete_failed")
		return
	}
	s.forgetSecureDeviceState(id)
	if s.peers != nil {
		s.peers.Remove(id)
	}
	_ = s.db.DeleteConfig("device_display_name_" + id)
	if s.auditLog != nil {
		s.auditLog.Log("quick_support_device_removed", s.remoteIP(r), id, map[string]string{"id": id})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "removed": true})
}

// actionJoinUnproved is the audit entry of a join under an enrolled ID
// that did not prove the ID's key.
const actionJoinUnproved audit.Action = "onboard_join_unproved"

// auditUnprovedEvery is how often one device's refused joins make an
// entry at most; the next entry counts the ones in between.
const auditUnprovedEvery = time.Minute

// unprovedAuditMax is how many devices the limit of auditJoinUnproved
// keeps; past it, entries older than the window go first.
const unprovedAuditMax = 10000

// unprovedAudit is when each device's last refused join was written to the
// audit and how many came after it unwritten.
type unprovedAudit struct {
	mu      sync.Mutex
	devices map[string]*unprovedSeen
}

type unprovedSeen struct {
	at      time.Time
	skipped int
}

// auditJoinUnproved decides whether the refused join of a known ID that
// did not prove its key gets an audit entry: one per device and
// auditUnprovedEvery, the next standing for the refusals in between
// (repeats), so a loop can't flood the audit. It writes the entry into the
// server's event log; due tells the console to write it into the audit list
// admins read (L522).
func (s *Server) auditJoinUnproved(deviceID, ip, reason string) (due bool, repeats int) {
	now := s.now()
	a := &s.joinUnproved
	a.mu.Lock()
	if a.devices == nil {
		a.devices = map[string]*unprovedSeen{}
	}
	seen := a.devices[deviceID]
	if seen != nil && now.Sub(seen.at) < auditUnprovedEvery {
		seen.skipped++
		a.mu.Unlock()
		return false, 0
	}
	if seen != nil {
		repeats = seen.skipped
	}
	if len(a.devices) >= unprovedAuditMax {
		for id, d := range a.devices {
			if now.Sub(d.at) >= auditUnprovedEvery {
				delete(a.devices, id)
			}
		}
	}
	if len(a.devices) < unprovedAuditMax || seen != nil {
		a.devices[deviceID] = &unprovedSeen{at: now}
	}
	a.mu.Unlock()
	details := map[string]string{"device_id": deviceID, "ip": ip, "reason": reason}
	if repeats > 0 {
		details["repeats"] = strconv.Itoa(repeats)
	}
	if s.auditLog != nil {
		s.auditLog.Log(actionJoinUnproved, ip, deviceID, details)
	}
	return true, repeats
}

// blockedReason is device_revoked when a device revoke put the ID on the
// blocklist, device_blocked when it is there for another reason, and ""
// when it is not blocked.
func (s *Server) blockedReason(id string) string {
	if s.blocklist == nil {
		return ""
	}
	entry, blocked := s.blocklist.IDEntry(id)
	switch {
	case !blocked:
		return ""
	case strings.HasPrefix(entry.Reason, security.RevokedReason):
		return "device_revoked"
	}
	return "device_blocked"
}
