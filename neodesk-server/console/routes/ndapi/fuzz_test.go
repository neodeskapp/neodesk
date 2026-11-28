package ndapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/console/httpx"
)

// FuzzNodeBase64: text in the standard alphabet, padded or not, reads as
// Go's decoder reads it; anything else never panics, and the bytes are
// never more than three for every four characters.
func FuzzNodeBase64(f *testing.F) {
	for _, s := range []string{"", "QQ==", "QUJD", "AAcOFRwjKjE4P0ZNVFtiaXB3foWMk5qhqK+2vcTL0tk=", "a-b_c", "é=x", "\x00\xff", "A==A"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := nodeBase64(s)
		if len(got) > len(s)*3/4+2 {
			t.Fatalf("%q gave %d bytes", s, len(got))
		}
		if strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/") != "" {
			return
		}
		want, err := base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			// A single character left over: Node drops it
			return
		}
		if string(got) != string(want) {
			t.Fatalf("%q: %x, Go reads %x", s, got, want)
		}
	})
}

// FuzzVerifyProof: whatever the headers and the key, the check never
// panics, and it passes only a signature of the payload by that key.
func FuzzVerifyProof(f *testing.F) {
	seed := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public := base64.StdEncoding.EncodeToString(seed.Public().(ed25519.PublicKey))
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	payload := proofPayload("POST", "/api/nd/register", "DEV1", public, now, "n")
	good := base64.StdEncoding.EncodeToString(ed25519.Sign(seed, payload))
	f.Add("DEV1", public, now, "n", good)
	f.Add("DEV1", public, "yesterday", "n", good)
	f.Add("D", " "+public, now, strings.Repeat("n", 300), "AAAA")
	f.Add("DEV1", "", "", "", "")
	f.Fuzz(func(t *testing.T, device, key, timestamp, nonce, signature string) {
		h := &handlers{nonces: newNonces(nil), now: time.Now}
		r := httptest.NewRequest("POST", "/api/nd/register", nil)
		r.Header.Set("X-ND-Enrollment-Timestamp", timestamp)
		r.Header.Set("X-ND-Enrollment-Nonce", nonce)
		r.Header.Set("X-ND-Enrollment-Signature", signature)
		if h.verifyProof(r, device, key) != "" {
			return
		}
		canonical, _ := canonicalKey(key)
		raw, _ := base64.StdEncoding.DecodeString(canonical)
		p := proofPayload("POST", "/api/nd/register", device, canonical, trim(httpx.Latin1(timestamp)), trim(httpx.Latin1(nonce)))
		if !ed25519.Verify(raw, p, nodeBase64(trim(httpx.Latin1(signature)))) {
			t.Fatalf("a proof passed that does not verify: %q %q %q %q %q", device, key, timestamp, nonce, signature)
		}
	})
}
