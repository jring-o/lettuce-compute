package workunit

import (
	"encoding/json"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// WorkUnitSummary is the abbreviated response for list endpoints.
type WorkUnitSummary struct {
	ID                  types.ID         `json:"id"`
	State               WorkUnitState    `json:"state"`
	Priority            WorkUnitPriority `json:"priority"`
	Parameters          json.RawMessage  `json:"parameters,omitempty"`
	AssignedVolunteerID *types.ID        `json:"assigned_volunteer_id"`
	ReassignmentCount   int              `json:"reassignment_count"`
	FlaggedForReview    bool             `json:"flagged_for_review"`
	CreatedAt           time.Time        `json:"created_at"`
}

// ToWorkUnitSummary converts a full WorkUnit to a WorkUnitSummary.
func ToWorkUnitSummary(wu *WorkUnit) WorkUnitSummary {
	return WorkUnitSummary{
		ID:                  wu.ID,
		State:               wu.State,
		Priority:            wu.Priority,
		Parameters:          wu.Parameters,
		AssignedVolunteerID: wu.AssignedVolunteerID,
		ReassignmentCount:   wu.ReassignmentCount,
		FlaggedForReview:    wu.FlaggedForReview,
		CreatedAt:           wu.CreatedAt,
	}
}

// GenerateRequest is the JSON body for POST /api/v1/leafs/{leaf_id}/work-units/generate.
type GenerateRequest struct {
	BatchSize      int                    `json:"batch_size"`
	ParameterSpace map[string]interface{} `json:"parameter_space"`
	InputData      interface{}            `json:"input_data,omitempty"`
	InputDataRef   *string                `json:"input_data_ref,omitempty"`
}

// GenerateResponse is the response for POST /api/v1/leafs/{leaf_id}/work-units/generate.
type GenerateResponse struct {
	BatchIDs         []types.ID `json:"batch_ids"`
	WorkUnitsCreated int        `json:"work_units_created"`
	Status           string     `json:"status"`
}
