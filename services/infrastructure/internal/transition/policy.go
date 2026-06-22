// Package transition is the single owner of work-unit redundancy decisions (TODO #50).
//
// Before this package, the one latent question — "who counts toward a unit, how many
// copies should be in flight, and what state should the unit be in" — was re-derived
// independently in ~9 places (SubmitResult, the validation engine, the fault monitor, the
// dispatch cache, and five SQL predicates), each hand-copying the same
// `CASE WHEN spot_check THEN 2 ELSE COALESCE(redundancy_factor,2)` arithmetic. That
// dispersion produced two distinct distinctness regressions (#49 being the second). This
// package consolidates the decision into ONE pure function (Decide) over ONE resolved
// policy (RedundancyPolicy), so the dispatch eligibility predicate and the unit-state
// transitioner are two views of the same source of truth — asserted by property tests so
// they cannot drift again.
//
// The numbers are split (the generalization #50 designs in): target_copies (how many to
// dispatch) and min_quorum (how many agreeing results validate), with hard caps bounding a
// non-converging unit. Every field defaults so that target == quorum == redundancy_factor,
// reproducing today's behavior byte-for-byte for any leaf that only sets redundancy_factor.
package transition

import (
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// defaultCopyRetryMargin is how many copies above the target a unit tolerates before
// dead-lettering when no explicit max_total_copies is set. Mirrors
// workunit.defaultCopyRetryMargin (kept in sync; that one is unexported) and the historical
// EffectiveMaxTotalCopies derivation (redundancy + 6).
const defaultCopyRetryMargin = 6

// RedundancyPolicy is the resolved, side-effect-free redundancy contract for a single work
// unit: the explicit target/quorum/caps after layering the per-unit overrides on top of the
// leaf config defaults. It is the SINGLE source of these numbers — Decide (the transitioner)
// and Dispatchable (the dispatch-cache / reserve eligibility) both read it, never re-derive.
type RedundancyPolicy struct {
	// TargetCopies is how many copies to dispatch concurrently. The dispatch predicate keeps
	// a unit dispatchable while live_copies + pending_results < TargetCopies.
	TargetCopies int
	// MinQuorum is how many agreeing results (within AgreementThreshold) validate the unit.
	// Invariant: 1 <= MinQuorum <= TargetCopies.
	MinQuorum int
	// AgreementThreshold is the ratio of the majority group to total results required to
	// accept (0..1, default 1.0). Carried verbatim from the leaf's validation_config.
	AgreementThreshold float64
	// MaxTotalCopies is the dead-letter ceiling: once this many copies (history rows) have
	// ever been created with quorum still unmet and no live copy, the unit parks FAILED.
	MaxTotalCopies int
	// MaxErrorCopies bounds wasted work: once this many copies end EXPIRED/ABANDONED (or
	// produce a DISAGREED result), the unit dead-letters even if MaxTotalCopies is not hit.
	// 0 = unlimited (today's behavior — only MaxTotalCopies bounds errors).
	MaxErrorCopies int
	// MaxSuccessCopies bounds over-dispatch: the dispatcher stops creating copies once this
	// many successful (agreeing) results exist. Defaults to TargetCopies.
	MaxSuccessCopies int
	// SpotCheck mirrors wu.spot_check: a single-copy unit randomly promoted to require a
	// 2-of-2 corroboration. When set, TargetCopies and MinQuorum are both forced to 2.
	SpotCheck bool
}

// ResolvePolicy resolves the redundancy contract for a unit by layering, in order:
//
//	per-unit stamped override (wu.TargetCopies etc., 0 = none)
//	  -> leaf validation_config (TargetCopies/MinQuorum/caps, 0 = none)
//	    -> redundancy_factor (the back-compat alias for target == quorum)
//
// and finally the spot_check promotion (forces 2-of-2). A unit/leaf that sets only
// redundancy_factor resolves to target == quorum == redundancy_factor with the historical
// dead-letter ceiling (redundancy + 6) — identical to pre-#50 behavior.
func ResolvePolicy(lf *leaf.Leaf, wu *workunit.WorkUnit) RedundancyPolicy {
	vc := lf.ValidationConfig

	target := wu.TargetCopies
	if target <= 0 {
		target = vc.EffectiveTargetCopies()
	}

	quorum := wu.MinQuorum
	if quorum <= 0 {
		quorum = vc.EffectiveMinQuorum()
	}

	// Spot-check promotes an otherwise single-copy unit to a 2-of-2 corroboration, matching
	// the historical effectiveRedundancy=2 override (both the completion threshold and the
	// dispatch headroom went to 2).
	if wu.SpotCheck {
		if target < 2 {
			target = 2
		}
		quorum = target
	}

	if target < 1 {
		target = 1
	}
	if quorum < 1 {
		quorum = 1
	}
	if quorum > target {
		quorum = target
	}

	p := RedundancyPolicy{
		TargetCopies:       target,
		MinQuorum:          quorum,
		AgreementThreshold: vc.AgreementThreshold,
		SpotCheck:          wu.SpotCheck,
	}
	if p.AgreementThreshold <= 0 {
		// Match ApplyValidationConfigDefaults: an unset threshold means unanimity (1.0).
		p.AgreementThreshold = 1.0
	}

	// Dead-letter ceiling: per-unit override, else leaf config, else target + retry margin
	// (the historical redundancy + 6).
	p.MaxTotalCopies = wu.MaxTotalCopies
	if p.MaxTotalCopies <= 0 {
		p.MaxTotalCopies = vc.MaxTotalCopies
	}
	if p.MaxTotalCopies <= 0 {
		p.MaxTotalCopies = target + defaultCopyRetryMargin
	}

	// Error ceiling: per-unit override, else leaf config, else 0 (unlimited — today).
	p.MaxErrorCopies = wu.MaxErrorCopies
	if p.MaxErrorCopies <= 0 {
		p.MaxErrorCopies = vc.MaxErrorCopies
	}

	// Success / over-dispatch ceiling: per-unit override, else leaf config, else target.
	p.MaxSuccessCopies = wu.MaxSuccessCopies
	if p.MaxSuccessCopies <= 0 {
		p.MaxSuccessCopies = vc.MaxSuccessCopies
	}
	if p.MaxSuccessCopies <= 0 {
		p.MaxSuccessCopies = target
	}

	return p
}
