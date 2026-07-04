package server

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/dispatchparity"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/standing"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
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

// paritySubject derives the trust subject for a synthetic volunteer row carrying `did` at
// binding status `binding` (BindingNone leaves it unbound) through the PRODUCTION rule
// trust.SubjectForVolunteer — never re-implemented inline, so this projection stays honest
// against the SQL half, which recomputes the same subject in SQL. The dispatchparity binding
// constants ("OK"/"STALE"/"REVOKED"/"") are the volunteers.did_binding_status domain, so
// they map straight onto volunteer.DIDBindingStatus*.
func paritySubject(id types.ID, did, binding string) string {
	v := &volunteer.Volunteer{ID: id}
	if binding != dispatchparity.BindingNone {
		d := did
		st := binding
		v.DID = &d
		v.DIDBindingStatus = &st
	}
	return trust.SubjectForVolunteer(v)
}

// projectGo translates one shared Scenario into the concrete inputs eligibleLocked
// consumes: it warms the leaf snapshot, builds the candidate, seeds the per-machine
// inflight counter, and returns the (volunteerID, hostKey, opts, candidate) tuple. The
// distinctness dimension is projected by trust SUBJECT: every same-DID row shares one
// minted DID (so their subjects collide while the binding is live), and every subject is
// derived through the production trust functions rather than re-implemented here.
func projectGo(t *testing.T, c *dispatchCache, s dispatchparity.Scenario) (types.ID, types.ID, workunit.AssignmentOptions, candidate) {
	t.Helper()

	leafID := types.NewID()
	unitID := types.NewID()
	requester := types.NewID()

	// One DID minted per scenario, shared by every same-DID row; a second, distinct DID
	// stands in for the different-DID control.
	scenarioDID := "did:plc:" + types.NewID().String()
	otherDID := "did:plc:" + types.NewID().String()

	// The requester's subject, from its own binding status — exactly what RequestWorkUnit
	// puts into opts.TrustSubject in production.
	reqSubject := paritySubject(requester, scenarioDID, s.RequesterBinding)

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
	// dbActiveCount = live copies + PENDING results (all coverage, self and others),
	// mirroring FindDispatchableBatch.active_assignments; contributors = their distinct
	// trust SUBJECTS, keyed exactly as ContributorSubjects would be at refill.
	dbActive := s.OtherLiveCopies + s.OtherPendingResults
	contributors := map[string]struct{}{}
	// trustedContributors is the refill-time snapshot of contributor subjects that already
	// count TRUSTED toward the unit (DispatchCandidate.TrustedContributorSubjects), sizing
	// the trusted-corroborator reservation. It mirrors what the SQL refill emits: the first
	// TrustedOtherLiveCopies live-holder subjects and the first TrustedOtherPendingResults
	// pending-author subjects — a SUBSET of the contributors, so the two agree on which of
	// the covering copies are trusted, not merely on the count.
	trustedContributors := map[string]struct{}{}
	// Anonymous other contributors carry fresh per-keypair sentinel subjects; they never
	// equal the requester's, so only the COUNT they add (already in dbActive) matters. Live
	// copies and pending results are built in separate loops so the trusted subset can be
	// drawn from each exactly as the SQL trusted-present count sources them (live holders by
	// current score, pending authors by the submission-time stamp).
	for i := 0; i < s.OtherLiveCopies; i++ {
		subj := trust.SubjectForVolunteerID(types.NewID())
		contributors[subj] = struct{}{}
		if i < s.TrustedOtherLiveCopies {
			trustedContributors[subj] = struct{}{}
		}
	}
	for i := 0; i < s.OtherPendingResults; i++ {
		subj := trust.SubjectForVolunteerID(types.NewID())
		contributors[subj] = struct{}{}
		if i < s.TrustedOtherPendingResults {
			trustedContributors[subj] = struct{}{}
		}
	}
	// Probation-held coverage (account standing, BG-24b forced replication): live copies held
	// by — or PENDING results submitted by — OTHER, distinct NON-OK accounts. Each is a real
	// DB row, so it counts in the RAW seed (dbActiveCount) and is recorded as a distinct
	// contributor subject exactly as the refill's contributor_subjects (which has NO standing
	// filter) would record it — but its non-countable portion is accumulated into
	// probationCoverage, mirroring nonCountableCoverageSQL / DispatchCandidate.ProbationCoverage.
	// eligibleLocked's coverage bound subtracts probationCoverage, so these cover no redundancy.
	// Their subjects are fresh per-keypair sentinels: never the requester's (no distinctness
	// collision) and never trusted (a non-OK account cannot be trusted-present).
	probationCoverage := 0
	for i := 0; i < s.OtherProbationLiveCopies; i++ {
		dbActive++
		probationCoverage++
		contributors[trust.SubjectForVolunteerID(types.NewID())] = struct{}{}
	}
	for i := 0; i < s.OtherProbationPendingResults; i++ {
		dbActive++
		probationCoverage++
		contributors[trust.SubjectForVolunteerID(types.NewID())] = struct{}{}
	}
	if s.SelfLiveCopy {
		dbActive++
		contributors[reqSubject] = struct{}{}
	}
	if s.SelfPendingResult {
		dbActive++
		contributors[reqSubject] = struct{}{}
	}
	// A DIFFERENT volunteer row bound to the SAME DID: its subject collides with the
	// requester's while the binding is live (OK/STALE) and reverts to a sentinel when
	// REVOKED — so the exclusion fires or not exactly as the subject rule dictates. It
	// consumes one unit of redundancy headroom too, so it is added to dbActive.
	if s.OtherSameDIDLiveCopy {
		dbActive++
		contributors[paritySubject(types.NewID(), scenarioDID, s.OtherBinding)] = struct{}{}
	}
	if s.OtherSameDIDPendingResult {
		dbActive++
		contributors[paritySubject(types.NewID(), scenarioDID, s.OtherBinding)] = struct{}{}
	}
	// The different-DID control: a distinct bound principal, mutually eligible with the
	// requester. It too consumes one unit of headroom.
	if s.OtherDifferentDIDLiveCopy {
		dbActive++
		contributors[paritySubject(types.NewID(), otherDID, dispatchparity.BindingOK)] = struct{}{}
	}
	benched := map[types.ID]struct{}{}
	if s.Benched() {
		benched[requester] = struct{}{}
	}

	// Resolve the candidate's trusted-corroborator requirement (K) and floor through the
	// PRODUCTION resolver (transition.TrustPolicy.ResolveTrust) — never re-derived inline —
	// so this projection cannot drift from the SQL twin (effTrustKSQL / effTrustFloorSQL).
	// The clamp target is the effective min quorum, which in these scenarios == TargetCopies
	// (a leaf that sets only redundancy_factor resolves target == quorum).
	trustK, trustFloor := transition.TrustPolicy{
		GateEnabled:             s.TrustGateEnabled,
		DefaultMinCorroborators: s.TrustDefaultK,
		DefaultFloor:            s.TrustDefaultFloor,
	}.ResolveTrust(leaf.ValidationConfig{
		MinTrustedCorroborators: s.LeafTrustK,
		TrustFloor:              s.LeafTrustFloor,
	}, s.TargetCopies)

	// Install the requester's current trust score into the cache snapshot the reservation
	// reads (omit when 0 = no trust row = untrusted). eligibleLocked classifies the
	// requester TRUSTED iff this meets trustFloor, in which case it bypasses the reservation.
	if s.RequesterTrustScore > 0 {
		if c.trustScores == nil {
			c.trustScores = make(map[string]int)
		}
		c.trustScores[reqSubject] = s.RequesterTrustScore
	}

	// Requester account standing (BG-24b): install the requester's RAW standing (standing +
	// benched_until) into the TTL snapshot the BENCHED gate reads, built as a standing.Entry —
	// exactly what refreshStanding would load from the store. Effective standing is NEVER
	// hand-computed: eligibleLocked resolves the entry through volunteer.EffectiveStanding at
	// read time (effectiveStandingLocked), so a live bench reads BENCHED, an EXPIRED bench
	// reads PROBATION, and a stored PROBATION reads PROBATION. StandingOK ("") leaves the
	// requester ABSENT from the snapshot (it carries only the non-OK minority), so the gate is
	// inert. Only the REQUESTER's standing goes here; the probation-held OTHER copies above are
	// pre-counted DB rows carried by probationCoverage, not snapshot entries.
	if s.RequesterStanding != dispatchparity.StandingOK {
		var entry standing.Entry
		switch s.RequesterStanding {
		case dispatchparity.StandingBenched:
			var until *time.Time
			if s.RequesterBenchExpired {
				past := c.now().Add(-time.Hour) // expired bench => effective PROBATION
				until = &past
			}
			entry = standing.Entry{Standing: volunteer.StandingBenched, BenchedUntil: until}
		case dispatchparity.StandingProbation:
			entry = standing.Entry{Standing: volunteer.StandingProbation}
		}
		c.standingSnapshot = map[types.ID]standing.Entry{requester: entry}
		c.standingSnapshotAt = c.now()
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
		probationCoverage:   probationCoverage,
		contributors:        contributors,
		benched:             benched,
		effectiveTrustK:     trustK,
		effectiveTrustFloor: trustFloor,
		trustedContributors: trustedContributors,
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
		// The requester's resolved trust subject drives the per-principal distinctness
		// checks — the whole point of the subject dimension.
		TrustSubject: reqSubject,
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
		// The requester holds an as-yet-unflushed in-memory reservation on this unit. Its
		// hold carries the requester's own subject; with opts.TrustSubject unset the
		// predicate falls back to the same sentinel, so the self-held scan matches.
		c.reservedInMem[cand.unit.ID] = map[types.ID]heldCopy{
			requester: {hostID: requester, subject: trust.SubjectForVolunteerID(requester)},
		}
		c.inflight[requester] = 1

		c.mu.Lock()
		ok, reason := c.eligibleLocked(requester, requester, o, cand)
		c.mu.Unlock()
		if ok || reason != rejectSelfHeld {
			t.Fatalf("want ineligible/self_held, got ok=%v reason=%s", ok, reason)
		}
	})

	t.Run("self_held_via_same_subject_different_volunteer", func(t *testing.T) {
		c := newParityCache(t)
		requester := types.NewID()
		other := types.NewID()
		_, cand := newCand(c, 2, 0)
		o := opts
		o.VolunteerID = requester
		// A DIFFERENT account holds an in-memory copy, but under the SAME live DID as the
		// requester: one principal. The self-held check is per-SUBJECT, so the requester
		// must be refused even though no holder shares its volunteer id.
		sharedDID := "did:plc:" + types.NewID().String()
		o.TrustSubject = sharedDID
		c.reservedInMem[cand.unit.ID] = map[types.ID]heldCopy{
			other: {hostID: other, subject: sharedDID},
		}
		c.inflight[other] = 1

		c.mu.Lock()
		ok, reason := c.eligibleLocked(requester, requester, o, cand)
		c.mu.Unlock()
		if ok || reason != rejectSelfHeld {
			t.Fatalf("want ineligible/self_held (same subject, different volunteer id), got ok=%v reason=%s", ok, reason)
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
		c.reservedInMem[cand.unit.ID] = map[types.ID]heldCopy{
			other: {hostID: other, subject: trust.SubjectForVolunteerID(other)},
		}

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
