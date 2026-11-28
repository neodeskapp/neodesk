package authn

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/auth"
	"github.com/neodeskapp/neodesk/neodesk-server/console/goapi"
)

// Two-factor sign-in with time-based codes: HMAC-SHA1, 6 digits, a step
// of 30 seconds, and no tolerance. Only the code of the current step is
// accepted; the code of the step before is refused.
//
// The secret is base32 text (RFC 4648) without padding, read as the API
// server reads the same column: the letters A to Z and the digits 2 to 7.
// Its bytes are the HMAC key as they are, whatever their number. A secret
// with any other character has no codes: every code is wrong for it.
// Secrets made here have 20 bytes.

const (
	totpIssuer     = "NeoDesk"
	totpStepMillis = 30_000
	totpDigits     = 6
	totpKeyLength  = 20
)

var errNotBase32 = errors.New("the two-factor secret is not base32 text")

// decodeSecret reads a stored secret.
func decodeSecret(secret string) ([]byte, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return nil, errNotBase32
	}
	return key, nil
}

// totpCode computes the code of secret for the step that contains at.
func totpCode(secret string, at time.Time) (string, error) {
	decoded, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	counter := math.Max(0, math.Floor(float64(at.UnixMilli())/30/1000))
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], uint64(counter))
	mac := hmac.New(sha1.New, decoded)
	mac.Write(message[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0f
	binaryCode := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, binaryCode%1_000_000), nil
}

// totpVerify reports whether token is the code of the current step. The
// token must be exactly the six digits: no spaces, no other digits. No
// token is the code of a secret that is not base32.
func totpVerify(token, secret string, at time.Time) (bool, error) {
	code, err := totpCode(secret, at)
	if errors.Is(err, errNotBase32) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hmac.Equal([]byte(token), []byte(code)), nil
}

// formEncode writes a value the way a URL query built from form fields
// has it: letters, digits and * - . _ stay, a space becomes "+", every
// other byte becomes %XX.
func formEncode(s string) string {
	s = toWellFormed(s)
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
		}
	}
	return b.String()
}

// OtpauthURL builds the address an authenticator app reads from the QR
// code:
//
//	otpauth://totp/NeoDesk:<account>?secret=<secret>&issuer=NeoDesk&digits=6&period=30
//
// The issuer has no space and there is no algorithm parameter, because
// some apps refuse those.
func OtpauthURL(account, secret string) string {
	return otpauthURL(account, secret, totpIssuer)
}

func otpauthURL(account, secret, issuer string) string {
	return "otpauth://totp/" + goapi.EncodeURIComponent(toWellFormed(issuer)) + ":" + goapi.EncodeURIComponent(toWellFormed(account)) +
		"?secret=" + formEncode(secret) + "&issuer=" + formEncode(issuer) + "&digits=6&period=30"
}

// generateSecret makes a secret of 20 random bytes, 160 bits, as 32
// base32 characters.
func (s *Service) generateSecret() (string, error) {
	raw := make([]byte, totpKeyLength)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return "", fmt.Errorf("two-factor secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// TOTPSetup is what the set-up of two-factor sign-in shows the user.
type TOTPSetup struct {
	// QRCode is OtpauthURL as a PNG data URL.
	QRCode string `json:"qrCode"`
	// Secret is the new secret, for typing it into an app.
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauthUrl"`
}

// GenerateTOTPSetup makes a new secret for the account, stores it at once
// (two-factor sign-in stays off until VerifyAndEnableTOTP) and returns it
// with its QR code. A secret from an unfinished set-up is replaced. It
// returns ErrUserNotFound for an unknown account.
func (s *Service) GenerateTOTPSetup(ctx context.Context, userID int64) (*TOTPSetup, error) {
	user, err := getUserByID(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, ErrUserNotFound
	}
	secret, err := s.generateSecret()
	if err != nil {
		return nil, err
	}
	if err := saveTOTPSecret(ctx, s.db, userID, secret); err != nil {
		return nil, err
	}
	address := OtpauthURL(user.Username, secret)
	if s.qr == nil {
		return nil, errors.New("authn: Options.QR is not set")
	}
	image, err := s.qr(address)
	if err != nil {
		return nil, fmt.Errorf("draw the two-factor QR code: %w", err)
	}
	return &TOTPSetup{QRCode: image, Secret: secret, OtpauthURL: address}, nil
}

// VerifyAndEnableTOTP finishes the set-up: when token is the current code
// of the stored secret it turns two-factor sign-in on and returns the 8
// new recovery codes (auth.GenerateRecoveryCodesFrom). Only their hashes
// are stored, as the API server stores them (auth.HashRecoveryCodes). It
// returns ErrTOTPNotSetUp when the account has no secret and
// ErrInvalidTOTPCode for a wrong code.
func (s *Service) VerifyAndEnableTOTP(ctx context.Context, userID int64, token string) ([]string, error) {
	user, err := getUserByID(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	if user == nil || user.TOTPSecret.String == "" {
		return nil, ErrTOTPNotSetUp
	}
	valid, err := totpVerify(token, user.TOTPSecret.String, s.now())
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInvalidTOTPCode
	}
	codes, err := auth.GenerateRecoveryCodesFrom(s.rand)
	if err != nil {
		return nil, fmt.Errorf("recovery codes: %w", err)
	}
	hashes, err := auth.HashRecoveryCodesCost(codes, s.recoveryCost)
	if err != nil {
		return nil, fmt.Errorf("hash the recovery codes: %w", err)
	}
	if err := enableTOTP(ctx, s.db, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// RenewRecoveryCodes replaces the account's recovery codes with 8 new ones
// when token is a current code of its second factor, and returns them.
// The code works once, as at sign-in. Only the hashes are stored, so the
// old codes stop working. It returns ErrTOTPNotSetUp for an account
// without two-factor sign-in and ErrInvalidTOTPCode for a wrong code.
func (s *Service) RenewRecoveryCodes(ctx context.Context, userID int64, token string) ([]string, error) {
	enabled, err := s.IsTOTPEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrTOTPNotSetUp
	}
	valid, err := s.VerifyTOTPCode(ctx, userID, token)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInvalidTOTPCode
	}
	codes, err := auth.GenerateRecoveryCodesFrom(s.rand)
	if err != nil {
		return nil, fmt.Errorf("recovery codes: %w", err)
	}
	hashes, err := auth.HashRecoveryCodesCost(codes, s.recoveryCost)
	if err != nil {
		return nil, fmt.Errorf("hash the recovery codes: %w", err)
	}
	if err := setRecoveryCodes(ctx, s.db, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// VerifyTOTPCode checks the second factor of a sign-in. It is false for an
// account without two-factor sign-in, and for a code of a step whose code
// already signed the account in: a code works once.
func (s *Service) VerifyTOTPCode(ctx context.Context, userID int64, token string) (bool, error) {
	user, err := getUserByID(ctx, s.db, userID)
	if err != nil {
		return false, err
	}
	if user == nil || !user.TOTPEnabled.True() || user.TOTPSecret.String == "" {
		return false, nil
	}
	now := s.now()
	valid, err := totpVerify(token, user.TOTPSecret.String, now)
	if err != nil || !valid {
		return false, err
	}
	return s.useTOTPStep(ctx, userID, totpStep(now))
}

// totpStep is the number of the 30-second step that contains at.
func totpStep(at time.Time) int64 {
	return int64(math.Max(0, math.Floor(float64(at.UnixMilli())/totpStepMillis)))
}

// totpStepTTL is how long the KV keeps the step of a code that signed an
// account in: past it, the clock has moved on and the code of that step is
// no longer accepted anyway.
const totpStepTTL = 3 * totpStepMillis * time.Millisecond

func totpStepKey(userID, step int64) string {
	return "totp:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(step, 10)
}

// useTOTPStep notes that a code of step signed the account in. It is false
// when a code of that step or the next one already did, on any instance
// that shares the KV: the step is taken with SetNX, so of two sign-ins
// with one code only one gets it.
func (s *Service) useTOTPStep(ctx context.Context, userID, step int64) (bool, error) {
	if _, later, err := s.kv.Get(ctx, totpStepKey(userID, step+1)); err != nil || later {
		return false, err
	}
	_, set, err := s.kv.SetNX(ctx, totpStepKey(userID, step), "1", totpStepTTL)
	return set, err
}

// VerifyRecoveryCode checks a recovery code in place of the second factor
// and uses it up, with the API server's check (auth.ConsumeRecoveryCode):
// the stored list holds hashes, or on older installs the codes as they
// were shown, which keep working until they are used or made anew. White
// space, dashes and the case of the letters do not matter. Stored text
// that is not a JSON list of texts holds no codes. It is false for an
// account without two-factor sign-in.
func (s *Service) VerifyRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	user, err := getUserByID(ctx, s.db, userID)
	if err != nil {
		return false, err
	}
	if user == nil || !user.TOTPEnabled.True() || user.TOTPRecoveryCodes.String == "" {
		return false, nil
	}
	left, matched, err := auth.ConsumeRecoveryCode(user.TOTPRecoveryCodes.String, toWellFormed(code))
	if errors.Is(err, auth.ErrMalformedRecoveryCodes) {
		return false, nil
	}
	if err != nil || !matched {
		return false, err
	}
	// Only the request that changes the list it read uses the code: of two
	// with the same code at the same time, the second finds the list changed
	return useRecoveryCode(ctx, s.db, userID, user.TOTPRecoveryCodes.String, left)
}

// DisableTOTP turns two-factor sign-in off and deletes the secret and the
// recovery codes.
func (s *Service) DisableTOTP(ctx context.Context, userID int64) error {
	return disableTOTP(ctx, s.db, userID)
}

// InvalidateRecoveryCodes makes the account's recovery codes stop
// working without new ones: the person makes new ones on Your account,
// with a code of their authenticator. Nobody else sees them.
func (s *Service) InvalidateRecoveryCodes(ctx context.Context, userID int64) error {
	return setRecoveryCodes(ctx, s.db, userID, "[]")
}

// RecoveryCodesLeft is the number of recovery codes the account can still
// use; 0 without two-factor sign-in or with a stored list that is no list.
func (s *Service) RecoveryCodesLeft(ctx context.Context, userID int64) (int, error) {
	user, err := getUserByID(ctx, s.db, userID)
	if err != nil || user == nil || !user.TOTPEnabled.True() {
		return 0, err
	}
	var codes []any
	if json.Unmarshal([]byte(user.TOTPRecoveryCodes.String), &codes) != nil {
		return 0, nil
	}
	return len(codes), nil
}

// IsTOTPEnabled reports whether the account has two-factor sign-in on. An
// unknown account has not.
func (s *Service) IsTOTPEnabled(ctx context.Context, userID int64) (bool, error) {
	user, err := getUserByID(ctx, s.db, userID)
	if err != nil {
		return false, err
	}
	return user != nil && user.TOTPEnabled.True(), nil
}
