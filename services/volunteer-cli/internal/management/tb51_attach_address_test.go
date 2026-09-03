package management

import (
	"net/http"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// TB-51: the desktop app's Add Server dialog posts whatever the volunteer
// typed, and its own Test Connection accepts "https://host/". The attach
// handler used to store that string verbatim as the gRPC target — which gRPC
// resolves to nothing — with no HTTP address at all. The stored entry must be
// the same one `init`/`attach` would write for that head.
func TestAttachLeaf_NormalizesTypedAddress(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach", `{"server_address":"https://h.example/"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var found *config.ServerConfig
	for i := range cfg.Servers {
		if cfg.Servers[i].Name == "h.example" {
			found = &cfg.Servers[i]
		}
	}
	if found == nil {
		names := make([]string, 0, len(cfg.Servers))
		for _, s := range cfg.Servers {
			names = append(names, s.Name+"="+s.GRPCAddress)
		}
		t.Fatalf("no head named h.example on disk; have %v", names)
	}
	if found.GRPCAddress != "h.example:443" {
		t.Errorf("grpc_address = %q, want h.example:443", found.GRPCAddress)
	}
	if found.HTTPAddress != "https://h.example" {
		t.Errorf("http_address = %q, want https://h.example", found.HTTPAddress)
	}
	if found.Insecure {
		t.Error("an https:// input must not be stored as insecure")
	}
}

// The duplicate check compares normalised gRPC targets, so the same head typed
// two ways is still one head.
func TestAttachLeaf_DuplicateDetectedAcrossSpellings(t *testing.T) {
	env := setupTestEnv(t)

	first := env.doRequest(t, "POST", "/api/v1/leafs/attach", `{"server_address":"h2.example.org"}`)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first attach: expected 200, got %d", first.StatusCode)
	}
	first.Body.Close()

	second := env.doRequest(t, "POST", "/api/v1/leafs/attach", `{"server_address":"https://H2.example.org:443/"}`)
	if second.StatusCode == http.StatusOK {
		t.Fatalf("second spelling of the same head was attached again")
	}
	second.Body.Close()
}

// An input that cannot name a head is refused and nothing is written.
func TestAttachLeaf_RefusesUnusableAddress(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.doRequest(t, "POST", "/api/v1/leafs/attach", `{"server_address":"ftp://h.example"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("ftp:// was accepted as a head address")
	}
	resp.Body.Close()
	cfg, _ := config.Load(env.cfgPath)
	for _, s := range cfg.Servers {
		if s.Name == "h.example" || s.GRPCAddress == "ftp://h.example" {
			t.Error("an unusable address was written to the config")
		}
	}
}
