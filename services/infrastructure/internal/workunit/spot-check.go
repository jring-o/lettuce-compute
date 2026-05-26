package workunit

import (
	"crypto/rand"
	"encoding/binary"
)

// ShouldSpotCheck returns true with approximately `percentage`% probability.
// Uses crypto/rand for unpredictable selection — no volunteer can predict
// which work units will be spot-checked.
func ShouldSpotCheck(percentage float64) bool {
	if percentage <= 0 {
		return false
	}
	if percentage >= 100 {
		return true
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// If crypto/rand fails, default to spot-checking (fail safe).
		return true
	}
	// Generate uniform float in [0, 100).
	v := float64(binary.LittleEndian.Uint64(buf[:])>>11) / (1 << 53) * 100.0
	return v < percentage
}
