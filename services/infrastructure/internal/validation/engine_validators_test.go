package validation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// runTwoResultsCfg wires a 2-result work unit for an arbitrary comparison config and
// runs validation. `configure` can set IgnoreFields/CompareFields on the leaf's
// ValidationConfig before validation runs.
func runTwoResultsCfg(t *testing.T, mode string, tol *float64, configure func(*leaf.ValidationConfig),
	dataA json.RawMessage, ckA string, dataB json.RawMessage, ckB string) (*ValidationResult, error) {
	t.Helper()
	leafID := types.NewID()
	wuID := types.NewID()
	vol1 := types.NewID()
	vol2 := types.NewID()

	proj := makeLeaf(leafID, 2, 1.0, mode, tol, 1.0)
	if configure != nil {
		configure(&proj.ValidationConfig)
	}
	wu := makeWorkUnit(wuID, leafID, workunit.WorkUnitStateCompleted)
	r1 := makeResult(wuID, vol1, ckA, dataA)
	r2 := makeResult(wuID, vol2, ckB, dataB)

	resultRepo := newMockResultRepo()
	resultRepo.addResult(r1)
	resultRepo.addResult(r2)
	wuRepo := newMockWorkUnitRepo()
	wuRepo.addWorkUnit(wu)
	leafRepo := newMockLeafRepo()
	leafRepo.addLeaf(proj)
	volRepo := newMockVolunteerRepo()
	volRepo.addVolunteer(makeVolunteer(vol1))
	volRepo.addVolunteer(makeVolunteer(vol2))

	engine := NewEngine(resultRepo, wuRepo, leafRepo, newMockCreditRepo(), nil, volRepo, newMockAssignmentRepo(), nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})
	return engine.TryValidate(context.Background(), wuID)
}

// TestExactCanonical_IgnoresVolatileField is the core fix: two EXACT results that differ
// ONLY in a wall-clock provenance field (compute_time_ms) — and therefore have different
// raw checksums — still reach agreement once that field is declared in ignore_fields,
// because the comparison key is recomputed canonically from the stored output.
func TestExactCanonical_IgnoresVolatileField(t *testing.T) {
	a := json.RawMessage(`{"blade_a":"m0","a_win_rate":0.7,"compute_time_ms":209}`)
	b := json.RawMessage(`{"blade_a":"m0","a_win_rate":0.7,"compute_time_ms":9684}`)
	vr, err := runTwoResultsCfg(t, "EXACT", nil,
		func(c *leaf.ValidationConfig) { c.IgnoreFields = []string{"compute_time_ms"} },
		a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED (results differ only in ignored compute_time_ms)", vr.Outcome)
	}
	if len(vr.AgreedResults) != 2 {
		t.Errorf("AgreedResults = %d, want 2", len(vr.AgreedResults))
	}
}

// TestExactCanonical_KeyOrderIndependent verifies canonicalization normalizes object key
// order, so two semantically-identical outputs with different key order (and different raw
// checksums) agree once any ignore_fields is set (which switches on the canonical path).
func TestExactCanonical_KeyOrderIndependent(t *testing.T) {
	a := json.RawMessage(`{"a_win_rate":0.7,"knockout_rate":0.6,"compute_time_ms":1}`)
	b := json.RawMessage(`{"knockout_rate":0.6,"compute_time_ms":2,"a_win_rate":0.7}`)
	vr, err := runTwoResultsCfg(t, "EXACT", nil,
		func(c *leaf.ValidationConfig) { c.IgnoreFields = []string{"compute_time_ms"} },
		a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED (key order + ignored field only)", vr.Outcome)
	}
}

// TestExactCanonical_RealDifferenceRejected confirms canonicalization does NOT mask a
// genuine science difference: stripping compute_time_ms still leaves a_win_rate differing,
// so the two results disagree and the unit is rejected.
func TestExactCanonical_RealDifferenceRejected(t *testing.T) {
	a := json.RawMessage(`{"a_win_rate":0.70,"compute_time_ms":209}`)
	b := json.RawMessage(`{"a_win_rate":0.60,"compute_time_ms":9684}`)
	vr, err := runTwoResultsCfg(t, "EXACT", nil,
		func(c *leaf.ValidationConfig) { c.IgnoreFields = []string{"compute_time_ms"} },
		a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED (a_win_rate genuinely differs)", vr.Outcome)
	}
}

// TestExactCanonical_EmptyOutputFallsBackToRawChecksum verifies that when there is no
// inline output to canonicalize (e.g. EXTERNAL_REFERENCE), EXACT falls back to the raw
// submitted checksum even though ignore_fields is set.
func TestExactCanonical_EmptyOutputFallsBackToRawChecksum(t *testing.T) {
	vr, err := runTwoResultsCfg(t, "EXACT", nil,
		func(c *leaf.ValidationConfig) { c.IgnoreFields = []string{"compute_time_ms"} },
		nil, "same-checksum", nil, "same-checksum")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED (equal raw checksums, no inline output)", vr.Outcome)
	}
}

// TestNumericTolerance_NestedCompareFields verifies a chaotic-sim shape: nested output with
// a fights[] array whose per-fight winners legitimately diverge across volunteers, but
// whose AGGREGATE science (a_win_rate, knockout_rate) agrees within epsilon. compare_fields
// restricts comparison to those aggregates, so the unit validates.
func TestNumericTolerance_NestedCompareFields(t *testing.T) {
	eps := 0.1
	a := json.RawMessage(`{"engine":"e","a_win_rate":0.70,"knockout_rate":0.6,
		"fights":[{"seed":0,"winner":"a","duration_s":12.4}],"compute_time_ms":209}`)
	b := json.RawMessage(`{"engine":"e","a_win_rate":0.62,"knockout_rate":0.6,
		"fights":[{"seed":0,"winner":"b","duration_s":18.1}],"compute_time_ms":9684}`)
	vr, err := runTwoResultsCfg(t, "NUMERIC_TOLERANCE", &eps,
		func(c *leaf.ValidationConfig) { c.CompareFields = []string{"a_win_rate", "knockout_rate"} },
		a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED (aggregates within epsilon; per-fight + provenance excluded)", vr.Outcome)
	}
}

// TestNumericTolerance_NestedCompareFields_BeyondEpsilon confirms the aggregate tolerance
// still bites: when a_win_rate diverges beyond epsilon the unit is rejected.
func TestNumericTolerance_NestedCompareFields_BeyondEpsilon(t *testing.T) {
	eps := 0.1
	a := json.RawMessage(`{"a_win_rate":0.70,"knockout_rate":0.6,"fights":[{"winner":"a"}]}`)
	b := json.RawMessage(`{"a_win_rate":0.50,"knockout_rate":0.6,"fights":[{"winner":"b"}]}`)
	vr, err := runTwoResultsCfg(t, "NUMERIC_TOLERANCE", &eps,
		func(c *leaf.ValidationConfig) { c.CompareFields = []string{"a_win_rate", "knockout_rate"} },
		a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED (a_win_rate diverges 0.20 > epsilon 0.1)", vr.Outcome)
	}
}

// TestNumericTolerance_IgnoreFieldsAllNumeric verifies that without compare_fields, ALL
// numeric leaves are compared within epsilon EXCEPT those in ignore_fields. A wall-clock
// field that differs by thousands is excluded, so the within-epsilon science validates.
func TestNumericTolerance_IgnoreFieldsAllNumeric(t *testing.T) {
	eps := 0.01
	a := json.RawMessage(`{"a_win_rate":0.700,"compute_time_ms":209}`)
	b := json.RawMessage(`{"a_win_rate":0.705,"compute_time_ms":9684}`)
	vr, err := runTwoResultsCfg(t, "NUMERIC_TOLERANCE", &eps,
		func(c *leaf.ValidationConfig) { c.IgnoreFields = []string{"compute_time_ms"} },
		a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeValidated {
		t.Fatalf("Outcome = %q, want VALIDATED (a_win_rate within epsilon; compute_time_ms ignored)", vr.Outcome)
	}
}

// TestNumericTolerance_WallClockNotIgnored_Rejects is the control for the above: WITHOUT
// ignore_fields the huge compute_time_ms difference dominates and the unit is rejected —
// demonstrating the field exclusion is what makes redundancy work.
func TestNumericTolerance_WallClockNotIgnored_Rejects(t *testing.T) {
	eps := 0.01
	a := json.RawMessage(`{"a_win_rate":0.700,"compute_time_ms":209}`)
	b := json.RawMessage(`{"a_win_rate":0.705,"compute_time_ms":9684}`)
	vr, err := runTwoResultsCfg(t, "NUMERIC_TOLERANCE", &eps, nil, a, "ck-a", b, "ck-b")
	if err != nil {
		t.Fatalf("TryValidate: %v", err)
	}
	if vr.Outcome != OutcomeRejected {
		t.Fatalf("Outcome = %q, want REJECTED (compute_time_ms differs by ~9000 >> epsilon)", vr.Outcome)
	}
}

// TestFlattenOutput_NestedPaths checks the flattener produces dotted/indexed paths and
// honors ignore/compare selection.
func TestFlattenOutput_NestedPaths(t *testing.T) {
	data := json.RawMessage(`{"a_win_rate":0.7,"fights":[{"winner":"a","duration_s":1.2}],"compute_time_ms":5}`)

	all, err := flattenOutput(data, []string{"compute_time_ms"}, nil)
	if err != nil {
		t.Fatalf("flattenOutput: %v", err)
	}
	if _, ok := all["compute_time_ms"]; ok {
		t.Error("compute_time_ms should have been ignored")
	}
	if v, ok := all["fights.0.winner"]; !ok || v.IsNum || v.Str != "a" {
		t.Errorf("fights.0.winner = %+v (ok=%v), want non-numeric \"a\"", v, ok)
	}
	if v, ok := all["fights.0.duration_s"]; !ok || !v.IsNum || v.Num != 1.2 {
		t.Errorf("fights.0.duration_s = %+v (ok=%v), want numeric 1.2", v, ok)
	}

	sel, err := flattenOutput(data, nil, []string{"a_win_rate"})
	if err != nil {
		t.Fatalf("flattenOutput(compare): %v", err)
	}
	if len(sel) != 1 {
		t.Errorf("compare_fields selection kept %d leaves, want 1 (a_win_rate)", len(sel))
	}
}
