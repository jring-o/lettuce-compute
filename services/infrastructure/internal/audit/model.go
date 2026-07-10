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
}

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
}
