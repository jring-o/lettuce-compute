// Package admission implements the registration admission gates (design: registration
// admission — per-IP creation caps + proof-of-work): the durable per-(IP bucket, UTC day)
// creation cap, and (in a follow-up) the registration proof-of-work challenge store. The
// gates apply ONLY to the create branch of volunteer registration — re-registration of an
// existing key never pays admission cost — and only while the corresponding
// LETTUCE_HEAD_REGISTRATION_* knob is enabled. Admission cost is a treadmill slower, never
// load-bearing: the load-bearing anti-abuse mechanisms are the trust gate and account
// standing, not this package.
package admission

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the minimal pgx surface the admission gates need (satisfied by *pgxpool.Pool and
// a pgx.Tx), mirroring standing.DBTX: the creation-cap increment MUST ride the same
// transaction as the volunteer INSERT so the counter and the row commit or roll back
// together.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CapPolicy is the head's creation-cap configuration, a plain struct (no config-package
// dependency, per the HeadDispatchConfig precedent) filled by main.go/router from
// HeadConfig.Effective* values. The zero value is the deploy-safety default: gate off.
type CapPolicy struct {
	// Enabled turns the per-IP-per-day creation cap on. Default false: registration
	// behaves exactly as before this package existed.
	Enabled bool
	// PerDay is the maximum number of volunteer rows a single IP bucket may create per
	// UTC day. Callers pass the EFFECTIVE (already-defaulted) value.
	PerDay int
}

// CreateGate carries the per-request admission inputs into the transactional create path
// (volunteer.Repository.CreateAdmitted). nil gate = no admission checks = exactly the
// legacy single-statement create. The two gates are orthogonal: CapPerDay <= 0 skips the
// creation-cap check (proof-of-work may be enforced with the cap off), and a nil Pow
// skips the challenge redemption (the cap may be enforced with proof-of-work off).
type CreateGate struct {
	// Bucket is the client's IP bucket (BucketForIP). Consulted only when CapPerDay > 0.
	Bucket string
	// CapPerDay is the effective per-bucket daily creation cap; <= 0 = cap not enforced.
	CapPerDay int
	// Pow, when non-nil, is the proof-of-work solution to redeem (single-use, inside the
	// same transaction) before the volunteer row may be created.
	Pow *PowRedemption
}

// ErrCreationCapExceeded is returned by ReserveCreationSlot when the bucket has already
// created its daily quota of volunteers. Handlers map it to the pinned refusal contract
// (gRPC FailedPrecondition + CapExceededMessage; REST 429).
var ErrCreationCapExceeded = errors.New("registration creation cap exceeded")

// CapExceededMessage is the pinned client-facing refusal text for a cap-exceeded
// registration. It deliberately avoids every word the volunteer CLI's
// IsVolunteerTooOldError classifier matches ("too old", "outdated", "version" + friends),
// so a capped registration is never misreported as a client-version problem.
const CapExceededMessage = "daily volunteer registration limit reached for this network; try again later"

// BucketForIP maps a client IP string to its cap bucket: the address itself for IPv4
// (IPv4-mapped IPv6 is unmapped first), the /64 prefix for IPv6 — a single IPv6 host
// trivially holds a whole /64, so per-address IPv6 buckets would be void. An unparseable
// input (e.g. the "unknown" sentinel the gRPC helper yields when there is no peer) is an
// error; callers fail closed while the gate is enabled.
func BucketForIP(ip string) (string, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("unbucketable client ip %q: %w", ip, err)
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String(), nil
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return "", fmt.Errorf("unbucketable client ip %q: %w", ip, err)
	}
	return prefix.String(), nil
}

// ReserveCreationSlot atomically increments the (bucket, UTC day) creation counter and
// enforces the cap, returning ErrCreationCapExceeded when the increment lands past it.
// It MUST run inside the same transaction as the volunteer INSERT: on any refusal or
// later failure the rollback undoes the increment, so the counter counts exactly the
// creations that committed. Concurrent same-bucket registrations serialize on the
// counter row lock, so the cap is exact across replicas with no advisory lock.
func ReserveCreationSlot(ctx context.Context, db DBTX, bucket string, capPerDay int) error {
	var count int
	err := db.QueryRow(ctx, `
		INSERT INTO registration_creation_counts (bucket, day, created_count)
		VALUES ($1, (NOW() AT TIME ZONE 'utc')::date, 1)
		ON CONFLICT (bucket, day) DO UPDATE
			SET created_count = registration_creation_counts.created_count + 1
		RETURNING created_count`,
		bucket,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to reserve creation slot: %w", err)
	}
	if count > capPerDay {
		return ErrCreationCapExceeded
	}
	return nil
}
