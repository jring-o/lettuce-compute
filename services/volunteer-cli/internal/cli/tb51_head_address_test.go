package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TB-51: the desktop wizard hands `init --server` exactly what the volunteer
// typed, and its own placeholder invited "https://…". That string used to be
// stored verbatim as the gRPC target (which gRPC resolves to nothing), the
// head's name became "https" and its HTTP address "https://https". Every
// accepted form must now land on the same, connectable entry.
func TestInitServerFlag_NormalizesTypedAddress(t *testing.T) {
	withContainerBackend(t)

	cases := []struct {
		name     string
		server   string
		grpc     string
		http     string
		headName string
		insecure bool
	}{
		{"https URL with trailing slash", "https://h.example/", "h.example:443", "https://h.example", "h.example", false},
		{"bare host", "h.example", "h.example:443", "https://h.example", "h.example", false},
		{"host with port", "h.example:8443", "h.example:8443", "https://h.example:8443", "h.example", false},
		{"http URL is a plain-text head", "http://localhost:9090", "localhost:9090", "http://localhost:9090", "localhost", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgFile := filepath.Join(dir, "config.yaml")
			cmd := newRootCmd()
			cmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir,
				"--cpu-cores", "2", "--memory-mb", "2048", "--server", tc.server})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			captureStdout(t, func() {
				if err := cmd.Execute(); err != nil {
					t.Fatalf("init failed: %v", err)
				}
			})

			loaded, err := config.Load(cfgFile)
			if err != nil {
				t.Fatalf("loading config: %v", err)
			}
			if len(loaded.Servers) != 1 {
				t.Fatalf("servers = %v, want one", loaded.Servers)
			}
			s := loaded.Servers[0]
			if s.GRPCAddress != tc.grpc {
				t.Errorf("grpc_address = %q, want %q", s.GRPCAddress, tc.grpc)
			}
			if s.HTTPAddress != tc.http {
				t.Errorf("http_address = %q, want %q", s.HTTPAddress, tc.http)
			}
			if s.Name != tc.headName {
				t.Errorf("name = %q, want %q", s.Name, tc.headName)
			}
			if s.Insecure != tc.insecure {
				t.Errorf("insecure = %v, want %v", s.Insecure, tc.insecure)
			}
		})
	}
}

// An address that cannot name a head is refused by init with the reason,
// instead of being written to the config for the daemon to choke on.
func TestInitServerFlag_RefusesUnusableAddress(t *testing.T) {
	withContainerBackend(t)
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir,
		"--cpu-cores", "2", "--memory-mb", "2048", "--server", "ftp://h.example"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	var err error
	captureStdout(t, func() { err = cmd.Execute() })
	if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("init accepted ftp://: err = %v", err)
	}
}

// `attach --server` used to append ":443" to whatever was typed, so a URL
// became "https://host:443" and the dial failed with "too many colons". The
// typed address and the port flags are now reconciled up front.
func TestResolveAttachTarget(t *testing.T) {
	cases := []struct {
		name                       string
		server                     string
		grpcPort, httpPort         int
		grpcGiven, httpGiven, insc bool
		want                       attachTarget
		wantErr                    string
	}{
		{"https URL, default ports", "https://h.example/", 443, 443, false, false, false,
			attachTarget{host: "h.example", grpcPort: 443, httpPort: 443}, ""},
		{"bare host with flags", "h.example", 9090, 8080, true, true, false,
			attachTarget{host: "h.example", grpcPort: 9090, httpPort: 8080}, ""},
		{"port in the address applies to both sides", "h.example:8443", 443, 443, false, false, false,
			attachTarget{host: "h.example", grpcPort: 8443, httpPort: 8443}, ""},
		{"explicit --http-port beats the address port for HTTP", "h.example:9090", 443, 8080, false, true, false,
			attachTarget{host: "h.example", grpcPort: 9090, httpPort: 8080}, ""},
		{"same gRPC port both ways is fine", "h.example:9090", 9090, 443, true, false, false,
			attachTarget{host: "h.example", grpcPort: 9090, httpPort: 9090}, ""},
		{"conflicting gRPC ports are refused", "h.example:9090", 9091, 443, true, false, false,
			attachTarget{}, "give the port once"},
		{"http:// implies insecure", "http://localhost:9090", 443, 443, false, false, false,
			attachTarget{host: "localhost", grpcPort: 9090, httpPort: 9090, insecure: true}, ""},
		{"--insecure survives an https input", "https://localhost", 443, 443, false, false, true,
			attachTarget{host: "localhost", grpcPort: 443, httpPort: 443, insecure: true}, ""},
		{"unusable input", "grpc://h.example", 443, 443, false, false, false,
			attachTarget{}, "unsupported scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAttachTarget(tc.server, tc.grpcPort, tc.httpPort, tc.grpcGiven, tc.httpGiven, tc.insc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("target = %+v, want %+v", got, tc.want)
			}
		})
	}
}
