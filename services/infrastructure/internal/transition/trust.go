package transition

import "github.com/lettuce-compute/infrastructure/internal/leaf"

// TrustPolicy is the head-level trust-gate configuration the Transitioner and the
// validation engine overlay onto each leaf's per-leaf overrides. It is the account-level
// Sybil defense for validation (see internal/trust): a unit validates only when its
// agreeing group contains enough DISTINCT, TRUSTED subjects, not merely enough copies.
//
// The zero value is the gate OFF: GateEnabled false makes ResolveTrust return K == 0 (the
// gate never blocks) while STILL resolving the real floor, so trust can accumulate before
// enforcement is ever switched on. That zero value is the deploy-safety default — every
// existing construction site that passes TrustPolicy{} keeps today's behavior exactly.
type TrustPolicy struct {
	// GateEnabled is the head master switch. When false the resolved K is 0 (the gate never
	// blocks a unit), but the floor is still resolved so accrual can use it.
	GateEnabled bool
	// DefaultMinCorroborators is the head-default K: the number of distinct trusted agreeing
	// subjects a unit needs to validate when a leaf does not override it.
	DefaultMinCorroborators int
	// DefaultFloor is the head-default trust floor: the snapshot score at or above which an
	// agreeing subject counts as trusted, when a leaf does not override it.
	DefaultFloor int
}

// ResolveTrust returns the effective (K, floor) for a leaf.
//
// K = 0 when the gate is disabled (the gate then never blocks); else the leaf override
// (vc.MinTrustedCorroborators) when > 0, else the head default — always clamped to
// minQuorum, because a quorum-sized agreeing group cannot contain more distinct subjects
// than its size, so a K above min_quorum could never be satisfied.
//
// floor = the leaf override (vc.TrustFloor) when > 0, else the head default. The floor is
// resolved REGARDLESS of GateEnabled: accrual credits trust before the gate is ever turned
// on (a subject must earn a score before enforcement can recognize it), and it needs the
// real floor to decide which agreeing subjects are trusted enough to corroborate others.
func (tp TrustPolicy) ResolveTrust(vc leaf.ValidationConfig, minQuorum int) (k, floor int) {
	floor = vc.TrustFloor
	if floor <= 0 {
		floor = tp.DefaultFloor
	}

	if !tp.GateEnabled {
		return 0, floor
	}

	k = vc.MinTrustedCorroborators
	if k <= 0 {
		k = tp.DefaultMinCorroborators
	}
	// A quorum-sized agreeing group holds at most min_quorum distinct subjects, so a K above
	// it is unsatisfiable; clamp so the gate can always be met by a fully-trusted quorum.
	if minQuorum >= 1 && k > minQuorum {
		k = minQuorum
	}
	return k, floor
}
