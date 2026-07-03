package server

import (
	"io"
	"log/slog"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/dispatchparity"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// This is the Go half of the dispatch-predicate parity suite. It drives the SHARED
// scenario table (internal/dispatchparity) through the in-memory hot-path predicate
// server.(*dispatchCache).eligibleLocked. The SQL half
// (internal/workunit/dispatch_predicate_parity_test.go) drives the SAME table
// through FindNextAssignable / FlushReservations / ReserveCopy. Because a single
// table feeds both, a change to the eligibility rule in either layer that is not
// mirrored in the other flips a shared expectation and fails here or there.
//
// This half needs no database: eligibleLocked reads only in-memory cache state, so
// each scenario is projected directly into a candidate + the cache's leaf / holder /
// inflight maps. It therefore runs as a plain unit test (no integration build tag),
// in the always-on "Infrastructure (Go)" CI check.
//
// How the shared table is consumed here: projectGo builds the candidate fields
// (effectiveRedundancy, dbActiveCount, contributors, benched) exactly as the cache's
// own refill path (FindDispatchableBatch -> fetchAndStage) would build them from the
// same DB rows the SQL half seeds — dbActiveCount = live copies + PENDING results,
// contributors = their distinct authors, benched = the requester iff its recent
// closed copy is a benching one. Everything else (leaf capabilities, hr-class pin,
// inflight counter, requester options) maps one-to-one onto the scenario primitives.

func newParityCache(t *testing.T) *dispatchCache {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// No deps needed: eligibleLocked never touches the DB (it reads peekLeaf from the
	// in-memory leafCache and the in-memory holder / inflight maps only).
	return newDispatchCache(dispatchCacheConfig{}, dispatchDeps{}, logger)
}

// projectGo translates one shared Scenario into the concrete inputs eligibleLocked
// consumes: it warms the leaf snapshot, builds the candidate, seeds the per-machine
// inflight counter, and returns the (volunteerID, hostKey, opts, candidate) tuple.
func projectGo(t *testing.T, c *dispatchCache, s dispatchparity.Scenario) (types.ID, types.ID, workunit.AssignmentOptions, candidate) {
	t.Helper()

	leafID := types.NewID()
	unitID := types.NewID()
	requester := types.NewID()

	// Requester's effective host key: the reported host id when present, else the
	// account id — exactly meterID / COALESCE(host_id, volunteer_id).
	var hostPtr *types.ID
	if s.ReportsHost {
		h := types.NewID()
		hostPtr = &h
	}
	hostKey := meterID(requester, hostPtr)

	// Leaf snapshot the capability + feasibility checks read.
	lf := &leaf.Leaf{
		ID: leafID,
		ExecutionConfig: leaf.ExecutionConfig{
			Runtime:     s.LeafRuntime,
			GPURequired: s.LeafGPURequired,
			MaxMemoryMB: s.LeafMaxMemoryMB,
			RscFpopsEst: s.LeafRscFpopsEst,
		},
		ResourceRequirements: leaf.ResourceRequirements{
			MinCPUCores: s.LeafMinCPUCores,
		},
	}
	c.leafMu.Lock()
	c.leafCache[leafID] = &cachedLeaf{leaf: lf, fetchedAt: c.now()}
	c.leafMu.Unlock()

	// Candidate as the cache's refill would have staged it.
	var hrClass *string
	if s.UnitHRClass != "" {
		cls := s.UnitHRClass
		hrClass = &cls
	}
	// dbActiveCount = live copies + PENDING results, mirroring
	// FindDispatchableBatch.active_assignments (all coverage, self and others).
	dbActive := s.OtherLiveCopies + s.OtherPendingResults
	contributors := map[types.ID]struct{}{}
	if s.SelfLiveCopy {
		dbActive++
		contributors[requester] = struct{}{}
	}
	if s.SelfPendingResult {
		dbActive++
		contributors[requester] = struct{}{}
	}
	// The other volunteers' identities never equal the requester, so they cannot
	// change the requester-membership checks; only the COUNT they contribute (already
	// in dbActive) matters. Distinct placeholders keep the set realistic.
	for i := 0; i < s.OtherLiveCopies+s.OtherPendingResults; i++ {
		contributors[types.NewID()] = struct{}{}
	}
	benched := map[types.ID]struct{}{}
	if s.Benched() {
		benched[requester] = struct{}{}
	}

	cand := candidate{
		unit: &workunit.WorkUnit{
			ID:              unitID,
			LeafID:          leafID,
			State:           workunit.WorkUnitStateQueued,
			DeadlineSeconds: s.DeadlineSeconds,
			HRClass:         hrClass,
		},
		effectiveRedundancy: s.TargetCopies,
		dbActiveCount:       dbActive,
		contributors:        contributors,
		benched:             benched,
	}

	if s.HostOtherInflight > 0 {
		c.inflight[hostKey] = s.HostOtherInflight
	}

	opts := workunit.AssignmentOptions{
		VolunteerID:             requester,
		MaxCPUCores:             s.RequesterMaxCPUCores,
		MaxMemoryMB:             s.RequesterMaxMemoryMB,
		MaxDiskMB:               1 << 40, // disk is analogous to CPU/memory; kept generous
		HasGPU:                  s.RequesterHasGPU,
		AvailableRuntimes:       s.RequesterRuntimes,
		MaxInflightPerVolunteer: s.MaxInflight,
		HRClass:                 s.RequesterHRClass,
		HostID:                  hostPtr,
		BenchmarkFPOPS:          s.RequesterBenchmarkFPOPS,
	}
	return requester, hostKey, opts, cand
}

// TestDispatchPredicateParity_Go asserts the in-memory hot-path predicate reaches
// the verdict the shared table expects for every scenario. A drift in eligibleLocked
// (a rule added, removed, or changed) that is not reflected in the shared table
// fails here; a drift that IS mirrored in the SQL layer but not here (or vice versa)
// fails because both halves consume the same table.
func TestDispatchPredicateParity_Go(t *testing.T) {
	for _, s := range dispatchparity.Scenarios() {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			c := newParityCache(t)
			requester, hostKey, opts, cand := projectGo(t, c, s)

			c.mu.Lock()
			got, reason := c.eligibleLocked(requester, hostKey, opts, cand)
			c.mu.Unlock()

			want := s.GoWant()
			if got != want {
				t.Fatalf("eligibleLocked verdict mismatch\n"+
					"  scenario:  %s (dimension %s)\n"+
					"  got:       eligible=%v (reason=%s)\n"+
					"  want:      eligible=%v\n"+
					"  divergence:%v\n"+
					"If you changed the dispatch eligibility rule, update the shared table in\n"+
					"internal/dispatchparity AND the SQL layer to match — the two must stay in lockstep.",
					s.Name, s.Dimension, got, reason, want, s.Divergence != nil)
			}
		})
	}
}

// TestDispatchPredicate_Go_InMemoryOnlyBranches pins the eligibleLocked branches that
// have NO SQL analog (they gate the cache's own not-yet-flushed in-memory holds), so
// they cannot ride in the shared cross-layer table. They are asserted here directly.
func TestDispatchPredicate_Go_InMemoryOnlyBranches(t *testing.T) {
	newCand := func(c *dispatchCache, redundancy, dbActive int) (types.ID, candidate) {
		leafID := types.NewID()
		lf := &leaf.Leaf{
			ID:                   leafID,
			ExecutionConfig:      leaf.ExecutionConfig{Runtime: leaf.RuntimeNative},
			ResourceRequirements: leaf.ResourceRequirements{MinCPUCores: 1},
		}
		c.leafMu.Lock()
		c.leafCache[leafID] = &cachedLeaf{leaf: lf, fetchedAt: c.now()}
		c.leafMu.Unlock()
		return leafID, candidate{
			unit:                &workunit.WorkUnit{ID: types.NewID(), LeafID: leafID, State: workunit.WorkUnitStateQueued},
			effectiveRedundancy: redundancy,
			dbActiveCount:       dbActive,
		}
	}
	opts := workunit.AssignmentOptions{
		MaxCPUCores:       4,
		MaxMemoryMB:       4096,
		MaxDiskMB:         1 << 40,
		AvailableRuntimes: []string{leaf.RuntimeNative},
		HRClass:           "unknown/unknown/unknown",
	}

	t.Run("self_held_via_in_memory_hold", func(t *testing.T) {
		c := newParityCache(t)
		requester := types.NewID()
		_, cand := newCand(c, 2, 0)
		o := opts
		o.VolunteerID = requester
		// The requester holds an as-yet-unflushed in-memory reservation on this unit.
		c.reservedInMem[cand.unit.ID] = map[types.ID]heldCopy{requester: {hostID: requester}}
		c.inflight[requester] = 1

		c.mu.Lock()
		ok, reason := c.eligibleLocked(requester, requester, o, cand)
		c.mu.Unlock()
		if ok || reason != rejectSelfHeld {
			t.Fatalf("want ineligible/self_held, got ok=%v reason=%s", ok, reason)
		}
	})

	t.Run("in_memory_holders_consume_redundancy_headroom", func(t *testing.T) {
		c := newParityCache(t)
		requester := types.NewID()
		other := types.NewID()
		_, cand := newCand(c, 2, 1) // one DB-active copy already
		o := opts
		o.VolunteerID = requester
		// Plus one distinct in-memory holder: 1 (db) + 1 (in-mem) == redundancy 2.
		c.reservedInMem[cand.unit.ID] = map[types.ID]heldCopy{other: {hostID: other}}

		c.mu.Lock()
		ok, reason := c.eligibleLocked(requester, requester, o, cand)
		c.mu.Unlock()
		if ok || reason != rejectRedundancyFull {
			t.Fatalf("want ineligible/redundancy_full (in-mem holder counts toward headroom), got ok=%v reason=%s", ok, reason)
		}
	})

	t.Run("leaf_not_cached_is_conservatively_skipped", func(t *testing.T) {
		c := newParityCache(t)
		requester := types.NewID()
		o := opts
		o.VolunteerID = requester
		// A candidate whose leaf was never warmed into the cache.
		cand := candidate{
			unit:                &workunit.WorkUnit{ID: types.NewID(), LeafID: types.NewID(), State: workunit.WorkUnitStateQueued},
			effectiveRedundancy: 2,
		}
		c.mu.Lock()
		ok, reason := c.eligibleLocked(requester, requester, o, cand)
		c.mu.Unlock()
		if ok || reason != rejectLeafNotCached {
			t.Fatalf("want ineligible/leaf_not_cached, got ok=%v reason=%s", ok, reason)
		}
	})
}
