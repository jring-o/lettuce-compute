package credit

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// LedgerEntry is an append-only record of a credit grant for a validated result.
// One entry is created per AGREED result. Rows are never updated or deleted.
type LedgerEntry struct {
	ID           types.ID  `json:"id"`
	VolunteerID  types.ID  `json:"volunteer_id"`
	LeafID       types.ID  `json:"leaf_id"`
	WorkUnitID   types.ID  `json:"work_unit_id"`
	ResultID     types.ID  `json:"result_id"`
	CreditAmount float64   `json:"credit_amount"`
	GrantedAt    time.Time `json:"granted_at"`
	CreatedAt    time.Time `json:"created_at"`
}
