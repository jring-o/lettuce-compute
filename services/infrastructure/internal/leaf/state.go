package leaf

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// validTransitions defines the 10 valid state transitions for a leaf.
var validTransitions = map[LeafState][]LeafState{
	StateDraft:       {StateConfiguring, StateArchived},
	StateConfiguring: {StateActive},
	StateActive:      {StatePaused, StateConfiguring, StateCompleted},
	StatePaused:      {StateActive, StateConfiguring, StateArchived},
	StateCompleted:   {StateArchived},
	// StateArchived has no valid outbound transitions.
}

// ValidateTransition checks whether a state transition is allowed.
func ValidateTransition(from, to LeafState) error {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return apierror.Conflict(
		fmt.Sprintf("invalid state transition from %s to %s", from, to),
		map[string]string{
			"code": "INVALID_STATE_TRANSITION",
			"from": string(from),
			"to":   string(to),
		},
	)
}

// configFailure records which config section failed validation and why.
type configFailure struct {
	Config string `json:"config"`
	Reason string `json:"reason"`
}

// CanActivate checks that all 4 required config sections pass validation.
// All failures are collected and reported together.
// Note: CreditConfig and ResourceRequirements are validated at creation time,
// not at activation — they are not activation prerequisites.
func CanActivate(p *Leaf) error {
	var failures []configFailure

	if err := ValidateExecutionConfig(&p.ExecutionConfig); err != nil {
		failures = append(failures, configFailure{Config: "execution_config", Reason: err.Error()})
	}
	if err := ValidateValidationConfig(&p.ValidationConfig); err != nil {
		failures = append(failures, configFailure{Config: "validation_config", Reason: err.Error()})
	}
	if err := ValidateFaultToleranceConfig(&p.FaultToleranceConfig); err != nil {
		failures = append(failures, configFailure{Config: "fault_tolerance_config", Reason: err.Error()})
	}
	if err := ValidateDataConfig(&p.DataConfig, p.TaskPattern, p.IsOngoing); err != nil {
		failures = append(failures, configFailure{Config: "data_config", Reason: err.Error()})
	}

	if len(failures) > 0 {
		return &apierror.APIError{
			Code:       "CONFIGURATION_INCOMPLETE",
			Message:    "leaf configuration is incomplete",
			Details:    failures,
			HTTPStatus: 400,
		}
	}
	return nil
}

// CanDelete checks whether a leaf may be deleted.
// Active leafs and leafs with credit history cannot be deleted.
// EffectiveGenerationMode normalizes a stored/requested generation_mode: the empty string is
// eager (the default the validator and the lazy manager both already treat it as).
func EffectiveGenerationMode(mode string) string {
	if mode == "" {
		return GenerationModeEager
	}
	return mode
}

// CanChangeGenerationMode gates a generation_mode flip on the leaf never having generated
// (★BG-22f): once ANY work unit exists — or the generation cursor has ever advanced — the two
// modes' bookkeeping is irreconcilable and a flip re-emits ordinals as duplicate trials.
// Eager emission never advances leafs.generation_cursor (the eager path passes a nil cursor
// advance), so an eager→lazy flip hands the lazy manager cursor offset 0 and it re-emits
// [0,batch)… with byte-identical seeds and trial indices; the symmetric lazy→eager flip
// re-arms the manual generate endpoint (whose lazy 409 no longer fires) to re-emit
// [0,cursor). There is no unique constraint on trial identity, so duplicates insert cleanly
// and burn real volunteer compute. Reconciling the cursor from emitted rows is not reliably
// possible (eager emission is re-runnable and stamps no ordinal for every pattern), so the
// honest surface is immutability-after-first-generation: pick the mode before generating —
// the same posture as the endpoint's LAZY_GENERATION_MANAGED refusal and the
// execution_config.runtime ACTIVE-state guard.
func CanChangeGenerationMode(ctx context.Context, pool *pgxpool.Pool, leafID types.ID, generationCursor []byte) error {
	cursor := string(generationCursor)
	if cursor != "" && cursor != "{}" && cursor != "null" {
		return apierror.Conflict(
			"generation_mode cannot be changed after work units have been generated: the generation cursor has advanced, and switching modes would re-emit already-generated trials",
			map[string]string{"code": "GENERATION_MODE_IMMUTABLE"},
		)
	}
	var hasUnits bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM work_units WHERE leaf_id = $1)",
		leafID,
	).Scan(&hasUnits); err != nil {
		return apierror.Internal("failed to check generated work units", err)
	}
	if hasUnits {
		return apierror.Conflict(
			"generation_mode cannot be changed after work units have been generated: switching modes would re-emit already-generated trials as duplicates",
			map[string]string{"code": "GENERATION_MODE_IMMUTABLE"},
		)
	}
	return nil
}

func CanDelete(ctx context.Context, pool *pgxpool.Pool, leafID types.ID, currentState LeafState) error {
	if currentState == StateActive {
		return apierror.Conflict(
			"cannot delete active leaf; pause and archive first",
			nil,
		)
	}

	var hasCredit bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM credit_ledger WHERE leaf_id = $1)",
		leafID,
	).Scan(&hasCredit)
	if err != nil {
		return apierror.Internal("failed to check credit history", err)
	}

	if hasCredit {
		return apierror.Conflict(
			"cannot delete leaf with credit history; archive instead",
			nil,
		)
	}

	// Audit-enforcement evidence (design doc §9.1, audit M7). A leaf whose audits
	// produced enforcement evidence can no longer CASCADE-delete cleanly even with NO
	// credit_ledger rows (e.g. cap-suppressed grants): migration 00021's RESTRICT FKs
	// (credit_adjustments.audit_id, audit_repairs.audit_id/result_id) would make the
	// delete 500 on the FK instead of 409ing here. Refuse deletion when the leaf has any
	// audit row in a non-NONE enforcement state OR with a repair recorded against it.
	var hasEnforcement bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM result_audits ra
			WHERE ra.leaf_id = $1
			  AND (ra.enforcement_state <> 'NONE'
			       OR EXISTS (SELECT 1 FROM audit_repairs ar WHERE ar.audit_id = ra.id))
		)`,
		leafID,
	).Scan(&hasEnforcement)
	if err != nil {
		return apierror.Internal("failed to check audit-enforcement history", err)
	}

	if hasEnforcement {
		return apierror.Conflict(
			"cannot delete leaf with audit-enforcement history; archive instead",
			nil,
		)
	}

	return nil
}

// TransitionLeaf orchestrates a state transition: validates the transition,
// checks prerequisites, updates the leaf state, and persists it.
func TransitionLeaf(ctx context.Context, repo Repository, p *Leaf, targetState LeafState) error {
	if err := ValidateTransition(p.State, targetState); err != nil {
		return err
	}

	// Activation prerequisites apply on EVERY transition into ACTIVE — first activation
	// (CONFIGURING -> ACTIVE) and resume (PAUSED -> ACTIVE) alike. Resume used to skip
	// this check, which let a leaf activated before a validation rule existed re-enter
	// ACTIVE with a config the rule now refuses (PB-36: pre-gate NUMERIC_TOLERANCE
	// leaves kept their honest-rejection footgun forever). A paused leaf that passed at
	// activation still passes here; one that predates a tightened rule gets the same
	// actionable 400 the configure path gives, at the moment an operator touches it.
	if targetState == StateActive {
		if err := CanActivate(p); err != nil {
			return err
		}
	}

	if targetState == StateCompleted && p.IsOngoing {
		return apierror.Conflict("ongoing leafs cannot be completed", nil)
	}

	previousState := p.State
	p.State = targetState
	if err := repo.Update(ctx, p); err != nil {
		p.State = previousState
		return err
	}
	return nil
}
