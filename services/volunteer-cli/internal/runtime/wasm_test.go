package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestWasmBinary compiles a Go program to WASM/WASI and returns the path.
func buildTestWasmBinary(t *testing.T, name, source string) string {
	t.Helper()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write test source: %v", err)
	}

	binPath := filepath.Join(dir, name+".wasm")

	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build test wasm binary %s: %v\n%s", name, err, out)
	}
	return binPath
}

// wasmEchoSource writes "hello\n" to LETTUCE_OUTPUT_FILE or stdout.
const wasmEchoSource = `package main

import "os"

func main() {
	outFile := os.Getenv("LETTUCE_OUTPUT_FILE")
	if outFile == "" {
		os.Stdout.WriteString("hello\n")
		return
	}
	os.WriteFile(outFile, []byte("hello\n"), 0644)
}
`

// wasmInputReaderSource reads LETTUCE_INPUT_FILE and copies to LETTUCE_OUTPUT_FILE.
const wasmInputReaderSource = `package main

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

// wasmParamsReaderSource reads LETTUCE_PARAMS_FILE to LETTUCE_OUTPUT_FILE.
const wasmParamsReaderSource = `package main

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

// wasmStdoutOnlySource writes to stdout only (no output.dat).
const wasmStdoutOnlySource = `package main

import "os"

func main() {
	os.Stdout.WriteString("stdout output\n")
}
`

// wasmBusyLoopSource runs an infinite loop making WASI calls (for cancellation testing).
// time.Sleep doesn't reliably block in wazero's WASI, so we use fd_write instead.
const wasmBusyLoopSource = `package main

import "os"

func main() {
	for {
		os.Stdout.Write([]byte("."))
	}
}
`

// serveWasmBinary starts an httptest.Server serving the given WASM binary data.
// Returns the server and a pointer to the request count.
func serveWasmBinary(t *testing.T, wasmPath string) (*httptest.Server, *int) {
	t.Helper()
	wasmData, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read wasm binary: %v", err)
	}
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Write(wasmData)
	}))
	t.Cleanup(ts.Close)
	return ts, &requestCount
}

// wasmSpec builds an ExecutionSpec for the module served at url, declaring the
// module file's correct SHA-256. A checksum is REQUIRED since PB-33 (Prepare
// fail-closes without one), so every prepare-path test declares it.
func wasmSpec(t *testing.T, wasmPath, url string) ExecutionSpec {
	t.Helper()
	sum, err := fileChecksumSHA256(wasmPath)
	if err != nil {
		t.Fatalf("checksum test wasm binary: %v", err)
	}
	return ExecutionSpec{
		Binaries:        map[string]string{"wasm": url},
		BinaryChecksums: map[string]string{"wasm": sum},
	}
}

func TestWasmRuntime_Name(t *testing.T) {
	wr := NewWasmRuntime(t.TempDir(), newTestLogger())
	if wr.Name() != "wasm" {
		t.Errorf("Name() = %q, want %q", wr.Name(), "wasm")
	}
}

func TestWasmRuntime_CanHandle(t *testing.T) {
	wr := NewWasmRuntime(t.TempDir(), newTestLogger())

	t.Run("with wasm binary", func(t *testing.T) {
		spec := &ExecutionSpec{
			Binaries: map[string]string{"wasm": "https://example.com/module.wasm"},
		}
		if !wr.CanHandle(spec) {
			t.Error("expected CanHandle to return true with wasm binary")
		}
	})

	t.Run("without wasm binary", func(t *testing.T) {
		spec := &ExecutionSpec{
			Binaries: map[string]string{"linux_amd64": "https://example.com/bin"},
		}
		if wr.CanHandle(spec) {
			t.Error("expected CanHandle to return false without wasm binary")
		}
	})

	t.Run("with container image", func(t *testing.T) {
		spec := &ExecutionSpec{
			Binaries: map[string]string{"wasm": "https://example.com/module.wasm"},
			Image:    "ubuntu:latest",
		}
		if wr.CanHandle(spec) {
			t.Error("expected CanHandle to return false when Image is set")
		}
	})

	t.Run("nil spec", func(t *testing.T) {
		if wr.CanHandle(nil) {
			t.Error("expected CanHandle to return false for nil spec")
		}
	})

	t.Run("empty binaries", func(t *testing.T) {
		spec := &ExecutionSpec{Binaries: map[string]string{}}
		if wr.CanHandle(spec) {
			t.Error("expected CanHandle to return false for empty binaries")
		}
	})

	t.Run("empty wasm URL", func(t *testing.T) {
		spec := &ExecutionSpec{
			Binaries: map[string]string{"wasm": ""},
		}
		if wr.CanHandle(spec) {
			t.Error("expected CanHandle to return false for empty wasm URL")
		}
	})
}

func TestWasmRuntime_PrepareAndExecute(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	ts, requestCount := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "412820d8-342d-4df7-83dc-a1123d00afe5", // was test-wu-1
		Runtime: "wasm",
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}

	// Prepare.
	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer wr.Cleanup(prep)

	if prep.BinaryPath == "" {
		t.Fatal("BinaryPath is empty")
	}
	if prep.WorkDir == "" {
		t.Fatal("WorkDir is empty")
	}
	if *requestCount != 1 {
		t.Errorf("expected 1 HTTP request, got %d", *requestCount)
	}

	// Execute.
	result, err := wr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if string(result.OutputData) != "hello\n" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "hello\n")
	}
	if result.OutputChecksum == "" {
		t.Error("OutputChecksum is empty")
	}
	if result.Metrics.WallClockSeconds < 0 {
		t.Error("WallClockSeconds should not be negative")
	}

	// Verify SHA-256 checksum.
	expectedChecksum := checksumSHA256([]byte("hello\n"))
	if result.OutputChecksum != expectedChecksum {
		t.Errorf("checksum = %s, want %s", result.OutputChecksum, expectedChecksum)
	}
}

func TestWasmRuntime_InputData(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "input-reader", wasmInputReaderSource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	inputData := []byte("test input data 12345")
	wu := &WorkUnit{
		ID:        "ae160889-6372-420b-8ebb-58261e0c0092", // was test-input
		Runtime:   "wasm",
		InputData: inputData,
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer wr.Cleanup(prep)

	result, err := wr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(result.OutputData) != string(inputData) {
		t.Errorf("OutputData = %q, want %q", result.OutputData, inputData)
	}
}

func TestWasmRuntime_Parameters(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "params-reader", wasmParamsReaderSource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	params := `{"key": "value", "count": 42}`
	wu := &WorkUnit{
		ID:             "fc91952e-691c-425b-8408-532a91835201", // was test-params
		Runtime:        "wasm",
		ParametersJSON: params,
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer wr.Cleanup(prep)

	result, err := wr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(result.OutputData) != params {
		t.Errorf("OutputData = %q, want %q", result.OutputData, params)
	}
}

func TestWasmRuntime_StdoutFallback(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "stdout-only", wasmStdoutOnlySource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "61fe1c99-971f-4d3a-8d2e-18262ad73626", // was test-stdout
		Runtime: "wasm",
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer wr.Cleanup(prep)

	result, err := wr.Execute(context.Background(), wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(result.OutputData) != "stdout output\n" {
		t.Errorf("OutputData = %q, want %q", result.OutputData, "stdout output\n")
	}
}

func TestWasmRuntime_MemoryLimit(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	spec := wasmSpec(t, wasmPath, ts.URL+"/module.wasm")
	spec.MaxMemoryMB = 1 // Very low; the Go runtime needs more than 1 MB
	wu := &WorkUnit{
		ID:            "ef24bd66-b4b1-4b61-873c-8718b38933be", // was test-memlimit
		Runtime:       "wasm",
		ExecutionSpec: spec,
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer wr.Cleanup(prep)

	// Execute should not panic. It may return an error or non-zero exit code
	// because Go's runtime needs more than 1 MB for initialization.
	result, err := wr.Execute(context.Background(), wu, prep)
	if err == nil && result != nil && result.ExitCode == 0 {
		t.Log("module succeeded despite low memory limit (unexpected but acceptable)")
	}
	// The key assertion: no panic, no crash.
}

func TestWasmRuntime_Cleanup(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "c72fe4b6-d204-4293-8825-f56363737ea5", // was test-cleanup
		Runtime: "wasm",
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Verify work dir exists.
	if _, err := os.Stat(prep.WorkDir); err != nil {
		t.Fatalf("work dir should exist: %v", err)
	}

	// Cleanup.
	if err := wr.Cleanup(prep); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Verify work dir is removed.
	if _, err := os.Stat(prep.WorkDir); !os.IsNotExist(err) {
		t.Error("work dir should be removed after Cleanup")
	}

	// Verify cached module still exists.
	cacheDir := filepath.Join(dataDir, "wasm-cache")
	entries, _ := os.ReadDir(cacheDir)
	if len(entries) == 0 {
		t.Error("cached module should not be removed by Cleanup")
	}
}

func TestWasmRuntime_CachedModule(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	ts, requestCount := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "a01d48dd-60c0-44ab-89aa-e1edd15e2607", // was test-cache-1
		Runtime: "wasm",
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}

	// First prepare â€” should download.
	prep1, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	defer wr.Cleanup(prep1)
	if *requestCount != 1 {
		t.Errorf("expected 1 request after first Prepare, got %d", *requestCount)
	}

	// Second prepare with same URL â€” should use cache.
	wu2 := &WorkUnit{
		ID:      "6df51fcc-8066-40cd-8509-ca56e1621942", // was test-cache-2
		Runtime: "wasm",
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
	}
	prep2, err := wr.Prepare(context.Background(), wu2)
	if err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	defer wr.Cleanup(prep2)

	if *requestCount != 1 {
		t.Errorf("expected still 1 request after second Prepare (cached), got %d", *requestCount)
	}

	// Both should point to the same cached binary.
	if prep1.BinaryPath != prep2.BinaryPath {
		t.Errorf("cache miss: prep1.BinaryPath=%s, prep2.BinaryPath=%s", prep1.BinaryPath, prep2.BinaryPath)
	}
}

func TestWasmRuntime_InvalidWasm(t *testing.T) {
	// Serve a non-WASM file. Its checksum is declared CORRECTLY so the download
	// passes verification and the magic-bytes check is what rejects it.
	notWasm := []byte("this is not a wasm module")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(notWasm)
	}))
	defer ts.Close()

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "e917c45d-0c77-41d7-8378-cc657d2e6e03", // was test-invalid
		Runtime: "wasm",
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{"wasm": ts.URL + "/not-wasm.bin"},
			BinaryChecksums: map[string]string{"wasm": checksumSHA256(notWasm)},
		},
	}

	_, err := wr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error for invalid WASM module")
	}
	if !strings.Contains(err.Error(), "magic") {
		t.Errorf("expected magic bytes error, got: %v", err)
	}
}

// TestWasmRuntime_PrepareRefusesMissingChecksum is the PB-33 regression test:
// a WASM work unit whose execution spec carries NO module checksum must be
// refused at Prepare, exactly like the native runtime — before the fix the
// module was downloaded, cached by URL, and executed unverified ("proceeding
// unverified (sandboxed)"). The gate must fire BEFORE any download.
func TestWasmRuntime_PrepareRefusesMissingChecksum(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	ts, requestCount := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "b7f1d1a2-5c3e-4f60-9d21-0a9b8c7d6e5f",
		Runtime: "wasm",
		ExecutionSpec: ExecutionSpec{
			Binaries: map[string]string{"wasm": ts.URL + "/module.wasm"},
			// No BinaryChecksums entry: the unverified case.
		},
	}

	_, err := wr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("Prepare succeeded on a checksum-less WASM module; must fail closed like native (PB-33)")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error = %q, want a checksum-refusal error", err)
	}
	if *requestCount != 0 {
		t.Errorf("module was downloaded despite the missing checksum (%d requests); the gate must fire before any fetch", *requestCount)
	}
	if entries, _ := os.ReadDir(filepath.Join(dataDir, "wasm-cache")); len(entries) != 0 {
		t.Errorf("unverified module was cached (%d entries); nothing may be committed to the cache", len(entries))
	}
}

// TestWasmRuntime_ChecksumMatch verifies that when a wasm checksum is supplied
// and matches, Prepare succeeds (C2 verification path for wasm).
func TestWasmRuntime_ChecksumMatch(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	wasmData, _ := os.ReadFile(wasmPath)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "0de86a89-ee10-42d5-8e05-5b6991328629", // was wasm-checksum-match
		Runtime: "wasm",
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{"wasm": ts.URL + "/module.wasm"},
			BinaryChecksums: map[string]string{"wasm": checksumSHA256(wasmData)},
		},
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare with matching wasm checksum should succeed: %v", err)
	}
	wr.Cleanup(prep)
}

// TestWasmRuntime_ChecksumMismatch verifies that a wasm module whose bytes do
// not match the declared checksum is rejected (C2 anti-tamper for wasm).
func TestWasmRuntime_ChecksumMismatch(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "echo", wasmEchoSource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "4ed0fc3c-42b4-4e29-80f6-35afe0e23e16", // was wasm-checksum-mismatch
		Runtime: "wasm",
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{"wasm": ts.URL + "/module.wasm"},
			BinaryChecksums: map[string]string{"wasm": checksumSHA256([]byte("wrong content"))},
		},
	}

	_, err := wr.Prepare(context.Background(), wu)
	if err == nil {
		t.Fatal("expected error for wasm checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %q, want to contain 'checksum mismatch'", err)
	}
}

func TestWasmRuntime_ContextCancellation(t *testing.T) {
	wasmPath := buildTestWasmBinary(t, "busyloop", wasmBusyLoopSource)
	ts, _ := serveWasmBinary(t, wasmPath)

	dataDir := t.TempDir()
	wr := NewWasmRuntime(dataDir, newTestLogger())
	wr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:      "97064468-9b0b-4a4e-8a02-b03cafebca80", // was test-cancel
		Runtime: "wasm",
		ExecutionSpec: wasmSpec(t, wasmPath, ts.URL+"/module.wasm"),
		DeadlineSeconds: 3, // Short deadline, module sleeps 30s
	}

	prep, err := wr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer wr.Cleanup(prep)

	_, err = wr.Execute(context.Background(), wu, prep)
	if err == nil {
		t.Fatal("expected error from timed-out execution")
	}
	errMsg := strings.ToLower(err.Error())
	if !strings.Contains(errMsg, "cancel") && !strings.Contains(errMsg, "deadline") {
		t.Errorf("expected cancellation/deadline error, got: %v", err)
	}
}
