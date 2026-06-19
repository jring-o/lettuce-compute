//go:build integration

package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// --- Scenario 1: Multi-Leaf Activation ---
// Verifies that multiple leafs can be active simultaneously, all serve WUs,
// GET /api/v1/head returns correct data, and deprecated /api/v1/projects alias works.

func TestHeadsLeafsE2E_MultiLeafActivation(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "hl-mla")

	// Create 3 leafs with different research areas.
	leafA := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name: "Multi Leaf A", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: defaultExecConfig(),
		ValConfig: defaultHLValConfig(), FTConfig: defaultFTConfig(),
		DataConfig: defaultDataConfig(), CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	leafB := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name: "Multi Leaf B", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: defaultExecConfig(),
		ValConfig: defaultHLValConfig(), FTConfig: defaultFTConfig(),
		DataConfig: defaultDataConfig(), CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})
	leafC := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name: "Multi Leaf C", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: defaultExecConfig(),
		ValConfig: defaultHLValConfig(), FTConfig: defaultFTConfig(),
		DataConfig: defaultDataConfig(), CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Generate 10 WUs for each leaf.
	generateLeafWUs(t, env, leafA.ID, 10)
	generateLeafWUs(t, env, leafB.ID, 10)
	generateLeafWUs(t, env, leafC.ID, 10)

	// Verify GET /api/v1/head returns correct data.
	headInfo := getHeadInfo(t, env)
	if headInfo.Name != "test-head" {
		t.Errorf("head name = %q, want %q", headInfo.Name, "test-head")
	}
	if len(headInfo.Leafs) != 3 {
		t.Fatalf("head leafs count = %d, want 3", len(headInfo.Leafs))
	}

	leafIDs := map[string]bool{
		leafA.ID.String(): false,
		leafB.ID.String(): false,
		leafC.ID.String(): false,
	}
	for _, li := range headInfo.Leafs {
		if li.State != "ACTIVE" {
			t.Errorf("leaf %s state = %q, want ACTIVE", li.Name, li.State)
		}
		if li.QueuedWorkUnits != 10 {
			t.Errorf("leaf %s queued_work_units = %d, want 10", li.Name, li.QueuedWorkUnits)
		}
		leafIDs[li.ID] = true
	}
	for id, found := range leafIDs {
		if !found {
			t.Errorf("leaf %s not found in head info", id)
		}
	}

	// Verify default_leaf_weights is populated.
	if len(headInfo.DefaultLeafWeights) == 0 {
		t.Error("default_leaf_weights is empty")
	}

	// Register a volunteer and request WUs from all leafs.
	volPubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPubKey, "HL Multi Leaf Vol")

	// Client-side rotation: request from each leaf in turn (matches real client behavior).
	singleLeafIDs := [][]string{
		{leafA.ID.String()},
		{leafB.ID.String()},
		{leafC.ID.String()},
		{leafA.ID.String()},
		{leafB.ID.String()},
		{leafC.ID.String()},
	}
	leafHits := map[string]int{}

	for i := 0; i < 6; i++ {
		wuResp := requestWUFromLeafs(t, env, ctx, volID, volPubKey, singleLeafIDs[i])
		leafHits[effectiveLeafID(wuResp)]++
		outputData := []byte(fmt.Sprintf(`{"result": "multi-leaf-%d"}`, i))
		submitWUResult(t, env, ctx, volID, volPubKey, wuResp.WorkUnitId, outputData)
	}

	// Verify WUs came from all 3 leafs (2 each).
	if len(leafHits) < 3 {
		t.Errorf("WUs came from only %d leaf(s), want 3; hits=%v", len(leafHits), leafHits)
	}

	// Verify deprecated GET /api/v1/projects still works.
	resp := httpReq(t, "GET", env.httpURL+"/api/v1/projects", nil)
	requireStatus(t, resp, http.StatusOK, "deprecated projects alias")
	resp.Body.Close()
}

// --- Scenario 2: Weighted Leaf Distribution ---
// Tests that leaf_ids filter in RequestWorkUnit distributes WUs according to volunteer selection.

func TestHeadsLeafsE2E_WeightedLeafDistribution(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "hl-wld")

	leafHeavy := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Weighted Heavy"))
	leafLight := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Weighted Light"))

	generateLeafWUs(t, env, leafHeavy.ID, 50)
	generateLeafWUs(t, env, leafLight.ID, 50)

	volPubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPubKey, "HL Weighted Vol")

	// Simulate client-side weighted selection: 70% heavy, 30% light.
	heavyCount := 0
	lightCount := 0
	totalRequests := 40

	for i := 0; i < totalRequests; i++ {
		// Client-side weight selection: first 70% go to heavy, rest to light.
		var targetLeafIDs []string
		if i%10 < 7 {
			targetLeafIDs = []string{leafHeavy.ID.String()}
		} else {
			targetLeafIDs = []string{leafLight.ID.String()}
		}

		wuResp := requestWUFromLeafs(t, env, ctx, volID, volPubKey, targetLeafIDs)

		if effectiveLeafID(wuResp) == leafHeavy.ID.String() {
			heavyCount++
		} else if effectiveLeafID(wuResp) == leafLight.ID.String() {
			lightCount++
		}

		outputData := []byte(fmt.Sprintf(`{"result": "weighted-%d"}`, i))
		submitWUResult(t, env, ctx, volID, volPubKey, wuResp.WorkUnitId, outputData)
	}

	// Verify approximate 70/30 distribution.
	expectedHeavy := float64(totalRequests) * 0.7
	expectedLight := float64(totalRequests) * 0.3

	chi2 := chiSquared(
		[]float64{float64(heavyCount), float64(lightCount)},
		[]float64{expectedHeavy, expectedLight},
	)

	// Chi-squared threshold for p > 0.01 with df=1: 6.635
	if chi2 > 6.635 {
		t.Errorf("distribution chi2 = %.2f (> 6.635), heavy=%d light=%d (expected ~%.0f/%.0f)",
			chi2, heavyCount, lightCount, expectedHeavy, expectedLight)
	}

	// Verify both leafs received some WUs.
	if heavyCount == 0 {
		t.Error("heavy leaf received 0 WUs (starvation)")
	}
	if lightCount == 0 {
		t.Error("light leaf received 0 WUs (starvation)")
	}

	t.Logf("Distribution: heavy=%d (%.1f%%), light=%d (%.1f%%), chi2=%.2f",
		heavyCount, float64(heavyCount)/float64(totalRequests)*100,
		lightCount, float64(lightCount)/float64(totalRequests)*100, chi2)
}

// --- Scenario 3: Concurrent Execution ---
// Tests that a volunteer can hold and complete multiple WUs from different leafs concurrently.

func TestHeadsLeafsE2E_ConcurrentExecution(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "hl-ce")

	leafX := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Concurrent X"))
	leafY := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Concurrent Y"))
	leafZ := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Concurrent Z"))

	generateLeafWUs(t, env, leafX.ID, 5)
	generateLeafWUs(t, env, leafY.ID, 5)
	generateLeafWUs(t, env, leafZ.ID, 5)

	volPubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPubKey, "HL Concurrent Vol")

	// Request 1 WU from each leaf (client-side selection) without submitting.
	perLeafIDs := [][]string{
		{leafX.ID.String()},
		{leafY.ID.String()},
		{leafZ.ID.String()},
	}
	wuResps := make([]*lettucev1.WorkUnitAssignment, 3)
	leafsHit := map[string]bool{}

	for i := 0; i < 3; i++ {
		wuResps[i] = requestWUFromLeafs(t, env, ctx, volID, volPubKey, perLeafIDs[i])
		leafsHit[effectiveLeafID(wuResps[i])] = true
	}

	// Verify all 3 different leafs represented.
	if len(leafsHit) < 3 {
		t.Errorf("concurrent WUs from only %d leaf(s), want 3", len(leafsHit))
	}

	// Submit all 3 results.
	for i, wuResp := range wuResps {
		outputData := []byte(fmt.Sprintf(`{"result": "concurrent-%d"}`, i))
		submitWUResult(t, env, ctx, volID, volPubKey, wuResp.WorkUnitId, outputData)
	}

	// Verify all 3 credited.
	volIDParsed := types.MustParseID(volID)
	var totalCreditEntries int
	err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE volunteer_id = $1", volIDParsed,
	).Scan(&totalCreditEntries)
	if err != nil {
		t.Fatalf("query credit: %v", err)
	}
	if totalCreditEntries < 3 {
		t.Errorf("credit entries = %d, want >= 3", totalCreditEntries)
	}
}

// --- Scenario 4: Pre-Fetch Queue with Deadline-on-Fetch ---
// Tests that multiple WUs can be assigned to the same volunteer simultaneously.

func TestHeadsLeafsE2E_PreFetchDeadline(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "hl-pfd")

	lf := createHLLeaf(t, env, ctx, userID, hlLeafOpts{
		Name: "PreFetch Leaf", TaskPattern: leaf.PatternParameterSweep,
		ExecConfig: defaultExecConfig(),
		ValConfig: defaultHLValConfig(),
		FTConfig: leaf.FaultToleranceConfig{
			HeartbeatIntervalSeconds:  60,
			MissedHeartbeatsThreshold: 3,
			DeadlineMultiplier:        3.0,
			MaxReassignments:          3,
		},
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	generateLeafWUs(t, env, lf.ID, 10)

	volPubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPubKey, "HL PreFetch Vol")

	leafIDs := []string{lf.ID.String()}

	// Request 4 WUs without submitting (simulating pre-fetch).
	wuResps := make([]*lettucev1.WorkUnitAssignment, 4)
	for i := 0; i < 4; i++ {
		wuResps[i] = requestWUFromLeafs(t, env, ctx, volID, volPubKey, leafIDs)
		if wuResps[i].DeadlineSeconds <= 0 {
			t.Errorf("WU %d deadline_seconds = %d, want > 0", i, wuResps[i].DeadlineSeconds)
		}
	}

	// Verify all 4 WUs are assigned (different IDs).
	wuIDs := map[string]bool{}
	for _, resp := range wuResps {
		if wuIDs[resp.WorkUnitId] {
			t.Errorf("duplicate WU ID: %s", resp.WorkUnitId)
		}
		wuIDs[resp.WorkUnitId] = true
	}
	if len(wuIDs) != 4 {
		t.Errorf("unique WU IDs = %d, want 4", len(wuIDs))
	}

	// Under Layer 1, a dispatched (batched) work unit is LEASED, not assigned: it stays
	// QUEUED with a live RESERVED copy held by this volunteer (the deadline/heartbeat
	// clock starts only at run-start). Per-copy model (migration 00006): the hold is a
	// work_unit_assignment_history row (outcome IS NULL, started_at IS NULL), not the
	// retired reserved_volunteer_id column. Verify the reserved copy and the lease expiry.
	for _, resp := range wuResps {
		var state string
		var reservedVol *string
		err := env.pool.QueryRow(ctx,
			`SELECT wu.state,
			        (SELECT h.volunteer_id::text FROM work_unit_assignment_history h
			         WHERE h.work_unit_id = wu.id AND h.outcome IS NULL AND h.started_at IS NULL
			         ORDER BY h.assigned_at DESC LIMIT 1)
			 FROM work_units wu WHERE wu.id = $1`,
			types.MustParseID(resp.WorkUnitId),
		).Scan(&state, &reservedVol)
		if err != nil {
			t.Fatalf("query WU state: %v", err)
		}
		if state != "QUEUED" {
			t.Errorf("WU %s state = %q, want QUEUED (reserved)", resp.WorkUnitId, state)
		}
		if reservedVol == nil || *reservedVol != volID {
			t.Errorf("WU %s reserved copy volunteer = %v, want %s", resp.WorkUnitId, reservedVol, volID)
		}
		if resp.ReservedUntilUnix <= 0 {
			t.Errorf("WU %s reserved_until_unix = %d, want > 0", resp.WorkUnitId, resp.ReservedUntilUnix)
		}
	}

	// Submit results for all 4.
	for i, resp := range wuResps {
		outputData := []byte(fmt.Sprintf(`{"result": "prefetch-%d"}`, i))
		submitWUResult(t, env, ctx, volID, volPubKey, resp.WorkUnitId, outputData)
	}

	// Verify all 4 credited.
	volIDParsed := types.MustParseID(volID)
	assertCreditExists(t, env.pool, ctx, volIDParsed, lf.ID, 4)

	// Request 1 more WU to verify queue still works.
	extraWU := requestWUFromLeafs(t, env, ctx, volID, volPubKey, leafIDs)
	outputData := []byte(`{"result": "prefetch-extra"}`)
	submitWUResult(t, env, ctx, volID, volPubKey, extraWU.WorkUnitId, outputData)
}

// --- Scenario 6: Leaf Enable/Disable ---
// Tests that leaf_ids filter correctly excludes leafs from WU assignment.

func TestHeadsLeafsE2E_LeafEnableDisable(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "hl-led")

	leafA := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Enable A"))
	leafB := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Enable B"))
	leafC := createHLLeaf(t, env, ctx, userID, hlDefaultLeafOpts("Enable C"))

	generateLeafWUs(t, env, leafA.ID, 20)
	generateLeafWUs(t, env, leafB.ID, 20)
	generateLeafWUs(t, env, leafC.ID, 20)

	volPubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, volPubKey, "HL Enable Vol")

	// Phase 1: Client-side rotation between A and B only (C excluded).
	leafHits := map[string]int{}
	abIDs := []string{leafA.ID.String(), leafB.ID.String()}

	for i := 0; i < 10; i++ {
		target := []string{abIDs[i%2]}
		wuResp := requestWUFromLeafs(t, env, ctx, volID, volPubKey, target)
		leafHits[effectiveLeafID(wuResp)]++
		outputData := []byte(fmt.Sprintf(`{"result": "phase1-%d"}`, i))
		submitWUResult(t, env, ctx, volID, volPubKey, wuResp.WorkUnitId, outputData)
	}

	// Verify zero WUs came from leafC.
	if leafHits[leafC.ID.String()] > 0 {
		t.Errorf("disabled leaf C received %d WUs, want 0", leafHits[leafC.ID.String()])
	}
	// Verify WUs came from both enabled leafs.
	if leafHits[leafA.ID.String()] == 0 {
		t.Error("leaf A received 0 WUs")
	}
	if leafHits[leafB.ID.String()] == 0 {
		t.Error("leaf B received 0 WUs")
	}

	// Phase 2: Client-side rotation across all 3 leafs (C now enabled).
	allIDs := []string{leafA.ID.String(), leafB.ID.String(), leafC.ID.String()}
	phase2Hits := map[string]int{}

	for i := 0; i < 9; i++ {
		target := []string{allIDs[i%3]}
		wuResp := requestWUFromLeafs(t, env, ctx, volID, volPubKey, target)
		phase2Hits[effectiveLeafID(wuResp)]++
		outputData := []byte(fmt.Sprintf(`{"result": "phase2-%d"}`, i))
		submitWUResult(t, env, ctx, volID, volPubKey, wuResp.WorkUnitId, outputData)
	}

	// Verify leafC now receives WUs.
	if phase2Hits[leafC.ID.String()] == 0 {
		t.Error("re-enabled leaf C received 0 WUs in phase 2")
	}

	// Verify total assignments are consistent across all phases.
	var totalAssigned int
	err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM work_unit_assignment_history WHERE volunteer_id = $1",
		types.MustParseID(volID),
	).Scan(&totalAssigned)
	if err != nil {
		t.Fatalf("query assignments: %v", err)
	}
	// Should be at least 19 (10 phase 1 + 9 phase 2).
	if totalAssigned < 19 {
		t.Errorf("total assignments = %d, want >= 19", totalAssigned)
	}

	t.Logf("Phase 1: A=%d, B=%d, C=%d | Phase 2: A=%d, B=%d, C=%d",
		leafHits[leafA.ID.String()], leafHits[leafB.ID.String()], leafHits[leafC.ID.String()],
		phase2Hits[leafA.ID.String()], phase2Hits[leafB.ID.String()], phase2Hits[leafC.ID.String()])
}

