package config

import (
	"strings"
	"testing"
)

// BG-34 regression: a downgrade-able sslmode (disable/allow/prefer) against a
// database host that is NOT on a private network must produce a boot warning;
// the same mode against loopback/private addresses (the bundled compose
// topology) must stay silent, and strict modes never warn. Table-driven, no
// database needed — IP-literal hosts skip DNS entirely.
func TestInsecureSSLModeWarning_BG34(t *testing.T) {
	tests := []struct {
		name     string
		sslMode  string
		host     string
		wantWarn bool
	}{
		// Weak mode + public host: warn.
		{"disable public IPv4", "disable", "8.8.8.8", true},
		{"prefer public IPv4", "prefer", "1.1.1.1", true},
		{"allow public IPv4", "allow", "8.8.8.8", true},
		{"disable public IPv6", "disable", "2001:4860:4860::8888", true},

		// Weak mode + non-public host: silent (the compose topology).
		{"disable loopback", "disable", "127.0.0.1", false},
		{"disable rfc1918 10/8", "disable", "10.1.2.3", false},
		{"disable rfc1918 172.16/12", "disable", "172.20.0.5", false},
		{"disable rfc1918 192.168/16", "disable", "192.168.1.10", false},
		{"disable cgnat", "disable", "100.64.0.7", false},
		{"prefer IPv6 loopback", "prefer", "::1", false},
		{"prefer IPv6 ULA", "prefer", "fd12:3456::1", false},
		{"disable localhost name", "disable", "localhost", false},

		// Strict modes: never warn, even on a public host.
		{"require public", "require", "8.8.8.8", false},
		{"verify-ca public", "verify-ca", "8.8.8.8", false},
		{"verify-full public", "verify-full", "8.8.8.8", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DatabaseConfig{Host: tt.host, SSLMode: tt.sslMode}
			warn := d.InsecureSSLModeWarning()
			if tt.wantWarn && warn == "" {
				t.Fatalf("ssl_mode=%q host=%q: expected a warning, got none", tt.sslMode, tt.host)
			}
			if !tt.wantWarn && warn != "" {
				t.Fatalf("ssl_mode=%q host=%q: expected silence, got warning: %s", tt.sslMode, tt.host, warn)
			}
			if tt.wantWarn {
				// The warning must be actionable: name the remedy and the guide.
				if !strings.Contains(warn, "verify-full") {
					t.Errorf("warning does not name the verify-full remedy: %s", warn)
				}
				if !strings.Contains(warn, tt.host) {
					t.Errorf("warning does not name the offending host: %s", warn)
				}
			}
		})
	}
}

// An unresolvable host warns too: the check could not establish the host is
// private, and a name that later resolves publicly carries the full downgrade
// hazard. RFC 6761 reserves .invalid to never resolve. (If a hijacking
// resolver answers it anyway, it answers with a public ad-server address —
// which also warns, so the assertion holds either way.)
func TestInsecureSSLModeWarning_UnresolvableHostWarns_BG34(t *testing.T) {
	d := DatabaseConfig{Host: "db.host.invalid", SSLMode: "prefer"}
	if warn := d.InsecureSSLModeWarning(); warn == "" {
		t.Fatal("expected a warning for a weak sslmode with an unresolvable host, got none")
	}
}
