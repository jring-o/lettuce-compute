//go:build integration

package credit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TestComputeVolunteerBreakdown_ByHost verifies Feature A: credit is broken down
// per machine (host) for an account whose work spans multiple hosts, plus an
// unattributed (nil host_id) bucket.
func TestComputeVolunteerBreakdown_ByHost(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, pool, "byhost")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	// Two hosts under one account, plus one unattributed result.
	host1 := types.NewID()
	host2 := types.NewID()
	mkHost := func(id types.ID, name string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO hosts (id, volunteer_id, display_name, hardware_capabilities, available_runtimes, is_active, last_seen_at)
			VALUES ($1, $2, $3, '{}'::jsonb, '{}'::text[], true, now())`,
			id, volID, name); err != nil {
			t.Fatalf("insert host %s: %v", name, err)
		}
	}
	mkHost(host1, "laptop")
	mkHost(host2, "desktop")

	now := time.Now().UTC()
	creditOnHost := func(hostID *types.ID, amount float64) {
		wuID := createTestWorkUnit(t, pool, leafID)
		resID := createTestResult(t, pool, wuID, volID,
			"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
		if _, err := pool.Exec(ctx, "UPDATE results SET host_id = $1 WHERE id = $2", hostID, resID); err != nil {
			t.Fatalf("attach host to result: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO credit_ledger (volunteer_id, leaf_id, work_unit_id, result_id, credit_amount, granted_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			volID, leafID, wuID, resID, amount, now); err != nil {
			t.Fatalf("insert credit: %v", err)
		}
	}
	creditOnHost(&host1, 2.0)
	creditOnHost(&host1, 1.0)
	creditOnHost(&host2, 4.0)
	creditOnHost(nil, 0.5) // unattributed (pre-host-split / per-account fallback)

	bd, err := ComputeVolunteerBreakdown(ctx, pool, volID)
	if err != nil {
		t.Fatalf("ComputeVolunteerBreakdown: %v", err)
	}

	if len(bd.ByHost) != 3 {
		t.Fatalf("by_host len = %d, want 3 (laptop, desktop, unattributed)", len(bd.ByHost))
	}

	byName := map[string]HostCredit{}
	var unattributed *HostCredit
	for i := range bd.ByHost {
		hc := bd.ByHost[i]
		if hc.HostID == nil {
			unattributed = &bd.ByHost[i]
			continue
		}
		byName[hc.Hostname] = hc
	}

	if got := byName["laptop"].Credit; got != 3.0 {
		t.Errorf("laptop credit = %v, want 3.0", got)
	}
	if got := byName["laptop"].WorkUnits; got != 2 {
		t.Errorf("laptop work_units = %d, want 2", got)
	}
	if got := byName["desktop"].Credit; got != 4.0 {
		t.Errorf("desktop credit = %v, want 4.0", got)
	}
	if byName["laptop"].LastSeen == nil {
		t.Error("laptop last_seen should be set from the hosts row")
	}
	if unattributed == nil {
		t.Fatal("expected an unattributed (nil host_id) bucket")
	}
	if unattributed.Credit != 0.5 {
		t.Errorf("unattributed credit = %v, want 0.5", unattributed.Credit)
	}
}

// TestComputeVolunteerBreakdown_ByHost_LabelsUnverifiedMetrics is a BG-06a item-3
// regression (fails on pre-fix code): the per-machine (by-host) breakdown surfaces
// volunteer-reported cpu/gpu-seconds, served nested in the VolunteerBreakdown
// envelope, so the producer must stamp the unverified-metrics provenance marker and
// it must survive to the JSON wire form.
func TestComputeVolunteerBreakdown_ByHost_LabelsUnverifiedMetrics(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, pool, "byhost-prov")
	leafID := createTestLeaf(t, pool, &userID)
	volID := createTestVolunteer(t, pool)

	host := types.NewID()
	if _, err := pool.Exec(ctx, `
		INSERT INTO hosts (id, volunteer_id, display_name, hardware_capabilities, available_runtimes, is_active, last_seen_at)
		VALUES ($1, $2, 'laptop', '{}'::jsonb, '{}'::text[], true, now())`,
		host, volID); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	wuID := createTestWorkUnit(t, pool, leafID)
	resID := createTestResult(t, pool, wuID, volID,
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	if _, err := pool.Exec(ctx, "UPDATE results SET host_id = $1 WHERE id = $2", host, resID); err != nil {
		t.Fatalf("attach host to result: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO credit_ledger (volunteer_id, leaf_id, work_unit_id, result_id, credit_amount, granted_at)
		VALUES ($1, $2, $3, $4, 2.0, now())`,
		volID, leafID, wuID, resID); err != nil {
		t.Fatalf("insert credit: %v", err)
	}

	bd, err := ComputeVolunteerBreakdown(ctx, pool, volID)
	if err != nil {
		t.Fatalf("ComputeVolunteerBreakdown: %v", err)
	}
	if len(bd.ByHost) == 0 {
		t.Fatal("expected at least one by-host row")
	}
	if bd.MetricsProvenance != MetricsProvenanceUnverified {
		t.Errorf("MetricsProvenance = %q, want %q", bd.MetricsProvenance, MetricsProvenanceUnverified)
	}
	b, err := json.Marshal(bd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), wantMarker) {
		t.Errorf("by-host breakdown JSON missing provenance marker %s\ngot: %s", wantMarker, b)
	}
}
