//go:build integration

package volunteer

// DB-backed integration tests for PgxHostRepository (BG-25 — server-issued host identity
// + the per-account host cap). They pin the load-bearing properties of the mint path: the
// hard cap on an account's TOTAL host rows, stale-slot eviction at mint time (the stalest
// slot is reclaimed, a recently-seen slot never is), exactness under concurrent mints (the
// volunteers-row lock serializes them), and the echo-refresh / last-seen-bump helpers.
//
// These reuse the shared volunteer integration harness (setupTestDB / newTestVolunteer in
// pgx-repo_test.go). setupTestDB's cleanup deletes the volunteers table, which cascades to
// hosts (the FK is ON DELETE CASCADE), but setupHostTestDB below also clears hosts at both
// ends so a leaked row can never bleed into the next serialized run. Like the rest of the
// suite these skip unless LETTUCE_TEST_DB_URL is set and must run with -p 1 (they share one
// database and DELETE-clean between runs).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// hostActiveWindow is the staleness threshold used across these tests: a host unseen for
// longer is evictable at mint time. 30 days mirrors the shipped default.
const hostActiveWindow = 30 * 24 * time.Hour

// setupHostTestDB wraps the shared setupTestDB helper, additionally clearing the hosts
// table at both ends. setupTestDB's own cleanup cascades hosts via the volunteers delete,
// but clearing explicitly keeps a host-scope test isolated even if that FK ever changes.
func setupHostTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	pool, base := setupTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM hosts"); err != nil {
		base()
		t.Fatalf("clean hosts: %v", err)
	}
	return pool, func() {
		_, _ = pool.Exec(ctx, "DELETE FROM hosts")
		base()
	}
}

// insertHostVolunteer creates one volunteer (account) row and returns its id. The mint path
// takes a FOR UPDATE lock on this row and the hosts FK references it, so a host test must
// create the account first.
func insertHostVolunteer(t *testing.T, pool *pgxpool.Pool) types.ID {
	t.Helper()
	v := newTestVolunteer()
	if err := NewPgxRepository(pool).Create(context.Background(), v); err != nil {
		t.Fatalf("create test volunteer: %v", err)
	}
	return v.ID
}

// newHostRow builds a fresh host row for volunteerID stamped with lastSeen. AvailableRuntimes
// is non-empty because the column is NOT NULL.
func newHostRow(volunteerID types.ID, lastSeen time.Time) *Host {
	ls := lastSeen
	return &Host{
		ID:                   types.NewID(),
		VolunteerID:          volunteerID,
		HardwareCapabilities: HardwareCapabilities{MaxCPUCores: 4},
		AvailableRuntimes:    []string{"NATIVE"},
		IsActive:             true,
		LastSeenAt:           &ls,
	}
}

// countHosts returns the number of host rows for volunteerID.
func countHosts(t *testing.T, pool *pgxpool.Pool, volunteerID types.ID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM hosts WHERE volunteer_id = $1", volunteerID).Scan(&n); err != nil {
		t.Fatalf("count hosts: %v", err)
	}
	return n
}

// hostExists reports whether a host row with id exists.
func hostExists(t *testing.T, pool *pgxpool.Pool, id types.ID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM hosts WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("host exists: %v", err)
	}
	return exists
}

// TestHostMint_CapExactness_ThirdRefused: at cap 2 with two recently-active hosts, the third
// mint is refused with (false, nil) — the refusal, not an error — and inserts nothing, so the
// account's total host rows never exceed the cap.
func TestHostMint_CapExactness_ThirdRefused(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)
	now := time.Now().UTC()

	h1 := newHostRow(vol, now)
	if ok, err := repo.Mint(ctx, h1, 2, hostActiveWindow); err != nil || !ok {
		t.Fatalf("first mint = (%v, %v), want (true, nil)", ok, err)
	}
	h2 := newHostRow(vol, now)
	if ok, err := repo.Mint(ctx, h2, 2, hostActiveWindow); err != nil || !ok {
		t.Fatalf("second mint = (%v, %v), want (true, nil)", ok, err)
	}

	h3 := newHostRow(vol, now)
	ok, err := repo.Mint(ctx, h3, 2, hostActiveWindow)
	if err != nil {
		t.Fatalf("third mint returned an error: %v", err)
	}
	if ok {
		t.Fatal("third mint at cap 2 (all slots recently active) should be refused (false), got true")
	}
	if n := countHosts(t, pool, vol); n != 2 {
		t.Errorf("host count = %d, want 2 (the hard cap holds)", n)
	}
	if hostExists(t, pool, h3.ID) {
		t.Error("a refused mint must not have inserted the third host row")
	}
}

// TestHostMint_ConcurrentAdmitsExactlyCap: many goroutines mint fresh hosts under one account
// at cap 3 with no stale slots. The volunteers-row FOR UPDATE lock serializes them, so exactly
// cap mints are admitted and exactly cap rows exist; the rest return the (false, nil) refusal.
func TestHostMint_ConcurrentAdmitsExactlyCap(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)
	now := time.Now().UTC()

	const capPerAccount = 3
	const goroutines = 8
	admitted := make([]bool, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h := newHostRow(vol, now)
			admitted[idx], errs[idx] = repo.Mint(ctx, h, capPerAccount, hostActiveWindow)
		}(i)
	}
	wg.Wait()

	minted := 0
	for i := range admitted {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if admitted[i] {
			minted++
		}
	}
	if minted != capPerAccount {
		t.Errorf("concurrent mints admitted = %d, want exactly cap %d", minted, capPerAccount)
	}
	if n := countHosts(t, pool, vol); n != capPerAccount {
		t.Errorf("host rows = %d, want exactly cap %d (the row lock serializes racing mints)", n, capPerAccount)
	}
}

// TestHostMint_EvictsStalePreservesRecent: at cap 2 with one STALE and one RECENT host, a
// third mint evicts the stale slot and inserts the new row, keeping the total at the cap; the
// recently-seen host is never evicted.
func TestHostMint_EvictsStalePreservesRecent(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)
	now := time.Now().UTC()

	stale := newHostRow(vol, now.Add(-40*24*time.Hour)) // unseen past the 30-day window
	if err := repo.Upsert(ctx, stale); err != nil {
		t.Fatalf("seed stale host: %v", err)
	}
	recent := newHostRow(vol, now)
	if err := repo.Upsert(ctx, recent); err != nil {
		t.Fatalf("seed recent host: %v", err)
	}

	fresh := newHostRow(vol, now)
	if ok, err := repo.Mint(ctx, fresh, 2, hostActiveWindow); err != nil || !ok {
		t.Fatalf("mint at cap with a stale slot = (%v, %v), want (true, nil)", ok, err)
	}
	if n := countHosts(t, pool, vol); n != 2 {
		t.Errorf("host count = %d, want 2 (evict-then-insert holds the cap)", n)
	}
	if hostExists(t, pool, stale.ID) {
		t.Error("the stale host should have been evicted")
	}
	if !hostExists(t, pool, recent.ID) {
		t.Error("the recently-seen host must be preserved (a working machine is never evicted)")
	}
	if !hostExists(t, pool, fresh.ID) {
		t.Error("the freshly minted host should have taken the reclaimed slot")
	}
}

// TestHostMint_EvictsOldestAmongMultipleStale: at cap 2 with BOTH hosts stale at different
// ages, the mint evicts the OLDEST (stalest) row and keeps the newer stale one — pinning the
// eviction order (ORDER BY last_seen_at ASC).
func TestHostMint_EvictsOldestAmongMultipleStale(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)
	now := time.Now().UTC()

	oldest := newHostRow(vol, now.Add(-50*24*time.Hour))
	if err := repo.Upsert(ctx, oldest); err != nil {
		t.Fatalf("seed oldest host: %v", err)
	}
	newerStale := newHostRow(vol, now.Add(-35*24*time.Hour))
	if err := repo.Upsert(ctx, newerStale); err != nil {
		t.Fatalf("seed newer-stale host: %v", err)
	}

	fresh := newHostRow(vol, now)
	if ok, err := repo.Mint(ctx, fresh, 2, hostActiveWindow); err != nil || !ok {
		t.Fatalf("mint at cap with two stale slots = (%v, %v), want (true, nil)", ok, err)
	}
	if n := countHosts(t, pool, vol); n != 2 {
		t.Errorf("host count = %d, want 2", n)
	}
	if hostExists(t, pool, oldest.ID) {
		t.Error("the OLDEST (stalest) host should have been evicted")
	}
	if !hostExists(t, pool, newerStale.ID) {
		t.Error("the newer of the two stale hosts must survive (only the stalest is evicted)")
	}
	if !hostExists(t, pool, fresh.ID) {
		t.Error("the freshly minted host should have been inserted")
	}
}

// TestHostMint_CapDisabledUnlimited: cap <= 0 disables the cap, so an account may hold an
// unbounded number of host rows (issuance stays server-owned, but no bound is enforced).
func TestHostMint_CapDisabledUnlimited(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)
	now := time.Now().UTC()

	const mints = 5
	for i := 0; i < mints; i++ {
		h := newHostRow(vol, now)
		if ok, err := repo.Mint(ctx, h, 0, hostActiveWindow); err != nil || !ok {
			t.Fatalf("mint %d with cap disabled = (%v, %v), want (true, nil)", i, ok, err)
		}
	}
	if n := countHosts(t, pool, vol); n != mints {
		t.Errorf("host count = %d, want %d (cap disabled admits every mint)", n, mints)
	}
}

// TestHostMint_MissingVolunteerNotFound: minting under an account that does not exist returns
// a NotFound apierror (the FOR UPDATE lock finds no volunteers row), not a silent success.
func TestHostMint_MissingVolunteerNotFound(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()

	h := newHostRow(types.NewID(), time.Now().UTC()) // account never created
	ok, err := repo.Mint(ctx, h, 5, hostActiveWindow)
	if ok {
		t.Fatal("mint under a missing volunteer must not succeed")
	}
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 404 {
		t.Fatalf("mint under a missing volunteer: err = %v, want a NotFound (404) apierror", err)
	}
}

// TestHostUpsert_EchoRefreshKeepsSameRow: re-upserting an existing host id lands on the SAME
// row (the echo-refresh path) and refreshes its per-machine facts rather than creating a
// second row.
func TestHostUpsert_EchoRefreshKeepsSameRow(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)
	now := time.Now().UTC()

	h := newHostRow(vol, now)
	if err := repo.Upsert(ctx, h); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	echo := &Host{
		ID:                   h.ID, // same issued id
		VolunteerID:          vol,
		HardwareCapabilities: HardwareCapabilities{MaxCPUCores: 8},
		AvailableRuntimes:    []string{"NATIVE", "CONTAINER"}, // changed capabilities
		IsActive:             true,
		LastSeenAt:           &now,
	}
	if err := repo.Upsert(ctx, echo); err != nil {
		t.Fatalf("echo upsert: %v", err)
	}
	if n := countHosts(t, pool, vol); n != 1 {
		t.Errorf("host count = %d, want 1 (echo refreshes the same row)", n)
	}
	got, err := repo.GetByID(ctx, h.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.AvailableRuntimes) != 2 {
		t.Errorf("available_runtimes = %v, want the refreshed two-runtime set", got.AvailableRuntimes)
	}
}

// TestHostUpdateLastSeen_Bumps: UpdateLastSeen advances last_seen_at (the eviction clock) and
// re-activates the host, without rewriting its capabilities.
func TestHostUpdateLastSeen_Bumps(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	ctx := context.Background()
	vol := insertHostVolunteer(t, pool)

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	h := newHostRow(vol, old)
	h.IsActive = false
	if err := repo.Upsert(ctx, h); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	if err := repo.UpdateLastSeen(ctx, h.ID); err != nil {
		t.Fatalf("UpdateLastSeen: %v", err)
	}
	got, err := repo.GetByID(ctx, h.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LastSeenAt == nil || !got.LastSeenAt.After(old) {
		t.Errorf("last_seen_at = %v, want a bump past the seeded %v", got.LastSeenAt, old)
	}
	if !got.IsActive {
		t.Error("UpdateLastSeen should re-activate the host (is_active = true)")
	}
}

// TestHostGetByID_NotFound: GetByID for an unissued id returns a NotFound apierror (the
// ownership oracle's definitive negative).
func TestHostGetByID_NotFound(t *testing.T) {
	pool, cleanup := setupHostTestDB(t)
	defer cleanup()

	repo := NewPgxHostRepository(pool)
	_, err := repo.GetByID(context.Background(), types.NewID())
	var apiErr *apierror.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 404 {
		t.Fatalf("GetByID(unknown) err = %v, want a NotFound (404) apierror", err)
	}
}
