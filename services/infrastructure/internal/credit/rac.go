package credit

import (
	"math"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// HalfLifeSeconds is the RAC (Recent Average Credit) decay half-life (7 days).
const HalfLifeSeconds = 604800

// RACEntry represents a volunteer's RAC for a specific project.
type RACEntry struct {
	VolunteerID   types.ID   `json:"volunteer_id"`
	LeafID     types.ID   `json:"leaf_id"`
	RAC           float64    `json:"rac"`
	TotalCredit   float64    `json:"total_credit"`
	LastCreditAt  *time.Time `json:"last_credit_at,omitempty"`
	LastUpdatedAt time.Time  `json:"last_updated_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// CalculateRAC computes the new RAC value using an exponential decay formula.
//
//	RAC = RAC_previous × decay_factor + new_credit × (1 - decay_factor)
//	where decay_factor = exp(-elapsed_seconds × ln(2) / half_life_seconds)
func CalculateRAC(previousRAC float64, elapsedSeconds float64, newCredit float64) float64 {
	if elapsedSeconds < 1 {
		// Sub-second elapsed time: add credit directly to avoid near-zero
		// weight from the exponential decay formula. This handles rapid
		// successive grants arriving within the same second.
		return previousRAC + newCredit
	}
	decayFactor := math.Exp(-elapsedSeconds * math.Ln2 / HalfLifeSeconds)
	return previousRAC*decayFactor + newCredit*(1-decayFactor)
}
