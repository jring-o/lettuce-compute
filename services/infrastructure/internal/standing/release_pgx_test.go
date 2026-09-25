//go:build integration

package standing

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// The head guide tells an operator how to release an account the backpressure machine
// holds. These tests pin the facts that advice rests on, so a change to either release
// path fails here before the guide is wrong:
//
//   - a clear hands the row back to the machine (OK / AUTO) with its decayed rejection
//     signal untouched, so an account still at or above the probation rate returns to
//     PROBATION on its very next adjudicated result, even an agreeing one;
//   - an operator-set OK sticks: the row becomes operator-owned and the machine neither
//     transitions it nor folds any result into it, until a clear hands it back.
func TestRelease_ClearKeepsTheSignalOperatorOKSticks(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())
	repo := NewPgxRepository(pool)

	t.Run("clear hands the account back with its signal", func(t *testing.T) {
		id := insertVolunteer(t, pool)
		// Rate 6/8 = 0.75 over sample 8: on PROBATION, well above the 0.5 entry rate.
		seedAuto(t, pool, id, 2, 6, volunteer.StandingProbation, nil)

		if _, err := repo.Clear(ctx, id); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		v := readVol(t, pool, id)
		if v.standing != volunteer.StandingOK || v.source != volunteer.StandingSourceAuto {
			t.Fatalf("after clear standing/source = %q/%q, want OK/AUTO", v.standing, v.source)
		}
		if !closeTo(v.good, 2, 1e-6) || !closeTo(v.bad, 6, 1e-6) {
			t.Fatalf("after clear good/bad = %v/%v, want 2/6 (a clear does not reset the signal)", v.good, v.bad)
		}

		// One AGREEING result: rate 6/9 ≈ 0.67, still at or above the 0.5 entry rate.
		out, err := rec.RecordAdjudicated(ctx, id, true)
		if err != nil {
			t.Fatalf("RecordAdjudicated: %v", err)
		}
		if !out.Applied || out.NewStanding != volunteer.StandingProbation {
			t.Fatalf("after clear + one agreeing result: applied=%v standing=%q, want applied PROBATION",
				out.Applied, out.NewStanding)
		}
	})

	t.Run("an operator OK sticks until a clear hands it back", func(t *testing.T) {
		id := insertVolunteer(t, pool)
		seedAuto(t, pool, id, 2, 6, volunteer.StandingProbation, nil)

		if _, err := repo.SetOperator(ctx, id, volunteer.StandingOK, nil, "released by the operator"); err != nil {
			t.Fatalf("SetOperator: %v", err)
		}
		// Even a DISAGREEING result neither moves the row nor folds into its signal.
		out, err := rec.RecordAdjudicated(ctx, id, false)
		if err != nil {
			t.Fatalf("RecordAdjudicated: %v", err)
		}
		if out.Applied {
			t.Fatalf("the machine folded into an operator-owned row: %+v", out)
		}
		v := readVol(t, pool, id)
		if v.standing != volunteer.StandingOK || v.source != volunteer.StandingSourceOperator {
			t.Fatalf("standing/source = %q/%q, want OK/OPERATOR", v.standing, v.source)
		}
		if !closeTo(v.good, 2, 1e-6) || !closeTo(v.bad, 6, 1e-6) {
			t.Fatalf("good/bad = %v/%v, want the signal frozen at 2/6", v.good, v.bad)
		}

		// A clear hands it back to the machine, signal included.
		if _, err := repo.Clear(ctx, id); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		out, err = rec.RecordAdjudicated(ctx, id, true)
		if err != nil {
			t.Fatalf("RecordAdjudicated after clear: %v", err)
		}
		if !out.Applied || out.NewStanding != volunteer.StandingProbation {
			t.Fatalf("after hand-back + one agreeing result: applied=%v standing=%q, want applied PROBATION",
				out.Applied, out.NewStanding)
		}
	})
}
