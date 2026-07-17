package config

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/netguard"
)

// weakSSLModes are the Postgres sslmode values that permit plaintext on the
// wire: "disable" never encrypts, and "allow"/"prefer" negotiate TLS but fall
// back to (or start from) plaintext when the server side declines — so an
// active on-path attacker can strip encryption without either end noticing.
// The stricter modes (require, verify-ca, verify-full) refuse plaintext
// outright.
var weakSSLModes = map[string]bool{"disable": true, "allow": true, "prefer": true}

// sslModeLookupTimeout bounds the single boot-time DNS lookup the warning
// check performs so a slow resolver can never stall boot.
const sslModeLookupTimeout = 5 * time.Second

// InsecureSSLModeWarning returns a non-empty operator-facing warning when the
// configured sslmode permits a plaintext downgrade AND the database host is
// not confined to loopback/private-network addresses (BG-34). It returns ""
// — no warning — when the mode is strict, or when every address the host
// classifies/resolves to is non-public (loopback, RFC1918, ULA, CGNAT, ...;
// the classification is netguard's, the head's single source of truth for
// "not internet-routable"). The bundled compose topology (sslmode=disable
// against Postgres on the private bridge network) is therefore silent; the
// hazard this catches is an operator pointing the head at an EXTERNAL
// Postgres while inheriting a downgrade-able mode. An unresolvable host warns
// too: privacy could not be established, and if the name later resolves
// publicly the downgrade hazard is real.
func (d DatabaseConfig) InsecureSSLModeWarning() string {
	if !weakSSLModes[d.SSLMode] {
		return ""
	}

	// IP literal: classify directly, no DNS involved.
	if ip := net.ParseIP(d.Host); ip != nil {
		if netguard.DisallowedIPReason(ip) != "" {
			return ""
		}
		return d.sslModeWarning([]string{ip.String()})
	}

	ctx, cancel := context.WithTimeout(context.Background(), sslModeLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, d.Host)
	if err != nil {
		return fmt.Sprintf("database ssl_mode %q permits a plaintext downgrade and host %q could not be resolved to confirm it is on a private network (%v); "+
			"use ssl_mode verify-full (LETTUCE_DB_SSL_MODE=verify-full) for any Postgres not on a loopback/private network — see guides/head-setup.md",
			d.SSLMode, d.Host, err)
	}

	var public []string
	for _, a := range addrs {
		if netguard.DisallowedIPReason(a.IP) == "" {
			public = append(public, a.IP.String())
		}
	}
	if len(public) == 0 {
		return ""
	}
	sort.Strings(public)
	return d.sslModeWarning(public)
}

func (d DatabaseConfig) sslModeWarning(publicAddrs []string) string {
	return fmt.Sprintf("database ssl_mode %q permits a silent plaintext downgrade and host %q is not on a private network (resolves to %s): "+
		"an on-path attacker can read or modify database traffic, including credentials. "+
		"Use ssl_mode verify-full (LETTUCE_DB_SSL_MODE=verify-full) for any Postgres not on a loopback/private network — see guides/head-setup.md",
		d.SSLMode, d.Host, strings.Join(publicAddrs, ", "))
}
