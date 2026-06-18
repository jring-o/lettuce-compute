package runtime

import (
	"bytes"
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
	"path/filepath"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// wasmMagic is the first 4 bytes of a valid WASM binary.
var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6D}

// WasmRuntime executes WASM modules using wazero with WASI support.
type WasmRuntime struct {
	dataDir    string
	logger     *slog.Logger
	httpClient *http.Client // injectable for testing
}

// NewWasmRuntime creates a WasmRuntime with the given data directory.
func NewWasmRuntime(dataDir string, logger *slog.Logger) *WasmRuntime {
	return &WasmRuntime{
		dataDir:    dataDir,
		logger:     logger,
		httpClient: http.DefaultClient,
	}
}

// Name returns "wasm".
func (w *WasmRuntime) Name() string { return "wasm" }

// CanHandle returns true if spec.Binaries contains a "wasm" key and no container image is set.
func (w *WasmRuntime) CanHandle(spec *ExecutionSpec) bool {
	if spec == nil || len(spec.Binaries) == 0 {
		return false
	}
	url, ok := spec.Binaries["wasm"]
	return ok && url != "" && spec.Image == ""
}

// Prepare downloads the WASM module (with caching), creates a work directory,
// and writes input data and parameters files.
func (w *WasmRuntime) Prepare(ctx context.Context, wu *WorkUnit) (*PrepareResult, error) {
	// SECURITY (H2): defense-in-depth — wu.ID is the trailing component of workDir
	// below. Reject non-UUID IDs before building any path so a malicious head can't
	// escape w.dataDir via path traversal.
	if err := ValidateWorkUnitID(wu.ID); err != nil {
		w.logger.Warn("wasm.Prepare: rejecting work unit with invalid ID", "work_unit_id", wu.ID, "error", err)
		return nil, err
	}

	wasmURL, ok := wu.ExecutionSpec.Binaries["wasm"]
	if !ok || wasmURL == "" {
		return nil, fmt.Errorf("no wasm binary URL in execution spec")
	}

	// SECURITY (C2): verify the module's SHA-256 when the head supplies one.
	// WASM runs sandboxed (wazero/WASI), so a missing checksum is lower risk —
	// we proceed but log a warning rather than fail closed (unlike native).
	expectedChecksum := strings.ToLower(wu.ExecutionSpec.BinaryChecksums["wasm"])

	// Download or use cached module.
	modulePath, err := w.ensureModule(ctx, wasmURL, expectedChecksum)
	if err != nil {
		return nil, fmt.Errorf("prepare wasm module: %w", err)
	}

	// Create work directory.
	workDir := filepath.Join(w.dataDir, "wasm-work", wu.ID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}

	result := &PrepareResult{
		BinaryPath: modulePath,
		WorkDir:    workDir,
	}

	// Write input data.
	if len(wu.InputData) > 0 {
		inputPath := filepath.Join(workDir, "input.dat")
		if err := os.WriteFile(inputPath, wu.InputData, 0o644); err != nil {
			return nil, fmt.Errorf("write input data: %w", err)
		}
		result.InputPath = inputPath
	} else if wu.InputDataURL != "" {
		inputPath := filepath.Join(workDir, "input.dat")
		if err := w.downloadToFile(ctx, wu.InputDataURL, inputPath); err != nil {
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
	}

	// Download and extract viz bundle if present. Viz is a dashboard-only concern
	// (the wasm module never reads it); a bad/missing bundle must NEVER block
	// compute, so we warn and continue without it. See TODO #39.
	vizPath, err := PrepareVizBundle(ctx, w.dataDir, workDir, &wu.ExecutionSpec, w.httpClient, w.logger)
	if err != nil {
		w.logger.Warn("wasm.Prepare: viz bundle prep failed; continuing without viz (compute unaffected)",
			"work_unit_id", wu.ID, "error", err)
		vizPath = ""
	}
	result.VizBundlePath = vizPath

	return result, nil
}

// Execute runs the WASM module via wazero and returns the result.
func (w *WasmRuntime) Execute(ctx context.Context, wu *WorkUnit, prep *PrepareResult) (*ExecutionResult, error) {
	// Apply deadline timeout.
	if wu.DeadlineSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(wu.DeadlineSeconds)*time.Second)
		defer cancel()
	}

	// Read WASM module bytes.
	wasmBytes, err := os.ReadFile(prep.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm module: %w", err)
	}

	// Create wazero runtime with optional memory limit.
	// WithCloseOnContextDone ensures the module is interrupted when the context
	// expires (deadline, cancellation). Without this, only WASI host function
	// boundaries are checked.
	runtimeConfig := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if wu.ExecutionSpec.MaxMemoryMB > 0 {
		maxPages := uint32(wu.ExecutionSpec.MaxMemoryMB) * 16 // 1 MB = 16 pages (64 KiB each)
		runtimeConfig = runtimeConfig.WithMemoryLimitPages(maxPages)
	}

	r := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	defer r.Close(ctx)

	// Instantiate WASI.
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Build environment variables.
	envVars := make(map[string]string)
	for k, v := range wu.EnvVars {
		envVars[k] = v
	}
	envVars["LETTUCE_WORK_DIR"] = "/work"
	envVars["LETTUCE_OUTPUT_FILE"] = "/work/output.dat"
	envVars["LETTUCE_PROGRESS_FILE"] = "/work/progress.txt"
	envVars["LETTUCE_CHECKPOINT_FILE"] = "/work/checkpoint.dat"
	if prep.InputPath != "" {
		envVars["LETTUCE_INPUT_FILE"] = "/work/input.dat"
	}
	paramsPath := filepath.Join(prep.WorkDir, "params.json")
	if _, err := os.Stat(paramsPath); err == nil {
		envVars["LETTUCE_PARAMS_FILE"] = "/work/params.json"
	}

	// Configure module with WASI.
	var stdoutBuf, stderrBuf bytes.Buffer
	fsConfig := wazero.NewFSConfig().WithDirMount(prep.WorkDir, "/work")

	moduleConfig := wazero.NewModuleConfig().
		WithStdout(&stdoutBuf).
		WithStderr(&stderrBuf).
		WithFSConfig(fsConfig).
		WithArgs("compute").
		WithName("compute")

	for k, v := range envVars {
		moduleConfig = moduleConfig.WithEnv(k, v)
	}

	// Compile module.
	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile wasm module: %w", err)
	}
	defer compiled.Close(ctx)

	// Execute (calls _start for WASI command modules).
	startTime := time.Now()
	mod, instantiateErr := r.InstantiateModule(ctx, compiled, moduleConfig)
	wallClock := time.Since(startTime)

	// Close module if returned (wazero returns it even with ExitError).
	if mod != nil {
		defer mod.Close(ctx)
	}

	// Determine exit code.
	exitCode := 0
	if instantiateErr != nil {
		// Check context cancellation first.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("execution cancelled: %w", ctx.Err())
		}
		var exitErr *sys.ExitError
		if errors.As(instantiateErr, &exitErr) {
			exitCode = int(exitErr.ExitCode())
		} else {
			return nil, fmt.Errorf("wasm execution failed: %w", instantiateErr)
		}
	}

	// Collect metrics.
	metrics := ExecutionMetrics{
		WallClockSeconds: int64(math.Ceil(wallClock.Seconds())),
		CPUCoresUsed:     1, // WASM is single-threaded
	}

	// Capture peak memory from module if available.
	if mod != nil {
		if mem := mod.Memory(); mem != nil {
			metrics.PeakMemoryMB = int32(mem.Size() / (1024 * 1024))
		}
	}

	// Read output: try output.dat first, fall back to stdout.
	outputPath := filepath.Join(prep.WorkDir, "output.dat")
	var outputData []byte
	if data, err := os.ReadFile(outputPath); err == nil {
		outputData = data
	} else if stdoutBuf.Len() > 0 {
		outputData = stdoutBuf.Bytes()
	}

	// Log stderr if present.
	if stderrBuf.Len() > 0 {
		w.logger.Debug("wasm stderr", "output", stderrBuf.String())
	}

	return &ExecutionResult{
		OutputData:     outputData,
		OutputChecksum: checksumSHA256(outputData),
		ExitCode:       exitCode,
		Metrics:        metrics,
	}, nil
}

// Cleanup removes the work directory. Cached modules are preserved.
func (w *WasmRuntime) Cleanup(prep *PrepareResult) error {
	if prep == nil || prep.WorkDir == "" {
		return nil
	}
	return os.RemoveAll(prep.WorkDir)
}

// ensureModule downloads the WASM module if not cached, returning the cached path.
//
// SECURITY (C2): when expectedChecksum is set (lowercase hex SHA-256), the cache
// is keyed by that content digest so a cache hit can only be bytes that already
// match, and a fresh download is verified before use (reject on mismatch). When
// no checksum is supplied the module is keyed by URL and only the WASM magic
// bytes are checked — acceptable because WASM runs sandboxed — but a warning is
// logged so operators know the artifact was unverified.
func (w *WasmRuntime) ensureModule(ctx context.Context, url, expectedChecksum string) (string, error) {
	cacheDir := filepath.Join(w.dataDir, "wasm-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	// Cache key: content checksum when known (tamper-proof), else URL hash.
	var cacheKey string
	if expectedChecksum != "" {
		cacheKey = expectedChecksum + ".wasm"
	} else {
		h := sha256.Sum256([]byte(url))
		cacheKey = hex.EncodeToString(h[:]) + ".wasm"
		w.logger.Warn("wasm module has no checksum in execution spec; proceeding unverified (sandboxed)", "url", url)
	}
	cachePath := filepath.Join(cacheDir, cacheKey)

	// Check cache.
	if _, err := os.Stat(cachePath); err == nil {
		w.logger.Debug("wasm module cache hit", "url", url, "path", cachePath)
		return cachePath, nil
	}

	w.logger.Info("downloading wasm module", "url", url, "expected_sha256", expectedChecksum)

	// Download to a temp file and verify BEFORE committing to the cache path, so a
	// concurrent prepare for the same checksum can never observe unverified bytes at
	// the cache path (matches the native runtime's verify-then-commit pattern).
	tmp, err := os.CreateTemp(cacheDir, ".wasm-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op once renamed away

	if err := w.downloadToFile(ctx, url, tmpPath); err != nil {
		return "", err
	}

	// Verify SHA-256 against the expected digest when present.
	if expectedChecksum != "" {
		actual, err := fileChecksumSHA256(tmpPath)
		if err != nil {
			return "", fmt.Errorf("checksum wasm module: %w", err)
		}
		if actual != expectedChecksum {
			w.logger.Warn("wasm module checksum mismatch, rejecting", "url", url,
				"expected_sha256", expectedChecksum, "actual_sha256", actual)
			return "", fmt.Errorf("wasm module checksum mismatch: expected %s, got %s", expectedChecksum, actual)
		}
	}

	// Verify WASM magic bytes before committing.
	if err := verifyWasmMagic(tmpPath); err != nil {
		return "", err
	}

	// Commit the verified bytes to the cache path.
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return "", fmt.Errorf("rename to cache: %w", err)
	}

	return cachePath, nil
}

// verifyWasmMagic checks that the file starts with the WASM magic bytes (\x00asm).
func verifyWasmMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open wasm module: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat wasm module: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("wasm module is empty")
	}

	magic := make([]byte, 4)
	n, err := f.Read(magic)
	if err != nil || n < 4 {
		return fmt.Errorf("wasm module too small (need at least 4 bytes for magic header)")
	}

	if !bytes.Equal(magic, wasmMagic) {
		return fmt.Errorf("invalid wasm module: bad magic bytes (got %x, want 0061736d)", magic)
	}

	return nil
}

// downloadToFile downloads a URL to the given path using atomic write.
func (w *WasmRuntime) downloadToFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

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
