package pow

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// seqBytes returns n bytes whose values are start, start+1, … (mod 256). The golden
// vector below is built from three non-overlapping runs so a cross-language solver can
// reproduce the exact inputs from a one-line description.
func seqBytes(start, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(start + i)
	}
	return b
}

// manualDigest recomputes the solution digest independently of this package's
// production code, spelling out the byte layout documented on VerifySolution:
//
//	SHA-256(challenge || publicKey || nonce as 8 big-endian bytes)
//
// The golden-vector test asserts this in-test recomputation equals the hardcoded hex
// constants, which is what actually pins the wire layout: if VerifySolution ever changed
// the preimage ordering or the nonce encoding, this helper and the constants would still
// agree but the real Solve / VerifySolution assertions would break.
func manualDigest(challenge, publicKey []byte, nonce uint64) []byte {
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], nonce)
	h := sha256.New()
	h.Write(challenge)
	h.Write(publicKey)
	h.Write(nb[:])
	return h.Sum(nil)
}

// TestLeadingZeroBits pins the difficulty metric: the number of most-significant zero
// bits before the first set bit, counted across whole zero bytes and then within the
// first non-zero byte. This is the exact quantity difficulty targets are compared
// against, so a cross-language solver must count bits identically.
func TestLeadingZeroBits(t *testing.T) {
	cases := []struct {
		name   string
		digest []byte
		want   int
	}{
		{"all-zero 32 bytes is fully saturated", make([]byte, 32), 256},
		{"high bit set in first byte is zero", append([]byte{0x80}, make([]byte, 31)...), 0},
		{"0x01 leaves seven leading zeros", append([]byte{0x01}, make([]byte, 31)...), 7},
		{"zero byte then 0xFF stops at eight", []byte{0x00, 0xFF}, 8},
		{"zero byte then 0x0F adds four", []byte{0x00, 0x0F}, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LeadingZeroBits(tc.digest); got != tc.want {
				t.Errorf("LeadingZeroBits(%x) = %d, want %d", tc.digest, got, tc.want)
			}
		})
	}
}

// TestVerifySolution_GoldenVector is THE cross-implementation pin for the proof-of-work
// solution rule. The dashboard's TypeScript solver (and any future client) is validated
// against these exact bytes, so the values are hardcoded real outputs, not derived
// in-test. If any assertion here changes, every independent solver implementation must be
// updated in lockstep or it will stop producing solutions the head accepts.
//
// Vector definition (reproducible from this description alone):
//
//	challenge  = 32 bytes 0x00,0x01,…,0x1f   (byte i = i)
//	publicKey  = 32 bytes 0x20,0x21,…,0x3f   (byte i = 32+i)
//	digest(n)  = SHA-256(challenge || publicKey || n as 8 big-endian bytes)
func TestVerifySolution_GoldenVector(t *testing.T) {
	const (
		// digest for nonce = 1. Pins the preimage byte layout (order + big-endian
		// nonce encoding). Its own leading-zero count is only 1, so it is not a
		// solution at any real difficulty — it exists purely to fix the layout.
		nonce1DigestHex = "7f812700537ee9f8def5ab067d299b26f39ddf28875594ae0f95b6b1e40ce4c0"

		// The first nonce (scanning from 0) whose digest clears difficulty 16. It
		// happens to clear 17 bits, so difficulty 17 also accepts it and difficulty
		// 18 rejects it — pinned exactly below.
		diff16Nonce     = uint64(19497)
		diff16DigestHex = "000040eb278da4930113209945a1b4d48614ba74bec414cc4b4418e1491e525b"
		diff16LZB       = 17
	)

	challenge := seqBytes(0, 32)  // 0x00..0x1f
	publicKey := seqBytes(32, 32) // 0x20..0x3f

	// (1) Byte-layout pin: an in-test SHA-256 over the documented preimage must equal
	// the hardcoded nonce=1 digest.
	if got := hex.EncodeToString(manualDigest(challenge, publicKey, 1)); got != nonce1DigestHex {
		t.Fatalf("nonce=1 digest = %s, want %s (preimage byte layout changed)", got, nonce1DigestHex)
	}

	// (2) The reference solver must return the pinned difficulty-16 nonce as the FIRST
	// solution. This is the strongest cross-language assertion: any solver scanning
	// nonces from 0 must arrive at exactly this value.
	if got := Solve(challenge, publicKey, 16); got != diff16Nonce {
		t.Fatalf("Solve(diff=16) = %d, want %d", got, diff16Nonce)
	}

	// (3) The difficulty-16 solution's digest is fixed, and so is its exact leading-zero
	// count.
	d16 := manualDigest(challenge, publicKey, diff16Nonce)
	if got := hex.EncodeToString(d16); got != diff16DigestHex {
		t.Fatalf("diff-16 digest = %s, want %s", got, diff16DigestHex)
	}
	if got := LeadingZeroBits(d16); got != diff16LZB {
		t.Fatalf("LeadingZeroBits(diff-16 digest) = %d, want %d", got, diff16LZB)
	}

	// (4) Verification is a >= comparison against the difficulty target: the nonce is
	// accepted at every difficulty up to its actual leading-zero count and rejected
	// above it.
	if !VerifySolution(challenge, publicKey, diff16Nonce, 16) {
		t.Errorf("VerifySolution(diff=16) = false, want true")
	}
	if !VerifySolution(challenge, publicKey, diff16Nonce, diff16LZB) {
		t.Errorf("VerifySolution(diff=%d) = false, want true", diff16LZB)
	}
	if VerifySolution(challenge, publicKey, diff16Nonce, diff16LZB+1) {
		t.Errorf("VerifySolution(diff=%d) = true, want false", diff16LZB+1)
	}
}

// TestSolve_RoundTrip checks the reference solver against verification on an input
// distinct from the golden vector: Solve must return a nonce that VerifySolution accepts
// at the same difficulty. Difficulty 8 keeps the brute force fast (~256 attempts).
func TestSolve_RoundTrip(t *testing.T) {
	const difficulty = 8

	// A fixed but non-sequential input, to show the round trip holds generally and not
	// only for the golden byte runs.
	challenge := make([]byte, 32)
	publicKey := make([]byte, 32)
	for i := range challenge {
		challenge[i] = byte(i*7 + 1)
		publicKey[i] = byte(i*13 + 5)
	}

	nonce := Solve(challenge, publicKey, difficulty)
	if !VerifySolution(challenge, publicKey, nonce, difficulty) {
		t.Fatalf("VerifySolution(Solve() = %d) = false, want true at difficulty %d", nonce, difficulty)
	}
	// The returned nonce must genuinely clear the target, not merely be accepted by a
	// buggy comparison.
	if got := LeadingZeroBits(manualDigest(challenge, publicKey, nonce)); got < difficulty {
		t.Errorf("solution digest has %d leading zero bits, want >= %d", got, difficulty)
	}
}

// TestVerifySolution_PubkeyBinding pins that the public key is part of the solution
// preimage: a nonce solved for key A is not a solution for a different key B. This is why
// a redeemed solution is single-purpose (see internal/admission's RedeemChallenge). The
// values are hardcoded from the reference solver so the negative case is deterministic —
// B's digest here has zero leading zero bits, well under the difficulty, so there is no
// probabilistic flake.
func TestVerifySolution_PubkeyBinding(t *testing.T) {
	const (
		difficulty = 12
		// First nonce (from 0) solving difficulty 12 for key A.
		bindNonce = uint64(2286)
		// A's digest at bindNonce clears exactly 12 bits; B's clears 0.
		aDigestHex = "000fb9757b2cac4a44c169521be41f7b7a7b56931dadb9d8bceab38e4a6b7e5c"
		bDigestHex = "9cc1515db812f8746ff8f4dc60d4f0240d47598b5473dc654373b2f2e51881cd"
	)

	challenge := seqBytes(0, 32) // 0x00..0x1f
	keyA := seqBytes(32, 32)     // 0x20..0x3f
	keyB := seqBytes(64, 32)     // 0x40..0x5f

	// The reference solver finds the pinned nonce for A.
	if got := Solve(challenge, keyA, difficulty); got != bindNonce {
		t.Fatalf("Solve(keyA, diff=%d) = %d, want %d", difficulty, got, bindNonce)
	}

	// Same challenge and nonce, different key ⇒ different digest. Pin both.
	dA := manualDigest(challenge, keyA, bindNonce)
	dB := manualDigest(challenge, keyB, bindNonce)
	if got := hex.EncodeToString(dA); got != aDigestHex {
		t.Fatalf("keyA digest = %s, want %s", got, aDigestHex)
	}
	if got := hex.EncodeToString(dB); got != bDigestHex {
		t.Fatalf("keyB digest = %s, want %s", got, bDigestHex)
	}
	if hex.EncodeToString(dA) == hex.EncodeToString(dB) {
		t.Fatalf("keyA and keyB produced the same digest; public key is not bound into the preimage")
	}
	if got := LeadingZeroBits(dB); got != 0 {
		t.Fatalf("keyB digest has %d leading zero bits, want 0 (deterministic-failure precondition)", got)
	}

	// The solution is valid for A and, deterministically, invalid for B.
	if !VerifySolution(challenge, keyA, bindNonce, difficulty) {
		t.Errorf("VerifySolution(keyA) = false, want true")
	}
	if VerifySolution(challenge, keyB, bindNonce, difficulty) {
		t.Errorf("VerifySolution(keyB) = true, want false (nonce must not carry across keys)")
	}
}
