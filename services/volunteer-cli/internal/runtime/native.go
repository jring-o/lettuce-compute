package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CommandModifier is a function that modifies an exec.Cmd before it is started.
// Used by S28 to apply OS-level resource limits (cgroups, job objects, setrlimit).
type CommandModifier func(cmd *exec.Cmd) error

// ProcessNotifier is called by NativeRuntime after cmd.Start() with the child PID.
// It returns a cleanup function that is called after the process exits.
type ProcessNotifier func(pid int) (cleanup func(), err error)

// NativeRuntime executes pre-compiled binaries for the volunteer's platform.
type NativeRuntime struct {
	dataDir         string
	logger          *slog.Logger
	cmdModifier     CommandModifier
	processNotifier ProcessNotifier
	httpClient      *http.Client // injectable for testing
}

// NewNativeRuntime creates a NativeRuntime with the given data directory.
func NewNativeRuntime(dataDir string, logger *slog.Logger) *NativeRuntime {
	return &NativeRuntime{
		dataDir:    dataDir,
		logger:     logger,
		httpClient: http.DefaultClient,
	}
}

// SetCommandModifier sets a function called on exec.Cmd before Start().
// This is the integration point for S28's resource limiter.
func (n *NativeRuntime) SetCommandModifier(fn CommandModifier) {
	n.cmdModifier = fn
}

// SetProcessNotifier sets a function called after cmd.Start() with the child PID.
// Used by S28 for post-start enforcement (cgroups, job objects).
func (n *NativeRuntime) SetProcessNotifier(fn ProcessNotifier) {
	n.processNotifier = fn
}

// Name returns "native".
func (n *NativeRuntime) Name() string { return "native" }

// platformKey returns the current OS/arch key (e.g., "linux_amd64").
func platformKey() string {
	return runtime.GOOS + "_" + runtime.GOARCH
}

// CanHandle returns true if spec.Binaries contains a key for the current platform.
func (n *NativeRuntime) CanHandle(spec *ExecutionSpec) bool {
	if spec == nil || len(spec.Binaries) == 0 {
		return false
	}
	_, ok := spec.Binaries[platformKey()]
	return ok
}

// Prepare downloads the binary (with caching), creates a work directory,
// and writes input data and parameters files.
func (n *NativeRuntime) Prepare(ctx context.Context, wu *WorkUnit) (*PrepareResult, error) {
	n.logger.Info("native.Prepare: starting", "work_unit_id", wu.ID, "leaf_id", wu.LeafID)

	// SECURITY (H2): defense-in-depth — wu.ID is the trailing component of workDir
	// below. Reject non-UUID IDs before building any path so a malicious head can't
	// escape n.dataDir via path traversal.
	if err := ValidateWorkUnitID(wu.ID); err != nil {
		n.logger.Warn("native.Prepare: rejecting work unit with invalid ID", "work_unit_id", wu.ID, "error", err)
		return nil, err
	}

	pk := platformKey()
	binaryURL, ok := wu.ExecutionSpec.Binaries[pk]
	if !ok {
		n.logger.Warn("native.Prepare: no binary for platform", "work_unit_id", wu.ID, "platform", pk, "available_platforms", binaryKeys(wu.ExecutionSpec.Binaries))
		return nil, fmt.Errorf("no binary for platform %s", pk)
	}

	// SECURITY (C2): native code runs directly on the host, so it MUST be
	// integrity-verified. Fail closed: reject the work unit if the head did not
	// supply an expected SHA-256 for our platform. The expected digest is
	// normalized to lowercase before comparison.
	expectedChecksum := strings.ToLower(wu.ExecutionSpec.BinaryChecksums[pk])
	if expectedChecksum == "" {
		n.logger.Warn("native.Prepare: missing binary checksum, refusing to run unverified native code",
			"work_unit_id", wu.ID, "platform", pk)
		return nil, fmt.Errorf("no binary checksum for platform %s: refusing to execute unverified native binary", pk)
	}
	n.logger.Debug("native.Prepare: downloading binary", "work_unit_id", wu.ID, "platform", pk, "url", binaryURL, "expected_sha256", expectedChecksum)

	// Download (or reuse a checksum-keyed cached copy) and verify integrity.
	binaryPath, err := n.ensureBinary(ctx, binaryURL, expectedChecksum)
	if err != nil {
		n.logger.Warn("native.Prepare: binary download/verify failed", "work_unit_id", wu.ID, "error", err)
		return nil, fmt.Errorf("prepare binary: %w", err)
	}
	n.logger.Debug("native.Prepare: binary ready", "work_unit_id", wu.ID, "path", binaryPath)

	// Create work directory.
	workDir := filepath.Join(n.dataDir, "work", wu.ID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}

	result := &PrepareResult{
		BinaryPath: binaryPath,
		WorkDir:    workDir,
	}

	// Write input data.
	if len(wu.InputData) > 0 {
		inputPath := filepath.Join(workDir, "input.dat")
		if err := os.WriteFile(inputPath, wu.InputData, 0o644); err != nil {
			return nil, fmt.Errorf("write input data: %w", err)
		}
		result.InputPath = inputPath
		n.logger.Debug("native.Prepare: wrote input data", "work_unit_id", wu.ID, "size", len(wu.InputData))
	} else if wu.InputDataURL != "" {
		inputPath := filepath.Join(workDir, "input.dat")
		if err := n.downloadFile(ctx, wu.InputDataURL, inputPath); err != nil {
			return nil, fmt.Errorf("download input data: %w", err)
		}
		result.InputPath = inputPath
	}

	// Write parameters JSON.
	if wu.ParametersJSON != "" {
		paramsPath := filepath.Join(workDir, "params.json")
		if err := os.WriteFile(paramsPath, []byte(wu.ParametersJSON), 0o644); err != nil {
			return nil, fmt.Errorf("write params: %w", err)
		}
		n.logger.Debug("native.Prepare: wrote params", "work_unit_id", wu.ID)
	}

	// Download and extract viz bundle if present.
	n.logger.Debug("native.Prepare: checking viz bundle", "work_unit_id", wu.ID)
	vizPath, err := PrepareVizBundle(ctx, n.dataDir, workDir, &wu.ExecutionSpec, n.httpClient, n.logger)
	if err != nil {
		n.logger.Warn("native.Prepare: viz bundle FAILED", "work_unit_id", wu.ID, "error", err)
		return nil, fmt.Errorf("prepare viz bundle: %w", err)
	}
	if vizPath != "" {
		n.logger.Info("native.Prepare: viz bundle ready", "work_unit_id", wu.ID, "path", vizPath)
	}
	result.VizBundlePath = vizPath

	n.logger.Info("native.Prepare: complete", "work_unit_id", wu.ID, "work_dir", workDir)
	return result, nil
}

// Execute runs the binary as a subprocess and collects output and metrics.
func (n *NativeRuntime) Execute(ctx context.Context, wu *WorkUnit, prep *PrepareResult) (*ExecutionResult, error) {
	// Apply deadline timeout.
	if wu.DeadlineSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(wu.DeadlineSeconds)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, prep.BinaryPath)
	cmd.Dir = prep.WorkDir

	// Build environment.
	env := os.Environ()
	for k, v := range wu.EnvVars {
		env = append(env, k+"="+v)
	}
	env = append(env, "LETTUCE_WORK_DIR="+prep.WorkDir)
	outputPath := filepath.Join(prep.WorkDir, "output.dat")
	env = append(env, "LETTUCE_OUTPUT_FILE="+outputPath)
	env = append(env, "LETTUCE_PROGRESS_FILE="+filepath.Join(prep.WorkDir, "progress.txt"))
	env = append(env, "LETTUCE_CHECKPOINT_FILE="+filepath.Join(prep.WorkDir, "checkpoint.dat"))
	if prep.InputPath != "" {
		env = append(env, "LETTUCE_INPUT_FILE="+prep.InputPath)
	}
	paramsPath := filepath.Join(prep.WorkDir, "params.json")
	if _, err := os.Stat(paramsPath); err == nil {
		env = append(env, "LETTUCE_PARAMS_FILE="+paramsPath)
	}
	cmd.Env = env

	// Apply command modifier (S28 resource limiter hook).
	if n.cmdModifier != nil {
		if err := n.cmdModifier(cmd); err != nil {
			return nil, fmt.Errorf("command modifier: %w", err)
		}
	}

	// Capture stdout/stderr to execution log, capped at 10 MB.
	logPath := filepath.Join(prep.WorkDir, "execution.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}
	limitedWriter := &limitedWriter{w: logFile, remaining: 10 * 1024 * 1024}
	cmd.Stdout = limitedWriter
	cmd.Stderr = limitedWriter

	// Start the process, apply post-start enforcement, then wait.
	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start process: %w", err)
	}

	// Notify caller of PID for suspend/resume support.
	if prep.PIDCallback != nil {
		prep.PIDCallback(cmd.Process.Pid)
	}

	// Apply post-start resource limits (cgroups, job objects).
	var enforcementCleanup func()
	if n.processNotifier != nil {
		cleanup, notifyErr := n.processNotifier(cmd.Process.Pid)
		if notifyErr != nil {
			cmd.Process.Kill()
			logFile.Close()
			return nil, fmt.Errorf("process notifier: %w", notifyErr)
		}
		enforcementCleanup = cleanup
	}

	// Start disk I/O monitoring (reads /proc/[pid]/io periodically on Linux).
	diskIOCleanup, diskIOMonitor := startDiskIOMonitor(cmd.Process.Pid, n.logger)

	runErr := cmd.Wait()
	wallClock := time.Since(startTime)
	logFile.Close()

	// Stop disk I/O monitoring before cleanup (captures final read).
	diskIOCleanup()

	if enforcementCleanup != nil {
		enforcementCleanup()
	}

	// Determine exit code. Check context first — if the deadline expired or
	// the context was cancelled, the process may have been killed (producing
	// an ExitError) but the root cause is cancellation.
	exitCode := 0
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("execution cancelled: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("execution failed: %w", runErr)
		}
	}

	// Collect resource metrics.
	metrics := ExecutionMetrics{
		WallClockSeconds: int64(math.Ceil(wallClock.Seconds())),
	}
	if cmd.ProcessState != nil {
		metrics.CPUSecondsUser = cmd.ProcessState.UserTime().Seconds()
		metrics.CPUSecondsSystem = cmd.ProcessState.SystemTime().Seconds()
		totalCPU := metrics.CPUSecondsUser + metrics.CPUSecondsSystem
		if wallClock.Seconds() > 0 {
			cores := int32(math.Ceil(totalCPU / wallClock.Seconds()))
			maxCores := int32(runtime.NumCPU())
			if cores > maxCores {
				cores = maxCores
			}
			if cores < 1 {
				cores = 1
			}
			metrics.CPUCoresUsed = cores
		} else {
			metrics.CPUCoresUsed = 1
		}
		collectPlatformMetrics(cmd, &metrics)
	}

	// Apply disk I/O metrics from monitor.
	applyDiskIOMetrics(diskIOMonitor, &metrics)

	// Read output.
	var outputData []byte
	if data, err := os.ReadFile(outputPath); err == nil {
		outputData = data
	} else if limitedWriter.written > 0 {
		// Fall back to stdout if output.dat doesn't exist.
		outputData, _ = os.ReadFile(logPath)
	}

	return &ExecutionResult{
		OutputData:     outputData,
		OutputChecksum: checksumSHA256(outputData),
		ExitCode:       exitCode,
		Metrics:        metrics,
	}, nil
}

// Cleanup removes the work directory. Cached binaries are preserved.
func (n *NativeRuntime) Cleanup(prep *PrepareResult) error {
	if prep == nil || prep.WorkDir == "" {
		return nil
	}
	return os.RemoveAll(prep.WorkDir)
}

// ensureBinary downloads the binary if not cached, verifies its SHA-256 against
// expectedChecksum (lowercase hex), and returns the cached path.
//
// SECURITY (C2): the on-disk cache is keyed by the EXPECTED content checksum,
// not by the URL. This means a cache hit guarantees the bytes already match the
// expected digest, so a previously poisoned URL-keyed entry can never be reused,
// and two leafs that legitimately ship the same artifact share one cached copy.
// After any fresh download the bytes are re-hashed and verified before the file
// is made executable; a mismatch deletes the download and fails closed.
func (n *NativeRuntime) ensureBinary(ctx context.Context, url, expectedChecksum string) (string, error) {
	if expectedChecksum == "" {
		// Defense in depth: callers must pre-check, but never run unverified.
		return "", fmt.Errorf("no expected checksum: refusing to download unverified native binary")
	}

	cacheDir := filepath.Join(n.dataDir, "cache", "binaries")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	// Cache key is the expected content SHA-256. Add .exe on Windows so the OS
	// recognizes the file as executable.
	cacheKey := expectedChecksum
	if runtime.GOOS == "windows" {
		cacheKey += ".exe"
	}
	cachePath := filepath.Join(cacheDir, cacheKey)

	// Check cache. A hit means the bytes already match the expected digest
	// (that is how the key was derived when the file was written).
	if _, err := os.Stat(cachePath); err == nil {
		n.logger.Debug("binary cache hit", "url", url, "path", cachePath, "sha256", expectedChecksum)
		return cachePath, nil
	}

	n.logger.Info("downloading binary", "url", url, "expected_sha256", expectedChecksum)

	// Download to a temp file in the cache dir so we can verify before committing.
	tmp, err := os.CreateTemp(cacheDir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op once renamed away

	if err := n.downloadFile(ctx, url, tmpPath); err != nil {
		return "", err
	}

	// Verify integrity before the binary is ever made executable or run.
	actual, err := fileChecksumSHA256(tmpPath)
	if err != nil {
		return "", fmt.Errorf("checksum downloaded binary: %w", err)
	}
	if actual != expectedChecksum {
		n.logger.Warn("native binary checksum mismatch, rejecting", "url", url,
			"expected_sha256", expectedChecksum, "actual_sha256", actual)
		return "", fmt.Errorf("binary checksum mismatch: expected %s, got %s", expectedChecksum, actual)
	}

	// Commit the verified bytes to the checksum-keyed cache path.
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return "", fmt.Errorf("rename to cache: %w", err)
	}

	// Set executable permission on Unix.
	if err := os.Chmod(cachePath, 0o755); err != nil {
		n.logger.Warn("failed to set executable permission", "error", err)
	}

	return cachePath, nil
}

// fileChecksumSHA256 returns the lowercase hex SHA-256 of a file's contents.
func fileChecksumSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadFile downloads a URL to the given path using atomic write.
func (n *NativeRuntime) downloadFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// Write to temp file in the same directory for atomic rename.
	dir := filepath.Dir(destPath)
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write download: %w", err)
	}
	tmp.Close()

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename to cache: %w", err)
	}

	return nil
}

// checksumSHA256 computes the SHA-256 hex digest of data.
func checksumSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// limitedWriter wraps a writer and stops writing after a byte limit.
type limitedWriter struct {
	w         io.Writer
	remaining int64
	written   int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.remaining <= 0 {
		return len(p), nil // silently discard
	}
	toWrite := p
	if int64(len(p)) > lw.remaining {
		toWrite = p[:lw.remaining]
	}
	n, err := lw.w.Write(toWrite)
	lw.remaining -= int64(n)
	lw.written += int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil // report full write to caller
}

// binaryKeys returns the keys of a map for logging.
func binaryKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
