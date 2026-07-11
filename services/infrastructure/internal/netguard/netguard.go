// Package netguard is the head's shared outbound-dial screen. Any URL the head fetches
// that an outside party can influence — a volunteer's DID document (atproto), a
// volunteer-submitted external output reference (contentverify) — must not let the head
// be steered at internal infrastructure. The guard runs as a net.Dialer.Control hook, so
// it screens the CONCRETE post-resolution IP on every connection attempt; screening at
// connect time (rather than parsing the URL host) also blunts DNS rebinding, where a name
// resolves publicly when first checked and privately at the moment of connection.
//
// Relocated from internal/atproto's unexported dial guard (design doc §10.4) and
// strictly stronger: beyond the standard library's classification (loopback, private,
// link-local, unspecified, multicast — including IPv4-mapped IPv6 forms) it refuses
// CGNAT 100.64.0.0/10 (a real internal-services range on cloud hosts that IsPrivate
// misses), NAT64 64:ff9b::/96 (a DNS64/NAT64 host maps it onto arbitrary IPv4 including
// loopback), 0.0.0.0/8 "this network" beyond the unspecified 0.0.0.0 itself, and the
// deprecated IPv4-compatible IPv6 range ::/96.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrDisallowedAddress is returned when a connection would target an address in a
// blocked range. Callers can match it with errors.Is to log a blocked fetch distinctly
// from an ordinary dial failure.
var ErrDisallowedAddress = errors.New("netguard: refusing to dial disallowed address")

// blockedNets are the ranges the standard library's classification predicates do not
// cover. Parsed once at init; mustCIDR panics only on a programmer error.
var blockedNets = []struct {
	net    *net.IPNet
	reason string
}{
	{mustCIDR("100.64.0.0/10"), "carrier-grade NAT"},
	{mustCIDR("64:ff9b::/96"), "NAT64"},
	{mustCIDR("0.0.0.0/8"), "this-network"},
	{mustCIDR("::/96"), "IPv4-compatible IPv6"},
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("netguard: bad built-in CIDR %q: %v", s, err))
	}
	return n
}

// DialControl is the net.Dialer.Control hook. Control runs on every connection attempt
// AFTER name resolution, with the concrete address the socket is about to connect to.
func DialControl(network, address string, _ syscall.RawConn) error {
	return Screen(address)
}

// Screen rejects an address whose IP is in a range the head must not reach when
// fetching outside-party-influenced URLs. address is the post-resolution "ip:port"
// form that net.Dialer.Control receives; a bare IP literal is also accepted.
func Screen(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// No port present; treat the whole value as the host.
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control is handed an already-resolved IP literal. Anything else is
		// unexpected; refuse rather than risk an unscreened dial.
		return fmt.Errorf("%w: %q is not an IP literal", ErrDisallowedAddress, host)
	}
	if reason := DisallowedIPReason(ip); reason != "" {
		return fmt.Errorf("%w: %s (%s)", ErrDisallowedAddress, ip, reason)
	}
	return nil
}

// DisallowedIPReason returns a short human-readable reason when ip falls in a blocked
// range, or an empty string when the address is allowed. The standard-library checks
// already account for IPv4-mapped IPv6 forms (e.g. ::ffff:127.0.0.1) and both IPv4 and
// IPv6 ranges: IsLoopback covers 127.0.0.0/8 and ::1; IsPrivate covers 10/8, 172.16/12,
// 192.168/16 and fc00::/7; the link-local checks cover 169.254.0.0/16 and fe80::/10;
// IsUnspecified covers 0.0.0.0 and ::. The blockedNets table adds the ranges the
// standard library classifies as ordinary unicast (see the package comment). Order
// matters only for the reason string: ::1 and :: sit inside ::/96 but are reported as
// loopback/unspecified by the earlier checks.
func DisallowedIPReason(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsLoopback():
		return "loopback"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "link-local"
	case ip.IsPrivate():
		return "private"
	case ip.IsMulticast():
		return "multicast"
	}
	for _, b := range blockedNets {
		if b.net.Contains(ip) {
			return b.reason
		}
	}
	return ""
}
