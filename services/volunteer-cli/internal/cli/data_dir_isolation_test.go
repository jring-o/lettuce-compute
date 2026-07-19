package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
)

// PB-1 / PB-8 regression coverage: an explicit --data-dir names an ISOLATED
// profile. init must create/modify that profile's config (<data-dir>/config.yaml),
// never the default profile's ~/.lettuce/config.yaml; the saved profile must be
// self-contained (no paths into the default profile); and a relative --data-dir
// must be absolutized before anything derives paths from it (a relative value
// resolved later against a compute child's working directory broke every native
// execution).

// withFakeHome points the OS home-dir lookup at a temp dir for the test, so the
// "default profile" path (~/.lettuce) is test-controlled on every platform.
func withFakeHome(t *testing.T) string {
	t.Helper()
	fakeHome := t.TempDir()
	t.Setenv("USERPROFILE", fakeHome) // Windows
	t.Setenv("HOME", fakeHome)        // Unix
	return fakeHome
}

func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestInitWithDataDirDoesNotTouchDefaultProfile(t *testing.T) {
	fakeHome := withFakeHome(t)

	// A pre-existing DEFAULT profile with known bytes.
	defaultDir := filepath.Join(fakeHome, ".lettuce")
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	defaultCfgPath := filepath.Join(defaultDir, "config.yaml")
	original := []byte("log_level: info\nmax_concurrent_tasks: 1\n# default-profile marker\n")
	if err := os.WriteFile(defaultCfgPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	profileDir := filepath.Join(t.TempDir(), "profile")
	if err := runCLI(t, "init", "--data-dir", profileDir, "--cpu-cores", "2", "--memory-mb", "1024", "--disk-gb", "5"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// The profile got its own config...
	profileCfg := filepath.Join(profileDir, "config.yaml")
	if _, err := os.Stat(profileCfg); err != nil {
		t.Errorf("init --data-dir did not write the profile's own config (%s): %v", profileCfg, err)
	}
	// ...and its own identity.
	if !identity.KeyPairExists(filepath.Join(profileDir, "identity.key"), filepath.Join(profileDir, "identity.pub")) {
		t.Errorf("init --data-dir did not create the profile's identity under %s", profileDir)
	}

	// The DEFAULT profile's config is byte-for-byte untouched.
	after, err := os.ReadFile(defaultCfgPath)
	if err != nil {
		t.Fatalf("default profile config unreadable after init: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("init --data-dir REWROTE the default profile's config:\n--- before ---\n%s\n--- after ---\n%s", original, after)
	}
}

func TestInitWithDataDirWritesSelfContainedProfile(t *testing.T) {
	fakeHome := withFakeHome(t)

	profileDir := filepath.Join(t.TempDir(), "profile")
	if err := runCLI(t, "init", "--data-dir", profileDir, "--cpu-cores", "2", "--memory-mb", "1024", "--disk-gb", "5"); err != nil {
		t.Fatalf("init: %v", err)
	}

	profileCfg := filepath.Join(profileDir, "config.yaml")
	raw, err := os.ReadFile(profileCfg)
	if err != nil {
		t.Fatalf("profile config not written: %v", err)
	}

	// Self-contained: nothing in the profile's config may point into the default
	// profile (the old behavior left host_id_file at ~/.lettuce/host.id, so
	// "isolated" volunteers shared the default profile's host identity).
	if strings.Contains(string(raw), fakeHome) {
		t.Errorf("profile config references the default profile's home (%s):\n%s", fakeHome, raw)
	}

	loaded, err := config.Load(profileCfg)
	if err != nil {
		t.Fatalf("loading profile config: %v", err)
	}
	if loaded.DataDir != profileDir {
		t.Errorf("profile data_dir = %q, want %q", loaded.DataDir, profileDir)
	}
	if !filepath.IsAbs(loaded.DataDir) {
		t.Errorf("profile data_dir %q is not absolute", loaded.DataDir)
	}
}

func TestInitWithRelativeDataDirAbsolutizes(t *testing.T) {
	withFakeHome(t)

	work := t.TempDir()
	t.Chdir(work)

	if err := runCLI(t, "init", "--data-dir", "rel-profile", "--cpu-cores", "2", "--memory-mb", "1024", "--disk-gb", "5"); err != nil {
		t.Fatalf("init: %v", err)
	}

	absProfile := filepath.Join(work, "rel-profile")
	profileCfg := filepath.Join(absProfile, "config.yaml")
	loaded, err := config.Load(profileCfg)
	if err != nil {
		t.Fatalf("loading profile config (was it written to the profile at all?): %v", err)
	}
	if _, err := os.Stat(profileCfg); err != nil {
		t.Fatalf("profile config not at %s: %v", profileCfg, err)
	}
	if !filepath.IsAbs(loaded.DataDir) {
		t.Errorf("saved data_dir %q is relative; it must be absolutized at startup", loaded.DataDir)
	}
	if loaded.DataDir != absProfile {
		t.Errorf("saved data_dir = %q, want %q", loaded.DataDir, absProfile)
	}
}

func TestCommandsAbsolutizeDataDir(t *testing.T) {
	withFakeHome(t)

	work := t.TempDir()
	t.Chdir(work)

	// A profile that already exists, with a config of its own.
	profile := filepath.Join(work, "rel-profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	c := config.Defaults()
	c.DataDir = profile
	if err := c.Save(filepath.Join(profile, "config.yaml")); err != nil {
		t.Fatal(err)
	}

	// Any non-init command run with a RELATIVE --data-dir must end up with an
	// absolute effective DataDir (the daemon derives cache/binary paths from it
	// while compute children run in their own working directories).
	if err := runCLI(t, "config", "get", "data_dir", "--data-dir", "rel-profile"); err != nil {
		t.Fatalf("config get: %v", err)
	}
	if cfg == nil {
		t.Fatal("global cfg not loaded")
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("effective DataDir %q is relative after command startup", cfg.DataDir)
	}
	if cfg.DataDir != profile {
		t.Errorf("effective DataDir = %q, want %q", cfg.DataDir, profile)
	}
}
