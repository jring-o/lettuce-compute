// Package dispatchparity holds the SINGLE, shared, table-driven definition of the
// work-unit dispatch eligibility scenarios that both dispatch predicate layers are
// checked against. It exists so a change to the eligibility rule in EITHER layer
// surfaces as a visible failure in a shared table rather than a silent behavioral
// drift between the two.
//
// The dispatch eligibility predicate — "may this requester be handed this QUEUED
// work unit right now?" — lives in FOUR hand-synchronized implementations:
//
//   - the in-memory Go hot path, server.(*dispatchCache).eligibleLocked, which
//     re-checks every per-requester rule against a cached candidate; and
//   - three SQL sites in workunit/pgx-repo.go: FindNextAssignable (the
//     authoritative read-side gate), FlushReservations (the batched hand-out
//     landing write), and ReserveCopy (the single-copy landing write).
//
// Nothing asserted that the four agree. A rule added to one could silently diverge
// from the others. This package is the shared spine of the parity tests that close
// that gap:
//
//   - internal/server/dispatch_predicate_parity_test.go projects each Scenario into
//     in-memory cache state and asserts eligibleLocked's verdict (a plain unit test,
//     always run); and
//   - internal/workunit/dispatch_predicate_parity_test.go seeds each Scenario into
//     Postgres and asserts FindNextAssignable's verdict, plus the subset of the rule
//     that FlushReservations / ReserveCopy re-enforce (an integration test).
//
// A Scenario carries ONLY primitive fields (ints, bools, strings, floats). It must
// not import the workunit or leaf packages: the workunit integration test lives in
// package workunit, so a dependency from this package onto workunit would be an
// import cycle. Each test translates the primitives into its own concrete state
// (workunit.AssignmentOptions, leaf.Leaf, DB rows).
//
// The predicate dimensions covered mirror the rule's structure exactly: redundancy
// headroom, self-held-copy exclusion, already-contributed exclusion, the
// post-failure cooldown ("benched"), the per-machine in-flight cap, homogeneous-
// redundancy hardware-class matching, runtime/capability fit, and
// feasibility-at-deadline — with boundary cases at each cap/limit edge.
package dispatchparity

// Dimension names the single predicate axis a Scenario primarily exercises. It
// drives two things: readable failure output, and which of the landing-write gates
// (FlushReservations / ReserveCopy) are expected to re-enforce the rule — those
// gates deliberately re-check only a subset (see EnforcedBy).
type Dimension string

const (
	DimBaseline          Dimension = "baseline"
	DimRedundancy        Dimension = "redundancy_headroom"
	DimSelfLiveCopy      Dimension = "self_held_copy"
	DimSelfPendingResult Dimension = "already_contributed"
	DimCooldown          Dimension = "post_failure_cooldown"
	DimInflightCap       Dimension = "per_machine_inflight_cap"
	DimHRClass           Dimension = "hr_class_match"
	DimCapability        Dimension = "runtime_capability"
	DimFeasibility       Dimension = "deadline_feasibility"
)

// CooldownState is the requester's most-recent CLOSED copy of the target unit — the
// history row that drives the post-failure cooldown. A copy benches the requester
// (gives it last refusal for ~one deadline so a fresh volunteer gets first crack)
// only when it is a genuine reliability signal: a copy that TIMED OUT (EXPIRED), or
// one the volunteer actually run-STARTED and then abandoned. A graceful return of
// un-started buffered work does not bench, nor does a failure older than the
// cooldown window.
type CooldownState int

const (
	// CooldownNone: no recent closed copy for the requester on this unit.
	CooldownNone CooldownState = iota
	// CooldownExpiredRecent: a copy that TIMED OUT within the cooldown window. Benches.
	CooldownExpiredRecent
	// CooldownStartedAbandon: a copy the requester run-started (started_at set) then
	// abandoned, within the window. Benches (a reliability signal).
	CooldownStartedAbandon
	// CooldownUnstartedAbandon: a graceful return of never-started buffered work
	// (ABANDONED, started_at NULL). Does NOT bench.
	CooldownUnstartedAbandon
	// CooldownExpiredStale: a timeout whose outcome_at is OLDER than the cooldown
	// window (roughly one deadline). Does NOT bench — the cooldown has elapsed.
	CooldownExpiredStale
)

// benches reports whether this cooldown state leaves the requester benched (last
// refusal) at hand-out time. This is the exact rule the SQL cooldown clauses and
// FindDispatchableBatch's benched snapshot implement; the Go projection uses it to
// build the candidate's benched set.
func (c CooldownState) benches() bool {
	return c == CooldownExpiredRecent || c == CooldownStartedAbandon
}

// Gate identifies one of the four predicate implementations under parity test.
type Gate int

const (
	// GateEligibleLocked is the in-memory Go hot-path predicate. It re-checks EVERY
	// dimension, so its expected verdict is always the scenario's full verdict.
	GateEligibleLocked Gate = iota
	// GateFindNextAssignable is the authoritative SQL read-side gate. It, too,
	// re-checks every dimension.
	GateFindNextAssignable
	// GateFlushReservations is the batched hand-out landing write. It re-checks the
	// per-volunteer distinctness / cooldown / feasibility rules AND redundancy
	// headroom, but delegates capability / hr-class / inflight-cap / leaf-filter to
	// the in-memory hand-out that produced the reservation.
	GateFlushReservations
	// GateReserveCopy is the single-copy landing write (spot-check landing +
	// ReserveNextAssignable's follow-up). It re-checks the per-volunteer distinctness
	// / cooldown / feasibility rules, but — unlike FlushReservations — does NOT
	// re-check redundancy headroom (it trusts FindNextAssignable, which its caller
	// ran first, to have gated that).
	GateReserveCopy
)

// LayerDivergence records a KNOWN, deliberate-for-now disagreement between the two
// full-predicate layers (eligibleLocked and FindNextAssignable) on a single input:
// the two are NOT equivalent functions there. The parity suite pins each layer to
// its CURRENT verdict so the suite stays green while documenting the drift in code;
// a behavior change on EITHER side flips a value here and trips the assertion,
// forcing a deliberate update. See the scenario's construction site for the
// analysis and the offending code lines.
type LayerDivergence struct {
	Go  bool   // eligibleLocked's current verdict for this scenario
	SQL bool   // FindNextAssignable's current verdict for this scenario
	Why string // one-line summary of the disagreement
}

// Scenario is one abstract dispatch-eligibility case. All fields are primitives so
// each layer's test can translate them into its own concrete state without this
// package depending on workunit / leaf.
type Scenario struct {
	Name      string
	Dimension Dimension

	// --- Target unit / leaf shape --------------------------------------------
	// TargetCopies is the leaf's effective redundancy (validation_config
	// redundancy_factor): how many distinct volunteers may each hold a copy.
	TargetCopies int
	// OtherLiveCopies is the number of live copies of the target unit already held
	// by OTHER, distinct volunteers (each a work_unit_assignment_history row with
	// outcome NULL). They consume redundancy headroom.
	OtherLiveCopies int
	// OtherPendingResults is the number of PENDING results other, distinct
	// volunteers have already submitted for the target unit. They too consume
	// redundancy headroom (a finished copy holds its slot via its result).
	OtherPendingResults int

	// --- This requester's relationship to the target unit --------------------
	// SelfLiveCopy: the requester already holds a live copy of the target unit.
	SelfLiveCopy bool
	// SelfPendingResult: the requester already authored a PENDING result for the
	// target unit (it has already contributed).
	SelfPendingResult bool
	// Cooldown: the requester's most-recent closed copy of the target unit.
	Cooldown CooldownState

	// --- Per-machine in-flight cap -------------------------------------------
	// MaxInflight is AssignmentOptions.MaxInflightPerVolunteer (0 = unlimited).
	MaxInflight int
	// HostOtherInflight is the number of live copies of OTHER units attributed to
	// the requester's machine (its effective host key). They count toward the
	// per-machine in-flight cap without touching the target unit's redundancy.
	HostOtherInflight int
	// ReportsHost: the requester reports a HostID, so the cap keys on host_id;
	// otherwise it keys on volunteer_id (COALESCE(host_id, volunteer_id)).
	ReportsHost bool

	// --- Homogeneous redundancy ----------------------------------------------
	// UnitHRClass is the hardware class the target unit is pinned to ("" = unpinned).
	UnitHRClass string
	// RequesterHRClass is the class the requester reports ("" = reports no class).
	RequesterHRClass string

	// --- Runtime / capability fit --------------------------------------------
	LeafRuntime          string   // leaf execution_config.runtime
	RequesterRuntimes    []string // runtimes the requester can execute
	LeafMinCPUCores      int
	RequesterMaxCPUCores int
	LeafMaxMemoryMB      int // leaf execution_config.max_memory_mb (the container limit)
	RequesterMaxMemoryMB int
	// LeafGPURequired maps to the leaf's execution_config.gpu_required flag (the
	// author-set flag; resource_requirements.gpu_required is left false, exercising
	// the "either flag gates presence" rule).
	LeafGPURequired bool
	RequesterHasGPU bool

	// --- Feasibility-at-deadline ---------------------------------------------
	// LeafRscFpopsEst is the leaf's per-unit floating-point-op estimate.
	LeafRscFpopsEst float64
	// RequesterBenchmarkFPOPS is the requester host's measured throughput (FP ops/s).
	RequesterBenchmarkFPOPS float64
	DeadlineSeconds         int

	// --- Expected verdict ----------------------------------------------------
	// Eligible is the verdict BOTH full-predicate layers are expected to reach. For
	// every scenario except a known divergence the two agree and this is the single
	// source of truth.
	Eligible bool
	// Divergence, when non-nil, marks a scenario on which the two layers are known
	// to disagree; each layer is then pinned to its own current verdict (see
	// LayerDivergence) and Eligible is ignored.
	Divergence *LayerDivergence
}

// GoWant returns the verdict eligibleLocked is expected to produce for this scenario.
func (s Scenario) GoWant() bool {
	if s.Divergence != nil {
		return s.Divergence.Go
	}
	return s.Eligible
}

// SQLWant returns the verdict FindNextAssignable is expected to produce.
func (s Scenario) SQLWant() bool {
	if s.Divergence != nil {
		return s.Divergence.SQL
	}
	return s.Eligible
}

// Benched reports whether the requester should be treated as benched (post-failure
// cooldown) in this scenario — the input the Go projection needs to build the
// candidate's benched set.
func (s Scenario) Benched() bool { return s.Cooldown.benches() }

// EnforcedBy reports whether gate g re-checks the predicate dimension this scenario
// exercises, and is therefore expected to reproduce the scenario's ineligibility.
// The two full-predicate gates enforce every dimension; the landing-write gates
// enforce only the per-volunteer distinctness / cooldown / feasibility rules (plus
// redundancy for FlushReservations), delegating the rest to the hand-out. A
// POSITIVE (eligible) scenario is admitted by every gate regardless — this only
// bounds which gates must REFUSE a given negative scenario.
func (s Scenario) EnforcedBy(g Gate) bool {
	switch g {
	case GateEligibleLocked, GateFindNextAssignable:
		return true
	case GateFlushReservations:
		switch s.Dimension {
		case DimRedundancy, DimSelfLiveCopy, DimSelfPendingResult, DimCooldown, DimFeasibility, DimBaseline:
			return true
		default: // capability, hr_class, inflight_cap — delegated to the hand-out
			return false
		}
	case GateReserveCopy:
		switch s.Dimension {
		// Note: redundancy is intentionally ABSENT — ReserveCopy does not re-check it.
		case DimSelfLiveCopy, DimSelfPendingResult, DimCooldown, DimFeasibility, DimBaseline:
			return true
		default:
			return false
		}
	}
	return false
}

// base returns a fully-eligible baseline scenario: an unpinned, resource-light
// NATIVE unit at redundancy 2 with no existing copies, no cooldown, no in-flight
// pressure, and feasibility skipped (no estimate). Each scenario in Scenarios()
// starts from this and perturbs exactly one dimension, so any resulting
// ineligibility is unambiguously attributable to that dimension. The requester
// reports a non-empty hardware class, matching production (HRClass() never yields
// an empty string).
func base() Scenario {
	return Scenario{
		TargetCopies:         2,
		LeafRuntime:          "NATIVE",
		RequesterRuntimes:    []string{"NATIVE"},
		LeafMinCPUCores:      1,
		RequesterMaxCPUCores: 4,
		LeafMaxMemoryMB:      512,
		RequesterMaxMemoryMB: 4096,
		RequesterHRClass:     "unknown/unknown/unknown",
		DeadlineSeconds:      3600,
		Eligible:             true,
		Dimension:            DimBaseline,
	}
}

// with applies a mutator to a fresh baseline scenario and stamps its name.
func with(name string, dim Dimension, mut func(*Scenario)) Scenario {
	s := base()
	s.Name = name
	s.Dimension = dim
	mut(&s)
	return s
}

// Scenarios returns the shared parity table. It is consumed verbatim by both the
// Go (eligibleLocked) and SQL (FindNextAssignable + FlushReservations +
// ReserveCopy) parity tests.
func Scenarios() []Scenario {
	return []Scenario{
		// --- baseline ---------------------------------------------------------
		with("baseline_eligible", DimBaseline, func(s *Scenario) {}),

		// --- redundancy headroom ---------------------------------------------
		with("redundancy_one_of_two_live_has_headroom", DimRedundancy, func(s *Scenario) {
			s.TargetCopies = 2
			s.OtherLiveCopies = 1 // 1 < 2 -> a second distinct copy still fits
			s.Eligible = true
		}),
		with("redundancy_two_of_two_live_full", DimRedundancy, func(s *Scenario) {
			s.TargetCopies = 2
			s.OtherLiveCopies = 2 // 2 == target -> no headroom
			s.Eligible = false
		}),
		with("redundancy_full_via_pending_results", DimRedundancy, func(s *Scenario) {
			s.TargetCopies = 2
			s.OtherPendingResults = 2 // finished copies hold their slots via results
			s.Eligible = false
		}),
		with("redundancy_full_mixed_live_and_pending", DimRedundancy, func(s *Scenario) {
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.OtherPendingResults = 1 // 1 + 1 == target
			s.Eligible = false
		}),
		with("redundancy_boundary_two_of_three_has_headroom", DimRedundancy, func(s *Scenario) {
			s.TargetCopies = 3
			s.OtherLiveCopies = 2 // 2 < 3 -> exactly one slot left
			s.Eligible = true
		}),
		with("redundancy_one_target_one_live_full", DimRedundancy, func(s *Scenario) {
			s.TargetCopies = 1
			s.OtherLiveCopies = 1
			s.Eligible = false
		}),

		// --- self-held copy exclusion ----------------------------------------
		with("self_live_copy_excluded_despite_headroom", DimSelfLiveCopy, func(s *Scenario) {
			s.TargetCopies = 2    // headroom exists (only the self copy is live)...
			s.SelfLiveCopy = true // ...but the requester already holds a copy
			s.Eligible = false
		}),

		// --- already contributed (prior result) ------------------------------
		with("self_pending_result_excluded", DimSelfPendingResult, func(s *Scenario) {
			s.TargetCopies = 2
			s.SelfPendingResult = true // already contributed a result
			s.Eligible = false
		}),

		// --- post-failure cooldown -------------------------------------------
		with("cooldown_expired_recent_benched", DimCooldown, func(s *Scenario) {
			s.Cooldown = CooldownExpiredRecent
			s.Eligible = false
		}),
		with("cooldown_started_abandon_benched", DimCooldown, func(s *Scenario) {
			s.Cooldown = CooldownStartedAbandon
			s.Eligible = false
		}),
		with("cooldown_unstarted_abandon_not_benched", DimCooldown, func(s *Scenario) {
			s.Cooldown = CooldownUnstartedAbandon // graceful never-started return (#59)
			s.Eligible = true
		}),
		with("cooldown_expired_stale_window_elapsed", DimCooldown, func(s *Scenario) {
			s.Cooldown = CooldownExpiredStale // failure older than the cooldown window
			s.Eligible = true
		}),

		// --- per-machine in-flight cap ---------------------------------------
		with("inflight_under_cap_admitted", DimInflightCap, func(s *Scenario) {
			s.MaxInflight = 3
			s.HostOtherInflight = 2 // 2 < 3
			s.Eligible = true
		}),
		with("inflight_at_cap_excluded", DimInflightCap, func(s *Scenario) {
			s.MaxInflight = 2
			s.HostOtherInflight = 2 // 2 == cap -> refused
			s.Eligible = false
		}),
		with("inflight_at_cap_by_host_key_excluded", DimInflightCap, func(s *Scenario) {
			s.MaxInflight = 1
			s.HostOtherInflight = 1
			s.ReportsHost = true // cap keyed on host_id rather than volunteer_id
			s.Eligible = false
		}),
		with("inflight_unlimited_admitted", DimInflightCap, func(s *Scenario) {
			s.MaxInflight = 0 // cap disabled
			s.HostOtherInflight = 5
			s.Eligible = true
		}),

		// --- homogeneous-redundancy hardware class ---------------------------
		with("hr_unpinned_unit_admits_any_class", DimHRClass, func(s *Scenario) {
			s.UnitHRClass = ""
			s.RequesterHRClass = "GenuineIntel/linux/amd64"
			s.Eligible = true
		}),
		with("hr_pinned_matching_class_admitted", DimHRClass, func(s *Scenario) {
			s.UnitHRClass = "GenuineIntel/linux/amd64"
			s.RequesterHRClass = "GenuineIntel/linux/amd64"
			s.Eligible = true
		}),
		with("hr_pinned_mismatched_class_excluded", DimHRClass, func(s *Scenario) {
			s.UnitHRClass = "GenuineIntel/linux/amd64"
			s.RequesterHRClass = "AppleSilicon/darwin/arm64"
			s.Eligible = false
		}),
		// KNOWN DIVERGENCE (recorded, not fixed — a security-relevant structural
		// finding for the orchestrator). A unit pinned to a class, requester reports
		// an EMPTY class:
		//   - FindNextAssignable ADMITS it: its clause is
		//       (wu.hr_class IS NULL OR $13::text = '' OR wu.hr_class = $13)
		//     where $13 is the requester class, so an empty requester class is treated
		//     as a wildcard (pgx-repo.go, the hr_class predicate ~L586).
		//   - eligibleLocked REFUSES it: its clause is
		//       cand.unit.HRClass != nil && *cand.unit.HRClass != "" && *cand.unit.HRClass != opts.HRClass
		//     which has NO empty-requester-class wildcard, so a pinned class "X" != ""
		//     rejects (dispatch_cache.go ~L930).
		// The two predicates are therefore not equivalent functions. It is latent
		// today because the hot path derives the requester class from
		// HardwareCapabilities.HRClass(), which never returns "" (missing components
		// collapse to "unknown"). The suite pins both current verdicts so the drift is
		// documented and any change to either clause trips this case.
		with("hr_pinned_empty_requester_class_KNOWN_DIVERGENCE", DimHRClass, func(s *Scenario) {
			s.UnitHRClass = "GenuineIntel/linux/amd64"
			s.RequesterHRClass = "" // requester reports no class
			s.Divergence = &LayerDivergence{
				Go:  false, // eligibleLocked rejects (no empty-class wildcard)
				SQL: true,  // FindNextAssignable admits ($13 = '' wildcard)
				Why: "empty requester hr_class: SQL treats as wildcard (admit), Go rejects a pinned unit",
			}
		}),

		// --- runtime / capability fit ----------------------------------------
		with("capability_all_match_admitted", DimCapability, func(s *Scenario) {
			// An explicit capability-positive (distinct from the baseline): every
			// dimension fits with room to spare.
			s.LeafRuntime = "CONTAINER"
			s.RequesterRuntimes = []string{"NATIVE", "CONTAINER"}
			s.Eligible = true
		}),
		with("capability_runtime_mismatch_excluded", DimCapability, func(s *Scenario) {
			s.LeafRuntime = "CONTAINER"
			s.RequesterRuntimes = []string{"NATIVE"} // cannot run CONTAINER
			s.Eligible = false
		}),
		with("capability_memory_over_budget_excluded", DimCapability, func(s *Scenario) {
			s.LeafMaxMemoryMB = 8192
			s.RequesterMaxMemoryMB = 4096 // leaf limit exceeds the requester's budget
			s.Eligible = false
		}),
		with("capability_cpu_over_budget_excluded", DimCapability, func(s *Scenario) {
			s.LeafMinCPUCores = 8
			s.RequesterMaxCPUCores = 4
			s.Eligible = false
		}),
		with("capability_gpu_required_but_absent_excluded", DimCapability, func(s *Scenario) {
			s.LeafGPURequired = true // execution_config.gpu_required set
			s.RequesterHasGPU = false
			s.Eligible = false
		}),

		// --- feasibility-at-deadline -----------------------------------------
		with("feasibility_fast_host_admitted", DimFeasibility, func(s *Scenario) {
			s.LeafRscFpopsEst = 1e12
			s.RequesterBenchmarkFPOPS = 1e12 // est 1s
			s.DeadlineSeconds = 10           // 1 <= 10
			s.Eligible = true
		}),
		with("feasibility_slow_host_excluded", DimFeasibility, func(s *Scenario) {
			s.LeafRscFpopsEst = 1e12
			s.RequesterBenchmarkFPOPS = 1e9 // est 1000s
			s.DeadlineSeconds = 10          // 1000 > 10
			s.Eligible = false
		}),
		with("feasibility_boundary_exactly_at_deadline_admitted", DimFeasibility, func(s *Scenario) {
			s.LeafRscFpopsEst = 1e12
			s.RequesterBenchmarkFPOPS = 1e9 // est 1000s
			s.DeadlineSeconds = 1000        // 1000 <= 1000 (feasible at the boundary)
			s.Eligible = true
		}),
		with("feasibility_no_benchmark_admitted", DimFeasibility, func(s *Scenario) {
			s.LeafRscFpopsEst = 1e12
			s.RequesterBenchmarkFPOPS = 0 // cannot estimate -> never refuse on a guess
			s.DeadlineSeconds = 10
			s.Eligible = true
		}),
	}
}
