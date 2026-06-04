package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/protobuf/proto"
)

// --- F3 hardening: protobuf-go drift guards ------------------------------------
//
// The C1 gRPC auth (see grpc_auth.go) verifies an Ed25519 signature over
//
//	<unix-ts>:<full-method>:<hex(sha256(deterministic-marshal(req)))>
//
// The volunteer-cli client (signer) and this infrastructure server (verifier)
// are SEPARATE Go modules with their own go.mod / go.sum. Both depend on
// google.golang.org/protobuf. If one module's protobuf-go is bumped to a
// version that changes any aspect of deterministic-marshal output (field
// ordering, map sort, varint encoding, ...) and the other isn't, every
// volunteer RPC silently fails Unauthenticated.
//
// Two complementary guards live here and a mirror in volunteer-cli:
//
//   Guard 1 (TestProtoDeterministicMarshal_StableHash):
//     Marshal a fixed, populated *lettucev1.RequestWorkUnitResponse with
//     proto.MarshalOptions{Deterministic: true} and assert the SHA-256 hex
//     of the bytes equals a hardcoded value. The mirror test in
//     services/volunteer-cli/internal/client/proto_determinism_test.go asserts
//     the SAME hash. If either side's protobuf-go changes byte output, that
//     test fails with a clear hash mismatch — the operator sees a deterministic
//     diff long before chasing "Unauthenticated" reports from the field.
//
//   Guard 2 (TestProtoVersionPinAligned):
//     Read both go.mod files and assert they declare the SAME
//     google.golang.org/protobuf version. Catches accidental `go get` bumps
//     in one module before any behavior actually drifts.
//
// The expected hash is NOT a secret. It's a stable test fixture. The chosen
// message exercises:
//   - a repeated nested message (assignments) carrying the per-unit fields,
//   - scalar string / int / bytes fields on the assignment,
//   - a doubly-nested message (ExecutionSpec) which itself contains
//     map<string,string> fields (binaries, binary_checksums),
//   - a map<string,string> field on the assignment (env_vars),
//   - a top-level scalar on the response (retry_after_seconds),
//   - and a deterministic *set* of map entries — deterministic marshal sorts
//     map keys, which is exactly the surface most likely to drift.
//
// To re-baseline the hash (e.g. after an intentional protobuf-go upgrade
// applied to BOTH modules):
//   1. Run this test; copy the hex printed in the failure message.
//   2. Paste the same value into expectedDeterministicHash here AND in
//      services/volunteer-cli/internal/client/proto_determinism_test.go.
//   3. Re-run both packages' tests; both should pass.

const expectedDeterministicHash = "c05bf60927406e51936da9d1c37c187d0cef9cdc4bc75af28a393c8f7dbc9316"

// fixedRequestWorkUnitResponse builds the canonical fixture for the marshal
// hash test. The exact field values are arbitrary; what matters is that this
// function returns BYTE-FOR-BYTE the same logical message here and in the
// volunteer-cli mirror test.
func fixedRequestWorkUnitResponse() *lettucev1.RequestWorkUnitResponse {
	return &lettucev1.RequestWorkUnitResponse{
		Assignments: []*lettucev1.WorkUnitAssignment{
			{
				WorkUnitId:               "wu-f3-fixture",
				LeafId:                   "leaf-fixture",
				Runtime:                  "container",
				InputData:                []byte{0x00, 0x01, 0x02, 0x03, 0xff},
				InputDataUrl:             "https://example.invalid/in",
				CodeArtifactUrl:          "https://example.invalid/code.tar.gz",
				ParametersJson:           `{"k":"v"}`,
				DeadlineSeconds:          3600,
				EnvVars: map[string]string{
					// Intentionally inserted out of sorted order — deterministic marshal
					// MUST emit them in sorted key order. If protobuf-go ever changes
					// that, the hash diverges.
					"ZETA":  "z",
					"alpha": "a",
					"MIKE":  "m",
				},
				ExecutionSpec: &lettucev1.ExecutionSpec{
					Binaries: map[string]string{
						"linux-amd64":   "https://example.invalid/linux.bin",
						"darwin-arm64":  "https://example.invalid/darwin.bin",
						"windows-amd64": "https://example.invalid/windows.exe",
					},
					Image:         "ghcr.io/example/img:tag",
					GpuRequired:   false,
					GpuType:       "",
					MaxMemoryMb:   2048,
					MaxDiskMb:     4096,
					NetworkAccess: true,
					BinaryChecksums: map[string]string{
						"linux-amd64":   "aaaa",
						"darwin-arm64":  "bbbb",
						"windows-amd64": "cccc",
					},
				},
				HasCheckpoint:             true,
				CheckpointSequence:        7,
				CheckpointIntervalSeconds: 600,
				RscFpopsEst:               1.25e9,
				ReservedUntilUnix:         1893456000,
			},
		},
		RetryAfterSeconds: 42,
	}
}

// TestProtoDeterministicMarshal_StableHash is Guard 1. See the file-level doc.
func TestProtoDeterministicMarshal_StableHash(t *testing.T) {
	msg := fixedRequestWorkUnitResponse()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		t.Fatalf("deterministic marshal: %v", err)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if got != expectedDeterministicHash {
		t.Fatalf(`deterministic-marshal hash drift detected.

  module:   services/infrastructure
  got:      %s
  expected: %s

This usually means google.golang.org/protobuf was bumped in this module
and changed deterministic-marshal output. If the change was intentional,
you MUST update services/volunteer-cli to the same protobuf-go version
AND copy the new hash into BOTH:
  - services/infrastructure/internal/server/proto_determinism_test.go
  - services/volunteer-cli/internal/client/proto_determinism_test.go

Otherwise the gRPC Ed25519 auth (C1) will silently fail for every
volunteer RPC — the client and server hash different bytes.`, got, expectedDeterministicHash)
	}
}

// TestProtoVersionPinAligned is Guard 2. See the file-level doc.
//
// It walks up from this test file to the workspace root and reads both
// services' go.mod files, extracting the google.golang.org/protobuf version
// from each (require block, single-line or multi-line). The two MUST match.
func TestProtoVersionPinAligned(t *testing.T) {
	// Find the workspace root by walking up until we see services/infrastructure.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := cwd
	for {
		if _, err := os.Stat(filepath.Join(root, "services", "infrastructure", "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("could not locate workspace root from %s", cwd)
		}
		root = parent
	}

	infraVer := readProtobufVersion(t, filepath.Join(root, "services", "infrastructure", "go.mod"))
	volVer := readProtobufVersion(t, filepath.Join(root, "services", "volunteer-cli", "go.mod"))

	if infraVer == "" {
		t.Fatalf("could not find google.golang.org/protobuf in services/infrastructure/go.mod")
	}
	if volVer == "" {
		t.Fatalf("could not find google.golang.org/protobuf in services/volunteer-cli/go.mod")
	}
	if infraVer != volVer {
		t.Fatalf(`google.golang.org/protobuf version pin drift between modules:

  services/infrastructure/go.mod: %s
  services/volunteer-cli/go.mod:  %s

Bump both modules together (and rerun TestProtoDeterministicMarshal_StableHash
in BOTH packages — re-baseline the expected hash if intentional). Otherwise
the gRPC Ed25519 auth (C1) is at risk of silent breakage: the client and
server may produce different deterministic-marshal bytes for the same logical
proto message and every volunteer RPC will fail Unauthenticated.`, infraVer, volVer)
	}
	t.Logf("google.golang.org/protobuf aligned at %s across both modules", infraVer)
}

// protobufVersionRE matches the version on a `google.golang.org/protobuf` line
// inside a go.mod file, in either single-line `require` form or as a member of
// a multi-line `require ( ... )` block.
var protobufVersionRE = regexp.MustCompile(`(?m)^\s*(?:require\s+)?google\.golang\.org/protobuf\s+(v\S+)`)

func readProtobufVersion(t *testing.T, goModPath string) string {
	t.Helper()
	data, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read %s: %v", goModPath, err)
	}
	m := protobufVersionRE.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}
