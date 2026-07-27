package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Regression test for TB-10 — a native leaf that fails produced no visible error.
//
// The container runtime has WARNed on a non-zero exit, with a tail of the
// captured execution log, since PB-23. The native runtime never did: it logged
// only "execution finished ... exit_code=2" at INFO, and the work dir carrying
// execution.log was removed moments later. A volunteer whose native units were
// all failing therefore had nothing in the daemon log that read as a problem,
// and reported that they were "never sent native work" while seventeen units
// were being fetched, failed and abandoned.

// failingSource is a program that writes a diagnosable message to stderr and
// exits non-zero — the shape of the real failure (a binary that dies during
// runtime init, TB-11).
const failingSource = `package main

import (
	"os"
)

func main() {
	os.Stderr.WriteString("fatal error: runtime: out of memory\n")
	os.Exit(2)
}
`

// TestNativeNonZeroExitLogsTheProcessOutput: a native process that exits
// non-zero must produce a WARN carrying what it printed, so the reason for the
// failure is in the volunteer's log before the work dir is cleaned up.
func TestNativeNonZeroExitLogsTheProcessOutput(t *testing.T) {
	bin := buildTestBinary(t, "failing", failingSource)
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(binData)
	}))
	defer ts.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	nr := NewNativeRuntime(t.TempDir(), logger)
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "9a4c0e18-1b2f-4d6a-9d31-5c7f0a2b3c4d",
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", binData),
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
	if result.ExitCode != 2 {
		t.Fatalf("ExitCode = %d, want 2 — the test binary did not fail as intended", result.ExitCode)
	}

	out := logs.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("a non-zero native exit produced no WARN; the failure is invisible at the default log level.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "out of memory") {
		t.Errorf("the WARN does not carry the failing process's own output, which is the only thing that explains the failure.\nlog:\n%s", out)
	}
	if !strings.Contains(out, ExecutionLogName) {
		t.Errorf("the WARN does not name the execution log's path, so a volunteer cannot go read the rest of it.\nlog:\n%s", out)
	}
}

// TestNativeCleanExitStaysQuiet: the WARN must fire only on failure. A healthy
// volunteer's log filling with warnings would train them to ignore it — the same
// failure mode TB-12 was filed for on the doctor side.
func TestNativeCleanExitStaysQuiet(t *testing.T) {
	bin := buildTestBinary(t, "echo-ok", echoSource)
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(binData)
	}))
	defer ts.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	nr := NewNativeRuntime(t.TempDir(), logger)
	nr.httpClient = ts.Client()

	wu := &WorkUnit{
		ID:              "1f2e3d4c-5b6a-4978-8695-0a1b2c3d4e5f",
		Runtime:         "native",
		DeadlineSeconds: 30,
		ExecutionSpec:   nativeSpec(ts.URL+"/binary", binData),
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer nr.Cleanup(prep)

	if _, err := nr.Execute(context.Background(), wu, prep); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(logs.String(), "exited non-zero") {
		t.Errorf("a clean run logged a non-zero-exit warning:\n%s", logs.String())
	}
}

// TestExecutionLogTailNamesAMissingLogExplicitly: an absent log must read as
// "nothing was captured", never as "the process printed nothing" — those are
// different facts and only one of them is a clue.
func TestExecutionLogTailNamesAMissingLogExplicitly(t *testing.T) {
	if got := ExecutionLogTail(t.TempDir()); got != noExecutionLog {
		t.Errorf("ExecutionLogTail on a dir with no log = %q, want %q", got, noExecutionLog)
	}
	if got := ExecutionLogTail(""); got != noExecutionLog {
		t.Errorf("ExecutionLogTail(\"\") = %q, want %q", got, noExecutionLog)
	}
	if got := ExecutionLogSummary(t.TempDir(), 100); got != "" {
		t.Errorf("ExecutionLogSummary with no log = %q, want empty so callers can omit it entirely", got)
	}
}
