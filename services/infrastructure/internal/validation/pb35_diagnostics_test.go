package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// PB-35 regression tests: the PB-10 remedy (compare_fields / ignore_fields) used to fail
// SILENTLY — a typo'd compare_fields rejected 100% of honest pairs with no diagnostic,
// an under-scoped ignore_fields left nondeterministic non-numeric / per-array-element
// fields disagreeing with nothing naming the field, and NUMERIC_TOLERANCE ignored the
// index-elision contract ignore_fields documents. These tests pin the loud behavior.

// compareEngineWithLog builds a read-only-Compare engine whose logger writes to the
// returned buffer, so tests can assert on the disagreement diagnostics.
func compareEngineWithLog() (*Engine, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, logger, nil, transition.TrustPolicy{}), &buf
}

// TestCompare_Disagreement_WarnsFirstDifferingNumericField is the filed PB-35 shape in
// miniature: honest outputs identical except a wall-clock field the (incomplete)
// ignore_fields does not cover. The results must still disagree (that is correct), but
// the comparison must now NAME the differing field so the leaf author can see WHY.
func TestCompare_Disagreement_WarnsFirstDifferingNumericField(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"unrelated_field"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"result": 3.1415, "compute_time_ms": 7}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"result": 3.1415, "compute_time_ms": 8}`))

	e, buf := compareEngineWithLog()
	wu := makeWorkUnit(wuID, proj.ID, workunit.WorkUnitStateCompleted)
	group, err := e.Compare(context.Background(), wu, proj, []*result.Result{r1, r2})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(group) >= 2 {
		t.Fatalf("outputs differing beyond tolerance on a compared field must not agree, got group of %d", len(group))
	}
	logged := buf.String()
	if !strings.Contains(logged, "compute_time_ms") {
		t.Fatalf("disagreement diagnostic must name the differing field; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "numeric_delta_exceeds_tolerance") {
		t.Fatalf("disagreement diagnostic must state the reason; log was:\n%s", logged)
	}
}

// TestCompare_Disagreement_WarnsNonNumericStringField covers the r3 escalation: a
// nondeterministic NON-numeric field (e.g. an ISO timestamp) compares by exact equality
// under NUMERIC_TOLERANCE, and its disagreement used to be indistinguishable from a
// wrong science result. The diagnostic must name it.
func TestCompare_Disagreement_WarnsNonNumericStringField(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"unrelated_field"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"result": 1.0, "finished_at": "2026-07-23T01:02:03Z"}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"result": 1.0, "finished_at": "2026-07-23T01:02:04Z"}`))

	e, buf := compareEngineWithLog()
	wu := makeWorkUnit(wuID, proj.ID, workunit.WorkUnitStateCompleted)
	group, err := e.Compare(context.Background(), wu, proj, []*result.Result{r1, r2})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(group) >= 2 {
		t.Fatalf("outputs differing on a compared string field must not agree, got group of %d", len(group))
	}
	logged := buf.String()
	if !strings.Contains(logged, "finished_at") {
		t.Fatalf("disagreement diagnostic must name the differing string field; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "string_mismatch") {
		t.Fatalf("disagreement diagnostic must state the reason; log was:\n%s", logged)
	}
}

// TestCompare_IgnoreFields_ElideArrayIndices pins the index-elision contract for
// NUMERIC_TOLERANCE: ignore_fields documents that "inside arrays the index is elided"
// (and EXACT honors it), but the numeric flatten used to match only the INDEXED path —
// per-array-element metadata like items.N.compute_ms was un-ignorable, so honest
// results disagreed under a compliant-looking config.
func TestCompare_IgnoreFields_ElideArrayIndices(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	proj.ValidationConfig.IgnoreFields = []string{"items.compute_ms"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"items": [{"v": 1.0, "compute_ms": 5}, {"v": 2.0, "compute_ms": 6}]}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"items": [{"v": 1.0, "compute_ms": 9}, {"v": 2.0, "compute_ms": 4}]}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) != 2 {
		t.Fatalf("per-array-element metadata listed in ignore_fields (index elided) must be ignored; want agreement of 2, got %d", len(group))
	}
}

// TestCompare_CompareFields_ElideArrayIndices is the compare_fields twin: a pattern
// selects the field in EVERY array element, matching the subtree semantics the knob
// documents.
func TestCompare_CompareFields_ElideArrayIndices(t *testing.T) {
	epsilon := 0.01
	proj := makeLeaf(types.NewID(), 2, 1.0, "NUMERIC_TOLERANCE", &epsilon, 1.0)
	proj.ValidationConfig.CompareFields = []string{"items.v"}

	wuID := types.NewID()
	r1 := makeResult(wuID, types.NewID(), "aaaa", json.RawMessage(`{"items": [{"v": 1.0, "compute_ms": 5}, {"v": 2.0, "compute_ms": 6}]}`))
	r2 := makeResult(wuID, types.NewID(), "bbbb", json.RawMessage(`{"items": [{"v": 1.0, "compute_ms": 9}, {"v": 2.0, "compute_ms": 4}]}`))

	group := compareGroup(t, proj, []*result.Result{r1, r2})
	if len(group) != 2 {
		t.Fatalf("compare_fields selecting a per-element field (index elided) must compare only it; want agreement of 2, got %d", len(group))
	}
}
