package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A fake `podman` for the tests that need a real process: how long a machine
// verb may run, and how it is stopped when it runs too long, cannot be seen
// through a mocked CommandExecutor. The test binary itself plays podman —
// TestMain hands over to fakePodmanMain when fakePodmanDirEnv is set — so the
// fake behaves the same on every platform.
//
// Its machine lives in files under that directory: "state" (running or
// stopped) and "starting", podman's own flag, set while a start is in
// progress. The flag is cleared when the start ends normally or is
// interrupted (podman's clean-up), and survives a kill — which is how
// `podman machine list` comes to read "Currently starting" for a machine that
// is running. An interrupted start also leaves an "interrupted" marker.
const (
	fakePodmanDirEnv = "LETTUCE_FAKE_PODMAN_DIR"
	// fakePodmanUpAfterEnv: how long into `machine start` the machine reads
	// running; unset or unparsable means it never comes up.
	fakePodmanUpAfterEnv = "LETTUCE_FAKE_PODMAN_UP_AFTER"
	// fakePodmanExitAfterEnv: how long `machine start` runs before it exits
	// successfully.
	fakePodmanExitAfterEnv = "LETTUCE_FAKE_PODMAN_EXIT_AFTER"
)

// fakePodmanMain is the fake's entry point; it returns the exit status.
func fakePodmanMain(args []string) int {
	dir := os.Getenv(fakePodmanDirEnv)
	switch {
	case len(args) >= 2 && args[0] == "machine" && args[1] == "inspect":
		state, _ := os.ReadFile(filepath.Join(dir, "state"))
		s := strings.TrimSpace(string(state))
		if s == "" {
			s = "stopped"
		}
		fmt.Printf(`[{"Name":"podman-machine-default","State":%q,"Resources":{"CPUs":2,"DiskSize":100,"Memory":2048},"ConnectionInfo":{}}]`+"\n", s)
		return 0
	case len(args) >= 2 && args[0] == "machine" && args[1] == "start":
		return fakePodmanStart(dir)
	}
	fmt.Fprintf(os.Stderr, "fake podman: unexpected command %v\n", args)
	return 125
}

func fakePodmanStart(dir string) int {
	flag := filepath.Join(dir, "starting")
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	_ = os.WriteFile(flag, []byte("true"), 0o644)
	fmt.Println(`Starting machine "podman-machine-default"`)

	var up <-chan time.Time
	if d, err := time.ParseDuration(os.Getenv(fakePodmanUpAfterEnv)); err == nil {
		up = time.After(d)
	}
	exitAfter, err := time.ParseDuration(os.Getenv(fakePodmanExitAfterEnv))
	if err != nil {
		exitAfter = time.Minute
	}
	exit := time.After(exitAfter)
	for {
		select {
		case <-up:
			_ = os.WriteFile(filepath.Join(dir, "state"), []byte("running"), 0o644)
			up = nil
		case <-exit:
			_ = os.Remove(flag)
			fmt.Println(`Machine "podman-machine-default" started successfully`)
			return 0
		case <-interrupts:
			_ = os.Remove(flag)
			_ = os.WriteFile(filepath.Join(dir, "interrupted"), nil, 0o644)
			return 1
		}
	}
}

// fakePodmanManager returns a manager on the Podman-machine path whose podman
// is the fake, run through the real command executor, and the fake's
// directory. upAfter < 0 means the machine never comes up.
func fakePodmanManager(t *testing.T, upAfter, exitAfter time.Duration) (*PodmanMachineManager, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	t.Cleanup(SetNeedsMachineForTest(true))
	dir := t.TempDir()
	t.Setenv(fakePodmanDirEnv, dir)
	if upAfter >= 0 {
		t.Setenv(fakePodmanUpAfterEnv, upAfter.String())
	} else {
		t.Setenv(fakePodmanUpAfterEnv, "")
	}
	t.Setenv(fakePodmanExitAfterEnv, exitAfter.String())

	orig := CommandExecutor
	CommandExecutor = defaultCommandExecutor
	t.Cleanup(func() { CommandExecutor = orig })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewPodmanMachineManager(exe, logger), dir
}

// fakePodmanFile reports whether the fake left the named file.
func fakePodmanFile(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}
