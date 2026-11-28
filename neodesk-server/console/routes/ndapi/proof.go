package ndapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
	"github.com/neodeskapp/neodesk/neodesk-server/console/httpx"
	"github.com/neodeskapp/neodesk/neodesk-server/console/routes/internal/jsreq"
)

// The Ed25519 proof of possession a device can send with its registration
// (lib/enrollmentProof.js), the same challenge the API server's
// /api/devices/register takes (nd-enrollment-v1). The device signs, with
// the private half of the key it registers or already has:
//
//	nd-enrollment-v1
//	<METHOD>
//	<PATH>
//	<DEVICE_ID>
//	<PUBLIC_KEY canonical base64>
//	<timestamp>
//	<nonce>
//
// joined by newlines, and sends timestamp, nonce and signature in the
// headers X-ND-Enrollment-Timestamp, -Nonce and -Signature. A proof is
// good for five minutes either side of now, and each nonce once per
// device for ten minutes.
const (
	proofVersion   = "nd-enrollment-v1"
	clockSkewMs    = 5 * 60 * 1000
	nonceMaxLength = 256
	nonceTTLMs     = 10 * 60 * 1000
)

// nonces are the nonces of good proofs, in a KV (kit.Deps.KV) the
// instances of an installation share, so a proof used on one is a replay
// on all. Each is kept with the time it was used for ten minutes.
type nonces struct {
	kv cluster.KV
}

// newNonces keeps the nonces in kv, or in memory when kv is nil.
func newNonces(kv cluster.KV) *nonces {
	if kv == nil {
		kv = cluster.NewMemKV()
	}
	return &nonces{kv: kv}
}

// markUsed notes a nonce at now (milliseconds) and reports whether it was
// noted already within ten minutes (a replay). When the KV does not
// answer, the proof counts as a replay and is refused.
func (n *nonces) markUsed(key string, now int64) bool {
	fresh, err := cluster.UseOnce(context.Background(), n.kv, "console:nd:nonce:"+key, time.UnixMilli(now), nonceTTLMs*time.Millisecond)
	return err != nil || !fresh
}

// canonicalKey is canonicalizePublicKey: the standard base64 of a key
// that is text and decodes to 32 bytes the forgiving way Node decodes.
// A key stored as bytes counts too: 32 bytes are the key itself, as the
// signal server keeps it, and other bytes hold the text the console
// stored in a column of bytes. ok is false for everything else.
func canonicalKey(v any) (key string, ok bool) {
	if raw, isBytes := v.([]byte); isBytes {
		if len(raw) == ed25519.PublicKeySize {
			return base64.StdEncoding.EncodeToString(raw), true
		}
		v = string(raw)
	}
	s, isText := v.(string)
	if !isText || trim(s) == "" {
		return "", false
	}
	raw := nodeBase64(trim(s))
	if len(raw) != ed25519.PublicKeySize {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(raw), true
}

// proofPayload is the text the device signs.
func proofPayload(method, path, deviceID, key, timestamp, nonce string) []byte {
	return []byte(strings.Join([]string{proofVersion, strings.ToUpper(method), path, deviceID, key, timestamp, nonce}, "\n"))
}

// proofHeaders are the three headers, trimmed.
func proofHeaders(r *http.Request) (timestamp, nonce, signature string) {
	return trim(headerText(r, "X-Nd-Enrollment-Timestamp")),
		trim(headerText(r, "X-Nd-Enrollment-Nonce")),
		trim(headerText(r, "X-Nd-Enrollment-Signature"))
}

// verifyProof checks the proof of a request for a device and a public
// key. It returns "" for a good proof and the reason otherwise, in the
// order the old module checked.
func (h *handlers) verifyProof(r *http.Request, deviceID string, publicKey any) string {
	timestamp, nonce, signature := proofHeaders(r)
	if timestamp == "" || nonce == "" || signature == "" {
		return "missing_proof_headers"
	}
	if length(nonce) > nonceMaxLength {
		return "nonce_too_long"
	}
	if !deviceIDPattern(deviceID) {
		return "invalid_device_id"
	}
	signedAt, ok := jsreq.ParseDate(timestamp, time.Local)
	if !ok {
		return "invalid_timestamp"
	}
	now := h.now().UnixMilli()
	if math.Abs(float64(now-signedAt)) > clockSkewMs {
		return "timestamp_outside_skew"
	}
	key, ok := canonicalKey(publicKey)
	if !ok {
		return "invalid_public_key"
	}
	sig := nodeBase64(signature)
	if len(sig) != ed25519.SignatureSize {
		return "invalid_signature_length"
	}
	raw, _ := base64.StdEncoding.DecodeString(key)
	// The path the device sent: the mount as spelled and the path below it
	payload := proofPayload(r.Method, httpx.RawPath(r), deviceID, key, timestamp, nonce)
	if !ed25519.Verify(ed25519.PublicKey(raw), payload, sig) {
		return "invalid_signature"
	}
	if h.nonces.markUsed("enrollment:"+deviceID+":"+nonce, now) {
		return "replayed_proof"
	}
	return ""
}
