package runtime

import (
	"os/exec"
	"reflect"
	"testing"
)

// withDetectGOOS makes the install-location probe behave as on goos for the
// duration of the test, whatever host the test runs on.
func withDetectGOOS(t *testing.T, goos string) {
	t.Helper()
	orig := detectGOOS
	t.Cleanup(func() { detectGOOS = orig })
	detectGOOS = goos
}

// withPresentFiles makes exactly the given paths "exist" for the
// install-location scan, so a macOS path can be present on any host.
func withPresentFiles(t *testing.T, present ...string) {
	t.Helper()
	withMockFileExists(t, func(path string) bool {
		for _, p := range present {
			if p == path {
				return true
			}
		}
		return false
	})
}

// withRealPodmanInstallPath restores the real install-location scan, which the
// package's TestMain stubs out so ordinary tests never find the host's Podman.
func withRealPodmanInstallPath(t *testing.T) {
	t.Helper()
	orig := podmanInstallPathFunc
	t.Cleanup(func() { podmanInstallPathFunc = orig })
	podmanInstallPathFunc = defaultPodmanInstallPath
}

// withMockDockerEngine overrides the engine-name probe behind the Docker socket.
func withMockDockerEngine(t *testing.T, engine string) {
	t.Helper()
	orig := dockerEngineFunc
	t.Cleanup(func() { dockerEngineFunc = orig })
	dockerEngineFunc = func() (string, string) { return engine, "" }
}

// TB-54: a Finder-launched app runs with /usr/bin:/bin:/usr/sbin:/sbin, so PATH
// lookup never sees the official installer's /opt/podman/bin (added to login
// shells via /etc/paths.d) or Homebrew's prefix. The daemon reported "no
// container runtime detected" beside a Terminal where `podman info` worked,
// and registered the host WASM-only.
func TestDetectPodman_DarwinFindsInstallLocationOffPath(t *testing.T) {
	withDetectGOOS(t, "darwin")
	withRealPodmanInstallPath(t)
	withMockLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	withPresentFiles(t, "/opt/podman/bin/podman")
	withMockDockerAvailable(t, false)
	withMockExecutor(t, func(name string, args ...string) ([]byte, error) {
		if name == "/opt/podman/bin/podman" && len(args) > 0 && args[0] == "--version" {
			return []byte("podman version 5.8.0\n"), nil
		}
		return nil, exec.ErrNotFound
	})

	info := DetectContainerBackendPreferred("", "")
	if info.Backend != BackendPodman {
		t.Fatalf("Backend = %s, want podman found at its macOS install location", info.Backend)
	}
	if info.BinaryPath != "/opt/podman/bin/podman" {
		t.Errorf("BinaryPath = %q, want /opt/podman/bin/podman", info.BinaryPath)
	}
	if info.Version != "5.8.0" {
		t.Errorf("Version = %q, want 5.8.0 (asked of the binary by absolute path)", info.Version)
	}
}

// The macOS list covers the official installer and both Homebrew prefixes, in
// that order; Linux relies on PATH alone.
func TestPodmanInstallCandidates_PerPlatform(t *testing.T) {
	darwin := podmanInstallCandidates("darwin")
	want := []string{"/opt/podman/bin/podman", "/opt/homebrew/bin/podman", "/usr/local/bin/podman"}
	if !reflect.DeepEqual(darwin, want) {
		t.Errorf("darwin candidates = %v, want %v", darwin, want)
	}
	if got := podmanInstallCandidates("linux"); len(got) != 0 {
		t.Errorf("linux candidates = %v, want none (PATH covers distro packages)", got)
	}
	windows := podmanInstallCandidates("windows")
	if len(windows) == 0 || windows[len(windows)-1] != `C:\Program Files\RedHat\Podman\podman.exe` {
		t.Errorf("windows candidates = %v, want the machine-scope MSI location last", windows)
	}
}

// With no binary anywhere and Podman Desktop's Docker-compatibility socket
// present, the Docker probe wins; the result must say that Podman is what
// answers, so the log and doctor stop calling it Docker.
func TestDetectContainerBackend_DockerSocketServedByPodmanIsLabelled(t *testing.T) {
	withDetectGOOS(t, "darwin")
	withRealPodmanInstallPath(t)
	withMockLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	withPresentFiles(t)
	withMockDockerAvailable(t, true)
	withMockDockerEngine(t, "podman")
	withMockExecutor(t, notFoundForAll)

	info := DetectContainerBackendPreferred("", "")
	if info.Backend != BackendDocker {
		t.Fatalf("Backend = %s, want docker (the socket is the only thing found)", info.Backend)
	}
	if info.Engine != "podman" {
		t.Errorf("Engine = %q, want podman", info.Engine)
	}

	// A preferred-Docker probe carries the label too.
	info = DetectContainerBackendPreferred("", BackendDocker)
	if info.Backend != BackendDocker || info.Engine != "podman" {
		t.Errorf("preferred docker: got %+v, want docker served by podman", info)
	}
}

// The classifier reads the server's version report: Podman's compatibility API
// lists a "Podman Engine" component, Docker lists "Engine" and its platform.
func TestEngineNameFromVersion(t *testing.T) {
	cases := []struct {
		platform   string
		components []string
		want       string
	}{
		{"linux/amd64/fedora-40", []string{"Podman Engine", "Conmon", "OCI Runtime (crun)"}, "podman"},
		{"Docker Engine - Community", []string{"Engine", "containerd", "runc", "docker-init"}, "docker"},
		{"Podman Desktop", nil, "podman"},
		{"", nil, "docker"},
	}
	for _, tc := range cases {
		if got := engineNameFromVersion(tc.platform, tc.components); got != tc.want {
			t.Errorf("engineNameFromVersion(%q, %v) = %q, want %q", tc.platform, tc.components, got, tc.want)
		}
	}
}
