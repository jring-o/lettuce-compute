package volunteer

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Host is one MACHINE running under a volunteer ACCOUNT (the Ed25519 keypair). A user
// runs the same key on every machine; the HEAD mints a random id for each machine at
// registration (BG-25 — clients never generate host ids), returns it in
// RegisterVolunteerResponse.host_id, and the client persists and echoes it thereafter.
// The per-machine facts that used to collide on the single volunteers row — advertised
// runtimes/hardware (the flapping-row bug), last-seen, the in-flight cap, the work-send
// floor — live here, one row per machine; credit/RAC/attestations and per-WU
// distinctness stay on the account (the volunteers row). See TODO #19 for the split and
// the BG-25 design for issuance.
type Host struct {
	ID                   types.ID             `json:"id"`
	VolunteerID          types.ID             `json:"volunteer_id"`
	DisplayName          *string              `json:"display_name,omitempty"`
	HardwareCapabilities HardwareCapabilities `json:"hardware_capabilities"`
	AvailableRuntimes    []string             `json:"available_runtimes"`
	IsActive             bool                 `json:"is_active"`
	LastSeenAt           *time.Time           `json:"last_seen_at,omitempty"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
}

// HostRepository is the data-access interface for per-machine host rows. It is separate
// from Repository (the account interface) so the host split touches none of the
// existing volunteer.Repository implementers.
type HostRepository interface {
	// Mint inserts a NEW host row under the per-account host cap (BG-25). h.ID must be
	// a fresh server-generated random id. capPerAccount <= 0 disables the cap (plain
	// insert). With a cap: the account's TOTAL host rows are hard-bounded — if the
	// account is at cap and at least one row is STALE (last_seen_at older than
	// activeWindow), the stalest row is DELETED in the same transaction and the new
	// row takes its slot; if all slots are recently active, nothing is inserted and
	// Mint returns (false, nil) — the refusal, not an error. Count/evict/insert
	// serialize per account via a row lock on the volunteers row, so two racing mints
	// admit exactly as many rows as the cap allows.
	Mint(ctx context.Context, h *Host, capPerAccount int, activeWindow time.Duration) (bool, error)
	// Upsert inserts the host or, if its id already exists, refreshes its per-machine
	// facts (display name, hardware, runtimes, active, last-seen). Used by the
	// echo-refresh path (a machine re-registering with its issued id); Mint is the
	// only path that creates NEW rows when the cap is enforced.
	Upsert(ctx context.Context, h *Host) error
	// GetByID returns a host by its issued id, or a NotFound apierror. The row carries
	// VolunteerID, so this is also the ownership oracle for work-path validation.
	GetByID(ctx context.Context, id types.ID) (*Host, error)
	// UpdateLastSeen bumps a host's last_seen_at/is_active without rewriting its
	// capabilities. Called (throttled) on the work path so a continuously working
	// machine never reads stale to the cap's eviction rule — load-bearing for BG-25's
	// "working machines are never evictable" property (audit F-A).
	UpdateLastSeen(ctx context.Context, id types.ID) error
}
