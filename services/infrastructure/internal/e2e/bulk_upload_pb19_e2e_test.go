//go:build integration

package e2e_test

// PB-19 regression (Phase 3 local campaign): bulk upload on a never-configured
// CUSTOM leaf. Bulk upload is explicitly allowed in CONFIGURING, but a leaf whose
// config was never PUT carries a zero FaultToleranceConfig, and the handler stamped
// its raw MaxReassignments (0) onto every row — failing the schema's >= 1 check and
// surfacing as a raw 500 INTERNAL_ERROR "bulk create work units" to an operator
// following the documented create → configure-transition → bulk flow. Differential:
// pre-fix this test fails with the 500; post-fix the upload succeeds with the
// documented defaults stamped.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

func TestBulkUpload_NeverConfiguredLeaf_Succeeds(t *testing.T) {
	env, cleanup := setupHeadsLeafsServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "bulk-noconfig")

	// Create a CUSTOM leaf and transition it to CONFIGURING — WITHOUT any config
	// PUT, exactly the documented minimal flow the campaign probe followed.
	createReq := leaf.CreateLeafRequest{
		Name:         "Bulk NoConfig Leaf",
		Description:  "PB-19 regression: bulk upload before any config PUT",
		ResearchArea: []string{"testing"},
		TaskPattern:  leaf.PatternCustom,
		Visibility:   leaf.VisibilityPublic,
		CreatorID:    &userID,
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs", createReq)
	requireStatus(t, resp, http.StatusCreated, "create leaf")
	var lf leaf.Leaf
	decodeJSON(t, resp, &lf)
	leafURL := env.httpURL + "/api/v1/leafs/" + lf.ID.String()

	resp = httpReq(t, "POST", leafURL+"/configure", nil)
	requireStatus(t, resp, http.StatusOK, "configure transition")
	resp.Body.Close()

	// A small, valid bulk body. Pre-fix: 500 INTERNAL_ERROR "bulk create work
	// units" (max_reassignments 0 vs the schema's >= 1 check). Post-fix: 201 with
	// the documented defaults stamped.
	bulkReq := map[string]any{
		"work_units": []map[string]any{
			{"parameters": map[string]any{"x": 1}},
			{"parameters": map[string]any{"x": 2}},
		},
	}
	resp = httpReq(t, "POST", leafURL+"/work-units/bulk", bulkReq)
	if resp.StatusCode != http.StatusCreated {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		t.Fatalf("bulk upload on a never-configured CONFIGURING leaf = %d (body %v), want 201: the documented create → configure-transition → bulk flow must not 500 (PB-19)", resp.StatusCode, body)
	}
	var bulkResp struct {
		WorkUnitsCreated int `json:"work_units_created"`
	}
	decodeJSON(t, resp, &bulkResp)
	if bulkResp.WorkUnitsCreated != 2 {
		t.Fatalf("work_units_created = %d, want 2", bulkResp.WorkUnitsCreated)
	}

	// The stamped rows must carry the DOCUMENTED defaults (the same values a config
	// PUT would apply), not raw zeros.
	var maxReassignments, deadlineSeconds int
	if err := env.pool.QueryRow(ctx,
		"SELECT max_reassignments, deadline_seconds FROM work_units WHERE leaf_id = $1 LIMIT 1",
		lf.ID).Scan(&maxReassignments, &deadlineSeconds); err != nil {
		t.Fatalf("query stamped rows: %v", err)
	}
	if maxReassignments < 1 {
		t.Fatalf("stamped max_reassignments = %d, want the documented default (>= 1)", maxReassignments)
	}
	if deadlineSeconds <= 0 {
		t.Fatalf("stamped deadline_seconds = %d, want > 0 (deadline_multiplier default applied)", deadlineSeconds)
	}
}
