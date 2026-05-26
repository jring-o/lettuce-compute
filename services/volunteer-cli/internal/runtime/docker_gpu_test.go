package runtime

import (
	"testing"
)

// TestBuildGPUDeviceRequests_NoGPU verifies no DeviceRequests are produced when
// the config requests no GPU.
func TestBuildGPUDeviceRequests_NoGPU(t *testing.T) {
	cfg := &ContainerConfig{Backend: BackendDocker}
	if reqs := buildGPUDeviceRequests(cfg, true); reqs != nil {
		t.Fatalf("expected nil for no-GPU config, got %+v", reqs)
	}
}

// TestBuildGPUDeviceRequests_DockerWithCDI verifies that on the Docker backend,
// when a CDI spec is present, the GPU is requested via Driver "cdi" with a
// per-index CDI device name (works under the toolkit's default CDI/auto mode).
func TestBuildGPUDeviceRequests_DockerWithCDI(t *testing.T) {
	cfg := &ContainerConfig{Backend: BackendDocker, GPUDeviceIDs: []string{"0"}}
	reqs := buildGPUDeviceRequests(cfg, true)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 device request, got %d", len(reqs))
	}
	if reqs[0].Driver != "cdi" {
		t.Errorf("Driver = %q, want \"cdi\"", reqs[0].Driver)
	}
	if len(reqs[0].DeviceIDs) != 1 || reqs[0].DeviceIDs[0] != "nvidia.com/gpu=0" {
		t.Errorf("DeviceIDs = %v, want [nvidia.com/gpu=0]", reqs[0].DeviceIDs)
	}
}

// TestBuildGPUDeviceRequests_DockerNoCDI verifies that on the Docker backend with
// no CDI spec present, the legacy Driver "nvidia" request is used as a fallback.
func TestBuildGPUDeviceRequests_DockerNoCDI(t *testing.T) {
	cfg := &ContainerConfig{Backend: BackendDocker, GPUDeviceIDs: []string{"0"}}
	reqs := buildGPUDeviceRequests(cfg, false)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 device request, got %d", len(reqs))
	}
	if reqs[0].Driver != "nvidia" {
		t.Errorf("Driver = %q, want \"nvidia\"", reqs[0].Driver)
	}
	if len(reqs[0].DeviceIDs) != 1 || reqs[0].DeviceIDs[0] != "0" {
		t.Errorf("DeviceIDs = %v, want [0]", reqs[0].DeviceIDs)
	}
	if len(reqs[0].Capabilities) != 1 || len(reqs[0].Capabilities[0]) != 1 || reqs[0].Capabilities[0][0] != "gpu" {
		t.Errorf("Capabilities = %v, want [[gpu]]", reqs[0].Capabilities)
	}
}

// TestBuildGPUDeviceRequests_PodmanAlwaysCDI verifies that the Podman backend
// always uses Driver "cdi" regardless of host CDI-spec detection (its
// Docker-compatible API ignores Driver "nvidia").
func TestBuildGPUDeviceRequests_PodmanAlwaysCDI(t *testing.T) {
	cfg := &ContainerConfig{Backend: BackendPodman, GPUDeviceIDs: []string{"1"}}
	reqs := buildGPUDeviceRequests(cfg, false)
	if len(reqs) != 1 || reqs[0].Driver != "cdi" {
		t.Fatalf("expected cdi driver for podman, got %+v", reqs)
	}
	if reqs[0].DeviceIDs[0] != "nvidia.com/gpu=1" {
		t.Errorf("DeviceIDs = %v, want [nvidia.com/gpu=1]", reqs[0].DeviceIDs)
	}
}

// TestBuildGPUDeviceRequests_AllGPUs verifies the "all GPUs" path (no explicit
// device IDs, Count != 0) produces nvidia.com/gpu=all under CDI and Count under
// the legacy fallback.
func TestBuildGPUDeviceRequests_AllGPUs(t *testing.T) {
	cdiCfg := &ContainerConfig{Backend: BackendDocker, GPUCount: -1}
	cdiReqs := buildGPUDeviceRequests(cdiCfg, true)
	if len(cdiReqs) != 1 || cdiReqs[0].Driver != "cdi" ||
		len(cdiReqs[0].DeviceIDs) != 1 || cdiReqs[0].DeviceIDs[0] != "nvidia.com/gpu=all" {
		t.Errorf("CDI all-GPUs request = %+v, want cdi/[nvidia.com/gpu=all]", cdiReqs)
	}

	legacyCfg := &ContainerConfig{Backend: BackendDocker, GPUCount: -1}
	legacyReqs := buildGPUDeviceRequests(legacyCfg, false)
	if len(legacyReqs) != 1 || legacyReqs[0].Driver != "nvidia" || legacyReqs[0].Count != -1 {
		t.Errorf("legacy all-GPUs request = %+v, want nvidia/Count=-1", legacyReqs)
	}
}

// TestNvidiaCDIAvailable verifies CDI-spec detection reuses the cdiSpecPaths
// locations via the overridable fileExists hook.
func TestNvidiaCDIAvailable(t *testing.T) {
	withMockFileExists(t, noFilesExist)
	if nvidiaCDIAvailable() {
		t.Error("expected false when no CDI spec present")
	}

	withMockFileExists(t, func(path string) bool { return path == "/etc/cdi/nvidia.yaml" })
	if !nvidiaCDIAvailable() {
		t.Error("expected true when /etc/cdi/nvidia.yaml present")
	}
}
