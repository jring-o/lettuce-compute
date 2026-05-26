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
	if err := ValidateDataConfig(&p.DataConfig, p.TaskPattern); err != nil {
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

	return nil
}

// TransitionLeaf orchestrates a state transition: validates the transition,
// checks prerequisites, updates the leaf state, and persists it.
func TransitionLeaf(ctx context.Context, repo Repository, p *Leaf, targetState LeafState) error {
	if err := ValidateTransition(p.State, targetState); err != nil {
		return err
	}

	// Activation prerequisites only apply when transitioning from CONFIGURING -> ACTIVE.
	// PAUSED -> ACTIVE (resume) skips this check.
	if targetState == StateActive && p.State == StateConfiguring {
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
