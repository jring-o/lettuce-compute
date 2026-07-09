package client

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/pow"
)

// powSeqBytes returns n bytes whose values are start, start+1, … — the head's golden
// vector definition (pow/pow_test.go), reproduced here so the CLI asserts the exact
// same inputs.
func powSeqBytes(start, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(start + i)
	}
	return b
}

// TestPowGoldenVector_ImportedRule pins that THIS CLI build carries the identical
// cross-module proof-of-work rule the head verifies against. It imports
// github.com/lettuce-compute/infrastructure/pow — the head's exported package, resolved
// through the go.mod replace — and asserts the canonical golden vector: challenge =
// bytes 0x00..0x1f, publicKey = bytes 0x20..0x3f, and Solve at difficulty 16 returns
// nonce 19497 (which clears exactly 17 leading zero bits). If the head ever changed the
// rule, this build would stop producing solutions the head accepts; this import-level
// assertion fails first, so the two builds cannot silently diverge on the PoW contract.
func TestPowGoldenVector_ImportedRule(t *testing.T) {
	challenge := powSeqBytes(0, 32)  // 0x00..0x1f
	publicKey := powSeqBytes(32, 32) // 0x20..0x3f
	const wantNonce = uint64(19497)

	if got := pow.Solve(challenge, publicKey, 16); got != wantNonce {
		t.Fatalf("pow.Solve(diff=16) = %d, want %d (CLI pow rule diverged from the head)", got, wantNonce)
	}
	if !pow.VerifySolution(challenge, publicKey, wantNonce, 16) {
		t.Error("pow.VerifySolution(diff=16) = false, want true for the golden nonce")
	}
	// The golden nonce clears exactly 17 bits: accepted at 17, rejected at 18.
	if !pow.VerifySolution(challenge, publicKey, wantNonce, 17) {
		t.Error("pow.VerifySolution(diff=17) = false, want true")
	}
	if pow.VerifySolution(challenge, publicKey, wantNonce, 18) {
		t.Error("pow.VerifySolution(diff=18) = true, want false")
	}
}
