package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config is the top-level configuration for the Lettuce infrastructure server.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	TLS      TLSConfig      `yaml:"tls"`
	Signing  SigningConfig   `yaml:"signing"`
	Storage  StorageConfig  `yaml:"storage"`
	Head     HeadConfig     `yaml:"head"`
}

// HeadConfig defines the identity for this Lettuce server (head).
// A head is a single infrastructure deployment — one server = one head = many leafs.
type HeadConfig struct {
	Name                    string         `yaml:"name"`
	Description             string         `yaml:"description"`
	URL                     string         `yaml:"url"`
	DefaultLeafWeights      map[string]int `yaml:"default_leaf_weights"`
	MaxInflightPerVolunteer int            `yaml:"max_inflight_per_volunteer"`
}

// Validate checks HeadConfig for required fields and valid values.
func (h HeadConfig) Validate() error {
	if h.Name == "" {
		return fmt.Errorf("head.name is required")
	}
	if h.URL != "" {
		u, err := url.Parse(h.URL)
		if err != nil {
			return fmt.Errorf("head.url is invalid: %w", err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("head.url must include scheme and host (e.g. https://your-domain.com)")
		}
	}
	for slug, weight := range h.DefaultLeafWeights {
		if weight <= 0 {
			return fmt.Errorf("head.default_leaf_weights[%q] must be > 0, got %d", slug, weight)
		}
	}
	if h.MaxInflightPerVolunteer < 0 {
		return fmt.Errorf("head.max_inflight_per_volunteer must be >= 0, got %d", h.MaxInflightPerVolunteer)
	}
	return nil
}

// EffectiveMaxInflight returns the max inflight WUs per volunteer,
// defaulting to 10 if not set (0).
func (h HeadConfig) EffectiveMaxInflight() int {
	if h.MaxInflightPerVolunteer <= 0 {
		return 10
	}
	return h.MaxInflightPerVolunteer
}

// StorageConfig defines local filesystem storage settings.
type StorageConfig struct {
	CheckpointDir string `yaml:"checkpoint_dir"`
}

// SigningConfig defines the Ed25519 signing key used for credit attestations.
type SigningConfig struct {
	PrivateKeyPath string `yaml:"private_key_path"`
	// AutoGenerate, when true, lets the server generate and persist a new
	// ephemeral signing key if the configured key file is missing. This is a
	// development-only convenience: in production the key is the platform's
	// external trust anchor and must be pre-generated. Defaults to false
	// (fail closed). Override via LETTUCE_SIGNING_KEY_AUTOGEN=true.
	AutoGenerate bool `yaml:"autogenerate"`
}

// ServerConfig defines listen addresses for HTTP and gRPC servers.
type ServerConfig struct {
	HTTPAddr    string `yaml:"http_addr"`
	GRPCAddr    string `yaml:"grpc_addr"`
	CORSOrigins string `yaml:"cors_origins"`
	// TrustedProxies is a comma-separated list of CIDRs and/or bare IPs of
	// reverse proxies whose X-Forwarded-For / X-Real-IP headers may be trusted
	// for client-IP extraction. Bare IPs are treated as /32 (IPv4) or /128
	// (IPv6). EMPTY by default: when empty, forwarding headers are never trusted
	// and the direct peer (RemoteAddr) is always used. Override via
	// LETTUCE_TRUSTED_PROXIES.
	TrustedProxies string `yaml:"trusted_proxies"`
}

// ParsedTrustedProxies parses the TrustedProxies string into a slice of
// *net.IPNet. Comma-separated entries may be CIDRs (e.g. "10.0.0.0/8") or bare
// IPs (e.g. "172.18.0.5"), where a bare IP is treated as a /32 (IPv4) or /128
// (IPv6) network. Empty or whitespace-only entries are skipped. Returns an
// error on the first malformed entry. An empty input yields a nil slice
// (the secure default: no header trust).
func (s ServerConfig) ParsedTrustedProxies() ([]*net.IPNet, error) {
	return ParseTrustedProxies(s.TrustedProxies)
}

// ParseTrustedProxies parses a comma-separated list of CIDRs and/or bare IPs
// into *net.IPNet networks. See ServerConfig.ParsedTrustedProxies for semantics.
func ParseTrustedProxies(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Try CIDR first.
		if _, ipNet, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, ipNet)
			continue
		}
		// Fall back to a bare IP → /32 or /128.
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("trusted_proxies: invalid CIDR or IP %q", entry)
		}
		mask := net.CIDRMask(32, 32)
		if ip.To4() == nil {
			mask = net.CIDRMask(128, 128)
		}
		nets = append(nets, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
	}
	return nets, nil
}

// DatabaseConfig defines PostgreSQL connection parameters.
type DatabaseConfig struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	Database        string `yaml:"database"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	SSLMode         string `yaml:"ssl_mode"`
	MaxConns        int    `yaml:"max_conns"`
	MinConns        int    `yaml:"min_conns"`
	MaxConnLifetime string `yaml:"max_conn_lifetime"`
	MaxConnIdleTime string `yaml:"max_conn_idle_time"`
}

// DatabaseURL returns a pgx-compatible connection string.
func (d DatabaseConfig) DatabaseURL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password),
		Host:   fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:   d.Database,
	}
	q := u.Query()
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// LogConfig defines logging behavior.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// TLSConfig defines TLS certificate paths.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}
