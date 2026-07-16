package leaf

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// mockUpdateRepo is a minimal in-memory Repository for exercising handleUpdate
// without a database (CI-runnable; the full handler_test.go is integration/DB-gated).
type mockUpdateRepo struct {
	leaf *Leaf
}

func (m *mockUpdateRepo) Create(context.Context, *Leaf) error { return nil }
func (m *mockUpdateRepo) GetByID(context.Context, types.ID) (*Leaf, error) {
	return m.leaf, nil
}
func (m *mockUpdateRepo) GetBySlug(context.Context, string, *types.ID) (*Leaf, error) {
	return m.leaf, nil
}
func (m *mockUpdateRepo) GetBySlugPublic(context.Context, string) (*Leaf, error) {
	return m.leaf, nil
}
func (m *mockUpdateRepo) List(context.Context, LeafListFilters, types.PaginationRequest) ([]*Leaf, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (m *mockUpdateRepo) Update(_ context.Context, p *Leaf) error { m.leaf = p; return nil }
func (m *mockUpdateRepo) Delete(context.Context, types.ID) error  { return nil }

func newUpdateTestLeaf() *Leaf {
	id := types.NewID()
	return &Leaf{
		ID:          id,
		Name:        "Beyblade Arena",
		Description: "physics tournament leaf for tests",
		State:       StateActive,
		TaskPattern: PatternParameterSweep,
		ValidationConfig: ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     ComparisonExact,
			MaxRetries:         3,
		},
	}
}

func doUpdate(t *testing.T, h *LeafHandler, id types.ID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/leafs/"+id.String(), bytes.NewBufferString(body))
	req.SetPathValue("leaf_id", id.String())
	rec := httptest.NewRecorder()
	h.handleUpdate(rec, req)
	return rec
}

// TestHandleUpdate_MergesValidationConfig verifies the #41 fix: sending ONLY ignore_fields
// in validation_config overlays that one field and PRESERVES the rest (redundancy,
// comparison_mode, threshold) instead of zeroing them via whole-block replace.
func TestHandleUpdate_MergesValidationConfig(t *testing.T) {
	lf := newUpdateTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"validation_config":{"ignore_fields":["compute_time_ms"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	vc := lf.ValidationConfig
	if vc.RedundancyFactor != 2 {
		t.Errorf("RedundancyFactor = %d, want 2 preserved (not zeroed by replace)", vc.RedundancyFactor)
	}
	if vc.ComparisonMode != ComparisonExact {
		t.Errorf("ComparisonMode = %q, want EXACT preserved", vc.ComparisonMode)
	}
	if vc.AgreementThreshold != 1.0 {
		t.Errorf("AgreementThreshold = %v, want 1.0 preserved", vc.AgreementThreshold)
	}
	if len(vc.IgnoreFields) != 1 || vc.IgnoreFields[0] != "compute_time_ms" {
		t.Errorf("IgnoreFields = %v, want [compute_time_ms]", vc.IgnoreFields)
	}
}

// TestHandleUpdate_RejectsInvalidValidationConfig verifies the #41 fix re-validates the
// merged block: an out-of-range agreement_threshold is now rejected on update (previously
// validation only ran at activation, so this was accepted silently on an ACTIVE leaf).
func TestHandleUpdate_RejectsInvalidValidationConfig(t *testing.T) {
	lf := newUpdateTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"validation_config":{"agreement_threshold":1.5}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want 4xx (agreement_threshold 1.5 is out of range); body=%s", rec.Body.String())
	}
}

// TestHandleUpdate_RejectsCompareFieldsWrongMode verifies the new compare_fields guard is
// enforced on update: compare_fields is only valid for NUMERIC_TOLERANCE.
func TestHandleUpdate_RejectsCompareFieldsWrongMode(t *testing.T) {
	lf := newUpdateTestLeaf() // ComparisonExact
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"validation_config":{"compare_fields":["a_win_rate"]}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want 4xx (compare_fields requires NUMERIC_TOLERANCE); body=%s", rec.Body.String())
	}
}

// newLazyMonteCarloTestLeaf is an ONGOING lazy Monte Carlo leaf with NO num_trials — a legal
// configuration (an ongoing leaf never exhausts and needs no total), and the launchpad for the
// ★BG-22e bypass: flipping is_ongoing to false makes num_trials load-bearing.
func newLazyMonteCarloTestLeaf() *Leaf {
	id := types.NewID()
	return &Leaf{
		ID:          id,
		Name:        "Lazy MC Lifecycle",
		Description: "finite-flip validation leaf for tests",
		State:       StateActive,
		TaskPattern: PatternMonteCarlo,
		IsOngoing:   true,
		ValidationConfig: ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     ComparisonExact,
			MaxRetries:         3,
		},
		DataConfig: DataConfig{
			TransferStrategy:   TransferInline,
			AggregationFormat:  AggregationJSON,
			MaxInputSizeBytes:  1048576,
			MaxOutputSizeBytes: 104857600,
			GenerationMode:     GenerationModeLazy,
			LazyThreshold:      50,
			LazyBatchSize:      100,
			SplittingConfig:    map[string]any{"seed_strategy": "hash"}, // no num_trials
		},
	}
}

// TestHandleUpdate_IsOngoingFlipRevalidatesDataConfig (★BG-22e): PATCH {"is_ongoing": false}
// with NO data_config block must re-run ValidateDataConfig against the new lifecycle — a
// finite lazy Monte Carlo leaf must declare splitting_config.num_trials, the total exhaustion
// is decided against. Pre-fix the flip was applied unconditionally and validation only ran
// when the request carried a data_config block, so this request landed the leaf in the exact
// state validation declares impossible and it generated full batches forever.
func TestHandleUpdate_IsOngoingFlipRevalidatesDataConfig(t *testing.T) {
	lf := newLazyMonteCarloTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"is_ongoing":false}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want 4xx: a finite lazy monte_carlo leaf with no num_trials never exhausts; body=%s", rec.Body.String())
	}
}

// TestHandleUpdate_IsOngoingFlipAcceptedWithDeclaredTotal: the same flip is fine when the
// stored config already declares the total (the rule binds on the invalid combination only).
func TestHandleUpdate_IsOngoingFlipAcceptedWithDeclaredTotal(t *testing.T) {
	lf := newLazyMonteCarloTestLeaf()
	lf.DataConfig.SplittingConfig["num_trials"] = float64(1000)
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"is_ongoing":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (num_trials declared, flip is legal); body=%s", rec.Code, rec.Body.String())
	}
	if lf.IsOngoing != false {
		t.Error("is_ongoing flip not persisted")
	}
}

// TestHandleUpdate_IsOngoingFlipWithNumTrialsInSameRequest: supplying the missing total in
// the same PATCH satisfies the rule — the data_config block validates against the NEW
// is_ongoing because the flip is applied before the config merges.
func TestHandleUpdate_IsOngoingFlipWithNumTrialsInSameRequest(t *testing.T) {
	lf := newLazyMonteCarloTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"is_ongoing":false,"data_config":{"splitting_config":{"seed_strategy":"hash","num_trials":500}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (total supplied in the same request); body=%s", rec.Code, rec.Body.String())
	}
	if lf.IsOngoing != false {
		t.Error("is_ongoing flip not persisted")
	}
}
