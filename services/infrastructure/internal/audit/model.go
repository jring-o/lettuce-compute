// Package audit implements post-hoc result audits: after a work unit VALIDATES, it may be
// sampled (crypto/rand, per-leaf rate overlaid on the head default) for re-execution by an
// operator-vetted trusted runner. The runner returns raw output bytes; the HEAD adjudicates
// them against the accepted output under the comparison semantics pinned at sampling time.
// Verdicts in this phase are recorded and logged only — consequences (slashing, clawback,
// retroactive repair) are a later, separately-gated phase.
//
// Design anchors: BG-01-result-integrity-design.md §4.4 and
// phase2-ground-truth-settlement-design.md §7 (slice 2).
package audit

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Status is the lifecycle state of an audit job.
type Status string

const (
	// StatusQueued: sampled, waiting for an eligible runner to claim it.
	StatusQueued Status = "QUEUED"
	// StatusClaimed: leased to a runner; the reclaim sweep requeues or expires it if
	// the lease lapses.
	StatusClaimed Status = "CLAIMED"
	// StatusCompleted: terminal WITH a verdict (MATCH / MISMATCH / INCONCLUSIVE).
	StatusCompleted Status = "COMPLETED"
	// StatusExpired: terminal WITHOUT a verdict — never serviced within the queue
	// lifetime, or the claim-attempt budget was exhausted.
	StatusExpired Status = "EXPIRED"
)

// Verdict is the head-computed outcome of a completed audit.
type Verdict string

const (
	VerdictMatch    Verdict = "MATCH"
	VerdictMismatch Verdict = "MISMATCH"
	// VerdictInconclusive: a re-execution happened or was attempted but could not be
	// adjudicated (artifact unavailable at claim time, unparseable output where value
	// comparison is required). Never a substitute for MISMATCH: an audit must not
	// fabricate a verdict a later enforcement phase would act on.
	VerdictInconclusive Verdict = "INCONCLUSIVE"
)

// Machine-readable verdict_detail reason prefixes for INCONCLUSIVE verdicts.
const (
	ReasonArtifactUnavailable = "ARTIFACT_UNAVAILABLE"
	ReasonCompareError        = "COMPARE_ERROR"
)

// Lifecycle constants (v1: deliberate constants, not knobs — the lease self-scales with
// the unit's own deadline; promote to configuration only when a real deployment needs it).
const (
	// MaxAttempts is the claim-attempt budget: a job whose lease lapses (or whose
	// runner reports an execution failure) is requeued until this many attempts have
	// been consumed, then EXPIRED.
	MaxAttempts = 3
	// LeaseFloor is the minimum claim lease. The effective lease is
	// max(unit deadline_seconds, LeaseFloor) — the same compute budget the original
	// volunteer had, floored so tiny-deadline units still leave room for artifact
	// fetch + startup.
	LeaseFloor = 10 * time.Minute
	// QueuedLifetime bounds how long an unclaimed job waits before EXPIRED. Kept well
	// under the default credit-maturation window (7 days) so a verdict lands while the
	// unit's credit is still immature: worst case ≈ QueuedLifetime + MaxAttempts
	// leases. Operators enabling audits should keep maturation days > 4.
	QueuedLifetime = 72 * time.Hour
	// MaxConcurrentClaims caps one runner's simultaneously CLAIMED jobs, bounding how
	// much of the backlog a broken or compromised runner can sit on. The runner CLI
	// executes serially; 8 is generous headroom for future parallel runners.
	MaxConcurrentClaims = 8
)

// Runner is a row of the admin-managed trusted_runners registry: a volunteer account the
// operator vouches for as an audit re-execution runner. Registry membership authorizes the
// AuditService claim/submit surface and upgrades the trust-accrual witness rule.
type Runner struct {
	ID          types.ID  `json:"id"`
	VolunteerID types.ID  `json:"volunteer_id"`
	Label       string    `json:"label"`
	Note        string    `json:"note,omitempty"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ComparisonSnapshot pins the leaf's validation-time comparison semantics into the audit
// row, so a later leaf-config edit can never change how the audit adjudicates.
type ComparisonSnapshot struct {
	ComparisonMode   string   `json:"comparison_mode"`
	NumericTolerance float64  `json:"numeric_tolerance,omitempty"`
	IgnoreFields     []string `json:"ignore_fields,omitempty"`
	CompareFields    []string `json:"compare_fields,omitempty"`
}

// Audit is one post-hoc re-execution job + its verdict (result_audits row).
type Audit struct {
	ID               types.ID `json:"id"`
	WorkUnitID       types.ID `json:"work_unit_id"`
	LeafID           types.ID `json:"leaf_id"`
	AcceptedResultID types.ID `json:"accepted_result_id"`
	// AcceptedComparisonKey is the winner's EXACT-mode grouping key at sampling time
	// (raw submitted checksum, or the canonical stripped-key form). Nil for
	// NUMERIC_TOLERANCE, whose verdict is value-level.
	AcceptedComparisonKey *string            `json:"accepted_comparison_key,omitempty"`
	ComparisonSnapshot    ComparisonSnapshot `json:"comparison_snapshot"`
	// RequiredHRClass gates claiming: nil = any runner; set = the unit's pinned
	// hardware class (set for every pinned unit regardless of comparison mode).
	RequiredHRClass *string `json:"required_hr_class,omitempty"`
	// ArtifactVersionID is provenance + the GC-pin join target (nil for unversioned
	// legacy winners; nulled by version deletion — the audit then lands INCONCLUSIVE).
	ArtifactVersionID *types.ID `json:"artifact_version_id,omitempty"`
	// ExecutionSnapshot is the effective ExecutionConfig the accepted winner ran,
	// pinned at sampling: the runner executes THIS, never a claim-time resolution of
	// owner-mutable leaf config.
	ExecutionSnapshot leaf.ExecutionConfig `json:"execution_snapshot"`
	Status            Status               `json:"status"`
	Verdict           *Verdict             `json:"verdict,omitempty"`
	VerdictDetail     *string              `json:"verdict_detail,omitempty"`
	Attempts          int                  `json:"attempts"`
	ClaimedBy         *types.ID            `json:"claimed_by,omitempty"`
	LeaseExpiresAt    *time.Time           `json:"lease_expires_at,omitempty"`
	// RunnerOutput holds the verbatim bytes the runner returned (bytea in the DB —
	// jsonb would normalize tokens and break re-hashing). RunnerOutputChecksum is the
	// HEAD-computed sha256 of exactly those bytes, never a runner-claimed value.
	RunnerOutput         []byte     `json:"-"`
	RunnerOutputChecksum *string    `json:"runner_output_checksum,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	ClaimedAt            *time.Time `json:"claimed_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`

	// Enforcement bookkeeping (slice 3, design doc §9). EnforcementEligible records the
	// enforcement knob's state at VERDICT-WRITE time — the observe-era pin (§7.11/F-M10)
	// made structural: rows stamped false are never actionable, ever.
	EnforcementEligible bool             `json:"enforcement_eligible"`
	EnforcementState    EnforcementState `json:"enforcement_state"`
	EnforcedAt          *time.Time       `json:"enforced_at,omitempty"`
	// ConfirmsAuditID links a second-runner confirmation audit to the ORIGINAL
	// (root) it re-checks. Nil on originals. Confirmation rows are never
	// enforcement roots.
	ConfirmsAuditID *types.ID `json:"confirms_audit_id,omitempty"`
	// ClaimedHRClass is the claimant's SERVER-computed hardware class, stamped by the
	// claim statement (audit M1: class-diverse confirmation for unpinned units keys on
	// the root's value; also admin provenance).
	ClaimedHRClass *string `json:"claimed_hr_class,omitempty"`
}

// EnforcementState is the enforcement sweep's per-root state machine (design doc §9.1).
// It is BOOKKEEPING, never the safety gate: confirmation resolution runs for every
// actionable root whatever its state (audit H1).
type EnforcementState string

const (
	// EnforcementNone: not actionable (ineligible, non-MISMATCH, or a confirmation row),
	// or an actionable root the verdict statement predates. An eligible MISMATCH original
	// is moved to AWAITING_CONFIRMATION inside the verdict UPDATE itself, so it is never
	// observable here.
	EnforcementNone EnforcementState = "NONE"
	// EnforcementAwaitingConfirmation: an eligible MISMATCH root waiting on a second
	// registered runner's independent verdict.
	EnforcementAwaitingConfirmation EnforcementState = "AWAITING_CONFIRMATION"
	// EnforcementEnforced: the consequence pass completed.
	EnforcementEnforced EnforcementState = "ENFORCED"
	// EnforcementContradicted: the confirmation MATCHed the accepted output, or the two
	// runners' outputs disagree with each other — no consequences; operator incident.
	EnforcementContradicted EnforcementState = "CONTRADICTED"
	// EnforcementStalled: confirmation attempts exhausted without an adjudicable second
	// verdict (runner starvation, vanished artifacts). Sticky; operator remediation.
	EnforcementStalled EnforcementState = "STALLED"
)

// Confirmation lifecycle constants (design doc §9.2, audit H2): confirmations are priority
// work, not opportunistic sampling — a tighter queue lifetime keeps the worst-case
// enforcement horizon inside the maturation window Validate() demands.
const (
	// ConfirmationQueuedLifetime bounds how long an unclaimed CONFIRMATION waits before
	// EXPIRED (originals use QueuedLifetime).
	ConfirmationQueuedLifetime = 24 * time.Hour
	// MaxConfirmationAttempts caps confirmation rows per root (derived as a COUNT of
	// confirmation rows — the per-row attempts column is lease accounting and resets
	// across re-enqueues, audit M4). At the cap the root goes STALLED.
	MaxConfirmationAttempts = 3
)

// Stats is the fault-monitor probe payload: deltas and ages that make the audit net's
// health operator-visible (a silently-dying queue was a named audit finding, F-M6).
type Stats struct {
	// MismatchTotal is the lifetime count of MISMATCH verdicts.
	MismatchTotal int
	// ExpiredTotal is the lifetime count of EXPIRED (verdict-less) jobs.
	ExpiredTotal int
	// QueuedCount is the current QUEUED backlog size.
	QueuedCount int
	// OldestQueuedAge is the age of the oldest QUEUED job (0 when the queue is empty).
	// A large value means claim starvation — e.g. a required hardware class no
	// registered runner presents.
	OldestQueuedAge time.Duration
	// IneligibleByLeaf counts validated-but-audit-ineligible units per leaf id since
	// process start (network access, CUSTOM, unpinned/ref-only NUMERIC, canon-empty —
	// the owner-steerable never-audited lanes, made operator-visible per the design
	// audit). Composed in from the validation engine's in-memory counter by the main.go
	// probe closure; nil when the engine counter is not wired.
	IneligibleByLeaf map[string]int64

	// --- slice-3 enforcement lanes (design doc §9.8) ---

	// EnforcedTotal is the lifetime count of roots that reached ENFORCED (consequences
	// applied).
	EnforcedTotal int
	// ContradictedTotal is the lifetime count of roots that reached CONTRADICTED (two
	// trusted runners disagreed about ground truth — an operator incident, never
	// throttled below its own WARN lane).
	ContradictedTotal int
	// StalledCount is the current count of roots stuck in STALLED (confirmation
	// attempts exhausted without an adjudicable second verdict — runner/hr-class
	// starvation).
	StalledCount int
	// OldestAwaitingConfirmationAge is the age of the oldest root still in
	// AWAITING_CONFIRMATION (by completed_at); 0 when none. A large value means a
	// starved confirmation queue: single-runner deployment or hr-class starvation.
	OldestAwaitingConfirmationAge time.Duration
	// InconclusiveByRunner counts COMPLETED-INCONCLUSIVE CONFIRMATION rows per
	// claimed_by runner id: a runner whose confirmations are anomalously INCONCLUSIVE
	// is either broken or suppressing enforcement (audit M2 — liveness denial must be
	// attributable, not silent). Confirmation rows only; nil when none.
	InconclusiveByRunner map[string]int
}

// FlaggedLeaf is one row of the admin flagged-leaves surface (design doc §9.8): a leaf
// with at least one enforced/contradicted/stalled ROOT audit. Derived on read (GROUP BY
// over result_audits) — no persisted flag column, per the alpha no-vestigial posture.
type FlaggedLeaf struct {
	LeafID types.ID `json:"leaf_id"`
	// OwnerID is the leaf's creator (leafs.creator_id); nil for creator-less leaves
	// (creator_public_key-only, or a since-deleted user — the FK is ON DELETE SET NULL).
	OwnerID           *types.ID `json:"owner_id,omitempty"`
	EnforcedCount     int       `json:"enforced_count"`
	ContradictedCount int       `json:"contradicted_count"`
	StalledCount      int       `json:"stalled_count"`
	// LastEnforcedAt is the newest enforced_at across the leaf's ENFORCED roots; nil
	// when the leaf is flagged only by CONTRADICTED/STALLED roots (which carry no
	// enforced_at).
	LastEnforcedAt *time.Time `json:"last_enforced_at,omitempty"`
}
