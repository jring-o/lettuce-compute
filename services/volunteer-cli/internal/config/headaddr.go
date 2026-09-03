package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// HeadAddress is a head's connection target as derived from whatever the
// volunteer typed: a bare host ("compute.example.org"), a host with a port
// ("compute.example.org:8443"), or a URL with a scheme and an optional path
// ("https://compute.example.org/"). Every path that stores a head goes through
// ParseHeadAddress so the same input yields the same config entry whether it
// came from `init --server`, `attach --server`, or the desktop app's attach
// request (TB-51: a URL used to be stored verbatim as the gRPC target, which
// gRPC cannot resolve, while the head's name came out as "https").
type HeadAddress struct {
	// Host is the bare host name (or IP literal), lower-cased. It doubles as the
	// head's default display name and the TLS server name.
	Host string
	// Port is the port the input carried, or 0 when it carried none. One
	// address names one port: when given it applies to gRPC and HTTP alike,
	// which matches a production head (everything multiplexed on 443).
	Port int
	// Insecure is true for an http:// input — the head serves plain HTTP, so
	// its gRPC is assumed plain as well.
	Insecure bool
}

// ParseHeadAddress normalises a typed head address. It accepts:
//
//   - a host name or IP: "compute.example.org", "192.0.2.10", "[2001:db8::1]"
//   - the same with a port: "compute.example.org:8443"
//   - an http:// or https:// URL with an optional path, query or fragment,
//     which are dropped: "https://compute.example.org/"
//
// Anything else (another scheme, credentials, an empty host, a bad port) is
// refused with a message that names the acceptable form.
func ParseHeadAddress(input string) (HeadAddress, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return HeadAddress{}, fmt.Errorf("head address is required (the head's host name, e.g. compute.example.org)")
	}

	var insecure bool
	withScheme := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		switch strings.ToLower(raw[:i]) {
		case "https":
		case "http":
			insecure = true
		default:
			return HeadAddress{}, fmt.Errorf("unsupported scheme %q in head address %q: give the head's host name, e.g. compute.example.org", raw[:i], raw)
		}
	} else {
		withScheme = "https://" + raw
	}

	u, err := url.Parse(withScheme)
	if err != nil {
		return HeadAddress{}, fmt.Errorf("invalid head address %q: %w", raw, err)
	}
	if u.User != nil {
		return HeadAddress{}, fmt.Errorf("invalid head address %q: credentials are not part of a head address", raw)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return HeadAddress{}, fmt.Errorf("invalid head address %q: no host name", raw)
	}

	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return HeadAddress{}, fmt.Errorf("invalid head address %q: port %q is not in 1-65535", raw, p)
		}
		port = n
	}

	return HeadAddress{Host: host, Port: port, Insecure: insecure}, nil
}

// defaultPort is the scheme's own port, used when the input carried none.
func (a HeadAddress) defaultPort() int {
	if a.Insecure {
		return 80
	}
	return 443
}

// EffectivePort is the port the address resolves to: the one it carried, or
// the scheme's default.
func (a HeadAddress) EffectivePort() int {
	if a.Port != 0 {
		return a.Port
	}
	return a.defaultPort()
}

// GRPCAddress is the "host:port" gRPC target for this head.
func (a HeadAddress) GRPCAddress() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(a.EffectivePort()))
}

// HTTPAddress is the base URL of the head's HTTP API. The port is omitted when
// it is the scheme's default, so a production head reads "https://host".
func (a HeadAddress) HTTPAddress() string {
	scheme := "https"
	if a.Insecure {
		scheme = "http"
	}
	if a.EffectivePort() == a.defaultPort() {
		return fmt.Sprintf("%s://%s", scheme, a.hostForURL())
	}
	return fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(a.Host, strconv.Itoa(a.EffectivePort())))
}

// hostForURL brackets an IPv6 literal for use in a URL without a port; with a
// port net.JoinHostPort does it.
func (a HeadAddress) hostForURL() string {
	if strings.Contains(a.Host, ":") {
		return "[" + a.Host + "]"
	}
	return a.Host
}
