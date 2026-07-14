package leaf

import (
	"strings"
	"testing"
)

// TestSSRFURLScreening is the BG-14 / ★BG-14c exit test (e): the head rejects a leaf
// whose binary, module, viz, or external input URL points at an internal address —
// 0.0.0.0, loopback/metadata, private, or CGNAT — across ALL runtimes, not just the
// native/wasm binary. It fails on the pre-fix code, which (a) missed 0.0.0.0
// entirely (only IsPrivate/IsLoopback/IsLinkLocalUnicast), (b) never screened the
// "viz" URL for WASM/CONTAINER leaves, and (c) never screened external_base_url.
func TestSSRFURLScreening(t *testing.T) {
	if binaryURLAllowInsecure {
		t.Skip("LETTUCE_BINARY_URL_ALLOW_INSECURE is set; SSRF screening is intentionally disabled")
	}

	// (1) The shared URL screen: internal literals rejected across the full netguard
	// range, a public https FQDN accepted.
	for _, u := range []string{
		"https://0.0.0.0/bin",         // unspecified — missed pre-fix
		"https://169.254.169.254/bin", // link-local / cloud metadata
		"https://10.0.0.1/bin",        // private
		"https://127.0.0.1/bin",       // loopback
		"https://100.64.0.1/bin",      // CGNAT — missed pre-fix
	} {
		if err := validateBinaryURL(u); err == nil {
			t.Errorf("validateBinaryURL(%q) = nil, want rejection", u)
		}
	}
	if err := validateBinaryURL("https://cdn.example.com/artifact"); err != nil {
		t.Errorf("validateBinaryURL(public https FQDN) = %v, want nil", err)
	}

	// (2) A WASM leaf whose VIZ url is internal must be rejected — viz was never
	// screened for non-native runtimes before this change (★BG-14c). Flipping only
	// the viz URL between the reject and accept cases proves it is the screened field.
	wasmLeaf := func(vizURL string) *ExecutionConfig {
		return &ExecutionConfig{
			Runtime: RuntimeWasm,
			Binaries: map[string]string{
				"wasm": "https://cdn.example.com/module.wasm",
				"viz":  vizURL,
			},
			MaxMemoryMB:   4096,
			MaxDiskMB:     10240,
			MaxCPUSeconds: 86400,
		}
	}
	if err := ValidateExecutionConfig(wasmLeaf("https://169.254.169.254/viz.tar")); err == nil {
		t.Error("ValidateExecutionConfig accepted a WASM leaf with an internal viz URL")
	}
	if err := ValidateExecutionConfig(wasmLeaf("https://cdn.example.com/viz.tar")); err != nil {
		t.Errorf("ValidateExecutionConfig rejected an otherwise-valid WASM+viz leaf: %v", err)
	}

	// (3) The external input base URL (the leaf's data source) must be screened too.
	extData := func(base string) *DataConfig {
		return &DataConfig{
			TransferStrategy:   TransferExternalReference,
			ExternalBaseURL:    &base,
			AggregationFormat:  "JSON",
			MaxInputSizeBytes:  1048576,
			MaxOutputSizeBytes: 104857600,
		}
	}
	err := ValidateDataConfig(extData("https://169.254.169.254/data"), PatternParameterSweep, false)
	if err == nil {
		t.Error("ValidateDataConfig accepted an internal external_base_url")
	} else if !strings.Contains(strings.ToLower(err.Error()), "external_base_url") {
		t.Errorf("expected an external_base_url rejection, got: %v", err)
	}
}

// TestImageRegistrySSRFScreening is the BG-14d exit test: a container leaf whose
// image reference names an INTERNAL registry authority (an IP literal in a blocked
// range, or localhost) is rejected head-side. The container image pull runs through
// the Docker/Podman engine, outside the daemon's netguard dial screen, so an
// unscreened registry like 169.254.169.254/repo would let the engine reach cloud
// metadata. Fails on the pre-fix code, which validated only the ociImageRefRegex
// shape (169.254.169.254/repo matches it) and never screened the registry host.
func TestImageRegistrySSRFScreening(t *testing.T) {
	if binaryURLAllowInsecure {
		t.Skip("LETTUCE_BINARY_URL_ALLOW_INSECURE is set; SSRF screening is intentionally disabled")
	}

	// Direct unit coverage of the registry-host classifier.
	blocked := []string{
		"169.254.169.254/repo",          // cloud metadata, bare IPv4 registry
		"169.254.169.254:5000/repo:tag", // metadata with an explicit port
		"127.0.0.1:5000/repo",           // loopback registry
		"10.0.0.1/team/repo:tag",        // private registry
		"[::1]:5000/repo",               // IPv6 loopback registry
		"localhost:5000/repo",           // localhost registry
		"[2002:7f00:1::]:5000/repo",     // 6to4-embedded loopback (BG-02d range)
	}
	for _, ref := range blocked {
		if err := validateImageRegistryHost(ref); err == nil {
			t.Errorf("validateImageRegistryHost(%q) = nil, want rejection", ref)
		}
	}
	allowed := []string{
		"ubuntu:22.04",                                        // default registry, single name
		"library/ubuntu:22.04",                                // default registry, namespaced
		"registry.example.com/team/repo:tag",                  // public registry FQDN
		"ghcr.io/org/image@sha256:" + strings.Repeat("a", 64), // digest-pinned
	}
	for _, ref := range allowed {
		if err := validateImageRegistryHost(ref); err != nil {
			t.Errorf("validateImageRegistryHost(%q) = %v, want allowed", ref, err)
		}
	}

	// Integration through ValidateExecutionConfig: flipping only the image between an
	// internal registry and a public one proves it is the screened field.
	containerLeaf := func(image string) *ExecutionConfig {
		img := image
		return &ExecutionConfig{
			Runtime:       RuntimeContainer,
			Image:         &img,
			MaxMemoryMB:   4096,
			MaxDiskMB:     10240,
			MaxCPUSeconds: 86400,
		}
	}
	if err := ValidateExecutionConfig(containerLeaf("169.254.169.254/repo:tag")); err == nil {
		t.Error("ValidateExecutionConfig accepted a container leaf with an internal registry")
	}
	if err := ValidateExecutionConfig(containerLeaf("registry.example.com/team/repo:tag")); err != nil {
		t.Errorf("ValidateExecutionConfig rejected a container leaf with a public registry: %v", err)
	}
}
