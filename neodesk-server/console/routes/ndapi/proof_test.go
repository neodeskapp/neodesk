package ndapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// canonicalKey reads a key the forgiving way Node's Buffer.from(text,
// 'base64') does and gives the standard form of 32 bytes; a stored key of
// 32 bytes is the key, other bytes are read as the text they hold (BUGS
// BD6); anything else is no key.
func TestCanonicalKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	std := base64.StdEncoding.EncodeToString(raw)
	url := base64.RawURLEncoding.EncodeToString(raw)
	for _, c := range []struct {
		in   any
		want string
	}{
		{std, std},
		{"  " + std + "\n", std},
		{url, std},
		{std[:10] + " !* " + std[10:], std},
		{std[:20] + "=" + std[20:], ""},
		{std + std[:4], std},
		{base64.StdEncoding.EncodeToString(append(raw, 1)), ""},
		{std[:42], ""},
		{"", ""},
		{"   ", ""},
		{[]byte(std), std},
		{raw, std},
		{[]byte(" " + url + " "), std},
		{[]byte{}, ""},
		{raw[:31], ""},
		{float64(5), ""},
		{nil, ""},
	} {
		got, ok := canonicalKey(c.in)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("canonicalKey(%q) = %q, %v; want %q", fmt.Sprint(c.in), got, ok, c.want)
		}
	}
}

// A nonce counts once per device for ten minutes.
func TestNonces(t *testing.T) {
	n := newNonces(nil)
	if n.markUsed("a", 0) || !n.markUsed("a", 1000) {
		t.Fatal("a nonce is a replay the second time")
	}
	if n.markUsed("a", nonceTTLMs+1) {
		t.Error("an old nonce is dropped")
	}
	if !n.markUsed("a", nonceTTLMs+2) {
		t.Error("a nonce noted again counts again")
	}
}

// Two instances that share one KV: a nonce used on one is a replay on
// the other.
func TestNoncesShared(t *testing.T) {
	kv := cluster.NewMemKV()
	a, b := newNonces(kv), newNonces(kv)
	now := time.Now().UnixMilli()
	if a.markUsed("enrollment:D:n", now) {
		t.Fatal("the first use is a replay")
	}
	if !b.markUsed("enrollment:D:n", now) {
		t.Error("a nonce used on one instance worked on the other")
	}
}

// The payload a device signs: the version, the method in capitals, the
// path, the device, the key, the time and the nonce, one per line.
func TestProofPayload(t *testing.T) {
	got := string(proofPayload("post", "/api/nd/register", "DEV1", "KEY", "2026-01-01T00:00:00Z", "n1"))
	want := "nd-enrollment-v1\nPOST\n/api/nd/register\nDEV1\nKEY\n2026-01-01T00:00:00Z\nn1"
	if got != want {
		t.Errorf("payload %q", got)
	}
	seed := make([]byte, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(want), ed25519.Sign(key, []byte(want))) {
		t.Error("a signature of the payload verifies")
	}
}

// pkEqual of the rename guard: bytes as they are, text of hex digits as
// hex, other text as UTF-8; values of the same length still differ.
func TestKeysEqual(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
	}{
		{"abcd", "abcd", true},
		{"abcd", "abce", false},
		{"0a0b", []byte{0x0a, 0x0b}, true},
		{"0A0B", []byte{0x0a, 0x0b}, true},
		{"0a0b", []byte{0x0a, 0x0c}, false},
		{"key", []byte("key"), true},
		{"key", []byte("kez"), false},
		{"", []byte("x"), false},
	} {
		if got := keysEqual(c.a, c.b); got != c.want {
			t.Errorf("keysEqual(%v, %v) = %v", c.a, c.b, got)
		}
	}
}
