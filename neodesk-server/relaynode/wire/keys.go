package wire

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// LoadOrGenerateKey reads the PEM (PKCS#8) Ed25519 key at path, or makes
// one and writes it with mode 0600. created says it was made now. Neither
// the key nor its file contents appear in an error.
func LoadOrGenerateKey(path string) (key ed25519.PrivateKey, created bool, err error) {
	if b, err := os.ReadFile(path); err == nil {
		k, err := parseKey(b)
		if err != nil {
			return nil, false, fmt.Errorf("key %s: %w", path, err)
		}
		return k, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("key %s: %w", path, err)
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, false, fmt.Errorf("encode key: %w", err)
	}
	if err := WriteFileExclusive(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return nil, false, fmt.Errorf("key %s: %w", path, err)
	}
	return k, true, nil
}

func parseKey(b []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not a PKCS#8 key")
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return ek, nil
}

// WriteFileExclusive creates path with mode 0600 and data; it fails when
// path exists. The parent directory is made with 0700 if missing.
func WriteFileExclusive(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// WriteFileAtomic replaces path with data, mode 0600, through a temporary
// file in the same directory.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	// CreateTemp makes the file with mode 0600
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// PublicKeyBase64 is the key's public half as standard base64, the form
// relay_nodes.public_key stores.
func PublicKeyBase64(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// SelfSignedCert makes a self-signed certificate for key. The pin covers
// only the public key, so the validity is long and not checked.
func SelfSignedCert(key ed25519.PrivateKey, commonName string) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(50, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// CertFromPEM builds a tls.Certificate from a PEM certificate and key; the
// certificate's public key must be key's.
func CertFromPEM(certPEM []byte, key ed25519.PrivateKey) (tls.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return tls.Certificate{}, errors.New("not a PEM certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, err
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !pub.Equal(key.Public()) {
		return tls.Certificate{}, errors.New("certificate does not belong to the key")
	}
	return tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key, Leaf: leaf}, nil
}

// CertPEM encodes the leaf of c as PEM.
func CertPEM(c tls.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
}

// SPKIPin is the sha256 (hex) of pub's SubjectPublicKeyInfo.
func SPKIPin(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// CertPin is the SPKI pin of a certificate.
func CertPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ErrPinMismatch is the TLS error when the server's key is not the pinned one.
var ErrPinMismatch = errors.New("central certificate does not match the pin")

// ClientTLS is the node's TLS config: TLS 1.3, it presents cert and
// accepts only a server whose SPKI sha256 is pin. The pin replaces CA
// verification, so the chain and name are not checked.
func ClientTLS(cert tls.Certificate, pin string) *tls.Config {
	want, _ := hex.DecodeString(pin)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // the pin below replaces chain verification
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrPinMismatch
			}
			got := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if len(want) != sha256.Size || subtle.ConstantTimeCompare(got[:], want) != 1 {
				return ErrPinMismatch
			}
			return nil
		},
	}
}

// ServerTLS is central's TLS config: TLS 1.3, it presents cert and requires
// a client certificate with an Ed25519 key. The handshake proves the
// client holds that key; who the key belongs to is the hub's lookup.
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := PeerKey(cs)
			return err
		},
	}
}

// PeerKey is the Ed25519 public key of the peer's leaf certificate.
func PeerKey(cs tls.ConnectionState) (ed25519.PublicKey, error) {
	if len(cs.PeerCertificates) == 0 {
		return nil, errors.New("no client certificate")
	}
	pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("client certificate key is not Ed25519")
	}
	return pub, nil
}
