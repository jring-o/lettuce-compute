package validation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// The comparators narrow each output to a compared field set before checking agreement.
// These tests pin the fail-closed contract: an EMPTY compared set — compare_fields that
// select a path present in no output, or ignore_fields that strip every leaf — is never
// agreement. Without it, two results whose actual content differs would form a validating
// quorum having compared nothing (NUMERIC_TOLERANCE matched two empty flattened maps;
// EXACT collapsed every ignore-all output to the same "{}" canonical checksum).

// compareOnlyEngine builds an engine for the read-only Compare path (the comparator entry
// the transitioner calls); no repository is touched, so none is wired.
func compareOnlyEngine() *Engine {
	return NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, testLogger(), nil, transition.TrustPolicy{})
}

func compareGroup(t *testing.T, proj *leaf.Leaf, results []*result.Result) []*result.Result {
	t.Helper()
	wu := makeWorkUnit(types.NewID(), proj.ID, workunit.WorkUnitStateCompleted)
	group, err := compareOnlyEngine().Compare(context.Background(), wu, proj, results)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return group
}

func TestCompare_NumericCompareFieldsMatchNothing_NoAgreement(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	// The compared path exists in NO output, so every result narrows to an empty set.
	proj.ValidationConfig.CompareFields = []string{"aggregate.score"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"x": 1}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"x": 2}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) >= 2 {
		t.Fatalf("two results with different content formed an agreeing group of %d having compared nothing (compare_fields matched no path)", len(group))
	}
}

func TestCompare_NumericIgnoreFieldsStripEverything_NoAgreement(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	// ignore_fields strips every leaf the outputs have, so every result narrows to an
	// empty set.
	proj.ValidationConfig.IgnoreFields = []string{"x"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"x": 1}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"x": 2}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) >= 2 {
		t.Fatalf("two results with different content formed an agreeing group of %d having compared nothing (ignore_fields stripped every leaf)", len(group))
	}
}

func TestCompare_ExactIgnoreAllFields_DoesNotGroup(t *testing.T) {
	proj := makeLeaf(types.NewID(), 2, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"x"}

	wuID := types.NewID()
	// Different content AND different submitted checksums; with every leaf stripped the
	// canonical form of both outputs is the same empty shape, which must not group.
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"x": 1}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"x": 2}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) >= 2 {
		t.Fatalf("two results with different content grouped on the empty canonical form (%d in group)", len(group))
	}
}

// Controls: a genuinely compared field still agrees — the fail-closed rule must not block
// real corroboration.

func TestCompare_NumericCompareFieldsPresent_StillAgrees(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	proj.ValidationConfig.CompareFields = []string{"score"}

	wuID := types.NewID()
	// Agree on the compared field within epsilon; differ on the excluded noise.
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"score": 1.0, "noise": 111}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"score": 1.0005, "noise": 999}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) != 2 {
		t.Fatalf("results agreeing on the compared field must group, got group of %d", len(group))
	}
}

func TestCompare_ExactIgnoreSomeFields_StillAgrees(t *testing.T) {
	proj := makeLeaf(types.NewID(), 2, 1.0, "EXACT", nil, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"compute_time_ms"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"v": 7, "compute_time_ms": 1}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"v": 7, "compute_time_ms": 2}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) != 2 {
		t.Fatalf("results identical after stripping the volatile field must group, got group of %d", len(group))
	}
}
