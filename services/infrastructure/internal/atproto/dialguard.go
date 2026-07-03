package atproto

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrDisallowedAddress is returned by the default client's dial guard when a
// connection would target a loopback, private, link-local, unspecified, or
// multicast address. PDS endpoints and did:web document URLs are
// volunteer-controlled and fetched by the head, so without this guard a hostile
// DID document could point the head at an internal service and turn it into a
// server-side request forgery proxy. Callers can match this with errors.Is to
// log a blocked resolution distinctly from an ordinary dial failure.
var ErrDisallowedAddress = errors.New("atproto: refusing to dial disallowed address")

// guardedDialControl is the net.Dialer.Control hook installed on the default
// client. Control runs on every connection attempt AFTER name resolution, with
// the concrete address the socket is about to connect to, so it screens the real
// destination IP. Screening at connect time (rather than parsing the URL host)
// also blunts DNS rebinding, where a name resolves to a public IP when the URL
// is first checked and to a private one at the moment of connection.
func guardedDialControl(network, address string, _ syscall.RawConn) error {
	return screenDialAddress(network, address)
}

// screenDialAddress rejects an address whose IP is in a range the head must not
// reach as part of unauthenticated, volunteer-controlled identity resolution.
// address is the post-resolution "ip:port" that net.Dialer.Control receives.
func screenDialAddress(network, address string) error {
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
	if reason := disallowedIPReason(ip); reason != "" {
		return fmt.Errorf("%w: %s (%s)", ErrDisallowedAddress, ip, reason)
	}
	return nil
}

// disallowedIPReason returns a short human-readable reason when ip falls in a
// blocked range, or an empty string when the address is allowed. The checks rely
// on the standard library's classification, which already accounts for
// IPv4-mapped IPv6 forms (e.g. ::ffff:127.0.0.1) and both IPv4 and IPv6 ranges:
// IsLoopback covers 127.0.0.0/8 and ::1; IsPrivate covers 10/8, 172.16/12,
// 192.168/16 and fc00::/7; the link-local checks cover 169.254.0.0/16 and
// fe80::/10; IsUnspecified covers 0.0.0.0 and ::.
func disallowedIPReason(ip net.IP) string {
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
	return ""
}
