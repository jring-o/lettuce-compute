package leaf

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// fakeListRepo serves a fixed leaf set through List; every other Repository method is
// inherited from the embedded interface and panics if reached (the scan must only List).
type fakeListRepo struct {
	Repository
	leafs []*Leaf
}

func (f *fakeListRepo) List(_ context.Context, filters LeafListFilters, _ types.PaginationRequest) ([]*Leaf, types.PaginationResponse, error) {
	var out []*Leaf
	for _, p := range f.leafs {
		if filters.State != nil && p.State != *filters.State {
			continue
		}
		out = append(out, p)
	}
	return out, types.PaginationResponse{HasMore: false}, nil
}

// TestWarnActiveConfigFootguns_FlagsPreGateLeaf covers the PB-36 retroactivity scan: an
// ACTIVE leaf whose stored config predates the PB-10 comparison-scoping gate (redundant
// NUMERIC_TOLERANCE, no scoping) is WARN-logged at boot; a compliant leaf is not.
func TestWarnActiveConfigFootguns_FlagsPreGateLeaf(t *testing.T) {
	tolerance := 0.001
	preGate := &Leaf{
		ID:    types.NewID(),
		Name:  "pre-gate-unscoped",
		State: StateActive,
		ValidationConfig: ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     ComparisonNumericTolerance,
			NumericTolerance:   &tolerance,
			MaxRetries:         3,
		},
	}
	compliant := &Leaf{
		ID:    types.NewID(),
		Name:  "scoped",
		State: StateActive,
		ValidationConfig: ValidationConfig{
			RedundancyFactor:   2,
			AgreementThreshold: 1.0,
			ComparisonMode:     ComparisonNumericTolerance,
			NumericTolerance:   &tolerance,
			MaxRetries:         3,
			CompareFields:      []string{"result"},
		},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	WarnActiveConfigFootguns(context.Background(), &fakeListRepo{leafs: []*Leaf{preGate, compliant}}, logger)

	logged := buf.String()
	if !strings.Contains(logged, preGate.ID.String()) {
		t.Fatalf("scan must WARN for the pre-gate unscoped leaf; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "requires comparison scoping") {
		t.Fatalf("scan WARN must carry the actionable validation error; log was:\n%s", logged)
	}
	if strings.Contains(logged, compliant.ID.String()) {
		t.Fatalf("scan must not flag a compliant leaf; log was:\n%s", logged)
	}
}
