package cli

import (
	"path/filepath"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	rtdetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// TestInitNonInteractive_DoesNotAutoScaleConcurrency verifies that non-interactive
// init (the path the desktop app uses) leaves max_concurrent_tasks at the safe
// default of 1 instead of scaling it to the CPU-core count. Auto-scaling
// previously let the daemon run several memory-bound containers at once and
// oversubscribe RAM.
func TestInitNonInteractive_DoesNotAutoScaleConcurrency(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")

	origBackendDetect := detectContainerBackendFunc
	detectContainerBackendFunc = func(bundledPath string) rtdetect.BackendInfo {
		return rtdetect.BackendInfo{Backend: rtdetect.BackendDocker}
	}
	defer func() { detectContainerBackendFunc = origBackendDetect }()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir,
		"--cpu-cores", "8", "--memory-mb", "32000"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("non-interactive init failed: %v", err)
	}

	loaded, err := config.Load(cfgFile)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if loaded.ResourceLimits.MaxCPUCores != 8 {
		t.Errorf("MaxCPUCores = %d, want 8 (flag should be honored)", loaded.ResourceLimits.MaxCPUCores)
	}
	if loaded.MaxConcurrentTasks != 1 {
		t.Errorf("MaxConcurrentTasks = %d, want 1 (must not auto-scale to CPU cores)", loaded.MaxConcurrentTasks)
	}
}
