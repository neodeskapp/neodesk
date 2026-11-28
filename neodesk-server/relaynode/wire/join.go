package wire

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// JoinPrefix starts every enrollment string.
const JoinPrefix = "nd1."

// Join is what an enrollment string carries: the central control address,
// the SPKI sha256 (hex) of the central certificate, and the one-use token.
type Join struct {
	Server string `json:"s"`
	Pin    string `json:"p"`
	Token  string `json:"t"`
}

// Encode returns the enrollment string: "nd1." and the base64url JSON.
func (j Join) Encode() string {
	b, _ := json.Marshal(j)
	return JoinPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// String leaves the token out, so a Join in a log line gives nothing away.
func (j Join) String() string {
	return fmt.Sprintf("join{server=%s pin=%s}", j.Server, j.Pin)
}

// GoString is String for %#v.
func (j Join) GoString() string { return j.String() }

// ParseJoin reads an enrollment string. Errors never quote the input.
func ParseJoin(s string) (Join, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, JoinPrefix) {
		return Join{}, errors.New("enrollment string does not start with nd1.")
	}
	raw := strings.TrimRight(s[len(JoinPrefix):], "=")
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Join{}, errors.New("enrollment string is not valid base64url")
	}
	var j Join
	if err := json.Unmarshal(b, &j); err != nil {
		return Join{}, errors.New("enrollment string does not hold the expected JSON")
	}
	if _, _, err := net.SplitHostPort(j.Server); err != nil || j.Server == "" {
		return Join{}, errors.New("enrollment string has no server host:port")
	}
	if pin, err := hex.DecodeString(j.Pin); err != nil || len(pin) != sha256.Size {
		return Join{}, errors.New("enrollment string has no valid certificate pin")
	}
	j.Pin = strings.ToLower(j.Pin)
	if j.Token == "" {
		return Join{}, errors.New("enrollment string has no token")
	}
	return j, nil
}

// String leaves the token out.
func (h Hello) String() string {
	tok := ""
	if h.Token != "" {
		tok = " token=<set>"
	}
	return fmt.Sprintf("hello{node_id=%s version=%s lan=%s public=%s%s}", h.NodeID, h.Version, h.LanAddr, h.PublicAddr, tok)
}

// GoString is String for %#v.
func (h Hello) GoString() string { return h.String() }
