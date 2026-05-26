package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/identity"
)

func TestInitCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	testCfgPath := filepath.Join(dir, "config.yaml")

	// Create a pipe to simulate stdin with all defaults (empty lines).
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	// Write enough newlines for all prompts, then close.
	go func() {
		// 6 prompts need Enter (defaults): cpu, memory, disk, scheduling mode, leaf mode, server host
		for i := 0; i < 6; i++ {
			w.Write([]byte("\n"))
		}
		w.Close()
	}()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--config", testCfgPath, "--data-dir", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init command failed: %v", err)
	}

	// Verify config file was created.
	if _, err := os.Stat(testCfgPath); err != nil {
		t.Errorf("config file not created: %v", err)
	}

	// Verify key files were created.
	keyFile := filepath.Join(dir, "identity.key")
	pubKeyFile := filepath.Join(dir, "identity.pub")

	if !identity.KeyPairExists(keyFile, pubKeyFile) {
		t.Error("key files not created")
	}

	// Verify key sizes.
	keyData, _ := os.ReadFile(keyFile)
	pubData, _ := os.ReadFile(pubKeyFile)
	if len(keyData) != 64 {
		t.Errorf("private key size = %d, want 64", len(keyData))
	}
	if len(pubData) != 32 {
		t.Errorf("public key size = %d, want 32", len(pubData))
	}
}

func TestInitReinitPrompt(t *testing.T) {
	dir := t.TempDir()
	testCfgPath := filepath.Join(dir, "config.yaml")

	// Create an existing config file.
	os.WriteFile(testCfgPath, []byte("log_level: info\n"), 0644)

	// Simulate declining reinit.
	r, w, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	go func() {
		w.Write([]byte("n\n"))
		w.Close()
	}()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--config", testCfgPath, "--data-dir", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init command failed: %v", err)
	}

	// Key files should NOT have been created since we declined.
	keyFile := filepath.Join(dir, "identity.key")
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("key file should not exist after declining reinit")
	}
}
