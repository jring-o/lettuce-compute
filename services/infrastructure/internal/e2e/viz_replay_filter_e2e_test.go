//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
)

// TestVizReplay_VolunteerResultFiltering exercises the volunteer result browsing
// flow for F34-viz-replay: results can be filtered by volunteer_id at the
// infrastructure API level.
//
// Steps:
//  1. Create and activate a leaf.
//  2. Generate work units.
//  3. Register two volunteers, each submitting results for different work units.
//  4. Query GET /api/v1/leafs/{leaf_id}/results?volunteer_id={vol1} — only vol1 results.
//  5. Query GET /api/v1/leafs/{leaf_id}/results?volunteer_id={vol2} — only vol2 results.
//  6. Query without volunteer_id — all results returned.
func TestVizReplay_VolunteerResultFiltering(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, env.pool, ctx, "viz-filter")

	// --- Step 1: Create and activate a leaf ---
	lf := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name:        "viz-filter-test",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:  defaultExecConfig(),
		ValConfig: leaf.ValidationConfig{
			RedundancyFactor:   1,
			AgreementThreshold: 1.0,
			ComparisonMode:     "EXACT",
			MaxRetries:         3,
		},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	leafID := lf.ID.String()

	// --- Step 2: Generate work units (4 total: 2 per volunteer) ---
	genReq := struct {
		ParameterSpace map[string]interface{} `json:"parameter_space"`
	}{
		ParameterSpace: map[string]interface{}{
			"x": []interface{}{1.0, 2.0, 3.0, 4.0},
		},
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs/"+leafID+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate WUs")
	resp.Body.Close()

	// --- Step 3: Register two volunteers and submit results ---
	pub1 := genVolunteerKey(t)
	pub2 := genVolunteerKey(t)

	volID1 := registerBetaVolunteer(t, env, ctx, pub1, "filter-vol-1", nil)
	volID2 := registerBetaVolunteer(t, env, ctx, pub2, "filter-vol-2", nil)

	// Volunteer 1 submits 2 results.
	vol1Output1, _ := json.Marshal(map[string]interface{}{"volunteer": 1, "run": 1, "value": 42.0})
	vol1Output2, _ := json.Marshal(map[string]interface{}{"volunteer": 1, "run": 2, "value": 43.0})
	requestSubmitResult(t, env, ctx, volID1, pub1, vol1Output1)
	requestSubmitResult(t, env, ctx, volID1, pub1, vol1Output2)

	// Volunteer 2 submits 2 results.
	vol2Output1, _ := json.Marshal(map[string]interface{}{"volunteer": 2, "run": 1, "value": 99.0})
	vol2Output2, _ := json.Marshal(map[string]interface{}{"volunteer": 2, "run": 2, "value": 100.0})
	requestSubmitResult(t, env, ctx, volID2, pub2, vol2Output1)
	requestSubmitResult(t, env, ctx, volID2, pub2, vol2Output2)

	// --- Step 4: Query with volunteer_id = vol1 ---
	vol1URL := fmt.Sprintf("%s/api/v1/leafs/%s/results?volunteer_id=%s", env.httpURL, leafID, volID1)
	resp = httpReq(t, "GET", vol1URL, nil)
	requireStatus(t, resp, http.StatusOK, "list results for vol1")

	var vol1Results struct {
		Data []*result.Result `json:"data"`
	}
	decodeJSON(t, resp, &vol1Results)

	if len(vol1Results.Data) != 2 {
		t.Fatalf("vol1 results: got %d, want 2", len(vol1Results.Data))
	}
	for _, r := range vol1Results.Data {
		if r.VolunteerID.String() != volID1 {
			t.Errorf("vol1 filter returned result with volunteer_id=%s, want %s", r.VolunteerID, volID1)
		}
	}

	// --- Step 5: Query with volunteer_id = vol2 ---
	vol2URL := fmt.Sprintf("%s/api/v1/leafs/%s/results?volunteer_id=%s", env.httpURL, leafID, volID2)
	resp = httpReq(t, "GET", vol2URL, nil)
	requireStatus(t, resp, http.StatusOK, "list results for vol2")

	var vol2Results struct {
		Data []*result.Result `json:"data"`
	}
	decodeJSON(t, resp, &vol2Results)

	if len(vol2Results.Data) != 2 {
		t.Fatalf("vol2 results: got %d, want 2", len(vol2Results.Data))
	}
	for _, r := range vol2Results.Data {
		if r.VolunteerID.String() != volID2 {
			t.Errorf("vol2 filter returned result with volunteer_id=%s, want %s", r.VolunteerID, volID2)
		}
	}

	// --- Step 6: Query without volunteer_id — all results ---
	allURL := fmt.Sprintf("%s/api/v1/leafs/%s/results", env.httpURL, leafID)
	resp = httpReq(t, "GET", allURL, nil)
	requireStatus(t, resp, http.StatusOK, "list all results")

	var allResults struct {
		Data []*result.Result `json:"data"`
	}
	decodeJSON(t, resp, &allResults)

	if len(allResults.Data) != 4 {
		t.Fatalf("all results: got %d, want 4", len(allResults.Data))
	}

	// Count results per volunteer to verify completeness.
	vol1Count := 0
	vol2Count := 0
	for _, r := range allResults.Data {
		switch r.VolunteerID.String() {
		case volID1:
			vol1Count++
		case volID2:
			vol2Count++
		default:
			t.Errorf("unexpected volunteer_id in results: %s", r.VolunteerID)
		}
	}
	if vol1Count != 2 {
		t.Errorf("all results: vol1 count = %d, want 2", vol1Count)
	}
	if vol2Count != 2 {
		t.Errorf("all results: vol2 count = %d, want 2", vol2Count)
	}

	// --- Bonus: verify invalid volunteer_id returns 400 ---
	badURL := fmt.Sprintf("%s/api/v1/leafs/%s/results?volunteer_id=not-a-uuid", env.httpURL, leafID)
	resp = httpReq(t, "GET", badURL, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid volunteer_id: got status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	resp.Body.Close()

	t.Logf("PASS: volunteer result filtering — vol1 got 2, vol2 got 2, unfiltered got 4, invalid ID rejected")
}
