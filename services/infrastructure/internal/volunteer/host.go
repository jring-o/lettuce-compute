package volunteer

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Host is one MACHINE running under a volunteer ACCOUNT (the Ed25519 keypair). A user
// runs the same key on every machine; each machine self-generates a stable host key,
// and the head derives a deterministic host id from (account id, host key). The
// per-machine facts that used to collide on the single volunteers row — advertised
// runtimes/hardware (the flapping-row bug), last-seen, the in-flight cap, the work-send
// floor — live here, one row per machine; credit/RAC/attestations and per-WU
// distinctness stay on the account (the volunteers row). See TODO #19.
type Host struct {
	ID                   types.ID             `json:"id"`
	VolunteerID          types.ID             `json:"volunteer_id"`
	HostKey              string               `json:"host_key"`
	DisplayName          *string              `json:"display_name,omitempty"`
	HardwareCapabilities HardwareCapabilities `json:"hardware_capabilities"`
	AvailableRuntimes    []string             `json:"available_runtimes"`
	IsActive             bool                 `json:"is_active"`
	LastSeenAt           *time.Time           `json:"last_seen_at,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
}

// HostRepository is the data-access interface for per-machine host rows. It is separate
// from Repository (the account interface) so adding the host split touches none of the
// existing volunteer.Repository implementers.
type HostRepository interface {
	// Upsert inserts the host or, if its id already exists, refreshes its per-machine
	// facts (display name, hardware, runtimes, active, last-seen). h.ID must be the
	// head's effective host id for (h.VolunteerID, h.HostKey).
	Upsert(ctx context.Context, h *Host) error
	// GetByID returns a host by its (effective) id, or a NotFound apierror.
	GetByID(ctx context.Context, id types.ID) (*Host, error)
	// UpdateLastSeen bumps a host's last_seen_at/is_active without rewriting its
	// capabilities. Best-effort liveness touch on the work-request path.
	UpdateLastSeen(ctx context.Context, id types.ID) error
}
