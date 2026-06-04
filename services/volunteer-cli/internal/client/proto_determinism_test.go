package client

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/protobuf/proto"
)

// --- F3 hardening: protobuf-go drift guard (volunteer-cli mirror) -------------
//
// MIRROR of services/infrastructure/internal/server/proto_determinism_test.go.
//
// The C1 gRPC auth signs and verifies over deterministic-marshal bytes of the
// request. Volunteer-cli (signer) and infrastructure (verifier) are SEPARATE
// Go modules with their own google.golang.org/protobuf dependency. If the two
// modules ever drift to versions of protobuf-go whose deterministic-marshal
// output differs, every authenticated volunteer RPC silently fails with
// Unauthenticated — the client signs over bytes X, the server hashes bytes Y.
//
// This test builds the SAME logical *lettucev1.RequestWorkUnitResponse as the
// mirror test on the infrastructure side and asserts the SHA-256 of the
// deterministic-marshal output matches the SAME hardcoded hash. When both
// tests pass, the two modules' protobuf-go runtimes are byte-compatible.
//
// If protobuf-go is bumped in only one module and changes marshal output,
// that module's test fails with a clear hash mismatch — operator notices
// immediately rather than discovering it via failed signatures in the field.
//
// The expected hash and the fixture-builder body MUST stay in sync with:
//   services/infrastructure/internal/server/proto_determinism_test.go
// (Re-baseline both files together whenever protobuf-go is intentionally
// upgraded in both modules.)

const expectedDeterministicHash = "c05bf60927406e51936da9d1c37c187d0cef9cdc4bc75af28a393c8f7dbc9316"

// fixedRequestWorkUnitResponse is the canonical fixture mirrored from the
// infrastructure test. Field values are deliberately identical — including
// the out-of-order map insertions, which deterministic marshal MUST sort.
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

// TestProtoDeterministicMarshal_StableHash is the volunteer-cli mirror of the
// Guard-1 test on the infrastructure side. See the file-level doc.
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

  module:   services/volunteer-cli
  got:      %s
  expected: %s

This usually means google.golang.org/protobuf was bumped in this module
and changed deterministic-marshal output. If the change was intentional,
you MUST update services/infrastructure to the same protobuf-go version
AND copy the new hash into BOTH:
  - services/volunteer-cli/internal/client/proto_determinism_test.go
  - services/infrastructure/internal/server/proto_determinism_test.go

Otherwise the gRPC Ed25519 auth (C1) will silently fail for every
volunteer RPC — the client and server hash different bytes.`, got, expectedDeterministicHash)
	}
}
