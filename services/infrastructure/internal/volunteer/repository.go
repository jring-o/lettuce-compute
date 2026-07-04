package volunteer

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// VolunteerListFilters controls filtering for List queries.
type VolunteerListFilters struct {
	IsActive       *bool           `json:"is_active,omitempty"`
	SchedulingMode *SchedulingMode `json:"scheduling_mode,omitempty"`
}

// Repository defines the data-access interface for volunteers.
type Repository interface {
	Create(ctx context.Context, v *Volunteer) error
	// CreateAdmitted creates a volunteer through the registration-admission gates
	// (internal/admission). With a non-nil gate the volunteer INSERT and the
	// per-(bucket, UTC day) creation-cap increment run in ONE transaction, so a
	// refusal (admission.ErrCreationCapExceeded) or failure rolls both back and the
	// cap counts exactly the creations that committed. A nil gate is exactly Create —
	// the knob-off inertness contract.
	CreateAdmitted(ctx context.Context, v *Volunteer, gate *admission.CreateGate) error
	GetByID(ctx context.Context, id types.ID) (*Volunteer, error)
	GetByPublicKey(ctx context.Context, publicKey []byte) (*Volunteer, error)
	GetByUserID(ctx context.Context, userID types.ID) (*Volunteer, error)
	Update(ctx context.Context, v *Volunteer) error
	UpdateLastSeen(ctx context.Context, id types.ID) error
	SetActive(ctx context.Context, id types.ID, active bool) error
	IncrementWorkUnitsCompleted(ctx context.Context, id types.ID) error
	IncrementWorkUnitsRejected(ctx context.Context, id types.ID) error
	List(ctx context.Context, filters VolunteerListFilters, page types.PaginationRequest) ([]*Volunteer, types.PaginationResponse, error)

	// MarkInactiveOlderThan sets is_active = false for all volunteers
	// whose last_seen_at < NOW() - threshold. Returns count of updated rows.
	MarkInactiveOlderThan(ctx context.Context, threshold time.Duration) (int, error)

	// --- Optional ATProto DID identity binding ---
	// These are the only writers of the volunteer row's did_* state.

	// SetDIDBinding records a freshly verified DID binding (status OK, failure
	// counter cleared, bound_at == checked_at == boundAt).
	SetDIDBinding(ctx context.Context, volunteerID types.ID, did, recordURI, recordCID string, boundAt time.Time) error
	// ListDIDBindingsForRecheck returns active (OK/STALE) bindings whose last check
	// predates checkedBefore, oldest-checked first, up to limit, for TTL re-verification.
	ListDIDBindingsForRecheck(ctx context.Context, checkedBefore time.Time, limit int) ([]*Volunteer, error)
	// MarkDIDBindingChecked records a successful re-verification (status OK, CID
	// refreshed, failure counter reset).
	MarkDIDBindingChecked(ctx context.Context, volunteerID types.ID, recordCID string, checkedAt time.Time) error
	// MarkDIDBindingCheckFailed records a failed re-verification attempt, escalating
	// the binding to STALE once consecutive failures reach staleAfter.
	MarkDIDBindingCheckFailed(ctx context.Context, volunteerID types.ID, checkedAt time.Time, staleAfter int) error
	// RevokeDIDBinding hard-revokes a binding (status REVOKED, terminal).
	RevokeDIDBinding(ctx context.Context, volunteerID types.ID, revokedAt time.Time) error
	// SetDIDFrozenUntil sets the post-key-rotation re-bind freeze deadline.
	SetDIDFrozenUntil(ctx context.Context, volunteerID types.ID, until time.Time) error
}
