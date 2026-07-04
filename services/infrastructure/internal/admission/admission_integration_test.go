//go:build integration

package admission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// setupTestDB connects to the integration Postgres and returns a pool plus a cleanup. It
// empties the admission tables both up front (a prior aborted run may have left rows) and on
// teardown. The integration packages share one database and DELETE-clean between runs, so the
// suite must be run with -p 1; these tests never run in parallel with each other.
func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}

	clean := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM registration_creation_counts")
		_, _ = pool.Exec(ctx, "DELETE FROM registration_challenges")
	}
	cleanup := func() {
		clean()
		pool.Close()
	}
	clean()
	return pool, cleanup
}

// readCount returns the current-UTC-day creation count for a bucket, and whether the row
// exists at all. Absent (false) is distinct from present-but-zero because the rollback test
// needs to tell "increment never committed" from "committed as 0".
func readCount(t *testing.T, pool *pgxpool.Pool, bucket string) (int, bool) {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT created_count FROM registration_creation_counts
		 WHERE bucket = $1 AND day = (NOW() AT TIME ZONE 'utc')::date`, bucket).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read count for bucket %q: %v", bucket, err)
	}
	return count, true
}

// TestReserveCreationSlot_CountsAndCap runs cap+2 calls directly on the pool (no surrounding
// transaction) and checks that the first `cap` succeed and every call past the cap returns the
// sentinel. NOTE ON SEMANTICS: ReserveCreationSlot increments THEN checks, and the rollback
// that undoes an over-cap increment belongs to the CALLER's transaction. Called bare on the
// pool each statement auto-commits, so a refused increment persists — the stored count here
// therefore reflects every call, cap or not. The cap exactness that depends on rollback is
// covered by the transaction tests below.
func TestReserveCreationSlot_CountsAndCap(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const capPerDay = 3
	bucket := "203.0.113.10"

	for i := 1; i <= capPerDay; i++ {
		if err := ReserveCreationSlot(ctx, pool, bucket, capPerDay); err != nil {
			t.Fatalf("call %d (within cap %d) unexpected error: %v", i, capPerDay, err)
		}
	}
	for i := capPerDay + 1; i <= capPerDay+2; i++ {
		err := ReserveCreationSlot(ctx, pool, bucket, capPerDay)
		if !errors.Is(err, ErrCreationCapExceeded) {
			t.Fatalf("call %d (past cap %d) err = %v, want ErrCreationCapExceeded", i, capPerDay, err)
		}
	}

	// Bare-on-pool: no caller rollback, so all cap+2 increments are durable.
	count, ok := readCount(t, pool, bucket)
	if !ok {
		t.Fatal("counter row absent after reservations")
	}
	if want := capPerDay + 2; count != want {
		t.Errorf("created_count = %d, want %d (every auto-committed call counts)", count, want)
	}
}

// TestReserveCreationSlot_TransactionRollbackUndoesIncrement is the load-bearing cap-exactness
// property: an increment made inside a transaction that later rolls back leaves no trace, and a
// committed one persists as exactly 1.
func TestReserveCreationSlot_TransactionRollbackUndoesIncrement(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const capPerDay = 5
	bucket := "203.0.113.20"

	// Reserve inside a transaction, then roll back: the row must be absent (or 0).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := ReserveCreationSlot(ctx, tx, bucket, capPerDay); err != nil {
		t.Fatalf("reserve in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if count, ok := readCount(t, pool, bucket); ok && count != 0 {
		t.Errorf("after rollback created_count = %d (row present), want row absent or 0", count)
	}

	// Reserve in a fresh transaction and commit: the count must be exactly 1.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	if err := ReserveCreationSlot(ctx, tx2, bucket, capPerDay); err != nil {
		t.Fatalf("reserve in tx 2: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	count, ok := readCount(t, pool, bucket)
	if !ok || count != 1 {
		t.Errorf("after commit created_count = %d (present=%v), want 1", count, ok)
	}
}

// TestReserveCreationSlot_ConcurrentSameBucket exercises the row-lock serialization guarantee:
// with cap 5 and 10 concurrent registrations that commit on success and roll back on the
// sentinel, exactly 5 must commit and the durable count must settle at exactly 5. Each
// transaction's INSERT ... ON CONFLICT DO UPDATE takes the counter row lock, so the increments
// serialize even without an advisory lock.
func TestReserveCreationSlot_ConcurrentSameBucket(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		capPerDay = 5
		workers   = 10
	)
	bucket := "203.0.113.30"

	var committed, refused int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			err = ReserveCreationSlot(ctx, tx, bucket, capPerDay)
			if errors.Is(err, ErrCreationCapExceeded) {
				atomic.AddInt64(&refused, 1)
				_ = tx.Rollback(ctx)
				return
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				t.Errorf("reserve: %v", err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			atomic.AddInt64(&committed, 1)
		}()
	}
	wg.Wait()

	if committed != capPerDay {
		t.Errorf("committed reservations = %d, want %d", committed, capPerDay)
	}
	if refused != workers-capPerDay {
		t.Errorf("refused reservations = %d, want %d", refused, workers-capPerDay)
	}
	count, ok := readCount(t, pool, bucket)
	if !ok || count != capPerDay {
		t.Errorf("final created_count = %d (present=%v), want %d", count, ok, capPerDay)
	}
}

// TestReserveCreationSlot_DistinctBucketsIndependent checks that two buckets keep independent
// counters, and that the row's day column equals the database's own current UTC date.
func TestReserveCreationSlot_DistinctBucketsIndependent(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	const capPerDay = 5
	bucketA := "203.0.113.40"
	bucketB := "203.0.113.41"

	for i := 0; i < 2; i++ {
		if err := ReserveCreationSlot(ctx, pool, bucketA, capPerDay); err != nil {
			t.Fatalf("reserve A #%d: %v", i, err)
		}
	}
	if err := ReserveCreationSlot(ctx, pool, bucketB, capPerDay); err != nil {
		t.Fatalf("reserve B: %v", err)
	}

	if count, ok := readCount(t, pool, bucketA); !ok || count != 2 {
		t.Errorf("bucket A created_count = %d (present=%v), want 2", count, ok)
	}
	if count, ok := readCount(t, pool, bucketB); !ok || count != 1 {
		t.Errorf("bucket B created_count = %d (present=%v), want 1 (independent of A)", count, ok)
	}

	// The row's stored day is the database's current UTC date.
	var rowDay, dbDay time.Time
	if err := pool.QueryRow(ctx,
		`SELECT day FROM registration_creation_counts WHERE bucket = $1`, bucketA).Scan(&rowDay); err != nil {
		t.Fatalf("read row day: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT (NOW() AT TIME ZONE 'utc')::date`).Scan(&dbDay); err != nil {
		t.Fatalf("read db date: %v", err)
	}
	if !rowDay.Equal(dbDay) {
		t.Errorf("row day = %v, want the db current UTC date %v", rowDay, dbDay)
	}
}

// TestCounterSweeper_PrunesOldRows seeds rows aged 10 and 8 days (both past the 7-day retention
// window) plus a fresh row, then runs the sweeper. Because Start sweeps once immediately and
// then blocks on a 6h ticker, we run it in a goroutine, poll until the aged rows are gone, then
// cancel the context to unblock and return. The retention edge is `day < today - 7`, so the
// 10- and 8-day rows are pruned and today's row survives.
func TestCounterSweeper_PrunesOldRows(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	todayBucket := "203.0.113.50-today"
	for _, age := range []int{10, 8} {
		bucket := fmt.Sprintf("203.0.113.50-age-%d", age)
		if _, err := pool.Exec(ctx,
			`INSERT INTO registration_creation_counts (bucket, day, created_count)
			 VALUES ($1, (NOW() AT TIME ZONE 'utc')::date - $2::int, 1)`, bucket, age); err != nil {
			t.Fatalf("seed aged row (%d days): %v", age, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO registration_creation_counts (bucket, day, created_count)
		 VALUES ($1, (NOW() AT TIME ZONE 'utc')::date, 1)`, todayBucket); err != nil {
		t.Fatalf("seed today row: %v", err)
	}

	oldRowCount := func() int {
		var c int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM registration_creation_counts
			 WHERE day < (NOW() AT TIME ZONE 'utc')::date - 7`).Scan(&c); err != nil {
			t.Fatalf("count aged rows: %v", err)
		}
		return c
	}
	if got := oldRowCount(); got != 2 {
		t.Fatalf("seeded aged rows = %d, want 2", got)
	}

	sweeperCtx, cancel := context.WithCancel(ctx)
	sweeper := NewCounterSweeper(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		sweeper.Start(sweeperCtx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for oldRowCount() != 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("aged rows not pruned within timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if _, ok := readCount(t, pool, todayBucket); !ok {
		t.Error("today's row was pruned, want it intact (day is within retention)")
	}
}
