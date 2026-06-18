package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// buildTestBinary compiles a small Go program into a temporary binary and
// returns its path. The caller is responsible for cleanup.
func buildTestBinary(t *testing.T, name, source string) string {
	t.Helper()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write test source: %v", err)
	}

	binName := name
	if goruntime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(dir, binName)

	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build test binary %s: %v\n%s", name, err, out)
	}
	return binPath
}

// echoSource is a Go program that writes "hello\n" to the LETTUCE_OUTPUT_FILE.
const echoSource = `package main

import (
	"os"
)

func main() {
	outFile := os.Getenv("LETTUCE_OUTPUT_FILE")
	if outFile == "" {
		os.Stdout.WriteString("hello\n")
		return
	}
	os.WriteFile(outFile, []byte("hello\n"), 0644)
}
`

// sleepSource is a Go program that sleeps for 30 seconds.
const sleepSource = `package main

import "time"

func main() {
	time.Sleep(30 * time.Second)
}
`

// inputReaderSource reads the input file and copies it to output.
const inputReaderSource = `package main

import "os"

func main() {
	inputFile := os.Getenv("LETTUCE_INPUT_FILE")
	outFile := os.Getenv("LETTUCE_OUTPUT_FILE")
	data, err := os.ReadFile(inputFile)
	if err != nil {
		os.Exit(1)
	}
	os.WriteFile(outFile, data, 0644)
}
`

// paramsReaderSource verifies the params file env var is set and non-empty.
const paramsReaderSource = `package main

import "os"

func main() {
	paramsFile := os.Getenv("LETTUCE_PARAMS_FILE")
	outFile := os.Getenv("LETTUCE_OUTPUT_FILE")
	if paramsFile == "" {
		os.WriteFile(outFile, []byte("no params"), 0644)
		return
	}
	data, err := os.ReadFile(paramsFile)
	if err != nil {
		os.WriteFile(outFile, []byte("read error"), 0644)
		return
	}
	os.WriteFile(outFile, data, 0644)
}
`

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// nativeSpec builds an ExecutionSpec for the current platform with the given
// binary URL and a BinaryChecksums entry computed from data. Native binaries are
// fail-closed (C2), so every native test must supply a matching checksum.
func nativeSpec(url string, data []byte) ExecutionSpec {
	pk := platformKey()
	return ExecutionSpec{
		Binaries:        map[string]string{pk: url},
		BinaryChecksums: map[string]string{pk: checksumSHA256(data)},
	}
}

func TestCanHandle(t *testing.T) {
	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	pk := platformKey()

	t.Run("matching platform", func(t *testing.T) {
		spec := &ExecutionSpec{
			Binaries: map[string]string{pk: "https://example.com/bin"},
		}
		if !nr.CanHandle(spec) {
			t.Error("expected CanHandle to return true for current platform")
		}
	})

	t.Run("unknown platform", func(t *testing.T) {
		spec := &ExecutionSpec{
			Binaries: map[string]string{"plan9_mips": "https://example.com/bin"},
		}
		if nr.CanHandle(spec) {
			t.Error("expected CanHandle to return false for unknown platform")
		}
	})

	t.Run("nil spec", func(t *testing.T) {
		if nr.CanHandle(nil) {
			t.Error("expected CanHandle to return false for nil spec")
		}
	})

	t.Run("empty binaries", func(t *testing.T) {
		spec := &ExecutionSpec{Binaries: map[string]string{}}
		if nr.CanHandle(spec) {
			t.Error("expected CanHandle to return false for empty binaries")
		}
	})
}

func TestPrepareAndCache(t *testing.T) {
	// Build a test binary.
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, err := os.ReadFile(echoBin)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}

	// Serve it via a test HTTP server.
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:            "412820d8-342d-4df7-83dc-a1123d00afe5", // was test-wu-1
		Runtime:       "native",
		ExecutionSpec: nativeSpec(ts.URL+"/binary", echoBinData),
	}

	// First Prepare: should download.
	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.BinaryPath == "" {
		t.Fatal("BinaryPath is empty")
	}
	if prep.WorkDir == "" {
		t.Fatal("WorkDir is empty")
	}
	if requestCount != 1 {
		t.Errorf("expected 1 HTTP request, got %d", requestCount)
	}

	// Verify binary exists and is executable.
	info, err := os.Stat(prep.BinaryPath)
	if err != nil {
		t.Fatalf("stat cached binary: %v", err)
	}
	if goruntime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Error("binary is not executable")
	}

	// Cleanup for next call.
	nr.Cleanup(prep)

	// Second Prepare with same URL: should use cache (no HTTP request).
	wu2 := &WorkUnit{
		ID:            "8fa383f3-fea9-465b-8e3a-94d9d46eb639", // was test-wu-2
		Runtime:       "native",
		ExecutionSpec: nativeSpec(ts.URL+"/binary", echoBinData),
	}
	prep2, err := nr.Prepare(context.Background(), wu2)
	if err != nil {
		t.Fatalf("Prepare (cached): %v", err)
	}
	if requestCount != 1 {
		t.Errorf("expected cache hit (still 1 request), got %d", requestCount)
	}
	nr.Cleanup(prep2)
}

func TestExecute(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "727e4c12-d568-4268-8a36-e60759d26e06", // was exec-test-1
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify output.
	if string(result.OutputData) != "hello\n" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "hello\n")
	}

	// Verify checksum.
	expectedChecksum := checksumSHA256([]byte("hello\n"))
	if result.OutputChecksum != expectedChecksum {
		t.Errorf("OutputChecksum = %q, want %q", result.OutputChecksum, expectedChecksum)
	}

	// Verify exit code.
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}

	// Verify metrics have non-zero values.
	if result.Metrics.WallClockSeconds < 1 {
		// Wall clock should be at least 1 second (ceil).
		// For very fast executions it could be 1 due to ceiling.
		t.Logf("WallClockSeconds = %d (may be 0 for very fast execution)", result.Metrics.WallClockSeconds)
	}
	if result.Metrics.CPUCoresUsed < 1 {
		t.Errorf("CPUCoresUsed = %d, want >= 1", result.Metrics.CPUCoresUsed)
	}
}

func TestExecuteTimeout(t *testing.T) {
	sleepBin := buildTestBinary(t, "sleeper", sleepSource)
	sleepBinData, _ := os.ReadFile(sleepBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(sleepBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "6cc1d0e9-0cbc-4f25-85ab-6605a2d6fe2b", // was timeout-test
		Runtime:         "native",
		DeadlineSeconds: 1, // 1 second deadline â€” process sleeps for 30s
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", sleepBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	start := time.Now()
	_, err = nr.Execute(context.Background(), wu, prep)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from timed-out execution")
	}
	if !strings.Contains(err.Error(), "cancelled") && !strings.Contains(err.Error(), "killed") && !strings.Contains(err.Error(), "deadline") {
		// On some platforms the error message may vary.
		t.Logf("timeout error (acceptable): %v", err)
	}

	// Should have completed in ~1-2 seconds, not 30.
	if elapsed > 10*time.Second {
		t.Errorf("execution took %v, expected ~1s timeout", elapsed)
	}
}

func TestExecuteWithInputData(t *testing.T) {
	inputBin := buildTestBinary(t, "inputreader", inputReaderSource)
	inputBinData, _ := os.ReadFile(inputBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(inputBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	testData := []byte("test input data 12345")
	wu := &WorkUnit{
		ID:              "d10bfdfb-27b8-4b20-8fb9-e603696cd7fa", // was input-test
		Runtime:         "native",
		InputData:       testData,
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", inputBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	// Verify input file was written.
	if prep.InputPath == "" {
		t.Fatal("InputPath is empty")
	}
	inputContent, err := os.ReadFile(prep.InputPath)
	if err != nil {
		t.Fatalf("read input file: %v", err)
	}
	if string(inputContent) != string(testData) {
		t.Errorf("input file content = %q, want %q", inputContent, testData)
	}

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Output should be the same as input (the binary copies input to output).
	if string(result.OutputData) != string(testData) {
		t.Errorf("OutputData = %q, want %q", result.OutputData, testData)
	}
}

func TestExecuteWithParams(t *testing.T) {
	paramsBin := buildTestBinary(t, "paramsreader", paramsReaderSource)
	paramsBinData, _ := os.ReadFile(paramsBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(paramsBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	params := `{"iterations": 100, "seed": 42}`
	wu := &WorkUnit{
		ID:              "be4a6be7-debc-46c7-89f3-a1d5195951f2", // was params-test
		Runtime:         "native",
		ParametersJSON:  params,
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", paramsBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Binary reads params file and writes content to output.
	if string(result.OutputData) != params {
		t.Errorf("OutputData = %q, want %q", result.OutputData, params)
	}
}

func TestCleanup(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:            "2a513923-d19c-4269-82ba-043bde6ab4c9", // was cleanup-test
		Runtime:       "native",
		ExecutionSpec: nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Verify work dir exists.
	if _, err := os.Stat(prep.WorkDir); err != nil {
		t.Fatalf("work dir should exist: %v", err)
	}

	cachePath := prep.BinaryPath

	// Cleanup.
	if err := nr.Cleanup(prep); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Work dir should be gone.
	if _, err := os.Stat(prep.WorkDir); !os.IsNotExist(err) {
		t.Error("work dir should be removed after cleanup")
	}

	// Cache should still exist.
	if _, err := os.Stat(cachePath); err != nil {
		t.Error("cached binary should be preserved after cleanup")
	}
}

func TestCleanupNil(t *testing.T) {
	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	if err := nr.Cleanup(nil); err != nil {
		t.Errorf("Cleanup(nil) should not error: %v", err)
	}
	if err := nr.Cleanup(&PrepareResult{}); err != nil {
		t.Errorf("Cleanup(empty) should not error: %v", err)
	}
}

func TestContextCancellationDuringPrepare(t *testing.T) {
	// Server that delays response.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
		w.Write([]byte("binary"))
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// The download times out before any checksum verification, but a checksum
	// is still required to pass the fail-closed gate.
	wu := &WorkUnit{
		ID:            "58ff98f8-b664-4ec6-8a99-9ba62261365a", // was cancel-test
		Runtime:       "native",
		ExecutionSpec: nativeSpec(ts.URL+"/binary", []byte("binary")),
	}

	_, err := nr.Prepare(ctx, wu)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// TestPrepareChecksumMatch verifies that a native binary with a matching
// checksum is accepted and prepared successfully (C2 happy path).
func TestPrepareChecksumMatch(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:            "ca813595-53b6-421e-8dbb-44134a608540", // was checksum-match
		Runtime:       "native",
		ExecutionSpec: nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare with matching checksum should succeed: %v", err)
	}
	defer nr.Cleanup(prep)

	// The cache path must be keyed by the expected content checksum.
	expected := checksumSHA256(echoBinData)
	if !strings.Contains(filepath.ToSlash(prep.BinaryPath), expected) {
		t.Errorf("cache path %q should be keyed by checksum %q", prep.BinaryPath, expected)
	}
}

// TestPrepareVizBrokenNonFatal verifies the core TODO #39 guarantee: a broken
// viz bundle in the spec must NOT block compute. Prepare succeeds and returns an
// empty VizBundlePath (viz is a dashboard-only concern the binary never reads).
func TestPrepareVizBrokenNonFatal(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	// A viz tarball with NO index.html anywhere -> extraction always fails.
	brokenViz := createTestTarball(t, map[string]string{
		"./main.js":   "console.log('no index');",
		"./style.css": "body{}",
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "viz") {
			w.Write(brokenViz)
			return
		}
		w.Write(echoBinData)
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()

	pk := platformKey()
	wu := &WorkUnit{
		ID:      "d2b7c0a1-9f3e-4c21-bb55-0a1b2c3d4e5f",
		Runtime: "native",
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{pk: ts.URL + "/binary", "viz": ts.URL + "/beyblade-viz.tar.gz"},
			BinaryChecksums: map[string]string{pk: checksumSHA256(echoBinData), "viz": checksumSHA256(brokenViz)},
		},
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("broken viz must NOT fail compute prepare: %v", err)
	}
	defer nr.Cleanup(prep)
	if prep.VizBundlePath != "" {
		t.Errorf("expected empty VizBundlePath on broken viz, got %q", prep.VizBundlePath)
	}
	if prep.BinaryPath == "" {
		t.Error("expected binary to be prepared despite broken viz")
	}
}

// TestPrepareVizWrappedDir verifies that a viz bundle wrapped in a single
// top-level directory (the beyblade-viz shape) prepares successfully end-to-end,
// with VizBundlePath pointing at the resolved wrapper root (TODO #39).
func TestPrepareVizWrappedDir(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	wrappedViz := createTestTarball(t, map[string]string{
		"beyblade-viz/index.html": "<html>wrapped</html>",
		"beyblade-viz/player.js":  "console.log('ok');",
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "viz") {
			w.Write(wrappedViz)
			return
		}
		w.Write(echoBinData)
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()

	pk := platformKey()
	wu := &WorkUnit{
		ID:      "f1e2d3c4-b5a6-4978-8869-7a6b5c4d3e2f",
		Runtime: "native",
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{pk: ts.URL + "/binary", "viz": ts.URL + "/beyblade-viz.tar.gz"},
			BinaryChecksums: map[string]string{pk: checksumSHA256(echoBinData), "viz": checksumSHA256(wrappedViz)},
		},
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("wrapped viz should prepare: %v", err)
	}
	defer nr.Cleanup(prep)
	if prep.VizBundlePath == "" {
		t.Fatal("expected non-empty VizBundlePath for wrapped viz")
	}
	if _, err := os.Stat(filepath.Join(prep.VizBundlePath, "index.html")); err != nil {
		t.Errorf("index.html should resolve under VizBundlePath: %v", err)
	}
}

// TestPrepareChecksumMismatch verifies that a native binary whose bytes do not
// match the declared checksum is rejected (C2 anti-tamper).
func TestPrepareChecksumMismatch(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()

	pk := platformKey()
	// Declare a checksum for different content than what is served.
	wrong := checksumSHA256([]byte("not the real binary"))
	wu := &WorkUnit{
		ID:      "eecc8723-b2e5-49ce-8f47-83146893d22f", // was checksum-mismatch
		Runtime: "native",
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{pk: ts.URL + "/binary"},
			BinaryChecksums: map[string]string{pk: wrong},
		},
	}

	_, err := nr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error for binary checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want to contain 'checksum mismatch'", err)
	}
}

// TestPrepareMissingChecksumRejected verifies the fail-closed policy: a native
// binary with no declared checksum is rejected before any download/exec (C2).
func TestPrepareMissingChecksumRejected(t *testing.T) {
	requested := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		w.Write([]byte("binary"))
	}))
	defer ts.Close()

	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	nr.httpClient = ts.Client()

	pk := platformKey()
	wu := &WorkUnit{
		ID:      "8fcd5cae-c17e-47fa-8cf8-1be2ce041ad4", // was no-checksum
		Runtime: "native",
		ExecutionSpec: ExecutionSpec{
			Binaries: map[string]string{pk: ts.URL + "/binary"},
			// No BinaryChecksums entry -> must fail closed.
		},
	}

	_, err := nr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error: native binary without checksum must be rejected")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error = %q, want to mention missing checksum", err)
	}
	if requested {
		t.Error("binary should not be downloaded when no checksum is provided (fail-closed)")
	}
}

func TestChecksumSHA256(t *testing.T) {
	data := []byte("hello world")
	h := sha256.Sum256(data)
	expected := hex.EncodeToString(h[:])

	got := checksumSHA256(data)
	if got != expected {
		t.Errorf("checksumSHA256 = %q, want %q", got, expected)
	}
}

func TestChecksumSHA256Empty(t *testing.T) {
	got := checksumSHA256(nil)
	// SHA-256 of empty input.
	h := sha256.Sum256(nil)
	expected := hex.EncodeToString(h[:])
	if got != expected {
		t.Errorf("checksumSHA256(nil) = %q, want %q", got, expected)
	}
}

func TestName(t *testing.T) {
	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	if nr.Name() != "native" {
		t.Errorf("Name() = %q, want %q", nr.Name(), "native")
	}
}

func TestCommandModifier(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	// Set a modifier that adds an env var.
	modifierCalled := false
	nr.SetCommandModifier(func(cmd *exec.Cmd) error {
		modifierCalled = true
		return nil
	})

	wu := &WorkUnit{
		ID:              "31a8397c-bcab-4312-83a3-7b30fdb30f1f", // was modifier-test
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	_, err = nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !modifierCalled {
		t.Error("command modifier was not called")
	}
}

func TestCommandModifierError(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	nr.SetCommandModifier(func(cmd *exec.Cmd) error {
		return fmt.Errorf("resource limit exceeded")
	})

	wu := &WorkUnit{
		ID:              "ccdb5adf-56dd-4217-8ac0-1255154e3fbc", // was modifier-err-test
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	_, err = nr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from modifier")
	}
	if !strings.Contains(err.Error(), "resource limit exceeded") {
		t.Errorf("error = %q, want to contain %q", err, "resource limit exceeded")
	}
}

// stdoutOnlySource writes to stdout, not to LETTUCE_OUTPUT_FILE.
const stdoutOnlySource = `package main

import "fmt"

func main() {
	fmt.Print("stdout output")
}
`

// exitCodeSource exits with code 2.
const exitCodeSource = `package main

import "os"

func main() {
	os.Exit(2)
}
`

func TestLimitedWriter(t *testing.T) {
	t.Run("normal write", func(t *testing.T) {
		var buf strings.Builder
		lw := &limitedWriter{w: &buf, remaining: 100}
		n, err := lw.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if buf.String() != "hello" {
			t.Errorf("buf = %q, want %q", buf.String(), "hello")
		}
		if lw.written != 5 {
			t.Errorf("written = %d, want 5", lw.written)
		}
	})

	t.Run("write exceeding limit", func(t *testing.T) {
		var buf strings.Builder
		lw := &limitedWriter{w: &buf, remaining: 3}
		n, err := lw.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("Write error: %v", err)
		}
		// Reports full length to caller.
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		// But only writes 3 bytes.
		if buf.String() != "hel" {
			t.Errorf("buf = %q, want %q", buf.String(), "hel")
		}
		if lw.written != 3 {
			t.Errorf("written = %d, want 3", lw.written)
		}
	})

	t.Run("write after limit exhausted", func(t *testing.T) {
		var buf strings.Builder
		lw := &limitedWriter{w: &buf, remaining: 0}
		n, err := lw.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if buf.String() != "" {
			t.Errorf("buf = %q, want empty", buf.String())
		}
	})
}

func TestExecuteStdoutFallback(t *testing.T) {
	stdoutBin := buildTestBinary(t, "stdoutonly", stdoutOnlySource)
	stdoutBinData, _ := os.ReadFile(stdoutBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(stdoutBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "c5b46d7c-a1db-4609-8ac0-febad508b743", // was stdout-test
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", stdoutBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Output should come from stdout fallback.
	if string(result.OutputData) != "stdout output" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "stdout output")
	}
	if result.OutputChecksum != checksumSHA256([]byte("stdout output")) {
		t.Error("checksum mismatch for stdout fallback output")
	}
}

func TestExecuteNonZeroExitCode(t *testing.T) {
	exitBin := buildTestBinary(t, "exitcode", exitCodeSource)
	exitBinData, _ := os.ReadFile(exitBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(exitBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "580e0e80-7742-4881-8ffc-b781dd657a09", // was exitcode-test
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", exitBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute should not error for non-zero exit: %v", err)
	}
	if result.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", result.ExitCode)
	}
}

func TestPrepareInputDataURL(t *testing.T) {
	echoBin := buildTestBinary(t, "inputreader", inputReaderSource)
	echoBinData, _ := os.ReadFile(echoBin)

	inputData := []byte("data from URL")
	binaryServed := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/input.dat" {
			w.Write(inputData)
			return
		}
		binaryServed = true
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "0acb8b45-bc42-4c3d-8f68-be8128476159", // was input-url-test
		Runtime:         "native",
		InputDataURL:    ts.URL + "/input.dat",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	if !binaryServed {
		t.Error("binary was not downloaded")
	}
	if prep.InputPath == "" {
		t.Fatal("InputPath is empty")
	}
	got, err := os.ReadFile(prep.InputPath)
	if err != nil {
		t.Fatalf("read input: %v", err)
	}
	if string(got) != "data from URL" {
		t.Errorf("input = %q, want %q", got, "data from URL")
	}

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(result.OutputData) != "data from URL" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "data from URL")
	}
}

func TestDownloadFileErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	// 404 download â€” checksum required to pass the fail-closed gate, but
	// verification is never reached because the download itself fails.
	wu := &WorkUnit{
		ID:            "7c66b21d-36fe-4ca7-82f4-044a63fa8c40", // was dl-error-test
		Runtime:       "native",
		ExecutionSpec: nativeSpec(ts.URL+"/binary", []byte("unused")),
	}

	_, err := nr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error for 404 download")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want to contain '404'", err)
	}
}

func TestPrepareNoMatchingPlatform(t *testing.T) {
	nr := NewNativeRuntime(t.TempDir(), newTestLogger())
	wu := &WorkUnit{
		ID:      "5f801756-2444-43ed-84f3-d3d5c7098fed", // was no-platform
		Runtime: "native",
		ExecutionSpec: ExecutionSpec{
			Binaries: map[string]string{"plan9_mips": "https://example.com/bin"},
		},
	}
	_, err := nr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error for missing platform binary")
	}
}

func TestProcessNotifier(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	// Set a notifier that records the PID and returns a cleanup.
	var notifiedPID int
	cleanupCalled := false
	nr.SetProcessNotifier(func(pid int) (func(), error) {
		notifiedPID = pid
		return func() { cleanupCalled = true }, nil
	})

	wu := &WorkUnit{
		ID:              "e6d23da4-ed83-48f7-8132-dce77d59449d", // was notifier-test
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	result, err := nr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if notifiedPID <= 0 {
		t.Errorf("process notifier was not called or PID is invalid: %d", notifiedPID)
	}
	if !cleanupCalled {
		t.Error("cleanup function from process notifier was not called")
	}
}

func TestProcessNotifierError(t *testing.T) {
	echoBin := buildTestBinary(t, "echo", echoSource)
	echoBinData, _ := os.ReadFile(echoBin)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(echoBinData)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	nr := NewNativeRuntime(dataDir, newTestLogger())
	nr.httpClient = ts.Client()

	// Set a notifier that returns an error.
	nr.SetProcessNotifier(func(pid int) (func(), error) {
		return nil, fmt.Errorf("cgroup creation failed")
	})

	wu := &WorkUnit{
		ID:              "b949e731-1465-4514-80f7-96fb46cfcf30", // was notifier-err-test
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", echoBinData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	_, err = nr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from notifier failure")
	}
	if !strings.Contains(err.Error(), "cgroup creation failed") {
		t.Errorf("error = %q, want to contain %q", err, "cgroup creation failed")
	}
}
