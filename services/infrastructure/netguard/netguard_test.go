package netguard

import (
	"errors"
	"testing"
)

// TestScreen pins the classifier range by range. The CGNAT, NAT64, this-network, and
// IPv4-compatible rows are the ranges ADDED at relocation (design doc §10.4) — each
// fails against a verbatim relocation of the atproto guard.
func TestScreen(t *testing.T) {
	cases := []struct {
		name    string
		address string
		allow   bool
	}{
		// Allowed: ordinary public addresses.
		{"public ipv4", "93.184.216.34:443", true},
		{"public ipv6", "[2606:2800:220:1:248:1893:25c8:1946]:443", true},
		// A public address just OUTSIDE each added range.
		{"public below cgnat", "100.63.255.255:443", true},
		{"public above cgnat", "100.128.0.0:443", true},
		{"public above this-network", "1.0.0.1:443", true},

		// Loopback.
		{"ipv4 loopback", "127.0.0.1:443", false},
		{"ipv4 loopback range", "127.9.9.9:8443", false},
		{"ipv6 loopback", "[::1]:443", false},
		{"ipv4-mapped loopback", "[::ffff:127.0.0.1]:443", false},

		// Private.
		{"private 10/8", "10.0.0.5:8443", false},
		{"private 172.16/12", "172.16.4.4:443", false},
		{"private 192.168/16", "192.168.1.10:443", false},
		{"private fc00::/7", "[fd00::1]:443", false},

		// Link-local.
		{"link-local ipv4", "169.254.169.254:80", false}, // cloud metadata endpoint
		{"link-local ipv6", "[fe80::1]:443", false},
		{"link-local ipv6 multicast", "[ff02::1]:443", false},

		// Unspecified.
		{"unspecified ipv4", "0.0.0.0:443", false},
		{"unspecified ipv6", "[::]:443", false},

		// Multicast.
		{"multicast ipv4", "224.0.0.1:443", false},

		// CGNAT 100.64.0.0/10 — stdlib IsPrivate misses it (added, §10.4).
		{"cgnat low", "100.64.0.1:443", false},
		{"cgnat mid", "100.100.50.50:443", false},
		{"cgnat high", "100.127.255.255:443", false},
		{"ipv4-mapped cgnat", "[::ffff:100.64.0.1]:443", false},

		// NAT64 64:ff9b::/96 — maps onto arbitrary IPv4 incl. loopback (added, §10.4).
		{"nat64 loopback", "[64:ff9b::7f00:1]:443", false},
		{"nat64 public-mapped", "[64:ff9b::5db8:d822]:443", false},

		// 0.0.0.0/8 this-network beyond the unspecified address itself (added, §10.4).
		{"this-network nonzero", "0.1.2.3:443", false},
		{"this-network high", "0.255.255.255:443", false},

		// Deprecated IPv4-compatible IPv6 ::/96 (added, §10.4). :: and ::1 are inside
		// this range too but classify as unspecified/loopback first — both blocked.
		{"ipv4-compatible", "[::93.184.216.34]:443", false},
		{"ipv4-compatible private", "[::10.0.0.1]:443", false},

		// IPv6-transition ranges (BG-02d): each embeds an IPv4 address a configured
		// tunnel can reach internally. Blocked regardless of whether a tunnel is present.
		// 6to4 2002::/16 encodes the IPv4 in octets 3–6: 2002:7f00:1:: == 127.0.0.1.
		{"6to4 loopback", "[2002:7f00:1::]:443", false},
		{"6to4 metadata", "[2002:a9fe:a9fe::]:80", false}, // 169.254.169.254
		{"6to4 arbitrary", "[2002:c0a8:1::1]:443", false}, // 192.168.0.1
		// Teredo 2001:0::/32.
		{"teredo", "[2001:0:4136:e378:8000:63bf:3fff:fdd2]:443", false},
		{"teredo low", "[2001:0::1]:443", false},
		// RFC8215 local-use NAT64 64:ff9b:1::/48 (distinct from the well-known /96).
		{"nat64 local-use", "[64:ff9b:1::7f00:1]:443", false},
		{"nat64 local-use high", "[64:ff9b:1:ffff::1]:443", false},
		// A public IPv6 address just OUTSIDE the transition ranges stays allowed.
		{"public below 6to4", "[2001:db8::1]:443", true}, // documentation range, not blocked here
		{"public above 6to4", "[2003::1]:443", true},     // just past 2002::/16
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Screen(tc.address)
			if tc.allow && err != nil {
				t.Fatalf("Screen(%q) = %v, want allowed", tc.address, err)
			}
			if !tc.allow {
				if err == nil {
					t.Fatalf("Screen(%q) allowed, want blocked", tc.address)
				}
				if !errors.Is(err, ErrDisallowedAddress) {
					t.Fatalf("Screen(%q) error = %v, want ErrDisallowedAddress", tc.address, err)
				}
			}
		})
	}
}

func TestScreenRejectsNonIP(t *testing.T) {
	// Control is handed a resolved IP literal; a bare hostname must be refused
	// rather than dialed unscreened.
	err := Screen("internal.example.com:443")
	if !errors.Is(err, ErrDisallowedAddress) {
		t.Fatalf("want ErrDisallowedAddress for a non-IP host, got %v", err)
	}
}

func TestScreenPortlessAddress(t *testing.T) {
	// A bare IP without a port must still be screened (SplitHostPort fails; the
	// whole value is treated as the host).
	if err := Screen("127.0.0.1"); !errors.Is(err, ErrDisallowedAddress) {
		t.Fatalf("want ErrDisallowedAddress for portless loopback, got %v", err)
	}
	if err := Screen("93.184.216.34"); err != nil {
		t.Fatalf("portless public address should be allowed, got %v", err)
	}
}
