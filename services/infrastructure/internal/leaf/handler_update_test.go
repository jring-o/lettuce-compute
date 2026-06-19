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
