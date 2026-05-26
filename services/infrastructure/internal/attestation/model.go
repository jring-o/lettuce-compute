package attestation

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Validation outcome constants for credit attestations.
const (
	OutcomeAgreed    = "AGREED"
	OutcomeDisagreed = "DISAGREED"
	OutcomeExpired   = "EXPIRED"
)

// Attestation is a cryptographically signed record of a credit grant (or non-grant)
// for a validated work unit. Attestations are append-only and serve as the trust
// anchor for external credit-attestation consumers.
type Attestation struct {
	ID                  types.ID       `json:"id"`
	LeafID           types.ID       `json:"leaf_id"`
	VolunteerPublicKey  []byte         `json:"-"`
	WorkUnitID          types.ID       `json:"work_unit_id"`
	RawMetrics          map[string]any `json:"raw_metrics"`
	ValidationOutcome   string         `json:"validation_outcome"`
	CreditAmount        float64        `json:"credit_amount"`
	AttestationTimestamp time.Time      `json:"attestation_timestamp"`
	Signature           []byte         `json:"-"`
	CreatedAt           time.Time      `json:"created_at"`
}
