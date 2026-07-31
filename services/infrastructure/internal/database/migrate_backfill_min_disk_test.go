//go:build integration

package database

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lettuce-compute/infrastructure/migrations"
)

// TB-31 regression (head half): leafs created before the head defaulted
// min_disk_mb at creation (ApplyResourceRequirementsDefaults) carry no value,
// so every volunteer substitutes its client-side unknown-need fallback for
// them — the collision that silently disk-gated default-configured volunteers
// on both lbry Beyblade leaves. Migration 00029 must stamp the same 1024 MB
// the creation path uses into every leaf whose min_disk_mb is absent or
// non-positive, and must leave declared values alone.
func TestMigration00029_BackfillsMinDiskMB(t *testing.T) {
	url := testDBURL(t)

	// Bring the schema to the version just BEFORE the backfill, exactly as a
	// pre-upgrade head left it. (Arriving from a later version runs 00029's
	// down, a declared no-op, so earlier data is untouched.)
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	sessionURL, err := migrationSessionURL(url)
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, sessionURL)
	if err != nil {
		t.Fatalf("creating migrator: %v", err)
	}
	if err := m.Migrate(28); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		m.Close()
		t.Fatalf("migrating to version 28: %v", err)
	}
	m.Close()

	ctx := context.Background()
	pool, err := NewPool(ctx, testDBConfig(t))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	slugs := []string{"tb31-legacy-empty", "tb31-explicit-zero", "tb31-declared"}
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM leafs WHERE slug = ANY($1)`, slugs)
	}
	cleanup() // clear leftovers from a previously failed run
	t.Cleanup(cleanup)

	insert := func(slug, resourceRequirements string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO leafs (
				name, slug, description, state, task_pattern,
				resource_requirements, creator_public_key
			) VALUES (
				$1, $2, 'A TB-31 migration regression leaf', 'ACTIVE', 'PARAMETER_SWEEP',
				$3::jsonb, $4
			)`,
			"TB-31 "+slug, slug, resourceRequirements, []byte("tb31-test-key"),
		)
		if err != nil {
			t.Fatalf("inserting %s: %v", slug, err)
		}
	}
	// The lbry Beyblade shape (created on v0.10.2, before the creation-time
	// default existed), an explicit zero, and an honestly declared value.
	insert("tb31-legacy-empty", `{}`)
	insert("tb31-explicit-zero", `{"min_cpu_cores":1,"min_disk_mb":0}`)
	insert("tb31-declared", `{"min_cpu_cores":1,"min_disk_mb":4096}`)

	if err := RunMigrations(url); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	want := map[string]int64{
		"tb31-legacy-empty":  1024,
		"tb31-explicit-zero": 1024,
		"tb31-declared":      4096,
	}
	for slug, wantMB := range want {
		var got int64
		err := pool.QueryRow(ctx,
			`SELECT COALESCE((resource_requirements->>'min_disk_mb')::bigint, 0) FROM leafs WHERE slug = $1`,
			slug).Scan(&got)
		if err != nil {
			t.Fatalf("reading %s back: %v", slug, err)
		}
		if got != wantMB {
			t.Errorf("%s: min_disk_mb = %d after migrations, want %d — legacy leafs keep forcing every volunteer onto the client-side fallback (TB-31)", slug, got, wantMB)
		}
	}
}
