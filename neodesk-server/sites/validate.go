package sites

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/neodeskapp/neodesk/neodesk-server/db"
)

// MaxNameLen is the longest site, node or VPN group name in characters.
const MaxNameLen = 100

var (
	// ErrInvalidName is a name that is empty after trimming or longer
	// than MaxNameLen characters.
	ErrInvalidName = errors.New("sites: name must be 1 to 100 characters")
	// ErrInvalidNetwork is a CIDR that does not parse or whose kind does
	// not fit its addresses.
	ErrInvalidNetwork = errors.New("sites: invalid network")
	// ErrInvalidPin is a pin of an unknown kind or with an empty or
	// malformed ref.
	ErrInvalidPin = errors.New("sites: invalid pin")
	// ErrNotFound is a site or node that does not exist.
	ErrNotFound = db.ErrSiteNotFound
	// ErrOtherOrg is a device pinned to a site of another organization.
	ErrOtherOrg = errors.New("sites: the site belongs to another organization")
	// ErrTokenInvalid is an enrollment token that is unknown, used or
	// expired.
	ErrTokenInvalid = db.ErrSiteTokenInvalid
	// ErrNodeKeyTaken is an enrollment with a public key an enrolled node
	// already has.
	ErrNodeKeyTaken = errors.New("sites: a relay node with this key is already enrolled")
)

// privateRanges are the address ranges of kind "private": RFC 1918, the
// shared address space of carrier-grade NAT (RFC 6598) and IPv6 unique
// local addresses.
var privateRanges = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7"} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// IsPrivate says ip is in a private range (RFC 1918, carrier-grade NAT,
// IPv6 unique local). A nil ip is not private.
func IsPrivate(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateNetwork checks a site network and returns its CIDR in canonical
// form (the network address, "10.1.0.0/16" for "10.1.2.3/16"). Kind
// "private" takes only networks inside a private range, "public" only
// networks that are not.
func ValidateNetwork(cidr, kind string) (string, error) {
	_, n, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a CIDR", ErrInvalidNetwork, cidr)
	}
	private := networkIsPrivate(n)
	switch kind {
	case db.SiteNetworkPrivate:
		if !private {
			return "", fmt.Errorf("%w: %s is not a private network", ErrInvalidNetwork, n)
		}
	case db.SiteNetworkPublic:
		if private {
			return "", fmt.Errorf("%w: %s is a private network", ErrInvalidNetwork, n)
		}
	default:
		return "", fmt.Errorf("%w: kind %q is neither public nor private", ErrInvalidNetwork, kind)
	}
	return n.String(), nil
}

// networkIsPrivate says n lies entirely inside one private range.
func networkIsPrivate(n *net.IPNet) bool {
	ones, bits := n.Mask.Size()
	for _, r := range privateRanges {
		rOnes, rBits := r.Mask.Size()
		if bits == rBits && ones >= rOnes && r.Contains(n.IP) {
			return true
		}
	}
	return false
}

// cleanName trims a name and checks its length.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxNameLen {
		return "", ErrInvalidName
	}
	return name, nil
}

// cleanVPNGroup trims a VPN group label; "" is no group.
func cleanVPNGroup(group string) (string, error) {
	group = strings.TrimSpace(group)
	if utf8.RuneCountInString(group) > MaxNameLen {
		return "", ErrInvalidName
	}
	return group, nil
}

// HashToken is how an enrollment token is stored: its sha256 in hex.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
