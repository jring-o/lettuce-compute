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
// headroom, self-held-copy exclusion, already-contributed exclusion, subject
// (DID) distinctness, the post-failure cooldown ("benched"), the per-machine
// in-flight cap, homogeneous-redundancy hardware-class matching,
// runtime/capability fit, feasibility-at-deadline, the trusted-corroborator
// reservation (an untrusted requester withheld from a slot the quorum still needs a
// trusted result to fill), account standing (a BENCHED requester refused all
// dispatch, and copies/results held or submitted by a neutralized account not
// counting toward redundancy coverage — forced replication), and leaf visibility
// (a non-PUBLIC leaf served only to a requester that pinned it by id, PB-38) — with
// boundary cases at each cap/limit edge.
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
	DimSubjectDistinct   Dimension = "subject_distinctness"
	DimCooldown          Dimension = "post_failure_cooldown"
	DimInflightCap       Dimension = "per_machine_inflight_cap"
	DimHRClass           Dimension = "hr_class_match"
	DimCapability        Dimension = "runtime_capability"
	DimFeasibility       Dimension = "deadline_feasibility"
	DimTrustReservation  Dimension = "trusted_corroborator_reservation"
	DimStanding          Dimension = "account_standing"
	DimVisibility        Dimension = "leaf_visibility"
)

// Leaf-visibility values for the DimVisibility dimension (PB-38). They mirror the
// leafs.visibility domain as plain strings so this package stays primitive-only. The
// ZERO value ("") reads as PUBLIC — the gate is then inert, so every pre-existing
// scenario is unaffected. The rule: a non-PUBLIC (UNLISTED/PRIVATE) leaf's units are
// served ONLY to a requester whose leaf filter names the leaf explicitly (the
// pin-by-id opt-in); an any-leaf request gets PUBLIC only, matching the catalog
// (GetHeadInfo lists PUBLIC ACTIVE leafs only).
const (
	VisibilityPublic   = "" // PUBLIC: the gate is inert (zero value)
	VisibilityUnlisted = "UNLISTED"
	VisibilityPrivate  = "PRIVATE"
)

// Requester account-standing values for the DimStanding dimension (account standing,
// BG-24b). They mirror the volunteers.standing domain as plain strings so this package
// stays primitive-only. The ZERO value ("") reads as OK — the standing gate is then inert,
// exactly as an account ABSENT from the head's non-OK standing snapshot resolves to OK. A
// requester's EFFECTIVE standing is never hand-computed in the projections: each derives it
// through the production resolver (volunteer.EffectiveStanding on the Go side; standingExprSQL
// on the SQL side) from the raw standing + benched_until this scenario carries.
const (
	StandingOK        = ""          // OK: the gate is inert (zero value)
	StandingProbation = "PROBATION" // dispatches normally; neutralized in coverage/verdict, never refused at dispatch
	StandingBenched   = "BENCHED"   // refused all dispatch while the bench is live
)

// Binding-status values for the subject-distinctness dimension. They mirror the
// volunteers.did_binding_status domain ('OK'/'STALE'/'REVOKED'; "" = unbound) as
// plain strings so this package stays primitive-only. The subject rule they feed
// is trust.SubjectForVolunteer — the account-level trust key from the trust
// subsystem: a volunteer row maps to its bound DID while the binding is live
// (OK, or STALE — re-verification failing suppresses quorum POWER elsewhere but
// does not change WHO the row is), and to a per-row "vol:<uuid>" sentinel when
// unbound or REVOKED (an explicitly severed binding reverts the row to keypair
// identity). Two rows with the SAME live DID are ONE principal: handing each a
// copy of one unit buys no extra corroboration (validation counts them as one
// subject) — it only wastes compute — so dispatch distinctness excludes on
// subject equality, not volunteer-id equality. Each parity projection MUST
// derive subjects through the production rule (trust.SubjectForVolunteer on the
// Go side; the shared SQL subject expression on the SQL side), never by
// re-implementing it inline.
const (
	BindingNone    = ""        // no DID binding: subject = the per-row sentinel
	BindingOK      = "OK"      // live binding: subject = the DID
	BindingStale   = "STALE"   // still the same principal: subject = the DID
	BindingRevoked = "REVOKED" // severed: subject = the per-row sentinel again
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
	// CooldownExpiredExhausted: a timeout INSIDE the cooldown window but older than
	// the pool-exhausted fallback grace (FallbackGraceSeconds). A bench row exists,
	// but whether it refuses depends on the unit's current coverage: with ZERO live
	// copies the fallback re-admits the requester (work never strands, PB-9); with
	// any live copy the bench holds.
	CooldownExpiredExhausted
)

// FallbackGraceSeconds is the pool-exhausted fallback grace of the post-failure
// cooldown (PB-9). It MUST equal workunit.BenchPoolExhaustedGraceSeconds — this
// package cannot import workunit (each layer translates primitives), so the SQL
// parity half asserts the two constants agree.
const FallbackGraceSeconds = 120

// CooldownOutcomeAgoSeconds returns how many seconds in the past the requester's
// benching outcome (outcome_at) should be placed for this scenario's cooldown state
// — the single timing rule BOTH projections consume (the SQL half seeds a history
// row this old; the Go half derives the candidate's timed bench entry from it).
func (s Scenario) CooldownOutcomeAgoSeconds() int {
	switch s.Cooldown {
	case CooldownExpiredStale:
		// Older than the cooldown window (~one deadline): the cooldown has elapsed.
		return s.DeadlineSeconds + 120
	case CooldownExpiredExhausted:
		// Inside the cooldown window but past the fallback grace.
		return FallbackGraceSeconds + 60
	default:
		// A fresh outcome (moments ago): inside both the window and the grace.
		return 0
	}
}

// benches reports whether this cooldown state creates a bench ENTRY for the
// requester (a benching history row inside the cooldown window). This is the exact
// membership rule the SQL cooldown clauses and FindDispatchableBatch's benched
// snapshot implement; the Go projection uses it to build the candidate's timed
// benched set. Whether an entry actually REFUSES additionally depends on its window
// and the pool-exhausted fallback (PB-9) — see CooldownExpiredExhausted.
func (c CooldownState) benches() bool {
	return c == CooldownExpiredRecent || c == CooldownStartedAbandon || c == CooldownExpiredExhausted
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
// analysis and the offending code lines. There are currently NO known divergences
// (the empty-requester-hr_class one was reconciled by aligning the SQL to the
// stricter Go semantics); the mechanism stays for the next one found.
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
	// LeafVisibility is the target leaf's visibility: VisibilityPublic (the ""
	// zero value, gate inert), VisibilityUnlisted, or VisibilityPrivate. A
	// non-PUBLIC leaf's units are served only when the requester names the leaf in
	// its leaf filter (AnyLeafRequest false) — the PB-38 visibility gate.
	LeafVisibility string
	// AnyLeafRequest, when true, makes the requester's leaf filter EMPTY (the
	// any-leaf fallback a volunteer with no cached catalog sends). The zero value
	// (false) scopes the request to the target leaf — the pin-by-id form, and the
	// shape every pre-visibility scenario always ran with on the SQL side.
	AnyLeafRequest bool
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

	// --- Subject (DID) distinctness ------------------------------------------
	// RequesterBinding is the requester row's DID-binding status: BindingNone
	// (unbound) or BindingOK/BindingStale/BindingRevoked, in which case the
	// requester's volunteer row carries the scenario's shared DID (each projection
	// mints one DID string per scenario and reuses it for every same-DID row).
	RequesterBinding string
	// OtherSameDIDLiveCopy: a DIFFERENT volunteer row, carrying the SAME DID as
	// the requester with binding status OtherBinding, holds a live copy of the
	// target unit. Like OtherLiveCopies it consumes one unit of redundancy
	// headroom, so with the baseline TargetCopies=2 headroom remains and any
	// exclusion is attributable to the subject rule alone.
	OtherSameDIDLiveCopy bool
	// OtherSameDIDPendingResult: as above, but the same-DID row authored a
	// PENDING result for the target unit instead of holding a live copy.
	OtherSameDIDPendingResult bool
	// OtherBinding is the same-DID other row's binding status (BindingOK /
	// BindingStale / BindingRevoked). Meaningful only when OtherSameDIDLiveCopy
	// or OtherSameDIDPendingResult is set.
	OtherBinding string
	// OtherDifferentDIDLiveCopy: a different volunteer row bound (BindingOK) to a
	// DIFFERENT DID holds a live copy — the control proving that distinct bound
	// principals remain mutually eligible.
	OtherDifferentDIDLiveCopy bool

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

	// --- Trusted-corroborator reservation ------------------------------------
	// The trust gate withholds a unit's last slots from an UNTRUSTED requester so the
	// quorum can still be completed by TRUSTED results. An untrusted requester (its
	// subject's current score < the resolved floor) is refused a copy iff
	//     live_copies + pending_results + 1 + max(0, K - trusted_present) > effective_target
	// where K is the resolved trusted-corroborator requirement and trusted_present is
	// the count of DISTINCT trusted subjects already covering the unit. A trusted
	// requester is never blocked. The ZERO value of every field here leaves the gate
	// OFF (TrustGateEnabled false -> resolved K == 0 -> the reservation is inert), so
	// every existing scenario is unaffected. The scenarios express the effective min
	// quorum (the clamp target for K) via TargetCopies: a leaf that sets only
	// redundancy_factor resolves target == quorum == TargetCopies.
	//
	// TrustGateEnabled is the head master switch (transition.TrustPolicy.GateEnabled).
	// When false, ResolveTrust yields K == 0 regardless of the leaf/default overrides.
	TrustGateEnabled bool
	// TrustDefaultK / TrustDefaultFloor are the head-default trusted-corroborator
	// requirement and floor used when the leaf does not override them.
	TrustDefaultK     int
	TrustDefaultFloor int
	// LeafTrustK / LeafTrustFloor are the per-leaf overrides
	// (validation_config.min_trusted_corroborators / trust_floor). 0 = inherit the
	// head default.
	LeafTrustK     int
	LeafTrustFloor int
	// RequesterTrustScore is the requester subject's current volunteer_trust score
	// (0 = no trust row, i.e. untrusted). The requester is TRUSTED — and so bypasses
	// the reservation — iff this is at or above the resolved floor.
	RequesterTrustScore int
	// TrustedOtherLiveCopies is how many of the OtherLiveCopies holders are trusted
	// (their current score meets the floor); it must be <= OtherLiveCopies. They count
	// toward trusted_present, shrinking the reservation.
	TrustedOtherLiveCopies int
	// TrustedOtherPendingResults is how many of the OtherPendingResults are STAMPED
	// trusted (their submission-time score met the floor); it must be <=
	// OtherPendingResults. They count toward trusted_present via the stamp — exactly
	// what the validation verdict will credit — not a later current score.
	TrustedOtherPendingResults int

	// --- Account standing (BG-24b) -------------------------------------------
	// The head neutralizes an unreliable account by moving its STANDING off OK. Two
	// distinct effects, both exercised here; the ZERO value of every field leaves the
	// dimension inert (an all-OK population), so every existing scenario is unaffected.
	//
	// (1) A BENCHED requester is refused ALL dispatch until its bench lapses — the
	//     per-account twin of the post-failure cooldown, re-checked by every gate
	//     including the ReserveCopy landing write. A PROBATION requester (or one whose
	//     bench has EXPIRED, which resolves to PROBATION) still dispatches normally: its
	//     results simply never corroborate, so it is neutralized in the coverage/verdict
	//     arithmetic below, not refused at hand-out.
	// (2) Forced replication: a live copy held by — or a PENDING result submitted by — a
	//     neutralized (non-OK) account does NOT COUNT toward a unit's redundancy coverage,
	//     so a unit whose raw coverage looks full stays open for a fresh OK requester until
	//     enough COUNTABLE copies exist. Only the countable coverage closes redundancy.
	//
	// RequesterStanding is the requester's own raw account standing: StandingOK ("" =
	// OK, the gate inert), StandingProbation, or StandingBenched. Its EFFECTIVE standing
	// is resolved from this (and RequesterBenchExpired) through the production resolver.
	RequesterStanding string
	// RequesterBenchExpired, with RequesterStanding == StandingBenched, places the bench
	// deadline (benched_until) in the PAST: the stored standing is BENCHED but its
	// EFFECTIVE standing resolves to PROBATION, so the requester dispatches (re-entry is
	// neutralized, never blocked). Meaningful only with a BENCHED requester; the zero
	// value (false) is a LIVE bench (benched_until NULL/indefinite → effective BENCHED).
	RequesterBenchExpired bool
	// OtherProbationLiveCopies is the number of live copies of the target unit held by
	// OTHER, distinct accounts whose CURRENT effective standing is PROBATION. Each
	// consumes a RAW redundancy slot (like OtherLiveCopies) and is recorded as a distinct
	// contributor subject, but is NON-COUNTABLE — it covers no redundancy — so it forces
	// replication around itself.
	OtherProbationLiveCopies int
	// OtherProbationPendingResults is the number of PENDING results on the target unit
	// from OTHER, distinct accounts, STAMPED PROBATION at submit (results.standing_at_submit
	// = 'PROBATION'). Like OtherProbationLiveCopies each consumes a raw slot but is
	// non-countable — the pending-by-stamped twin of the live-by-current filter.
	OtherProbationPendingResults int

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

// benchedRequester reports whether the requester's own account standing (BG-24b) resolves
// to a LIVE bench — the per-account refusal every dispatch gate AND every landing write
// (including ReserveCopy) enforces, exactly like the post-failure cooldown. A bench whose
// deadline has passed (RequesterBenchExpired) resolves to PROBATION and does NOT refuse, and
// a PROBATION requester dispatches normally, so both leave this false. It is the one standing
// refusal ReserveCopy re-checks (see EnforcedBy); the forced-replication coverage refusal is a
// unit-level headroom invariant ReserveCopy delegates to the read-side gate.
func (s Scenario) benchedRequester() bool {
	return s.RequesterStanding == StandingBenched && !s.RequesterBenchExpired
}

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
		// The batched landing write re-checks redundancy headroom AND the
		// trusted-corroborator reservation (both are unit-level headroom invariants it
		// owns at the landing write), plus the per-volunteer distinctness / cooldown /
		// feasibility rules AND the BENCHED account-standing gate. DimStanding is enforced
		// unconditionally here because FlushReservations owns EVERY standing refusal a
		// negative standing scenario can carry: the BENCHED reserver gate
		// (standingExprSQL("vv") <> 'BENCHED'), and the countable-coverage headroom /
		// trusted reservation (both embed countableCoverageSQL).
		case DimRedundancy, DimTrustReservation, DimStanding, DimSelfLiveCopy, DimSelfPendingResult, DimSubjectDistinct, DimCooldown, DimFeasibility, DimBaseline:
			return true
		default: // capability, hr_class, inflight_cap, visibility — delegated to the hand-out
			// (visibility is a SELECTION property: the landing write cannot see the
			// request's leaf filter, and it must land a pinned-UNLISTED hand-out, so
			// the read-side gates own the refusal — PB-38.)
			return false
		}
	case GateReserveCopy:
		switch s.Dimension {
		// Note: redundancy AND the trusted-corroborator reservation are intentionally
		// ABSENT — ReserveCopy does not re-check either. Both are unit-level headroom
		// invariants owned by the read-side gate (FindNextAssignable), which ReserveCopy's
		// caller runs first; ReserveCopy re-enforces only the per-volunteer distinctness /
		// cooldown / feasibility guards. DimTrustReservation follows DimRedundancy exactly.
		case DimSelfLiveCopy, DimSelfPendingResult, DimSubjectDistinct, DimCooldown, DimFeasibility, DimBaseline:
			return true
		case DimStanding:
			// A standing scenario splits: the BENCHED-requester refusal IS a per-account
			// landing invariant ReserveCopy re-checks (standingExprSQL("vv") <> 'BENCHED',
			// the cooldown parallel), but the forced-replication countable-coverage refusal
			// is unit-level headroom ReserveCopy delegates to the read-side gate — exactly
			// as DimRedundancy is absent here. So ReserveCopy enforces a standing scenario
			// iff its refusal is the live-bench one.
			return s.benchedRequester()
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

		// --- subject (DID) distinctness ---------------------------------------
		// Two volunteer rows sharing a live DID binding are ONE principal
		// (trust.SubjectForVolunteer): the exclusions that keep a unit's redundant
		// copies on distinct principals must fire on subject equality, not
		// volunteer-id equality. All same-DID scenarios keep TargetCopies=2 with a
		// single other contributor, so redundancy headroom remains and any
		// refusal is the subject rule's.
		with("subject_same_did_live_copy_excluded", DimSubjectDistinct, func(s *Scenario) {
			s.RequesterBinding = BindingOK
			s.OtherSameDIDLiveCopy = true // the requester's OTHER device holds a copy
			s.OtherBinding = BindingOK
			s.Eligible = false
		}),
		with("subject_same_did_pending_result_excluded", DimSubjectDistinct, func(s *Scenario) {
			s.RequesterBinding = BindingOK
			s.OtherSameDIDPendingResult = true // the other device already contributed
			s.OtherBinding = BindingOK
			s.Eligible = false
		}),
		with("subject_same_did_stale_other_still_excluded", DimSubjectDistinct, func(s *Scenario) {
			// STALE suppresses quorum POWER, not identity: the row still maps to
			// its DID, so the two devices are still one principal.
			s.RequesterBinding = BindingOK
			s.OtherSameDIDLiveCopy = true
			s.OtherBinding = BindingStale
			s.Eligible = false
		}),
		with("subject_requester_stale_still_excluded", DimSubjectDistinct, func(s *Scenario) {
			s.RequesterBinding = BindingStale
			s.OtherSameDIDLiveCopy = true
			s.OtherBinding = BindingOK
			s.Eligible = false
		}),
		with("subject_same_did_revoked_other_admitted", DimSubjectDistinct, func(s *Scenario) {
			// REVOKED severs the binding: the other row reverts to its per-row
			// keypair sentinel, so the two rows are distinct principals again.
			s.RequesterBinding = BindingOK
			s.OtherSameDIDLiveCopy = true
			s.OtherBinding = BindingRevoked
			s.Eligible = true
		}),
		with("subject_requester_revoked_admitted", DimSubjectDistinct, func(s *Scenario) {
			s.RequesterBinding = BindingRevoked
			s.OtherSameDIDLiveCopy = true
			s.OtherBinding = BindingOK
			s.Eligible = true
		}),
		with("subject_different_did_admitted", DimSubjectDistinct, func(s *Scenario) {
			// Distinct bound principals stay mutually eligible.
			s.RequesterBinding = BindingOK
			s.OtherDifferentDIDLiveCopy = true
			s.Eligible = true
		}),
		with("subject_bound_requester_vs_unbound_contributor_admitted", DimSubjectDistinct, func(s *Scenario) {
			// A bound requester and an anonymous unbound contributor have
			// different subjects (DID vs sentinel): eligible.
			s.RequesterBinding = BindingOK
			s.OtherLiveCopies = 1
			s.Eligible = true
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
		with("cooldown_pool_exhausted_fallback_readmitted", DimCooldown, func(s *Scenario) {
			// PB-9 (head-setup §Redundancy): a benching outcome older than the fallback
			// grace, and the unit still has ZERO live copies — no fresh volunteer took it
			// in all that time. The bench yields so the work does not strand.
			s.Cooldown = CooldownExpiredExhausted
			s.Eligible = true
		}),
		with("cooldown_pool_exhausted_but_unit_covered_still_benched", DimCooldown, func(s *Scenario) {
			// Control for the fallback: same aged outcome, but ANOTHER volunteer holds a
			// live copy — the pool is not exhausted, so the bench holds for the rest of
			// its window (fresh volunteers keep first refusal).
			s.Cooldown = CooldownExpiredExhausted
			s.OtherLiveCopies = 1 // TargetCopies=2 → headroom remains; refusal is the bench
			s.Eligible = false
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
		// RECONCILED DIVERGENCE. A unit pinned to a class, requester reports an EMPTY
		// class: FindNextAssignable historically ADMITTED it (its clause carried an
		// empty-requester-class wildcard, `$13::text = ''`) while eligibleLocked
		// REFUSED it — the two predicates were not equivalent functions. The SQL was
		// aligned to the stricter Go semantics: a pinned unit admits only its own
		// class, and a requester reporting no class is not the pinned class (its
		// results would not be bit-comparable with the pinned cohort's). The gRPC hot
		// path never emits an empty class (HardwareCapabilities.HRClass() collapses
		// missing components to "unknown"), but the browser-volunteer path builds
		// AssignmentOptions with no HRClass at all, so before the reconciliation it
		// could be handed class-pinned units. Both layers now refuse.
		with("hr_pinned_empty_requester_class_excluded", DimHRClass, func(s *Scenario) {
			s.UnitHRClass = "GenuineIntel/linux/amd64"
			s.RequesterHRClass = "" // requester reports no class
			s.Eligible = false
		}),
		with("hr_unpinned_unit_admits_empty_requester_class", DimHRClass, func(s *Scenario) {
			// The strict rule above must not over-reject: a requester with no class is
			// excluded only from PINNED units; unpinned units remain open to it.
			s.UnitHRClass = ""
			s.RequesterHRClass = ""
			s.Eligible = true
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

		// --- trusted-corroborator reservation --------------------------------
		// The trust gate keeps a unit's LAST slots reserved for TRUSTED subjects so an
		// UNTRUSTED requester cannot burn a slot the quorum still needs a trusted result
		// to fill. Refuse an untrusted requester iff
		//     live + pending + 1 + max(0, K - trusted_present) > target
		// (target == the effective min quorum == TargetCopies in these scenarios). Every
		// negative row keeps redundancy headroom (live + pending < target) so the refusal
		// is attributable to the reservation ALONE, not to redundancy being full. K and the
		// floor are resolved by transition.TrustPolicy.ResolveTrust from the head defaults +
		// per-leaf overrides carried on the scenario.
		with("trust_gate_off_control_inert", DimTrustReservation, func(s *Scenario) {
			// Proof of inertness: the leaf asks for 2 trusted corroborators and the
			// requester is untrusted, but the HEAD gate is OFF, so ResolveTrust yields
			// K == 0 and the reservation withholds nothing — the last slot is handed out.
			s.TrustGateEnabled = false
			s.LeafTrustK = 2
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted (irrelevant with the gate off)
			s.TargetCopies = 2
			s.OtherLiveCopies = 1 // one slot left; would be withheld iff the gate were on
			s.Eligible = true
		}),
		with("trust_last_slot_reserved_untrusted_refused", DimTrustReservation, func(s *Scenario) {
			// Gate on, K = 1 (head default), no trusted subject present, one copy already
			// covering redundancy: the unit's LAST slot is reserved for a trusted result,
			// so the untrusted requester is refused despite redundancy headroom remaining.
			//   1 (live) + 1 (this) + max(0, 1 - 0) = 3 > 2 -> refused.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = false
		}),
		with("trust_trusted_requester_bypasses_reservation", DimTrustReservation, func(s *Scenario) {
			// Identical to the refusal above, but the requester's own score meets the
			// floor: a trusted requester can fill a reserved slot itself, so the
			// reservation never withholds one from it.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 25 // >= floor -> trusted
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = true
		}),
		with("trust_trusted_live_holder_frees_slot", DimTrustReservation, func(s *Scenario) {
			// The single existing live copy is held by a TRUSTED subject, so the K = 1
			// requirement is already covered: nothing stays reserved and the untrusted
			// requester takes the remaining slot.
			//   1 (live) + 1 (this) + max(0, 1 - 1) = 2 <= 2 -> admitted.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.TrustedOtherLiveCopies = 1 // trusted_present = 1
			s.Eligible = true
		}),
		with("trust_stamped_pending_frees_slot", DimTrustReservation, func(s *Scenario) {
			// As above but the covering copy is a finished PENDING result whose STAMPED
			// submission-time score met the floor. A stamped trusted pending author counts
			// toward K exactly like a trusted live holder (the stamp is what validation
			// credits), so the slot is freed and the untrusted requester is admitted.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted
			s.TargetCopies = 2
			s.OtherPendingResults = 1
			s.TrustedOtherPendingResults = 1 // trusted_present = 1
			s.Eligible = true
		}),
		with("trust_free_slot_beyond_reservation_admitted", DimTrustReservation, func(s *Scenario) {
			// Target 3 with one copy present and K = 1 reserved: a slot remains BEYOND the
			// reservation, so the untrusted requester is admitted.
			//   1 (live) + 1 (this) + max(0, 1 - 0) = 3 <= 3 -> admitted.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted
			s.TargetCopies = 3
			s.OtherLiveCopies = 1
			s.Eligible = true
		}),
		with("trust_k2_reserves_two_slots_refused", DimTrustReservation, func(s *Scenario) {
			// K = 2 (head default) with target 3 and one untrusted copy present: TWO slots
			// stay reserved for trusted results, leaving only one free.
			//   1 (live) + 1 (this) + max(0, 2 - 0) = 4 > 3 -> refused.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 2
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted
			s.TargetCopies = 3
			s.OtherLiveCopies = 1
			s.Eligible = false
		}),
		with("trust_k_clamped_to_quorum_refused", DimTrustReservation, func(s *Scenario) {
			// The leaf demands K = 5 trusted corroborators, but the effective min quorum
			// (== TargetCopies here) is 2: ResolveTrust CLAMPS K down to 2, since a
			// quorum-sized agreeing group can hold at most quorum distinct subjects, so an
			// unclamped K could never be satisfied. Even clamped, with no trusted subject
			// present the whole target is reserved.
			//   0 (none) + 1 (this) + max(0, 2 - 0) = 3 > 2 -> refused.
			s.TrustGateEnabled = true
			s.LeafTrustK = 5 // leaf override, clamped down to quorum 2
			s.TrustDefaultFloor = 25
			s.RequesterTrustScore = 0 // untrusted
			s.TargetCopies = 2
			s.Eligible = false
		}),
		with("trust_leaf_floor_override_untrusts_requester_refused", DimTrustReservation, func(s *Scenario) {
			// Per-leaf floor override raises the bar: the requester's score 30 clears the
			// head default floor (25) but NOT the leaf override (50), so it is UNTRUSTED for
			// this leaf and the reserved last slot is withheld.
			//   1 (live) + 1 (this) + max(0, 1 - 0) = 3 > 2 -> refused.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.LeafTrustFloor = 50     // overrides the head default 25
			s.RequesterTrustScore = 30 // >= 25 default, < 50 override -> untrusted here
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = false
		}),
		with("trust_leaf_floor_inherit_trusts_requester_admitted", DimTrustReservation, func(s *Scenario) {
			// Sibling control for the row above: identical, but the leaf does NOT override
			// the floor (inherits the head default 25), so the SAME score 30 is trusted and
			// bypasses the reservation. This isolates the refusal above to the override, not
			// the score.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.LeafTrustFloor = 0       // inherit the head default 25
			s.RequesterTrustScore = 30 // >= 25 -> trusted
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = true
		}),

		// --- BG-01a floor clamp + tighten-only floor in the reservation ------
		// These three exercise the BG-01a changes to the resolved trust floor (ResolveTrust +
		// its effTrustFloorSQL twin, now GREATEST(1, GREATEST(leaf, default))). Each keeps
		// redundancy headroom so the verdict is attributable to the reservation alone.
		with("trust_clamp_floor0_default_untrusts_score0_refused", DimTrustReservation, func(s *Scenario) {
			// >= 1 clamp: gate on, head default floor 0. WITHOUT the clamp a score-0 requester
			// counts as trusted (0 >= 0) and bypasses the reservation; the clamp resolves the floor
			// to 1, so score 0 is UNTRUSTED and the reserved last slot is withheld. Fails against
			// pre-clamp code (which would admit).
			//   1 (live) + 1 (this) + max(0, 1 - 0) = 3 > 2 -> refused.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 0 // clamped up to 1 by ResolveTrust / effTrustFloorSQL
			s.RequesterTrustScore = 0
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = false
		}),
		with("trust_clamp_floor0_admits_score1_requester", DimTrustReservation, func(s *Scenario) {
			// Clamp boundary control: with the floor clamped to 1, a requester scoring exactly 1 IS
			// trusted (1 >= 1) and bypasses the reservation — proving the clamp raises the bar to
			// exactly 1, not above it, so it never over-rejects.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 0 // clamped to 1
			s.RequesterTrustScore = 1
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = true
		}),
		with("trust_leaf_floor_below_default_tightened_untrusts_requester_refused", DimTrustReservation, func(s *Scenario) {
			// Tighten-only (F-H5): a leaf sets trust_floor 1, BELOW the head default 25. Pre-fix the
			// leaf's 1 would win and a score-10 requester would clear it (trusted, bypassing the
			// reservation). Tighten-only resolves the floor to max(1, 25) = 25, so score 10 is
			// UNTRUSTED and the reserved last slot is withheld. Fails against the pre-fix
			// leaf-when-positive rule (which would admit).
			//   1 (live) + 1 (this) + max(0, 1 - 0) = 3 > 2 -> refused.
			s.TrustGateEnabled = true
			s.TrustDefaultK = 1
			s.TrustDefaultFloor = 25
			s.LeafTrustFloor = 1 // below the head default; tighten-only raises the effective floor to 25
			s.RequesterTrustScore = 10
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.Eligible = false
		}),

		// --- account standing (BG-24b) ---------------------------------------
		// Two neutralization mechanisms, both driven by a volunteer's STANDING moving off
		// OK. (A) A BENCHED requester is refused ALL dispatch until its bench lapses — a
		// per-account refusal every gate enforces, ReserveCopy included (the cooldown
		// parallel). (B) Forced replication: a live copy held by, or a PENDING result
		// submitted by, a neutralized (non-OK) account does NOT COUNT toward redundancy
		// coverage, so a unit that looks raw-full stays open for a fresh OK requester —
		// only the COUNTABLE coverage closes redundancy (the DimRedundancy parallel;
		// ReserveCopy delegates it to the read-side gate). Every field's zero value is
		// inert (an all-OK population), so the whole dimension leaves prior scenarios
		// untouched. A leaf that sets only redundancy_factor resolves effective
		// target == TargetCopies here, exactly as the redundancy/trust dimensions assume.
		//
		// NOT expressible as a distinct row (documented deliberately): "a floor-scoring
		// covering copy whose account is PROBATION is excluded from trusted_present, so an
		// untrusted requester still hits the reservation". A floor-scoring covering copy's
		// standing is provably NET-NEUTRAL on the reservation verdict — going non-OK drops it
		// from BOTH the countable coverage (−1) and trusted_present (which raises the reserved
		// term GREATEST(0, K − present) by +1), and while the reservation binds (present < K)
		// those exactly cancel. So no single-field scenario can make the copy's standing FLIP
		// a reservation verdict; the trusted_present standing filter is verified only via the
		// counting parity, not observable here. Skipped rather than contort the table with a
		// field that decouples coverage from trusted_present (which standing inherently
		// couples).
		with("standing_benched_requester_refused", DimStanding, func(s *Scenario) {
			// (A) A requester the head has BENCHED (indefinite bench, benched_until NULL) is
			// refused every dispatch and every landing write. Redundancy is wide open
			// (TargetCopies 2, no copies) so the bench is unambiguously the reason.
			s.RequesterStanding = StandingBenched
			s.Eligible = false
		}),
		with("standing_expired_bench_dispatches_as_probation", DimStanding, func(s *Scenario) {
			// (A, neutralized) The stored standing is BENCHED but the bench deadline has
			// passed, so the EFFECTIVE standing resolves to PROBATION — re-entry is
			// neutralized, not blocked, and the account is dispatched again.
			s.RequesterStanding = StandingBenched
			s.RequesterBenchExpired = true
			s.Eligible = true
		}),
		with("standing_probation_requester_dispatches", DimStanding, func(s *Scenario) {
			// (A, neutralized) A PROBATION requester still dispatches: neutralization happens
			// in the coverage/verdict arithmetic (its results never corroborate), not as a
			// dispatch refusal.
			s.RequesterStanding = StandingProbation
			s.Eligible = true
		}),
		with("standing_probation_live_copies_force_replication_admit_ok", DimStanding, func(s *Scenario) {
			// (B) Both live copies covering this redundancy-2 unit are held by PROBATION
			// accounts, so RAW coverage sits AT the target (2) while COUNTABLE coverage is 0.
			// A fresh OK requester still finds headroom — full replication forced around the
			// neutralized copies. Enforced by the redundancy gates (EligibleLocked,
			// FindNextAssignable, FlushReservations); a positive verdict, so every gate admits.
			s.TargetCopies = 2
			s.OtherProbationLiveCopies = 2
			s.Eligible = true
		}),
		with("standing_probation_pending_results_force_replication_admit_ok", DimStanding, func(s *Scenario) {
			// (B) As above but the covering copies are finished PENDING results STAMPED
			// PROBATION at submit — non-countable via the pending-by-stamped arm. Raw
			// coverage 2 == target, countable 0, so the OK requester is admitted.
			s.TargetCopies = 2
			s.OtherProbationPendingResults = 2
			s.Eligible = true
		}),
		with("standing_mixed_coverage_probation_netted_admit_ok", DimStanding, func(s *Scenario) {
			// (B, boundary) One OK (countable) live copy plus one PROBATION (non-countable)
			// one: raw coverage 2 == target, but countable coverage is 1 < 2, so a slot
			// remains. This is the exact edge where the standing filter flips the verdict —
			// without it the raw count 2 would close the unit; with it the probation copy is
			// netted out and the OK requester is admitted.
			s.TargetCopies = 2
			s.OtherLiveCopies = 1
			s.OtherProbationLiveCopies = 1
			s.Eligible = true
		}),
		with("standing_probation_requester_over_probation_coverage_admit", DimStanding, func(s *Scenario) {
			// (A+B) Compositional: a PROBATION requester (not refused at dispatch) taking a
			// forced-replication slot on a unit whose only live copy is PROBATION-held. The
			// requester-standing gate (PROBATION dispatches) and the countable-coverage gate
			// (probation copy covers nothing, headroom stays open) both admit, independently.
			s.RequesterStanding = StandingProbation
			s.TargetCopies = 2
			s.OtherProbationLiveCopies = 1
			s.Eligible = true
		}),
		with("standing_coverage_full_net_of_probation_refused", DimStanding, func(s *Scenario) {
			// (B, refused control) Two OK (countable) live copies fill the redundancy-2
			// target NET of a third, PROBATION-held copy: countable coverage 2 == target, so
			// the OK requester is refused (rejectRedundancyFull class). The probation copy is
			// present but netted out — proving it is the countability, not the raw count, that
			// closes the unit. A unit-level headroom refusal: enforced by EligibleLocked,
			// FindNextAssignable and FlushReservations, but NOT ReserveCopy (the DimRedundancy
			// parallel — ReserveCopy delegates coverage to the read-side gate).
			s.TargetCopies = 2
			s.OtherLiveCopies = 2
			s.OtherProbationLiveCopies = 1
			s.Eligible = false
		}),
		with("standing_inert_control_matches_baseline", DimStanding, func(s *Scenario) {
			// Gate-inertness control: every standing field at its zero value (OK requester, no
			// probation coverage) must reach the SAME eligible verdict as the plain baseline,
			// pinning that a non-standing deployment is byte-for-byte unchanged.
			s.Eligible = true
		}),

		// --- leaf visibility (PB-38) -----------------------------------------
		// A leaf hidden from the catalog (GetHeadInfo lists PUBLIC ACTIVE only) must
		// not dispatch through catalog-driven requests: an any-leaf request (the
		// volunteer fallback with no leaf filter) gets PUBLIC leafs only, while a
		// request that names the leaf id explicitly (the pin-by-id opt-in — attach
		// --leaf / a browser body naming the id) is still served. Enforced by the two
		// read-side gates; the landing writes deliberately delegate (they cannot see
		// the request's leaf filter, and must land a pinned-UNLISTED hand-out).
		with("visibility_unlisted_any_leaf_refused", DimVisibility, func(s *Scenario) {
			s.LeafVisibility = VisibilityUnlisted
			s.AnyLeafRequest = true
			s.Eligible = false
		}),
		with("visibility_unlisted_pinned_by_id_served", DimVisibility, func(s *Scenario) {
			s.LeafVisibility = VisibilityUnlisted
			s.AnyLeafRequest = false // leaf filter names the target leaf
			s.Eligible = true
		}),
		with("visibility_private_any_leaf_refused", DimVisibility, func(s *Scenario) {
			s.LeafVisibility = VisibilityPrivate
			s.AnyLeafRequest = true
			s.Eligible = false
		}),
		with("visibility_private_pinned_by_id_served", DimVisibility, func(s *Scenario) {
			s.LeafVisibility = VisibilityPrivate
			s.AnyLeafRequest = false
			s.Eligible = true
		}),
		with("visibility_public_any_leaf_served_control", DimVisibility, func(s *Scenario) {
			// Control: a PUBLIC leaf serves the any-leaf fallback exactly as before.
			s.LeafVisibility = VisibilityPublic
			s.AnyLeafRequest = true
			s.Eligible = true
		}),
	}
}
