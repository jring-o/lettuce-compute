//go:build integration

package standing

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// testCfg is a small backpressure config: MinSample 3 so a scenario needs only a couple of
// folds (or a seeded accumulator pair) to cross the transition gate. It satisfies the
// effective-config invariant 0 < OKRate < ProbationRate <= BenchRate <= 1.
func testCfg() BackpressureConfig {
	return BackpressureConfig{
		ProbationRate: 0.5,
		OKRate:        0.2,
		BenchRate:     0.8,
		MinSample:     3,
		BenchFor:      time.Hour,
	}
}

// volSignal is the raw signal + standing state of a volunteer row, for direct assertions.
type volSignal struct {
	good      float64
	bad       float64
	updatedAt *time.Time
	standing  string
	benched   *time.Time
	source    string
	reason    *string
	changedAt *time.Time
}

func readVol(t *testing.T, pool *pgxpool.Pool, id types.ID) volSignal {
	t.Helper()
	var v volSignal
	if err := pool.QueryRow(context.Background(), `
		SELECT rejection_good, rejection_bad, rejection_updated_at,
		       standing, benched_until, standing_source, standing_reason, standing_changed_at
		FROM volunteers WHERE id = $1`, id).
		Scan(&v.good, &v.bad, &v.updatedAt, &v.standing, &v.benched, &v.source, &v.reason, &v.changedAt); err != nil {
		t.Fatalf("read volunteer row: %v", err)
	}
	return v
}

// seedAuto sets an AUTO row's accumulators and standing with rejection_updated_at = NOW(),
// so a single following fold decays the seeded values by a negligible factor (~1). This
// lets a transition scenario reach a chosen (sample, rate) in one fold with comfortable
// margins above/around MinSample, instead of racing sub-millisecond decay near the gate.
func seedAuto(t *testing.T, pool *pgxpool.Pool, id types.ID, good, bad float64, standing string, benched *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE volunteers
		SET rejection_good = $2, rejection_bad = $3, rejection_updated_at = NOW(),
		    standing = $4, benched_until = $5, standing_source = 'AUTO'
		WHERE id = $1`, id, good, bad, standing, benched); err != nil {
		t.Fatalf("seed AUTO signal: %v", err)
	}
}

func closeTo(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol+tol*math.Abs(want)
}

// 1. First fold on a fresh row: no decay (accumulators exactly 0 or 1), Applied=true, and
// standing does not move (sample 1 < MinSample).
func TestRecordAdjudicated_FirstFoldNoDecay(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	t.Run("disagreed", func(t *testing.T) {
		id := insertVolunteer(t, pool)
		out, err := rec.RecordAdjudicated(ctx, id, false)
		if err != nil {
			t.Fatalf("RecordAdjudicated: %v", err)
		}
		if !out.Applied {
			t.Fatal("Applied = false, want true")
		}
		if out.OldStanding != volunteer.StandingOK || out.NewStanding != volunteer.StandingOK {
			t.Errorf("standing = %q -> %q, want OK -> OK", out.OldStanding, out.NewStanding)
		}
		if !closeTo(out.Rate, 1.0, 1e-9) || !closeTo(out.Sample, 1.0, 1e-9) {
			t.Errorf("rate/sample = %v/%v, want 1/1", out.Rate, out.Sample)
		}
		v := readVol(t, pool, id)
		if !closeTo(v.good, 0, 1e-9) || !closeTo(v.bad, 1, 1e-9) {
			t.Errorf("good/bad = %v/%v, want 0/1 (no decay)", v.good, v.bad)
		}
		if v.updatedAt == nil {
			t.Error("rejection_updated_at not set after first fold")
		}
		if v.standing != volunteer.StandingOK || v.source != volunteer.StandingSourceAuto {
			t.Errorf("standing/source = %q/%q, want OK/AUTO", v.standing, v.source)
		}
		if v.reason != nil || v.changedAt != nil {
			t.Errorf("reason/changed_at = %v/%v, want nil/nil below MinSample", v.reason, v.changedAt)
		}
	})

	t.Run("agreed", func(t *testing.T) {
		id := insertVolunteer(t, pool)
		out, err := rec.RecordAdjudicated(ctx, id, true)
		if err != nil {
			t.Fatalf("RecordAdjudicated: %v", err)
		}
		if !out.Applied {
			t.Fatal("Applied = false, want true")
		}
		if !closeTo(out.Rate, 0.0, 1e-9) || !closeTo(out.Sample, 1.0, 1e-9) {
			t.Errorf("rate/sample = %v/%v, want 0/1", out.Rate, out.Sample)
		}
		v := readVol(t, pool, id)
		if !closeTo(v.good, 1, 1e-9) || !closeTo(v.bad, 0, 1e-9) {
			t.Errorf("good/bad = %v/%v, want 1/0 (no decay)", v.good, v.bad)
		}
	})
}

// 2. Decay math: seed accumulators one half-life in the past, fold once, assert the pair
// was halved-then-incremented. The fold stamps rejection_updated_at to the same NOW() it
// used in the decay factor, so the expected factor is computed from the read-back timestamp
// (exact elapsed) and only floating-point tolerance is needed.
func TestRecordAdjudicated_DecayMath(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	halfLife := float64(reliability.HalfLifeSeconds)
	past := time.Now().UTC().Add(-time.Duration(halfLife) * time.Second).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE volunteers SET rejection_good = 4, rejection_bad = 2, rejection_updated_at = $2
		WHERE id = $1`, id, past); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := rec.RecordAdjudicated(ctx, id, true) // agreed => +1 to good
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if !out.Applied {
		t.Fatal("Applied = false, want true")
	}

	v := readVol(t, pool, id)
	if v.updatedAt == nil {
		t.Fatal("rejection_updated_at not set")
	}
	elapsed := v.updatedAt.Sub(past).Seconds()
	factor := math.Exp(-elapsed * math.Ln2 / halfLife)
	wantGood := 4*factor + 1
	wantBad := 2 * factor
	if math.Abs(v.good-wantGood) > 1e-6*math.Max(1, wantGood) {
		t.Errorf("rejection_good = %v, want %v (factor %v)", v.good, wantGood, factor)
	}
	if math.Abs(v.bad-wantBad) > 1e-6*math.Max(1, wantBad) {
		t.Errorf("rejection_bad = %v, want %v (factor %v)", v.bad, wantBad, factor)
	}
}

// 3. OPERATOR row: the fold matches no AUTO row, so Applied=false and NOT ONE column moves
// (accumulators, timestamp, and standing are all left exactly as seeded).
func TestRecordAdjudicated_OperatorRowUntouched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	seeded := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE volunteers
		SET rejection_good = 5, rejection_bad = 5, rejection_updated_at = $2,
		    standing = 'PROBATION', standing_source = 'OPERATOR'
		WHERE id = $1`, id, seeded); err != nil {
		t.Fatalf("seed operator row: %v", err)
	}

	out, err := rec.RecordAdjudicated(ctx, id, false)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.Applied {
		t.Fatal("Applied = true, want false for an OPERATOR row")
	}

	v := readVol(t, pool, id)
	if !closeTo(v.good, 5, 1e-9) || !closeTo(v.bad, 5, 1e-9) {
		t.Errorf("good/bad = %v/%v, want 5/5 unchanged", v.good, v.bad)
	}
	if v.updatedAt == nil || !v.updatedAt.Equal(seeded) {
		t.Errorf("rejection_updated_at = %v, want %v unchanged", v.updatedAt, seeded)
	}
	if v.standing != volunteer.StandingProbation || v.source != volunteer.StandingSourceOperator {
		t.Errorf("standing/source = %q/%q, want PROBATION/OPERATOR unchanged", v.standing, v.source)
	}
}

// 4. Missing volunteer: Applied=false and no error.
func TestRecordAdjudicated_MissingVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	out, err := rec.RecordAdjudicated(ctx, types.NewID(), false)
	if err != nil {
		t.Fatalf("RecordAdjudicated missing volunteer: %v", err)
	}
	if out.Applied {
		t.Error("Applied = true, want false for an absent volunteer")
	}
}

// 5. MinSample gate: two rejections give a 100% rate but sample ~2 < MinSample 3, so no
// transition fires — the accumulators fold but standing stays OK.
func TestRecordAdjudicated_MinSampleGate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	if _, err := rec.RecordAdjudicated(ctx, id, false); err != nil {
		t.Fatalf("first fold: %v", err)
	}
	out, err := rec.RecordAdjudicated(ctx, id, false) // second fold: bad ~2, sample ~2, rate 1.0
	if err != nil {
		t.Fatalf("second fold: %v", err)
	}
	if out.Sample >= testCfg().MinSample {
		t.Fatalf("sample = %v, test setup error: want < MinSample %v", out.Sample, testCfg().MinSample)
	}
	if !closeTo(out.Rate, 1.0, 1e-6) {
		t.Errorf("rate = %v, want ~1.0 (both folds rejected)", out.Rate)
	}
	if out.NewStanding != volunteer.StandingOK {
		t.Errorf("NewStanding = %q, want OK (100%% rate but sample < MinSample)", out.NewStanding)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingOK || v.changedAt != nil || v.reason != nil {
		t.Errorf("row standing/changed/reason = %q/%v/%v, want OK with no transition metadata", v.standing, v.changedAt, v.reason)
	}
}

// 6. OK -> PROBATION once sample >= MinSample and rate >= ProbationRate: benched_until stays
// NULL, the reason names the rate, standing_changed_at is set, and the source stays AUTO.
func TestRecordAdjudicated_OKToProbation(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	seedAuto(t, pool, id, 2, 2, volunteer.StandingOK, nil) // fold(false) => good 2, bad 3, sample 5, rate 0.6

	out, err := rec.RecordAdjudicated(ctx, id, false)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.OldStanding != volunteer.StandingOK || out.NewStanding != volunteer.StandingProbation {
		t.Errorf("standing = %q -> %q, want OK -> PROBATION", out.OldStanding, out.NewStanding)
	}
	if !closeTo(out.Rate, 0.6, 1e-3) || !closeTo(out.Sample, 5, 1e-3) {
		t.Errorf("rate/sample = %v/%v, want ~0.6/~5", out.Rate, out.Sample)
	}
	if out.BenchedUntil != nil {
		t.Errorf("BenchedUntil = %v, want nil on OK -> PROBATION", out.BenchedUntil)
	}

	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingProbation || v.benched != nil {
		t.Errorf("row standing/benched = %q/%v, want PROBATION/nil", v.standing, v.benched)
	}
	if v.source != volunteer.StandingSourceAuto {
		t.Errorf("source = %q, want AUTO", v.source)
	}
	if v.changedAt == nil {
		t.Error("standing_changed_at not set on transition")
	}
	if v.reason == nil || !strings.Contains(*v.reason, "backpressure") || !strings.Contains(*v.reason, "60.0") {
		t.Errorf("reason = %v, want a backpressure reason naming the 60.0%% rate", v.reason)
	}
}

// 7. No rung-skipping: a fresh OK row whose rate is already >= BenchRate moves to PROBATION,
// never straight to BENCHED.
func TestRecordAdjudicated_NoRungSkip(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	seedAuto(t, pool, id, 0, 3, volunteer.StandingOK, nil) // fold(false) => good 0, bad 4, sample 4, rate 1.0

	out, err := rec.RecordAdjudicated(ctx, id, false)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.NewStanding != volunteer.StandingProbation {
		t.Errorf("NewStanding = %q, want PROBATION (must not skip straight to BENCHED at rate >= BenchRate from OK)", out.NewStanding)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingProbation || v.benched != nil {
		t.Errorf("row standing/benched = %q/%v, want PROBATION/nil", v.standing, v.benched)
	}
}

// 8. PROBATION -> BENCHED at rate >= BenchRate: benched_until is set to about NOW()+BenchFor.
func TestRecordAdjudicated_ProbationToBenched(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cfg := testCfg()
	rec := NewPgxRecorder(pool, cfg)

	id := insertVolunteer(t, pool)
	seedAuto(t, pool, id, 0, 3, volunteer.StandingProbation, nil) // fold(false) => bad 4, sample 4, rate 1.0

	before := time.Now().UTC()
	out, err := rec.RecordAdjudicated(ctx, id, false)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	after := time.Now().UTC()
	if out.OldStanding != volunteer.StandingProbation || out.NewStanding != volunteer.StandingBenched {
		t.Errorf("standing = %q -> %q, want PROBATION -> BENCHED", out.OldStanding, out.NewStanding)
	}
	if out.BenchedUntil == nil {
		t.Fatal("BenchedUntil = nil, want a deadline on PROBATION -> BENCHED")
	}
	wantLo := before.Add(cfg.BenchFor).Add(-time.Minute)
	wantHi := after.Add(cfg.BenchFor).Add(time.Minute)
	if out.BenchedUntil.Before(wantLo) || out.BenchedUntil.After(wantHi) {
		t.Errorf("BenchedUntil = %v, want ~NOW()+%v (within a minute)", out.BenchedUntil, cfg.BenchFor)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingBenched || v.benched == nil {
		t.Errorf("row standing/benched = %q/%v, want BENCHED/<deadline>", v.standing, v.benched)
	}
	if v.changedAt == nil {
		t.Error("standing_changed_at not set on transition")
	}
	if v.source != volunteer.StandingSourceAuto {
		t.Errorf("source = %q, want AUTO", v.source)
	}
}

// 9. PROBATION -> OK at rate <= OKRate: benched_until is cleared.
func TestRecordAdjudicated_ProbationToOK(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	seedAuto(t, pool, id, 5, 0, volunteer.StandingProbation, nil) // fold(true) => good 6, bad 0, sample 6, rate 0.0

	out, err := rec.RecordAdjudicated(ctx, id, true)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.OldStanding != volunteer.StandingProbation || out.NewStanding != volunteer.StandingOK {
		t.Errorf("standing = %q -> %q, want PROBATION -> OK", out.OldStanding, out.NewStanding)
	}
	if out.BenchedUntil != nil {
		t.Errorf("BenchedUntil = %v, want nil on transition to OK", out.BenchedUntil)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingOK || v.benched != nil {
		t.Errorf("row standing/benched = %q/%v, want OK/nil", v.standing, v.benched)
	}
	if v.changedAt == nil {
		t.Error("standing_changed_at not set on transition")
	}
}

// 10. Live BENCHED row: even at a high sample and 100% rate the fold only updates the
// accumulators; the effective standing is BENCHED, so no transition arm fires and standing
// is left untouched.
func TestRecordAdjudicated_LiveBenchedAccumulatesOnly(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	live := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	seedAuto(t, pool, id, 0, 5, volunteer.StandingBenched, &live) // fold(false) => bad 6, sample 6, rate 1.0

	out, err := rec.RecordAdjudicated(ctx, id, false)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.OldStanding != volunteer.StandingBenched || out.NewStanding != volunteer.StandingBenched {
		t.Errorf("standing = %q -> %q, want BENCHED -> BENCHED", out.OldStanding, out.NewStanding)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingBenched {
		t.Errorf("standing = %q, want BENCHED unchanged", v.standing)
	}
	if v.benched == nil || !v.benched.Equal(live) {
		t.Errorf("benched_until = %v, want %v unchanged", v.benched, live)
	}
	if !closeTo(v.bad, 6, 1e-3) {
		t.Errorf("rejection_bad = %v, want ~6 (accumulator still folded)", v.bad)
	}
}

// 11. Expired-bench row: stored BENCHED with a past benched_until resolves to PROBATION
// effectively, so a low rate at sample >= MinSample transitions the stored standing to OK —
// re-entry through the hysteresis exit overwrites the stale stored BENCHED.
func TestRecordAdjudicated_ExpiredBenchReentersOK(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	expired := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	seedAuto(t, pool, id, 5, 0, volunteer.StandingBenched, &expired) // eff PROBATION; fold(true) => rate 0.0

	out, err := rec.RecordAdjudicated(ctx, id, true)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.OldStanding != volunteer.StandingBenched || out.NewStanding != volunteer.StandingOK {
		t.Errorf("standing = %q -> %q, want BENCHED -> OK via the hysteresis exit", out.OldStanding, out.NewStanding)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingOK || v.benched != nil {
		t.Errorf("row standing/benched = %q/%v, want OK/nil", v.standing, v.benched)
	}
}

// 12. Hysteresis band: a PROBATION row whose rate sits strictly between OKRate and BenchRate
// stays PROBATION — neither the bench arm nor the exit arm fires.
func TestRecordAdjudicated_HysteresisBandStaysProbation(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	rec := NewPgxRecorder(pool, testCfg())

	id := insertVolunteer(t, pool)
	seedAuto(t, pool, id, 3, 3, volunteer.StandingProbation, nil) // fold(false) => good 3, bad 4, sample 7, rate ~0.571

	out, err := rec.RecordAdjudicated(ctx, id, false)
	if err != nil {
		t.Fatalf("RecordAdjudicated: %v", err)
	}
	if out.NewStanding != volunteer.StandingProbation {
		t.Errorf("NewStanding = %q, want PROBATION (OKRate < rate < BenchRate is the hysteresis band)", out.NewStanding)
	}
	if out.Rate <= testCfg().OKRate || out.Rate >= testCfg().BenchRate {
		t.Fatalf("rate = %v, test setup error: want strictly inside (%v, %v)", out.Rate, testCfg().OKRate, testCfg().BenchRate)
	}
	v := readVol(t, pool, id)
	if v.standing != volunteer.StandingProbation {
		t.Errorf("standing = %q, want PROBATION unchanged in the band", v.standing)
	}
}
