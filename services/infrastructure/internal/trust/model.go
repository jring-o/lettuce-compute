package trust

import (
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// Entry is one account-level trust record: a subject and the trust it has accrued.
type Entry struct {
	// Subject is the account-level trust key (a DID or a "vol:<uuid>" sentinel).
	Subject string `json:"subject"`
	// Score is the quorum-power number the trust gate compares against a floor.
	// Operator-seedable; accrual raises it alongside CleanUnits, a slash zeroes it.
	Score int `json:"score"`
	// CleanUnits is the earned-by-corroborated-work counter (audit trail). Accrual
	// increments it with Score; operator seeding does not; a slash retains it.
	CleanUnits int `json:"clean_units"`
	// SlashedAt is when the subject was last slashed (score zeroed), or nil if never.
	SlashedAt *time.Time `json:"slashed_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// SubjectVolunteerPrefix prefixes the per-keypair sentinel subject minted for a
// volunteer that has no live DID binding: "vol:<volunteer-uuid>". It namespaces
// keypair-identity subjects so they can never collide with a DID (which is a URI in a
// different scheme, e.g. "did:plc:...").
const SubjectVolunteerPrefix = "vol:"

// SubjectForVolunteer returns the trust subject for a volunteer row: the bound DID when
// the binding is live (status OK or STALE — a STALE binding is still the same principal,
// its quorum POWER is suppressed separately at snapshot time, see QuorumPowerSuppressed),
// else the per-keypair sentinel "vol:<volunteer-uuid>" (unbound, or REVOKED — an
// explicitly severed binding reverts the row to keypair identity).
func SubjectForVolunteer(v *volunteer.Volunteer) string {
	if v.DID != nil && *v.DID != "" && v.DIDBindingStatus != nil &&
		(*v.DIDBindingStatus == volunteer.DIDBindingStatusOK ||
			*v.DIDBindingStatus == volunteer.DIDBindingStatusStale) {
		return *v.DID
	}
	return SubjectForVolunteerID(v.ID)
}

// SubjectForVolunteerID returns the sentinel subject for a bare volunteer id — the
// fallback for legacy results stamped before this feature (NULL trust_subject) and the
// value SubjectForVolunteer returns for any volunteer without a live DID binding.
func SubjectForVolunteerID(id types.ID) string {
	return SubjectVolunteerPrefix + id.String()
}

// QuorumPowerSuppressed reports whether this volunteer's snapshot score must be recorded
// as ZERO regardless of its subject's actual score:
//   - a STALE binding: re-verification is failing, so the head can no longer confirm the
//     account still controls the DID; fail closed on the PRIVILEGE (quorum power) while
//     leaving the identity mapping (the subject stays the DID) intact, or
//   - an active rotation freeze: did_frozen_until is in the future, a post-key-rotation
//     cool-down during which the (re)bound identity does not yet get to vote.
//
// This is the enforcement consumer of the freeze the DID re-check worker records. It does
// NOT change which subject the result is stamped with (SubjectForVolunteer owns that); it
// only forces the stamped SCORE to zero so a suppressed volunteer contributes a copy but
// no trusted-corroborator power.
func QuorumPowerSuppressed(v *volunteer.Volunteer, now time.Time) bool {
	if v.DIDBindingStatus != nil && *v.DIDBindingStatus == volunteer.DIDBindingStatusStale {
		return true
	}
	if v.DIDFrozenUntil != nil && v.DIDFrozenUntil.After(now) {
		return true
	}
	return false
}
