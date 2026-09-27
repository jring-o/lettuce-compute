package server

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/reliability"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// The per-machine in-flight cap scales with the machine: a host the reliability quota has
// proven may hold more than the flat cap, up to a ceiling of 2 copies per advertised CPU
// core and per GPU, one more copy per validated unit past the ramp. These tests drive the
// hand-out through the reliability store (the refresher), as a running head does.

// ceilingCache builds a quota-on cache (floor 2, flat cap 10) with nUnits redundancy-1 units
// staged on one native leaf, and the given host's reliability score loaded by the refresher.
func ceilingCache(t *testing.T, host types.ID, score float64, nUnits int) *dispatchCache {
	t.Helper()
	repo := &fakeReliabilityRepo{inputs: []reliability.BudgetInput{{HostID: host, Score: score}}}
	c, _, leafRepo := newQuotaCache(true, 2, 10, repo)
	c.refreshBudgetsOnce(context.Background())
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	for i := 0; i < nUnits; i++ {
		c.stageUnit(types.NewID(), leafID, 1, 0)
	}
	return c
}

// ceilingOpts is capableOpts for a machine with its own host id, advertising cores CPU cores.
func ceilingOpts(vol, host types.ID, cores int) workunit.AssignmentOptions {
	opts := capableOpts(vol, 10)
	opts.HostID = &host
	opts.MaxCPUCores = cores
	return opts
}

// TestHandOut_InflightCeiling_ProvenBigHostPassesFlatCap: a 32-core machine with a long
// validated record holds 2 x 32 = 64 copies, not the flat 10.
func TestHandOut_InflightCeiling_ProvenBigHostPassesFlatCap(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, host, 1000, 100)

	res, _ := c.HandOut(vol, ceilingOpts(vol, host, 32), 100)
	if len(res) != 64 {
		t.Fatalf("proven 32-core host hand-out = %d, want its ceiling 2*32 = 64", len(res))
	}

	// At the ceiling the empty reply names the cap with the machine's own figure.
	res, _, noWork := c.HandOutWithReason(vol, ceilingOpts(vol, host, 32), 10)
	if len(res) != 0 || noWork.reason != lettucev1.NoWorkReason_NO_WORK_REASON_INFLIGHT_CAP ||
		noWork.inflightCap != 64 || noWork.inflightHeld != 64 {
		t.Fatalf("at the ceiling: got %d units, reason %v, cap %d, held %d; want 0, INFLIGHT_CAP, 64, 64",
			len(res), noWork.reason, noWork.inflightCap, noWork.inflightHeld)
	}
}

// TestHandOut_InflightCeiling_EarnedPastFlatCap: past the ramp, each validated unit earns one
// more copy, so a big machine with a short record is between the flat cap and its ceiling.
func TestHandOut_InflightCeiling_EarnedPastFlatCap(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, host, reliability.DefaultRampUnits+7, 100)

	res, _ := c.HandOut(vol, ceilingOpts(vol, host, 32), 100)
	if len(res) != 17 {
		t.Fatalf("32-core host 7 units past the ramp: hand-out = %d, want 10 + 7 = 17", len(res))
	}
}

// TestHandOut_InflightCeiling_ColdBigHostStartsAtFloor: advertising many cores earns nothing
// by itself; a host with no record starts at the floor.
func TestHandOut_InflightCeiling_ColdBigHostStartsAtFloor(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, types.NewID(), 1000, 100) // the store knows some other host only

	res, _ := c.HandOut(vol, ceilingOpts(vol, host, 256), 100)
	if len(res) != 2 {
		t.Fatalf("cold 256-core host hand-out = %d, want the floor 2", len(res))
	}
}

// TestHandOut_InflightCeiling_SmallHostKeepsFlatCap: a machine whose ceiling would be below
// the flat cap keeps exactly the flat cap, however long its record.
func TestHandOut_InflightCeiling_SmallHostKeepsFlatCap(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, host, 1000, 100)

	res, _ := c.HandOut(vol, ceilingOpts(vol, host, 2), 100)
	if len(res) != 10 {
		t.Fatalf("proven 2-core host hand-out = %d, want the flat cap 10", len(res))
	}
}

// TestHandOut_InflightCeiling_FollowsAdvertisedCores: the ceiling is read from each request,
// so a machine that lowers its advertised cores is held to the lower ceiling at once.
func TestHandOut_InflightCeiling_FollowsAdvertisedCores(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, host, 1000, 100)

	if res, _ := c.HandOut(vol, ceilingOpts(vol, host, 16), 100); len(res) != 32 {
		t.Fatalf("16-core hand-out = %d, want 2*16 = 32", len(res))
	}
	// Now advertising 8 cores: ceiling 16, and it already holds 32.
	res, _, noWork := c.HandOutWithReason(vol, ceilingOpts(vol, host, 8), 100)
	if len(res) != 0 || noWork.inflightCap != 16 {
		t.Fatalf("after lowering to 8 cores: got %d units, cap %d; want 0 and the lower ceiling 16",
			len(res), noWork.inflightCap)
	}
}

// TestHandOut_InflightCeiling_CountsGPUs: each advertised GPU raises the ceiling like a core.
func TestHandOut_InflightCeiling_CountsGPUs(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, host, 1000, 100)

	opts := ceilingOpts(vol, host, 2)
	opts.HasGPU = true
	opts.GPUVendors = []string{"NVIDIA", "NVIDIA", "NVIDIA", "NVIDIA"}
	res, _ := c.HandOut(vol, opts, 100)
	if len(res) != 12 {
		t.Fatalf("2 cores + 4 GPUs hand-out = %d, want 2*(2+4) = 12", len(res))
	}
}

// TestHandOut_InflightCeiling_ProbationStaysAtFloor: a PROBATION account is pinned to the
// floor whatever its machine's ceiling and record.
func TestHandOut_InflightCeiling_ProbationStaysAtFloor(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	c := ceilingCache(t, host, 1000, 100)
	c.setStandingSnapshot(map[types.ID]standing.Entry{vol: probationEntry()})

	res, _ := c.HandOut(vol, ceilingOpts(vol, host, 32), 100)
	if len(res) != 2 {
		t.Fatalf("PROBATION 32-core host hand-out = %d, want the floor 2", len(res))
	}
}

// TestHandOut_InflightCeiling_QuotaOffKeepsFlatCap: with the reliability quota off every
// machine keeps the flat cap, as that switch promises.
func TestHandOut_InflightCeiling_QuotaOffKeepsFlatCap(t *testing.T) {
	vol, host := types.NewID(), types.NewID()
	repo := &fakeReliabilityRepo{inputs: []reliability.BudgetInput{{HostID: host, Score: 1000}}}
	c, _, leafRepo := newQuotaCache(false, 2, 10, repo)
	c.refreshBudgetsOnce(context.Background())
	leafID := types.NewID()
	c.warm(nativeLeaf(leafID, 1, false, 0), leafRepo)
	for i := 0; i < 100; i++ {
		c.stageUnit(types.NewID(), leafID, 1, 0)
	}

	res, _ := c.HandOut(vol, ceilingOpts(vol, host, 32), 100)
	if len(res) != 10 {
		t.Fatalf("quota off, 32-core host hand-out = %d, want the flat cap 10", len(res))
	}
}
