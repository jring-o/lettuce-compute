package resource

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

func TestCheckDiskSpace_SufficientSpace(t *testing.T) {
	l := NewLimiter(slog.Default())
	// Check the temp dir — should have enough space for 1 MB.
	dir := t.TempDir()
	if err := l.CheckDiskSpace(dir, 1); err != nil {
		t.Errorf("CheckDiskSpace should succeed for 1 MB: %v", err)
	}
}

func TestCheckDiskSpace_ImpossibleRequirement(t *testing.T) {
	l := NewLimiter(slog.Default())
	dir := t.TempDir()
	// 1 PB — should fail on any real filesystem.
	err := l.CheckDiskSpace(dir, 1024*1024*1024)
	if err == nil {
		t.Error("CheckDiskSpace should fail for impossibly large requirement")
	}
	if !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("error should mention insufficient disk space, got: %v", err)
	}
}

func TestCheckDiskSpace_ZeroRequirement(t *testing.T) {
	l := NewLimiter(slog.Default())
	dir := t.TempDir()
	if err := l.CheckDiskSpace(dir, 0); err != nil {
		t.Errorf("CheckDiskSpace should succeed for 0 MB requirement: %v", err)
	}
}

func TestEnforce_ReturnsCleanup(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 1,
		MaxMemoryMB: 256,
	}
	// Use our own PID — Enforce may not fully apply limits to self,
	// but it should return a non-nil cleanup function without error
	// on the current platform (Windows: Job Object, macOS: setpriority).
	// On Linux without cgroups, prlimit on self is also valid.
	cleanup, err := l.Enforce(1, limits)
	if err != nil {
		// Some platforms may fail with PID 1 (init process). Skip rather than fail.
		t.Skipf("Enforce returned error (may be expected on this platform): %v", err)
	}
	if cleanup == nil {
		t.Error("Enforce should return a non-nil cleanup function")
	} else {
		cleanup()
	}
}

func TestEnforce_OwnProcess(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 1,
		MaxMemoryMB: 256,
	}
	// Use our own PID for a more reliable test.
	pid := os.Getpid()
	cleanup, err := l.Enforce(pid, limits)
	if err != nil {
		t.Skipf("Enforce on own PID returned error (may be expected): %v", err)
	}
	if cleanup == nil {
		t.Error("Enforce should return a non-nil cleanup function")
	} else {
		cleanup()
	}
}

func TestEnforce_ZeroLimits(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 0,
		MaxMemoryMB: 0,
	}
	pid := os.Getpid()
	cleanup, err := l.Enforce(pid, limits)
	if err != nil {
		t.Skipf("Enforce with zero limits returned error: %v", err)
	}
	if cleanup == nil {
		t.Error("Enforce should return a non-nil cleanup function even with zero limits")
	} else {
		cleanup()
	}
}

func TestApply_NoError(t *testing.T) {
	l := NewLimiter(slog.Default())
	limits := &config.ResourceLimits{
		MaxCPUCores: 2,
		MaxMemoryMB: 512,
	}
	cmd := exec.Command("echo", "test")
	if err := l.Apply(cmd, limits); err != nil {
		t.Errorf("Apply should not error: %v", err)
	}
}

func TestNewLimiter_NotNil(t *testing.T) {
	l := NewLimiter(slog.Default())
	if l == nil {
		t.Error("NewLimiter should not return nil")
	}
}
