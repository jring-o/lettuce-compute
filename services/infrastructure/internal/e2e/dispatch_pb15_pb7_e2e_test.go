//go:build integration

package e2e_test

// PB-15 / PB-7 end-to-end regression tests (Phase 3 local campaign): the
// user-visible halves of the flush-window races, driven through the real gRPC
// service against real Postgres. Differential: this file uses only pre-fix test
// helpers, so it can be dropped onto the pre-fix tree and demonstrably FAILS there.

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/server"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// TestDispatchCache_StartWorkBeforeFlush_SpotCheck is the spot-check variant of the
// Major-3 flush-race guard (PB-15): a spot-checked hand-out's copy write rides the
// SEPARATE spot-check queue, which StartWork's forced flush never drained — so with
// the flush ticker out of the picture a volunteer that run-starts immediately after
// the hand-out (a warm-cache volunteer in production) was denied EVERY time with
// "work unit no longer reserved for this volunteer" and dropped the unit.
//
// The spot-check decision is probabilistic and its configured rate is capped at 20%,
// so the test sweeps 40 units, run-starting each immediately after its hand-out:
// every spot-checked hand-out among them (P(none) = 0.8^40 ≈ 0.01%) exercised the
// race, and pre-fix any one of them fails the run-start.
func TestDispatchCache_StartWorkBeforeFlush_SpotCheck(t *testing.T) {
	// Flush interval 10 minutes: the periodic flush cannot land the spot-check copy;
	// only StartWork's forced flush can.
	env, cleanup := setupHeadsLeafsServerWithCacheCfg(t, server.HeadDispatchConfig{FlushIntervalMs: 600000})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const unitCount = 40

	userID := createTestUser(t, env.pool, ctx, "cache-spotcheck-flushrace")
	opts := hlDefaultLeafOpts("Cache SpotCheck FlushRace Leaf")
	// Redundancy-1 leaf with spot-check at the 20% config ceiling: each first hold
	// rolls the spot-check die; a marked unit routes its reservation write to the
	// spot-check queue.
	opts.ValConfig = leaf.ValidationConfig{
		RedundancyFactor:    1,
		AgreementThreshold:  1.0,
		ComparisonMode:      "EXACT",
		MaxRetries:          3,
		SpotCheckEnabled:    true,
		SpotCheckPercentage: 20,
	}
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, unitCount)

	// Several volunteers round-robin the sweep: the cache's in-memory per-machine
	// in-flight counter is reconciled only on the 30s tick, so a single volunteer
	// would stall at the in-flight cap long before covering all units.
	const volunteers = 5
	pubKeys := make([][]byte, volunteers)
	volIDs := make([]string, volunteers)
	for i := 0; i < volunteers; i++ {
		pubKeys[i] = genVolunteerKey(t)
		volIDs[i] = registerHLVolunteer(t, env, ctx, pubKeys[i], "cache-spotcheck-vol")
	}

	startedUnits := make(map[string]bool)
	attempts := 0
	deadline := time.Now().Add(60 * time.Second)
	for len(startedUnits) < unitCount && time.Now().Before(deadline) {
		i := attempts % volunteers
		attempts++
		pubKey, volID := pubKeys[i], volIDs[i]
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (cache): %v", err)
		}
		if len(resp.Assignments) == 0 {
			if attempts%volunteers == 0 {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		wuID := resp.Assignments[0].WorkUnitId

		// StartWork immediately after hand-out — the warm-cache volunteer's ~5ms
		// run-start, inside the flush window. It must succeed for EVERY copy,
		// spot-checked or not: the forced flush must land the copy row (normal OR
		// spot-check queue) before Assign reads it.
		swResp, err := env.grpc.StartWork(signFor(t, ctx, pubKey), &lettucev1.StartWorkRequest{
			WorkUnitId: wuID, VolunteerId: volID,
		})
		if err != nil {
			t.Fatalf("StartWork in flush window (unit %s): %v", wuID, err)
		}
		if !swResp.Ok {
			var spot bool
			_ = env.pool.QueryRow(ctx, "SELECT spot_check FROM work_units WHERE id = $1", wuID).Scan(&spot)
			t.Fatalf("StartWork inside the flush window returned ok=false (%q, unit %s, spot_check=%v): the volunteer drops the unit — PB-15 (spot-check variant)", swResp.Message, wuID, spot)
		}
		startedUnits[wuID] = true

		// Submit so the RUNNING copy closes and frees the volunteer's in-flight slot.
		out := []byte(`{"result":"spotcheck_flushrace_ok"}`)
		if _, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuID, VolunteerId: volID, PublicKey: pubKey,
			OutputData: out, OutputChecksumSha256: sha256Hex(out),
			Metadata: &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1},
		}); err != nil {
			t.Fatalf("SubmitResult (unit %s): %v", wuID, err)
		}
	}
	if len(startedUnits) != unitCount {
		t.Fatalf("run-started %d/%d distinct units before the deadline", len(startedUnits), unitCount)
	}

	// Prove the sweep exercised the spot-check path: at least one unit must have
	// been marked spot-check (the forced flush lands MarkSpotCheck, so the flag is
	// visible despite the parked ticker). P(none of 40) ≈ 0.01%.
	var spotChecked int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_units WHERE leaf_id = $1 AND spot_check = true", lf.ID).Scan(&spotChecked); err != nil {
		t.Fatalf("count spot-checked units: %v", err)
	}
	if spotChecked == 0 {
		t.Fatal("no unit was spot-checked across the sweep; the spot-check race was not exercised (statistically ~impossible at 20% over 40 units)")
	}
	t.Logf("run-started %d units, %d spot-checked, all inside the flush window", len(startedUnits), spotChecked)
}

// TestAbandonBufferedCopy_InFlushWindow_ReleasesCleanly reproduces PB-7: a prepare
// that fails fast (bad artifact URL, netguard refusal — observed live at hand-out
// + ~5ms) makes the volunteer abandon the BUFFERED, never-started unit while its
// reservation write is still in the flush window. Pre-fix the head answered
// FailedPrecondition "no live copy found for this volunteer and work unit", kept the
// in-memory hold AND the queued write, and the orphaned RESERVED row it later
// flushed churned the unit for ~a reservation window before it could redispatch —
// failed-prepare units cycled every ~1.5 minutes instead of releasing instantly.
func TestAbandonBufferedCopy_InFlushWindow_ReleasesCleanly(t *testing.T) {
	// Flush interval 10 minutes: the abandon arrives strictly inside the flush
	// window, exactly like a fast prepare failure.
	env, cleanup := setupHeadsLeafsServerWithCacheCfg(t, server.HeadDispatchConfig{FlushIntervalMs: 600000})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "cache-buffered-abandon")
	lf := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Cache Buffered Abandon Leaf"))
	generateLeafWUs(t, env, lf.ID, 2)

	pubKeyA := genVolunteerKey(t)
	volA := registerHLVolunteer(t, env, ctx, pubKeyA, "cache-abandon-volA")

	var wuID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKeyA), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volA,
			PublicKey:      pubKeyA,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 1,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (cache): %v", err)
		}
		if len(resp.Assignments) > 0 {
			wuID = resp.Assignments[0].WorkUnitId
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if wuID == "" {
		t.Fatal("cache never served a work unit")
	}

	// Abandon the buffered (never-started) unit immediately — the fast
	// prepare-failure path. The head must release it cleanly, not refuse.
	abResp, err := env.grpc.AbandonWorkUnit(signFor(t, ctx, pubKeyA), &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wuID,
		VolunteerId: volA,
		Reason:      "prepare failed (regression: PB-7 buffered-copy abandon)",
	})
	if err != nil {
		t.Fatalf("AbandonWorkUnit for a buffered copy inside the flush window was refused (%v): failed-prepare units churn instead of releasing (PB-7)", err)
	}
	if !abResp.Requeued {
		t.Fatalf("AbandonWorkUnit response not requeued: %s", abResp.Message)
	}

	// No live copy may remain for volA — neither now nor later (a late flush must
	// not resurrect an orphaned RESERVED row for the abandoned hold).
	var liveRows int
	env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE work_unit_id = $1 AND volunteer_id = $2 AND outcome IS NULL",
		wuID, volA).Scan(&liveRows)
	if liveRows != 0 {
		t.Fatalf("live copy rows for volA after buffered abandon = %d, want 0", liveRows)
	}

	// The unit must redispatch promptly to a DIFFERENT volunteer, who can run-start
	// it — the clean release the abandon is for.
	pubKeyB := genVolunteerKey(t)
	volB := registerHLVolunteer(t, env, ctx, pubKeyB, "cache-abandon-volB")
	got := ""
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, rerr := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKeyB), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volB,
			PublicKey:      pubKeyB,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 2,
		})
		if rerr != nil {
			t.Fatalf("RequestWorkUnit volB: %v", rerr)
		}
		for _, a := range resp.Assignments {
			if a.WorkUnitId == wuID {
				got = a.WorkUnitId
			}
		}
		if got != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got == "" {
		t.Fatal("abandoned unit was not redispatched to a fresh volunteer within 15s")
	}
	swResp, err := env.grpc.StartWork(signFor(t, ctx, pubKeyB), &lettucev1.StartWorkRequest{
		WorkUnitId: wuID, VolunteerId: volB,
	})
	if err != nil {
		t.Fatalf("StartWork volB after redispatch: %v", err)
	}
	if !swResp.Ok {
		t.Fatalf("StartWork volB after redispatch returned ok=false (%q)", swResp.Message)
	}
}
