package audit

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Typed errors the handlers map to API/RPC status codes. Pinned here so the gRPC
// service, the admin handlers, and the pgx implementations agree on one contract.
var (
	// ErrNotRegistered: the authenticated volunteer has no ACTIVE trusted_runners row.
	// The AuditService surface maps it to PermissionDenied with a constant-shape
	// message (registry membership IS the authorization).
	ErrNotRegistered = errors.New("volunteer is not an active trusted runner")
	// ErrNotClaimant: the audit row is not CLAIMED by this runner (already completed,
	// reclaimed, or never claimed). The submit surface maps it to FailedPrecondition;
	// the runner CLI treats that as job-done.
	ErrNotClaimant = errors.New("audit is not claimed by this runner")
	// ErrUnknownVolunteer: registry registration referenced a volunteer id that does
	// not exist (admin surface: 400).
	ErrUnknownVolunteer = errors.New("unknown volunteer id")
)

// RunnersRepository is the admin-managed trusted_runners registry.
type RunnersRepository interface {
	// Register creates the registry row for a volunteer, or reactivates + relabels an
	// existing one (upsert on the UNIQUE volunteer_id). ErrUnknownVolunteer when the
	// volunteer does not exist.
	Register(ctx context.Context, volunteerID types.ID, label, note string) (*Runner, error)
	// Deactivate sets active = false. Rows are never deleted: claimed_by provenance on
	// past audits must survive (enforcement phases need it). Deactivating an unknown
	// volunteer is a no-op returning ErrNotRegistered.
	Deactivate(ctx context.Context, volunteerID types.ID) error
	// List returns every registry row, active and inactive, newest first.
	List(ctx context.Context) ([]*Runner, error)
	// GetActiveByVolunteerID resolves the ACTIVE registry row for a volunteer —
	// the AuditService authorization step. ErrNotRegistered when absent or inactive.
	GetActiveByVolunteerID(ctx context.Context, volunteerID types.ID) (*Runner, error)
	// ActiveRunnerSubjects returns the CURRENT trust subjects of all ACTIVE runners
	// (computed through the golden-pinned subject expression over the volunteers
	// join — never denormalized, because DID binding changes subjects). Empty slice =
	// registry has no active runners (the accrual rule then stays legacy).
	ActiveRunnerSubjects(ctx context.Context) ([]string, error)
}

// Enqueuer is the narrow write surface the validation engine's sampling hook uses.
// Implemented by AuditsRepository; split out so the engine depends on one method.
type Enqueuer interface {
	// Enqueue inserts a QUEUED audit row. The partial unique index (one OPEN audit per
	// unit) makes a duplicate enqueue a constraint error the caller logs and drops —
	// sampling is best-effort and never fails validation.
	Enqueue(ctx context.Context, a *Audit) error
}

// AuditsRepository is the full job-lifecycle store for result_audits.
type AuditsRepository interface {
	Enqueuer

	// Claim atomically claims the oldest QUEUED job the runner is eligible for
	// (required_hr_class NULL or equal to runnerHRClass), respecting
	// MaxConcurrentClaims per runner. The lease is computed in-SQL as
	// max(unit deadline_seconds, LeaseFloor) from now. Returns (nil, nil) when
	// nothing is claimable. Attempts is incremented by the claim itself.
	Claim(ctx context.Context, runnerID types.ID, runnerHRClass string) (*Audit, error)

	// GetByID loads one audit row (RunnerOutput excluded — verdict rows can be large).
	GetByID(ctx context.Context, id types.ID) (*Audit, error)

	// CompleteVerdict finalizes a CLAIMED job with a head-computed verdict, storing
	// the verbatim runner bytes + head-computed checksum. Guarded: the row must be
	// CLAIMED by runnerID, else ErrNotClaimant (a lapsed-but-unswept lease still
	// completes — sweeps are lazy; an already-reclaimed row does not).
	// enforcementEligible stamps the enforcement knob's state at verdict-write time
	// (the observe-era pin, §7.11/F-M10); the SAME statement moves an eligible
	// MISMATCH ORIGINAL to AWAITING_CONFIRMATION so it is never observable in NONE
	// (audit H1 — the state column is bookkeeping, but no crash window may leave an
	// actionable root outside the confirmation flow).
	CompleteVerdict(ctx context.Context, id, runnerID types.ID, verdict Verdict, detail string, runnerOutput []byte, checksum string, enforcementEligible bool) error

	// CompleteInconclusive finalizes a job INCONCLUSIVE outside the submit path (the
	// claim handler uses it when the sampled artifacts cannot be resolved). Guarded on
	// the row still being CLAIMED by runnerID.
	CompleteInconclusive(ctx context.Context, id, runnerID types.ID, detail string) error

	// ReleaseFailure handles a runner-reported execution failure: attempts <
	// MaxAttempts → back to QUEUED (claim fields cleared); else EXPIRED. The error
	// message is recorded in verdict_detail either way (EXPIRED rows carry no verdict,
	// but the detail text preserves why — the CHECK constraint only ties verdict to
	// COMPLETED). Guarded on CLAIMED-by-runnerID, else ErrNotClaimant.
	ReleaseFailure(ctx context.Context, id, runnerID types.ID, errMsg string) error

	// SweepLapsedLeases requeues CLAIMED rows whose lease has lapsed (attempts <
	// MaxAttempts) and expires the rest. Returns (requeued, expired).
	SweepLapsedLeases(ctx context.Context) (requeued int, expired int, err error)

	// SweepStaleQueued expires QUEUED rows older than QueuedLifetime.
	SweepStaleQueued(ctx context.Context) (expired int, err error)

	// Stats returns the fault-monitor probe payload.
	Stats(ctx context.Context) (Stats, error)

	// List returns audit rows for the admin read surface, newest first, optionally
	// filtered; limit <= 0 applies a server default.
	List(ctx context.Context, f ListFilter) ([]*Audit, error)

	// --- slice-3 enforcement surface (design doc §9) ---

	// EnqueueConfirmation inserts a QUEUED second-runner confirmation for the root:
	// a new audit row copying the root's unit/leaf/accepted/snapshot/pin columns with
	// confirms_audit_id = rootID. The one-open-audit-per-unit unique index makes a
	// duplicate enqueue a constraint error the caller treats as already-enqueued.
	EnqueueConfirmation(ctx context.Context, rootID types.ID) (*Audit, error)

	// GetRunnerOutput returns the persisted verbatim runner bytes of a COMPLETED
	// audit (excluded from every other read — verdict rows can be large).
	GetRunnerOutput(ctx context.Context, id types.ID) ([]byte, error)

	// ListActionableRoots returns eligible MISMATCH ORIGINALS still in
	// NONE/AWAITING_CONFIRMATION, oldest completed_at first (the partial-index scan).
	ListActionableRoots(ctx context.Context, limit int) ([]*Audit, error)

	// ConfirmationsForRoot returns every confirmation row of the root, newest first.
	// Their COUNT is the derived confirmation-attempt counter (audit M4 — the per-row
	// attempts column is lease accounting and resets across re-enqueues).
	ConfirmationsForRoot(ctx context.Context, rootID types.ID) ([]*Audit, error)

	// SetEnforcementState transitions a root's bookkeeping. Guarded UPDATE from the
	// non-terminal states (NONE/AWAITING_CONFIRMATION) only; ENFORCED also stamps
	// enforced_at. Returns false when the guard missed (already terminal).
	SetEnforcementState(ctx context.Context, id types.ID, state EnforcementState) (bool, error)

	// ClaimRepair inserts the audit_repairs idempotency claim for (auditID, resultID);
	// claimed=false when the result was already repaired by any audit (the UNIQUE
	// result_id guard for the non-idempotent repair effects, design doc §9.6).
	ClaimRepair(ctx context.Context, auditID, resultID types.ID) (claimed bool, err error)
}

// FlaggedLeavesReader is the admin flagged-leaves read surface (design doc §9.8). It is
// DELIBERATELY separate from AuditsRepository: the gRPC AuditService consumes the
// job-lifecycle interface but never this admin read, so keeping it off AuditsRepository
// leaves that consumer (and its test fakes) untouched. The concrete PgxAuditsRepository
// satisfies both; the AdminHandler derives this view from its audits repo at construction.
type FlaggedLeavesReader interface {
	// FlaggedLeaves returns one row per leaf with ≥ 1 ENFORCED/CONTRADICTED/STALLED ROOT
	// audit, with per-state counts, the newest enforced_at, and the owner id.
	// Newest-enforced first.
	FlaggedLeaves(ctx context.Context) ([]FlaggedLeaf, error)
}

// ListFilter narrows the admin list. Zero values mean "no filter".
type ListFilter struct {
	Status           Status
	Verdict          Verdict
	LeafID           *types.ID
	EnforcementState EnforcementState
	Limit            int
}

// Adjudicator computes the verdict for a returned re-execution output, entirely
// head-side. Implemented by the validation package (which owns the comparison
// semantics); consumed by the AuditService submit handler; wired in main.go — the type
// lives here so neither package imports the other.
//
// Contract (spec §7.4): case selection dispatches on the ACCEPTED KEY'S SHAPE, never on
// snapshot fields alone; canon-form keys are adjudicated by VALUE (never key-string
// across the raw/stored-normalization boundary); NUMERIC verdicts MATCH iff the runner
// output is within tolerance of ANY accepted output; unadjudicable inputs yield
// VerdictInconclusive with a ReasonCompareError detail — never a fabricated MISMATCH.
// acceptedOutputs carries the stored output_data of every AGREED result on the unit
// (representative first); it may contain nil entries for ref-only results.
type Adjudicator func(snap ComparisonSnapshot, acceptedKey string, acceptedOutputs []json.RawMessage, runnerOutput []byte) (Verdict, string, error)

// LeaseFor computes the claim lease for a unit: the same compute budget the original
// volunteer had, floored at LeaseFloor. Kept next to the constants so the SQL claim
// (which computes it in-database) has one Go source of truth to be tested against.
func LeaseFor(deadlineSeconds int) time.Duration {
	d := time.Duration(deadlineSeconds) * time.Second
	if d < LeaseFloor {
		return LeaseFloor
	}
	return d
}
