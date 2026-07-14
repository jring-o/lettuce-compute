package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Unit tests (no DB) for the spot-check reclaim re-evaluate (design §4.4, BG-21d): after a
// successful ClearSpotCheck the fault monitor must re-Evaluate the unit exactly once so its
// single PENDING result validates (the resolved quorum drops to 1). Before this fix the unit
// sat QUEUED forever with one complete, never-credited result.

// spotCheckReclaimRepo implements only the WorkUnitRepository methods ScanOnce touches when
// the sole work to do is a stuck spot-check reclaim: the expired-copy, dispatch-claim, and
// stale-checkpoint sweeps return empty; FindStuckSpotCheckUnits yields the seeded units; and
// ClearSpotCheck records the id (or fails, when clearErr is set). The embedded nil interface
// panics on anything else — which this path never calls.
type spotCheckReclaimRepo struct {
	workunit.WorkUnitRepository
	stuck    []*workunit.WorkUnit
	cleared  []types.ID
	clearErr error
}

func (r *spotCheckReclaimRepo) FindExpiredCopies(context.Context, int) ([]*workunit.Copy, error) {
	return nil, nil
}

func (r *spotCheckReclaimRepo) FindStuckSpotCheckUnits(context.Context, int) ([]*workunit.WorkUnit, error) {
	return r.stuck, nil
}

func (r *spotCheckReclaimRepo) ClearSpotCheck(_ context.Context, id types.ID) error {
	if r.clearErr != nil {
		return r.clearErr
	}
	r.cleared = append(r.cleared, id)
	return nil
}

func (r *spotCheckReclaimRepo) ClearExpiredDispatchClaims(context.Context) (int64, error) {
	return 0, nil
}

func (r *spotCheckReclaimRepo) FindRunningWithStaleCheckpoints(context.Context, int) ([]workunit.StaleCheckpointInfo, error) {
	return nil, nil
}

// spyEvaluator counts Evaluate calls and records the unit ids, standing in for the concrete
// transitioner through the fault monitor's unitEvaluator interface.
type spyEvaluator struct {
	calls   []types.ID
	outcome transition.Outcome
	err     error
}

func (s *spyEvaluator) Evaluate(_ context.Context, id types.ID) (transition.Outcome, error) {
	s.calls = append(s.calls, id)
	return s.outcome, s.err
}

func newSpotCheckMonitor(repo workunit.WorkUnitRepository, ev unitEvaluator) *FaultMonitor {
	return &FaultMonitor{
		workUnitRepo: repo,
		transitioner: ev,
		logger:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		batchSize:    100,
	}
}

func TestScanOnce_SpotCheckClearTriggersOneEvaluate(t *testing.T) {
	stuckID := types.NewID()
	repo := &spotCheckReclaimRepo{stuck: []*workunit.WorkUnit{{ID: stuckID}}}
	spy := &spyEvaluator{outcome: transition.OutcomeValidated}
	m := newSpotCheckMonitor(repo, spy)

	if err := m.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}

	if len(repo.cleared) != 1 || repo.cleared[0] != stuckID {
		t.Fatalf("ClearSpotCheck calls = %v, want exactly [%s]", repo.cleared, stuckID)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("Evaluate calls after spot-check clear = %d, want exactly 1", len(spy.calls))
	}
	if spy.calls[0] != stuckID {
		t.Fatalf("Evaluate called for %s, want the cleared unit %s", spy.calls[0], stuckID)
	}
}

func TestScanOnce_SpotCheckClearFailureSkipsEvaluate(t *testing.T) {
	stuckID := types.NewID()
	repo := &spotCheckReclaimRepo{
		stuck:    []*workunit.WorkUnit{{ID: stuckID}},
		clearErr: errors.New("simulated clear failure"),
	}
	spy := &spyEvaluator{}
	m := newSpotCheckMonitor(repo, spy)

	if err := m.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("Evaluate must not run when ClearSpotCheck fails; got %d call(s)", len(spy.calls))
	}
}
