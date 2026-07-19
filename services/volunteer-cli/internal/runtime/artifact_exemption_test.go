package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/lettuce-compute/infrastructure/netguard"
)

// PB-6 regression coverage: the artifact netguard needs an explicit, loud,
// OFF-by-default local-testing opt-in — scoped to named heads, with zero
// weakening for every other head. These tests run the REAL production guarded
// client (no injected test client) against a loopback artifact server, which is
// exactly the documented local-testing topology the released volunteer used to
// refuse unconditionally.

// prepareAgainstLoopback runs NativeRuntime.Prepare — with its production
// guarded client — for a unit whose binary is served from a loopback httptest
// server, and returns Prepare's error.
func prepareAgainstLoopback(t *testing.T, sourceHead string) error {
	t.Helper()

	payload := []byte("#!/bin/sh\nexit 0\n")
	sum := sha256.Sum256(payload)
	checksum := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	nr := NewNativeRuntime(t.TempDir(), logger) // production guarded client — NOT overridden

	wu := &WorkUnit{
		ID:         "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		LeafID:     "3fa85f64-5717-4562-b3fc-2c963f66afa7",
		Runtime:    "native",
		SourceHead: sourceHead,
		ExecutionSpec: ExecutionSpec{
			Binaries:        map[string]string{platformKey(): srv.URL + "/bin"},
			BinaryChecksums: map[string]string{platformKey(): checksum},
		},
	}

	prep, err := nr.Prepare(context.Background(), wu)
	if err == nil {
		nr.Cleanup(prep)
	}
	return err
}

// Without the opt-in, a loopback artifact URL must be refused by the netguard —
// the guard's default posture is not weakened by this feature existing.
func TestArtifactNetguard_DefaultRefusesLoopback(t *testing.T) {
	t.Setenv(AllowPrivateArtifactsEnv, "")
	err := prepareAgainstLoopback(t, "local-head")
	if err == nil {
		t.Fatal("Prepare succeeded against a loopback artifact URL with no opt-in; the netguard default must refuse it")
	}
	if !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Fatalf("expected the netguard refusal, got: %v", err)
	}
}

// The core PB-6 fix: a head explicitly named in the env opt-in may serve units
// whose artifacts live at private/loopback addresses.
func TestArtifactNetguard_OptInAllowsLoopbackForNamedHead(t *testing.T) {
	t.Setenv(AllowPrivateArtifactsEnv, "local-head")
	if err := prepareAgainstLoopback(t, "local-head"); err != nil {
		t.Fatalf("Prepare failed for an explicitly opted-in head: %v", err)
	}
}

// Scoping: the opt-in names heads. A unit from any OTHER head keeps the full
// dial screen even while the env var is set.
func TestArtifactNetguard_OptInDoesNotCoverOtherHeads(t *testing.T) {
	t.Setenv(AllowPrivateArtifactsEnv, "my-dev-head")
	err := prepareAgainstLoopback(t, "some-public-head")
	if !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Fatalf("expected the netguard refusal for a non-opted-in head, got: %v", err)
	}
}

// A unit with no recorded source head never matches the opt-in, even against a
// sloppy env value with empty entries.
func TestArtifactNetguard_EmptySourceHeadNeverExempt(t *testing.T) {
	t.Setenv(AllowPrivateArtifactsEnv, " , ,")
	err := prepareAgainstLoopback(t, "")
	if !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Fatalf("expected the netguard refusal for an unknown source head, got: %v", err)
	}
}

// There is no wildcard: "*" is a head name, not a match-everything.
func TestArtifactNetguard_NoWildcard(t *testing.T) {
	t.Setenv(AllowPrivateArtifactsEnv, "*")
	err := prepareAgainstLoopback(t, "local-head")
	if !errors.Is(err, netguard.ErrDisallowedAddress) {
		t.Fatalf("expected the netguard refusal ('*' must not act as a wildcard), got: %v", err)
	}
}
