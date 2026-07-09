// Package pow holds THE registration proof-of-work solution rule — the single
// cross-module implementation shared by every Go party to the contract:
//
//   - the head's server-side verification (internal/admission redeems challenges by
//     delegating here),
//   - the volunteer CLI's solver (a SEPARATE Go module, which is why this package is
//     exported at the module root rather than living under internal/ — Go's internal
//     rule would make it unimportable there),
//   - and, by golden byte vector rather than by import, the dashboard's TypeScript
//     solver.
//
// The rule (pinned by the golden vector in pow_test.go; changing it orphans every
// shipped solver):
//
//	digest = SHA-256(challenge || publicKey || nonce as 8 big-endian bytes)
//	valid  = LeadingZeroBits(digest) >= difficultyBits
//
// This package is deliberately dependency-free (stdlib only) and knows nothing about
// challenges' issuance, storage, TTLs, or refusal messages — those are head-side
// admission concerns (internal/admission).
package pow

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
)

// VerifySolution reports whether nonce solves the challenge for publicKey at the given
// difficulty. See the package comment for the exact rule; the publicKey is bound into
// the preimage, so a solution is single-purpose per key.
func VerifySolution(challenge, publicKey []byte, nonce uint64, difficultyBits int) bool {
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], nonce)
	h := sha256.New()
	h.Write(challenge)
	h.Write(publicKey)
	h.Write(nb[:])
	return LeadingZeroBits(h.Sum(nil)) >= difficultyBits
}

// LeadingZeroBits counts the leading zero bits of digest.
func LeadingZeroBits(digest []byte) int {
	n := 0
	for _, b := range digest {
		if b == 0 {
			n += 8
			continue
		}
		n += bits.LeadingZeros8(b)
		break
	}
	return n
}

// Solve brute-forces a nonce for the challenge (the reference solver the volunteer CLI
// ships; tests use it with a low difficulty). It scans nonces from 0 and returns the
// first solution, so it never terminates only if no solution exists in the uint64 space
// — astronomically unlikely for any sane difficulty.
func Solve(challenge, publicKey []byte, difficultyBits int) uint64 {
	for nonce := uint64(0); ; nonce++ {
		if VerifySolution(challenge, publicKey, nonce, difficultyBits) {
			return nonce
		}
	}
}
