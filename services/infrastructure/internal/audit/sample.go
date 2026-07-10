package audit

import (
	"crypto/rand"
	"encoding/binary"
)

// ShouldSample returns true with probability rate (a fraction in [0, 1]). It is the
// fraction-domain sibling of workunit.ShouldSpotCheck (deliberately a new function: the
// legacy spot-check mechanism is percentage-domain and belongs to a different,
// pre-validation subsystem — D5). crypto/rand so no volunteer can predict which
// validated units are audited; selection is post-hoc and uniform (§4.4), drawn AFTER
// validation so dispatch behavior carries zero signal.
func ShouldSample(rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// If crypto/rand fails, default to sampling (fail safe — the same direction
		// as ShouldSpotCheck: a broken RNG must not quietly disable the audit net).
		return true
	}
	// Uniform float in [0, 1) from the top 53 bits.
	v := float64(binary.LittleEndian.Uint64(buf[:])>>11) / (1 << 53)
	return v < rate
}
