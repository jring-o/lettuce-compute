package validation

import (
	"encoding/json"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// comparatorEngine builds a minimal Engine that only exercises the read-only comparators
// (compareExact / compareNumericTolerance) — they touch no repositories, only the logger and the
// leaf config.
func comparatorEngine() *Engine {
	return NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})
}

func idSet(rs []*result.Result) map[types.ID]bool {
	m := make(map[types.ID]bool, len(rs))
	for _, r := range rs {
		m[r.ID] = true
	}
	return m
}

// TestCompareExact_DegradesUnreadableRow covers §4.3 for EXACT: an unreadable row (here, malformed
// output under a leaf that canonicalizes via ignore_fields, so comparisonKey errors) is excluded
// from grouping. A bad row among an honest quorum leaves the honest group as the majority; an
// all-bad set yields no majority (nil) rather than aborting.
func TestCompareExact_DegradesUnreadableRow(t *testing.T) {
	e := comparatorEngine()
	proj := makeLeaf(types.NewID(), 3, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"t"} // forces comparisonKey to parse+canonicalize

	wuID := types.NewID()
	honestA := makeResult(wuID, types.NewID(), "ck-a", json.RawMessage(`{"v":1,"t":"x"}`))
	honestB := makeResult(wuID, types.NewID(), "ck-b", json.RawMessage(`{"v":1,"t":"y"}`)) // same canon as A
	bad := makeResult(wuID, types.NewID(), "ck-bad", json.RawMessage(`not json`))          // canon parse fails

	t.Run("bad_among_honest_quorum", func(t *testing.T) {
		majority, err := e.compareExact(proj, []*result.Result{honestA, honestB, bad})
		if err != nil {
			t.Fatalf("compareExact must not error on an unreadable row: %v", err)
		}
		got := idSet(majority)
		if len(majority) != 2 || !got[honestA.ID] || !got[honestB.ID] {
			t.Fatalf("majority = %d results, want exactly the honest pair", len(majority))
		}
		if got[bad.ID] {
			t.Error("excluded (unreadable) row must never appear in the majority group")
		}
	})

	t.Run("all_bad_no_majority", func(t *testing.T) {
		bad2 := makeResult(wuID, types.NewID(), "ck-bad2", json.RawMessage(`also not json`))
		majority, err := e.compareExact(proj, []*result.Result{bad, bad2})
		if err != nil {
			t.Fatalf("compareExact must not error on all-bad input: %v", err)
		}
		if len(majority) != 0 {
			t.Fatalf("majority = %d, want 0 (everything excluded -> no group)", len(majority))
		}
	})
}

// TestCompareNumeric_DegradesUnreadableRow covers §4.3 for NUMERIC_TOLERANCE: a row whose output
// cannot be flattened (empty / non-finite) is excluded from clique candidacy. A bad row among an
// honest quorum leaves the honest group as the majority; an all-bad set yields an empty group.
func TestCompareNumeric_DegradesUnreadableRow(t *testing.T) {
	e := comparatorEngine()
	eps := 0.01
	proj := makeLeaf(types.NewID(), 3, 1.0, "NUMERIC_TOLERANCE", &eps, 1.0)

	wuID := types.NewID()
	honestA := makeResult(wuID, types.NewID(), "n-a", json.RawMessage(`{"x":1.000}`))
	honestB := makeResult(wuID, types.NewID(), "n-b", json.RawMessage(`{"x":1.005}`))  // within epsilon of A
	badEmpty := makeResult(wuID, types.NewID(), "n-empty", nil)                        // flatten: empty output
	badInf := makeResult(wuID, types.NewID(), "n-inf", json.RawMessage(`{"x":1e400}`)) // flatten: non-finite

	t.Run("bad_among_honest_quorum", func(t *testing.T) {
		majority, err := e.compareNumericTolerance(proj, []*result.Result{honestA, badEmpty, honestB, badInf})
		if err != nil {
			t.Fatalf("compareNumericTolerance must not error on unreadable rows: %v", err)
		}
		got := idSet(majority)
		if len(majority) != 2 || !got[honestA.ID] || !got[honestB.ID] {
			t.Fatalf("majority = %d results, want exactly the honest pair", len(majority))
		}
		if got[badEmpty.ID] || got[badInf.ID] {
			t.Error("excluded (unreadable) rows must never appear in the majority group")
		}
	})

	t.Run("all_bad_no_majority", func(t *testing.T) {
		majority, err := e.compareNumericTolerance(proj, []*result.Result{badEmpty, badInf})
		if err != nil {
			t.Fatalf("compareNumericTolerance must not error on all-bad input: %v", err)
		}
		if len(majority) != 0 {
			t.Fatalf("majority = %d, want 0 (everything excluded -> no group)", len(majority))
		}
	})

	t.Run("lone_bad_is_not_a_singleton_majority", func(t *testing.T) {
		// The critical guard: a single unreadable row must NOT become a validating singleton clique
		// (a quorum-1 leaf would otherwise let garbage validate).
		majority, err := e.compareNumericTolerance(proj, []*result.Result{badInf})
		if err != nil {
			t.Fatalf("compareNumericTolerance must not error: %v", err)
		}
		if len(majority) != 0 {
			t.Fatalf("majority = %d, want 0 (a lone unreadable row is never a majority)", len(majority))
		}
	})
}

// TestVerdict_ExcludedRowCountsInTotalNotMajority ties the degraded comparator to the verdict:
// an excluded row remains in `pending`, so BuildComparisonVerdict counts it in Total but never in
// MajorityCount, and the strict-majority gate blocks a lone bad row from validating even at
// quorum 1.
func TestVerdict_ExcludedRowCountsInTotalNotMajority(t *testing.T) {
	e := comparatorEngine()
	proj := makeLeaf(types.NewID(), 3, 0.66, "EXACT", nil, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"t"}

	wuID := types.NewID()

	t.Run("bad_among_honest_quorum", func(t *testing.T) {
		honestA := makeResult(wuID, types.NewID(), "v-a", json.RawMessage(`{"v":1,"t":"x"}`))
		honestB := makeResult(wuID, types.NewID(), "v-b", json.RawMessage(`{"v":1,"t":"y"}`))
		bad := makeResult(wuID, types.NewID(), "v-bad", json.RawMessage(`not json`))
		pending := []*result.Result{honestA, honestB, bad}

		majority, err := e.compareExact(proj, pending)
		if err != nil {
			t.Fatalf("compareExact: %v", err)
		}
		v := transition.BuildComparisonVerdict(pending, majority, 0)
		if v.Total != 3 {
			t.Errorf("Total = %d, want 3 (the excluded row still counts)", v.Total)
		}
		if v.MajorityCount != 2 {
			t.Errorf("MajorityCount = %d, want 2 (only the honest pair)", v.MajorityCount)
		}
		if !(2*v.MajorityCount > v.Total) {
			t.Errorf("strict-majority gate 2*%d > %d must hold for the honest pair", v.MajorityCount, v.Total)
		}
	})

	t.Run("lone_bad_blocked_even_at_quorum_1", func(t *testing.T) {
		bad := makeResult(wuID, types.NewID(), "v-lonebad", json.RawMessage(`not json`))
		pending := []*result.Result{bad}

		majority, err := e.compareExact(proj, pending)
		if err != nil {
			t.Fatalf("compareExact: %v", err)
		}
		v := transition.BuildComparisonVerdict(pending, majority, 0)
		if v.Total != 1 {
			t.Errorf("Total = %d, want 1 (the bad row counts toward Total)", v.Total)
		}
		if v.MajorityCount != 0 {
			t.Errorf("MajorityCount = %d, want 0 (the bad row can never be a majority)", v.MajorityCount)
		}
		// Both the strict-majority gate and the quorum floor block it, even with quorum == 1.
		if 2*v.MajorityCount > v.Total {
			t.Error("strict-majority gate must NOT pass for a lone bad row")
		}
		if v.MajorityCount >= 1 {
			t.Error("quorum-1 floor must NOT be met by a lone bad row")
		}
	})
}
