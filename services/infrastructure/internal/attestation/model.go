package attestation

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Validation outcome constants for credit attestations.
const (
	OutcomeAgreed    = "AGREED"
	OutcomeDisagreed = "DISAGREED"
	// OutcomeRevoked marks a revocation attestation: a signed statement that the head has
	// clawed back credit previously attested by the referenced grant attestation.
	OutcomeRevoked = "REVOKED"
)

// Attestation schema versions. Rows written before the versioned format are schema_version 1
// and verify forever under the frozen v1 canonical form (canonicalV1); every new row is v2 —
// a hard cutover, no dual-write.
const (
	SchemaVersionV1 = 1
	SchemaVersionV2 = 2
)

// Domain-separation context literals. The context is an explicit field INSIDE the canonical
// JSON (not a byte prefix), so the byte spaces of the v2 grant form, the v2 revocation form,
// and the context-free v1 form are pairwise disjoint: a payload signed under one form can
// never verify under another.
const (
	ContextGrantV2      = "lettuce/credit-attestation/v2"
	ContextRevocationV2 = "lettuce/credit-attestation-revocation/v2"
)

// PolicyVersion is the version of the head's validation/credit-policy semantics, stamped
// into (and signed within) every v2 grant attestation so external consumers can interpret
// the quorum descriptor. Bump it in any change that alters what a quorum means (gates,
// subject-counting rules, credit semantics); the published verification recipe
// (guides/attestation-verification.md) keeps the changelog.
const PolicyVersion = 1

// QuorumDescriptor is the signed summary of the quorum event behind a v2 grant/reject
// attestation: what the resolved redundancy policy DEMANDED (min_quorum,
// min_trusted_corroborators, target_copies, trust_floor) and what the comparison DELIVERED
// (group_size, pending_size, trusted_corroborators), in DISTINCT-SUBJECT units (see
// transition.ComparisonVerdict — copies from one principal corroborate as one). All fields
// are integers so the canonical form contains no JSON floats and is serializer-agnostic:
// the audit sampling rate is expressed in parts-per-million.
//
// On a rejected unit, group_size is the size of the LOSING clique — the largest coherent
// agreeing group, which failed the acceptance gates. The attestation's validation_outcome
// states the consequence; the descriptor states the arithmetic.
type QuorumDescriptor struct {
	AuditRatePPM            int `json:"audit_rate_ppm"`
	GroupSize               int `json:"group_size"`
	MinQuorum               int `json:"min_quorum"`
	MinTrustedCorroborators int `json:"min_trusted_corroborators"`
	PendingSize             int `json:"pending_size"`
	TargetCopies            int `json:"target_copies"`
	TrustFloor              int `json:"trust_floor"`
	TrustedCorroborators    int `json:"trusted_corroborators"`
}

// Attestation is a cryptographically signed record of a credit decision (grant, rejection,
// or revocation) for a validated work unit. Attestations are append-only and serve as the
// trust anchor for external credit-attestation consumers.
type Attestation struct {
	ID                 types.ID `json:"id"`
	SchemaVersion      int      `json:"schema_version"`
	LeafID             types.ID `json:"leaf_id"`
	VolunteerPublicKey []byte   `json:"-"`
	WorkUnitID         types.ID `json:"work_unit_id"`

	// v2 grant/reject fields (nil on v1 rows; QuorumDescriptor and PolicyVersion are also
	// nil on revocations, which are not validation events).
	ResultID         *types.ID         `json:"result_id,omitempty"`
	OutputChecksum   *string           `json:"output_checksum,omitempty"`
	QuorumDescriptor *QuorumDescriptor `json:"quorum_descriptor,omitempty"`
	PolicyVersion    *int              `json:"policy_version,omitempty"`

	// Revocation fields (nil everywhere else): the original grant attestation being revoked,
	// the credit_adjustments row that caused the revocation, and that adjustment's
	// machine-readable reason code (charset ^[A-Z0-9_]{1,64}$ — enforced upstream so the
	// signed bytes stay escape-free under any JSON serializer).
	RevokesAttestationID *types.ID `json:"revokes_attestation_id,omitempty"`
	AdjustmentID         *types.ID `json:"adjustment_id,omitempty"`
	Reason               *string   `json:"reason,omitempty"`

	RawMetrics        map[string]any `json:"raw_metrics"`
	ValidationOutcome string         `json:"validation_outcome"`
	CreditAmount      float64        `json:"credit_amount"`
	// CreditAmountCanonical is the exact fixed-scale decimal string ("1.000000") that v2
	// forms sign AND the repository stores as the numeric parameter. Signing and storage
	// share one representation so the stored value can never round differently from the
	// signed bytes (Go rounds the binary value half-to-even; Postgres rounds a decimal
	// half-away — two rules that disagree on tie-adjacent values). Populated by
	// CanonicalCreditString at build time and from credit_amount::text at scan time; empty
	// on freshly-constructed v1 test fixtures, which sign the float64 directly.
	CreditAmountCanonical string    `json:"-"`
	AttestationTimestamp  time.Time `json:"attestation_timestamp"`
	Signature             []byte    `json:"-"`
	CreatedAt             time.Time `json:"created_at"`
}
