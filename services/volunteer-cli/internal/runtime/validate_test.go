package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testUUID derives a deterministic canonical UUID from an arbitrary label so
// table-driven tests can keep readable names while supplying IDs that pass
// ValidateWorkUnitID (H2). Two different labels yield two different UUIDs.
func testUUID(label string) string {
	sum := sha256.Sum256([]byte(label))
	h := hex.EncodeToString(sum[:])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

func TestValidateWorkUnitID_AcceptsCanonicalUUID(t *testing.T) {
	valid := []string{
		"123e4567-e89b-12d3-a456-426614174000",
		"00000000-0000-0000-0000-000000000000",
		"ABCDEF01-2345-6789-ABCD-EF0123456789", // uppercase
		"abcdef01-2345-6789-abcd-ef0123456789", // lowercase
	}
	for _, id := range valid {
		if err := ValidateWorkUnitID(id); err != nil {
			t.Errorf("ValidateWorkUnitID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidateWorkUnitID_RejectsMalicious(t *testing.T) {
	invalid := []string{
		"",                                          // empty
		"../../evil",                                // relative traversal
		"..\\..\\evil",                              // windows traversal
		"../../../../Windows/Temp/evil",             // deep traversal
		"/etc/cron.d/evil",                          // absolute unix path
		`C:\Windows\Temp\evil`,                      // absolute windows path
		"foo/bar",                                   // contains slash
		"foo\\bar",                                  // contains backslash
		"not-a-uuid",                                // plain string
		"123e4567e89b12d3a456426614174000",          // hyphen-less (uuid.Parse would accept this; we reject)
		"{123e4567-e89b-12d3-a456-426614174000}",    // brace-wrapped (uuid.Parse would accept; we reject)
		"urn:uuid:123e4567-e89b-12d3-a456-426614174000", // URN form (uuid.Parse would accept; we reject)
		"123e4567-e89b-12d3-a456-426614174000\x00",  // trailing null byte
		"123e4567-e89b-12d3-a456-42661417400",       // too short
		"123e4567-e89b-12d3-a456-4266141740000",     // too long
		"123e4567-e89b-12d3-a456-42661417400g",      // non-hex char
	}
	for _, id := range invalid {
		if err := ValidateWorkUnitID(id); err == nil {
			t.Errorf("ValidateWorkUnitID(%q) = nil, want error", id)
		}
	}
}

// TestRuntimesRejectTraversalIDBeforeWrite verifies that the native, container,
// and wasm Prepare paths reject a malicious work unit ID before any directory is
// created outside the data dir (H2 defense-in-depth).
func TestRuntimesRejectTraversalIDBeforeWrite(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const evilID = "../../evil"

	t.Run("native", func(t *testing.T) {
		dataDir := t.TempDir()
		nr := NewNativeRuntime(dataDir, logger)
		wu := &WorkUnit{
			ID:            evilID,
			ExecutionSpec: nativeSpec("http://127.0.0.1:0/bin", []byte("x")),
		}
		_, err := nr.Prepare(context.Background(), wu)
		if err == nil {
			t.Fatal("native Prepare accepted traversal ID, want error")
		}
		if !strings.Contains(err.Error(), "invalid work unit ID") {
			t.Errorf("native Prepare error = %q, want 'invalid work unit ID'", err.Error())
		}
		assertNoEscape(t, dataDir)
	})

	t.Run("wasm", func(t *testing.T) {
		dataDir := t.TempDir()
		wr := NewWasmRuntime(dataDir, logger)
		wu := &WorkUnit{
			ID:            evilID,
			ExecutionSpec: ExecutionSpec{Binaries: map[string]string{"wasm": "http://127.0.0.1:0/mod.wasm"}},
		}
		_, err := wr.Prepare(context.Background(), wu)
		if err == nil {
			t.Fatal("wasm Prepare accepted traversal ID, want error")
		}
		if !strings.Contains(err.Error(), "invalid work unit ID") {
			t.Errorf("wasm Prepare error = %q, want 'invalid work unit ID'", err.Error())
		}
		assertNoEscape(t, dataDir)
	})

	t.Run("container", func(t *testing.T) {
		dataDir := t.TempDir()
		cr := NewContainerRuntimeWithClient(dataDir, logger, &MockDockerClient{})
		wu := &WorkUnit{
			ID:            evilID,
			ExecutionSpec: ExecutionSpec{Image: "alpine:latest"},
		}
		_, err := cr.Prepare(context.Background(), wu)
		if err == nil {
			t.Fatal("container Prepare accepted traversal ID, want error")
		}
		if !strings.Contains(err.Error(), "invalid work unit ID") {
			t.Errorf("container Prepare error = %q, want 'invalid work unit ID'", err.Error())
		}
		assertNoEscape(t, dataDir)
	})
}

// assertNoEscape fails if an "evil" directory was created in the data dir's
// parent (i.e. one level up), which is where "../../evil" relative to a
// work subdir would land if traversal were not blocked.
func assertNoEscape(t *testing.T, dataDir string) {
	t.Helper()
	parent := filepath.Dir(dataDir)
	if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
		t.Errorf("traversal escaped: 'evil' directory created at %s", filepath.Join(parent, "evil"))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(parent), "evil")); err == nil {
		t.Errorf("traversal escaped two levels up to %s", filepath.Join(filepath.Dir(parent), "evil"))
	}
}
