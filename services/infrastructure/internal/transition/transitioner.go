package transition

import (
	"context"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

// Outcome is the terminal-ish result of one Evaluate, for the caller's structured log.
type Outcome string

const (
	OutcomeNoop         Outcome = ""
	OutcomeWaiting      Outcome = "WAITING"
	OutcomeValidated    Outcome = "VALIDATED"
	OutcomeRejected     Outcome = "REJECTED"
	OutcomeDeadLettered Outcome = "FAILED"
)

// Comparator is the validation-engine surface the transitioner orchestrates: a read-only
// comparator plus the accept/reject EFFECTS. The engine decides nothing on its own now — the
// transitioner owns the validate/reject/wait/dead-letter decision via Decide.
type Comparator interface {
	// FilterPending returns the version-homogeneous subset of pending results.
	FilterPending([]*result.Result) []*result.Result
	// Compare returns the largest agreeing group (no writes). An error (e.g. the CUSTOM stub,
	// or a non-finite output) is treated by the transitioner as "cannot validate yet".
	Compare(ctx context.Context, wu *workunit.WorkUnit, lf *leaf.Leaf, pending []*result.Result) ([]*result.Result, error)
	// ApplyAccept performs the validate effects (mark AGREED/DISAGREED, COMPLETED->VALIDATED,
	// credit/RAC/attest). The unit must already be COMPLETED.
	ApplyAccept(ctx context.Context, wu *workunit.WorkUnit, lf *leaf.Leaf, pending, majority []*result.Result) error
	// ApplyReject performs the reject effects (mark DISAGREED, COMPLETED->REJECTED, requeue).
	ApplyReject(ctx context.Context, wu *workunit.WorkUnit, pending []*result.Result) error
}

// WorkUnitStore is the narrow work-unit repo surface the transitioner needs.
type WorkUnitStore interface {
	GetByID(ctx context.Context, id types.ID) (*workunit.WorkUnit, error)
	MarkCompleted(ctx context.Context, id types.ID) error
	CountLiveCopies(ctx context.Context, workUnitID types.ID) (int, error)
	// CountProbationLiveCopies returns the live copies whose HOLDER's CURRENT effective standing
	// is not OK (BG-24b) — the probation-held copies Decide EXCLUDES from redundancy coverage so
	// the unit forces full replication around them. This is a REQUIRED store method, not an
	// optional type-asserted capability: it feeds Decide's coverage arithmetic (a CORRECTNESS
	// input), and a silent zero-fallback would quietly disable forced replication and let honest
	// volunteers be penalized in a reject round. Compile-time satisfaction is the guarantee; the
	// type-assertion idiom stays reserved for observability probes, never decision inputs.
	CountProbationLiveCopies(ctx context.Context, workUnitID types.ID) (int, error)
	CountTotalCopies(ctx context.Context, workUnitID types.ID) (int, error)
	CountErrorCopies(ctx context.Context, workUnitID types.ID) (int, error)
	DeadLetterIfExhausted(ctx context.Context, workUnitID types.ID) (bool, error)
	// ExpireLiveCopies closes ALL live copies of a unit with the given outcome (used to
	// SUPERSEDE the over-dispatch extras left running when a target>quorum unit validates).
	ExpireLiveCopies(ctx context.Context, workUnitID types.ID, outcome string) (int, error)
}

// LeafStore is the narrow leaf repo surface the transitioner needs.
type LeafStore interface {
	GetByID(ctx context.Context, id types.ID) (*leaf.Leaf, error)
}

// ResultStore is the narrow result repo surface the transitioner needs.
type ResultStore interface {
	ListByWorkUnit(ctx context.Context, workUnitID types.ID) ([]*result.Result, error)
}

// Locker serializes decisions per unit. Implementations may be cross-replica (PgxLocker) or a
// no-op (tests). A Locker must run fn exactly once.
type Locker interface {
	WithUnitLock(ctx context.Context, key int64, fn func() error) error
}

// Transitioner is the SINGLE entry point + decider for work-unit state transitions. Every site
// that used to decide "complete / validate / reject / dead-letter / requeue" now calls Evaluate,
// which loads an immutable snapshot, runs the pure Decide, and applies the one decision via the
// proven copy/validation primitives — under a per-unit lock so two replicas can't half-apply an
// invariant.
type Transitioner struct {
	locker      Locker
	wus         WorkUnitStore
	leaves      LeafStore
	results     ResultStore
	comparator  Comparator
	trustPolicy TrustPolicy
	logger      *slog.Logger
}

// NewTransitioner wires the transitioner. trustPolicy is the head trust-gate configuration
// overlaid onto each leaf (its zero value = gate off, the behavior-preserving default).
// logger may be nil (a discard logger is used).
func NewTransitioner(locker Locker, wus WorkUnitStore, leaves LeafStore, results ResultStore, comparator Comparator, trustPolicy TrustPolicy, logger *slog.Logger) *Transitioner {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Transitioner{locker: locker, wus: wus, leaves: leaves, results: results, comparator: comparator, trustPolicy: trustPolicy, logger: logger}
}

// Evaluate re-evaluates a unit after a result submit or a copy close and applies the single
// redundancy decision under the per-unit lock. Idempotent: a WAIT or a no-op leaves the unit
// untouched, so it is safe to call after every relevant event. The returned Outcome is for the
// caller's log only.
func (t *Transitioner) Evaluate(ctx context.Context, workUnitID types.ID) (Outcome, error) {
	var outcome Outcome
	err := t.locker.WithUnitLock(ctx, unitLockKey(workUnitID), func() error {
		o, e := t.decideAndApply(ctx, workUnitID)
		outcome = o
		return e
	})
	return outcome, err
}

func (t *Transitioner) decideAndApply(ctx context.Context, id types.ID) (Outcome, error) {
	wu, err := t.wus.GetByID(ctx, id)
	if err != nil {
		return OutcomeNoop, err
	}
	if workunit.IsTerminalState(wu.State) {
		return OutcomeNoop, nil
	}

	lf, err := t.leaves.GetByID(ctx, wu.LeafID)
	if err != nil {
		return OutcomeNoop, err
	}
	policy := ResolvePolicyWithTrust(lf, wu, t.trustPolicy)

	all, err := t.results.ListByWorkUnit(ctx, id)
	if err != nil {
		return OutcomeNoop, err
	}
	var pending []*result.Result
	for _, r := range all {
		if r.ValidationStatus == result.ValidationPending {
			pending = append(pending, r)
		}
	}
	pending = t.comparator.FilterPending(pending) // version-homogeneous (never compare across versions)

	live, err := t.wus.CountLiveCopies(ctx, id)
	if err != nil {
		return OutcomeNoop, err
	}
	total, err := t.wus.CountTotalCopies(ctx, id)
	if err != nil {
		return OutcomeNoop, err
	}
	errCopies, err := t.wus.CountErrorCopies(ctx, id)
	if err != nil {
		return OutcomeNoop, err
	}
	probationLive, err := t.wus.CountProbationLiveCopies(ctx, id)
	if err != nil {
		return OutcomeNoop, err
	}

	// Probation-standing coverage (BG-24b): copies that do NOT count toward redundancy because
	// the holder's CURRENT standing (live copies) or the submit-time stamp (pending results) was
	// not OK — Decide forces full replication around them. The live count needs the holders'
	// CURRENT standing (a live copy has stamped nothing yet), so it comes from the store; the
	// pending count reads the stamps already in the filtered slice.
	probationPending := 0
	for _, r := range pending {
		if !StandingCountable(r) {
			probationPending++
		}
	}

	snap := UnitSnapshot{
		State:                 wu.State,
		Policy:                policy,
		LiveCopies:            live,
		ProbationLiveCopies:   probationLive,
		TotalCopies:           total,
		ErrorCopies:           errCopies,
		PendingCount:          len(pending),
		ProbationPendingCount: probationPending,
	}

	var majority []*result.Result
	if len(pending) >= policy.MinQuorum {
		m, cerr := t.comparator.Compare(ctx, wu, lf, pending)
		if cerr != nil {
			// A comparator error (CUSTOM stub, or a non-finite / unparseable output) is
			// non-fatal — exactly as the legacy SubmitResult swallowed a TryValidate error.
			// Park the unit COMPLETED (it has a full set; it is observably "validating") and
			// wait; an operator/validator fix can later resolve it. This preserves the
			// "stuck COMPLETED, validation error logged" behavior rather than rejecting.
			t.logger.Error("validation comparison failed; unit parked pending", "work_unit_id", id, "error", cerr)
			if cmErr := t.wus.MarkCompleted(ctx, id); cmErr != nil {
				return OutcomeNoop, cmErr
			}
			return OutcomeWaiting, nil
		}
		majority = m
		// Build the verdict in DISTINCT SUBJECTS, not raw results: copies from one principal
		// corroborate as one, and a self-contradicting principal corroborates as none. The
		// resolved trust floor decides which agreeing subjects count as trusted corroborators
		// (Decide's fourth gate). Behavior-preserving today: every result has a distinct
		// volunteer and nobody is DID-bound, so subject counts equal result counts.
		snap.Comparison = BuildComparisonVerdict(pending, majority, policy.TrustFloor)
	}

	d := Decide(snap)
	switch d.Action {
	case ActionValidate:
		if err := t.wus.MarkCompleted(ctx, id); err != nil {
			return OutcomeNoop, err
		}
		if err := t.comparator.ApplyAccept(ctx, wu, lf, pending, majority); err != nil {
			return OutcomeNoop, err
		}
		// Over-dispatch hygiene (TODO #50): validate-at-quorum can leave extra copies still
		// running when target_copies > min_quorum. Close them SUPERSEDED so they are not later
		// reaped EXPIRED (which would charge the holding host a bad reliability signal for work
		// that was merely superseded). Best-effort + inert for target == quorum (no extras).
		if n, serr := t.wus.ExpireLiveCopies(ctx, id, string(assignment.OutcomeSuperseded)); serr != nil {
			t.logger.Warn("failed to supersede extra live copies after validation", "work_unit_id", id, "error", serr)
		} else if n > 0 {
			t.logger.Info("superseded extra in-flight copies after validate-at-quorum", "work_unit_id", id, "count", n)
		}
		return OutcomeValidated, nil

	case ActionReject:
		if err := t.wus.MarkCompleted(ctx, id); err != nil {
			return OutcomeNoop, err
		}
		if err := t.comparator.ApplyReject(ctx, wu, pending); err != nil {
			return OutcomeNoop, err
		}
		return OutcomeRejected, nil

	case ActionDeadLetter:
		failed, err := t.wus.DeadLetterIfExhausted(ctx, id)
		if err != nil {
			return OutcomeNoop, err
		}
		if failed {
			t.logger.Warn("work unit dead-lettered (redundancy unmet; copy budget exhausted)", "work_unit_id", id)
			return OutcomeDeadLettered, nil
		}
		return OutcomeNoop, nil

	default: // ActionWait
		if d.CompleteFirst {
			if err := t.wus.MarkCompleted(ctx, id); err != nil {
				return OutcomeNoop, err
			}
		}
		return OutcomeWaiting, nil
	}
}

// unitLockKey hashes a work-unit id to the int64 advisory-lock key space (mirrors the FNV-64a
// keying leadership.go uses for the singleton-jobs lock).
func unitLockKey(id types.ID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("lettuce:wu-transition:"))
	_, _ = h.Write([]byte(id.String()))
	return int64(h.Sum64())
}

// --- Lockers ---

// NoopLocker runs fn without any lock. Correctness then rests entirely on the optimistic state
// guards + unique constraints (exactly today's model). Used in tests and as the fallback.
type NoopLocker struct{}

func (NoopLocker) WithUnitLock(_ context.Context, _ int64, fn func() error) error { return fn() }

// lockAcquireTimeout bounds how long PgxLocker waits for a dedicated connection + the advisory
// lock before degrading to lock-free execution. Short, so a saturated pool never stalls a
// submit on lock acquisition.
const lockAcquireTimeout = 2 * time.Second

// PgxLocker serializes per-unit decisions with a cross-replica Postgres session advisory lock,
// taken on a dedicated pooled connection. It is BEST-EFFORT: if a connection or the lock can't
// be acquired within lockAcquireTimeout (a saturated pool), it degrades to running fn WITHOUT
// the lock — correctness is still guaranteed by the optimistic state guards + unique
// constraints, so the lock only ever AVOIDS wasted concurrent work, it is never load-bearing.
type PgxLocker struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewPgxLocker builds a PgxLocker over the pool. logger may be nil.
func NewPgxLocker(pool *pgxpool.Pool, logger *slog.Logger) *PgxLocker {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &PgxLocker{pool: pool, logger: logger}
}

func (l *PgxLocker) WithUnitLock(ctx context.Context, key int64, fn func() error) error {
	actx, cancel := context.WithTimeout(ctx, lockAcquireTimeout)
	defer cancel()

	conn, err := l.pool.Acquire(actx)
	if err != nil {
		// Pool saturated: degrade to lock-free (still correct via optimistic guards).
		return fn()
	}
	locked := false
	defer func() {
		if locked {
			if _, uerr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key); uerr != nil {
				// Could not release the session lock — drop the connection so it is released by
				// disconnection rather than leaking onto a pooled connection.
				_ = conn.Conn().Close(context.Background())
			}
		}
		conn.Release()
	}()

	if _, err := conn.Exec(actx, "SELECT pg_advisory_lock($1)", key); err != nil {
		// Could not acquire the lock in time (contended/slow): degrade to lock-free.
		return fn()
	}
	locked = true
	return fn()
}

// discard is an io.Writer that drops everything (for the nil-logger fallback).
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
