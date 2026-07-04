package transition

import (
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// SubjectForResult returns the account-level trust subject a result is attributed to: the
// subject stamped at submit time when present, else the per-keypair sentinel derived from
// the volunteer id. The fallback is what makes a legacy result (created before the trust
// feature, so TrustSubject is nil) behave as its own distinct per-volunteer subject with
// score 0 — exactly as if every account were unbound and untrusted, which is the
// behavior-preserving default. Exported so the validation engine's accrual path applies the
// SAME subject fallback the verdict builder does (one rule, one place; internal/trust is a
// frozen dependency here, so the fallback lives with its consumers).
func SubjectForResult(r *result.Result) string {
	if r.TrustSubject != nil && *r.TrustSubject != "" {
		return *r.TrustSubject
	}
	return trust.SubjectForVolunteerID(r.VolunteerID)
}

// ScoreForResult returns the submission-time snapshot score for a result (nil -> 0). The
// snapshot, not a live re-read, is authoritative: acceptance must use the score the subject
// had WHEN IT SUBMITTED, so a later slash or accrual cannot retroactively change a decision.
func ScoreForResult(r *result.Result) int {
	if r.TrustScoreAtSubmit != nil {
		return *r.TrustScoreAtSubmit
	}
	return 0
}

// BuildComparisonVerdict computes the subject-level comparison verdict from the compared
// pending set and the comparator's majority (result-level) group. It is a pure function:
// the same inputs always yield the same verdict, with no I/O.
//
// It collapses raw results to DISTINCT SUBJECTS (see ComparisonVerdict) and applies the
// coherence rule — a subject corroborates the majority only when EVERY one of its pending
// results is in the majority group, so a principal that contradicts itself across its
// devices counts toward the total but never toward the agreeing (or trusted) group. This is
// the account-level Sybil accounting the trust gate rests on.
//
// trustFloor is the resolved score at or above which an agreeing subject counts as trusted;
// TrustedMajorityCount is the number of coherent agreeing subjects whose max snapshot score
// meets it. A nil or empty majority yields MajorityCount == 0 and TrustedMajorityCount == 0
// (the tie-decides-nothing rule): with no majority group, every subject has a result outside
// it and none can be coherent-agreeing.
func BuildComparisonVerdict(pending, majority []*result.Result, trustFloor int) *ComparisonVerdict {
	majorityIDs := make(map[types.ID]bool, len(majority))
	for _, r := range majority {
		majorityIDs[r.ID] = true
	}

	// Per-subject accumulation across the pending set: whether the subject has any result
	// OUTSIDE the majority group (which makes it incoherent / disagreeing), and the max
	// snapshot score over its results (they should be equal per subject, but max is
	// defensive against a stale straggler).
	type subjectAgg struct {
		outOfMajority bool
		maxScore      int
	}
	subjects := make(map[string]*subjectAgg)
	for _, r := range pending {
		subj := SubjectForResult(r)
		agg := subjects[subj]
		if agg == nil {
			agg = &subjectAgg{}
			subjects[subj] = agg
		}
		if !majorityIDs[r.ID] {
			agg.outOfMajority = true
		}
		if sc := ScoreForResult(r); sc > agg.maxScore {
			agg.maxScore = sc
		}
	}

	total := len(subjects)
	majorityCount := 0
	trustedMajorityCount := 0
	for _, agg := range subjects {
		if agg.outOfMajority {
			continue // incoherent or disagreeing testimony never corroborates
		}
		majorityCount++
		if agg.maxScore >= trustFloor {
			trustedMajorityCount++
		}
	}

	ratio := 0.0
	if total > 0 {
		ratio = float64(majorityCount) / float64(total)
	}

	return &ComparisonVerdict{
		MajorityCount:        majorityCount,
		Total:                total,
		Ratio:                ratio,
		TrustedMajorityCount: trustedMajorityCount,
	}
}
