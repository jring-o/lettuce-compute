package runtime

import "testing"

// TB-73: the Docker probe finds whatever answers the Docker socket and used to
// record no version for it, so the app's runtime card had nothing to show
// beside the engine's name. The server's own version now rides along with the
// engine name (Podman's version on a Podman-served socket).
func TestTB73_DockerProbeCarriesTheServerVersion(t *testing.T) {
	orig := dockerEngineFunc
	t.Cleanup(func() { dockerEngineFunc = orig })
	dockerEngineFunc = func() (string, string) { return "podman", "5.3.1" }

	info := dockerBackendInfo()
	if info.Backend != BackendDocker || info.Engine != "podman" {
		t.Fatalf("dockerBackendInfo() = %+v, want backend docker served by podman", info)
	}
	if info.Version != "5.3.1" {
		t.Errorf("Version = %q, want the server's 5.3.1 (TB-73)", info.Version)
	}
}
