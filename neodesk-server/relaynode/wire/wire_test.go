package wire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Authorize{ID: 7, UUID: "u-1", Initiator: "111", Target: "222", ExpiresUnixMs: 1234}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, Ping{}); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(buf.Bytes()[:4]); int(got) != bytes.IndexByte(buf.Bytes()[4:], '}')+1 {
		t.Fatalf("length prefix %d does not cover the first JSON body", got)
	}
	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var out Authorize
	if f.Type != TypeAuthorize || f.Decode(&out) != nil || out != in {
		t.Fatalf("got %s %+v, want authorize %+v", f.Type, out, in)
	}
	if !strings.HasPrefix(string(f.Body), `{"t":"authorize",`) {
		t.Fatalf("body %s does not start with the type", f.Body)
	}
	f, err = ReadFrame(&buf)
	if err != nil || f.Type != TypePing || string(f.Body) != `{"t":"ping"}` {
		t.Fatalf("ping frame: %v %q %s", err, f.Type, f.Body)
	}
}

func frameOfSize(n int) []byte {
	// {"t":"x","p":"…"} padded to n bytes
	head, tail := `{"t":"x","p":"`, `"}`
	body := head + strings.Repeat("a", n-len(head)-len(tail)) + tail
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	copy(out[4:], body)
	return out
}

func TestReadFrameCapsAtOneMiB(t *testing.T) {
	f, err := ReadFrame(bytes.NewReader(frameOfSize(MaxFrame)))
	if err != nil || f.Type != "x" {
		t.Fatalf("a frame of exactly 1 MiB: %v", err)
	}
	// Only the header: a reader must refuse without waiting for the body
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("1 MiB + 1: got %v, want ErrFrameTooLarge", err)
	}
	big := Error{Code: "x", Msg: strings.Repeat("a", MaxFrame)}
	if err := WriteFrame(&bytes.Buffer{}, big); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("writing over 1 MiB: got %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameRefusesFramesWithoutType(t *testing.T) {
	for _, body := range []string{`{}`, `[1]`, `{"t":""}`} {
		b := make([]byte, 4+len(body))
		binary.BigEndian.PutUint32(b, uint32(len(body)))
		copy(b[4:], body)
		if _, err := ReadFrame(bytes.NewReader(b)); err == nil {
			t.Fatalf("%s: read without error", body)
		}
	}
}

func testPin() string {
	sum := sha256.Sum256([]byte("pin"))
	return hex.EncodeToString(sum[:])
}

func TestJoinRoundTripKeepsTokenOutOfStrings(t *testing.T) {
	j := Join{Server: "central.example:21123", Pin: testPin(), Token: "tok-SECRET-123"}
	s := j.Encode()
	if !strings.HasPrefix(s, "nd1.") || strings.ContainsAny(s[4:], "+/=") {
		t.Fatalf("%q is not nd1. and base64url", s)
	}
	got, err := ParseJoin(s)
	if err != nil || got != j {
		t.Fatalf("ParseJoin: %+v %v", got, err)
	}
	for _, out := range []string{j.String(), fmt.Sprintf("%v", j), fmt.Sprintf("%+v", j), fmt.Sprintf("%#v", j),
		fmt.Sprintf("%v", Hello{Token: j.Token}), fmt.Sprintf("%#v", Hello{Token: j.Token})} {
		if strings.Contains(out, j.Token) {
			t.Fatalf("%q shows the token", out)
		}
	}
}

func TestParseJoinErrorsDoNotQuoteInput(t *testing.T) {
	enc := func(js string) string { return "nd1." + b64url(js) }
	bad := []string{
		"tok-SECRET-123",
		"nd1.!!!tok-SECRET-123",
		enc(`{"s":"no-port","p":"` + testPin() + `","t":"tok-SECRET-123"}`),
		enc(`{"s":"h:1","p":"abc","t":"tok-SECRET-123"}`),
		enc(`{"s":"h:1","p":"` + testPin() + `","t":""}`),
	}
	for _, s := range bad {
		_, err := ParseJoin(s)
		if err == nil {
			t.Fatalf("%q parsed", s)
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), s) {
			t.Fatalf("error %q quotes the input", err)
		}
	}
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestKeyIsMadeOnceWithMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "node.key")
	k1, created, err := LoadOrGenerateKey(path)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	k2, created, err := LoadOrGenerateKey(path)
	if err != nil || created || !k1.Equal(k2) {
		t.Fatalf("second load: created=%v err=%v same=%v", created, err, k1.Equal(k2))
	}
	if runtime.GOOS == "windows" {
		t.Skip("no Unix file modes on Windows")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", st.Mode().Perm())
	}
	if err := WriteFileAtomic(filepath.Join(filepath.Dir(path), "state.json"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(filepath.Join(filepath.Dir(path), "state.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, want 0600", st.Mode().Perm())
	}
}

func TestPinIsTheCertificateSPKI(t *testing.T) {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	c, err := SelfSignedCert(k, "x")
	if err != nil {
		t.Fatal(err)
	}
	pin, err := SPKIPin(k.Public())
	if err != nil || pin != CertPin(c.Leaf) {
		t.Fatalf("SPKIPin %s, CertPin %s, %v", pin, CertPin(c.Leaf), err)
	}
	again, err := CertFromPEM(CertPEM(c), k)
	if err != nil || CertPin(again.Leaf) != pin {
		t.Fatalf("CertFromPEM: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := CertFromPEM(CertPEM(c), other); err == nil {
		t.Fatal("a certificate loaded with another key")
	}
}
