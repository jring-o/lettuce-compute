//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/lettuce-compute/infrastructure/migrations"
)

// A unit's deadline now comes from the leaf's deadline_seconds, else the head's
// default (6 h); the head no longer reads deadline_multiplier or no_deadline.
// Migration 00032 must leave every stored leaf resolving to the deadline it had:
// a multiplier leaf gets its computed deadline_seconds, a no_deadline leaf keeps
// none (the head default is the ceiling it was stamped with), an explicit deadline
// is untouched, and the retired keys stay for a head still on the previous release.
func TestMigration00032_KeepsEveryLeafsDeadline(t *testing.T) {
	url := testDBURL(t)

	// Bring the schema to the version just BEFORE the backfill, exactly as a
	// pre-upgrade head left it. (Arriving from a later version runs 00032's down,
	// a declared no-op, so earlier data is untouched.)
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
	if err := m.Migrate(31); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		m.Close()
		t.Fatalf("migrating to version 31: %v", err)
	}
	m.Close()

	ctx := context.Background()
	pool, err := NewPool(ctx, testDBConfig(t))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// The stored shapes: each fleet leaf (read 2026-09-26) and the edges.
	type leafCase struct {
		slug   string
		stored string
		// wantDeadline is the deadline_seconds the leaf must carry afterwards; 0 = none.
		wantDeadline int64
		// before is the deadline the previous release resolved (0 = the 6 h ceiling
		// or, for a never-configured leaf, none it could activate with).
		before string
	}
	cases := []leafCase{
		{"deadline-grep", `{"no_deadline":true,"deadline_multiplier":3,"max_reassignments":3}`, 0, "21600 (ceiling)"},
		{"deadline-scios-pi", `{"no_deadline":false,"deadline_multiplier":3,"deadline_seconds":900,"max_reassignments":3}`, 900, "900"},
		{"deadline-beyblade", `{"no_deadline":false,"deadline_multiplier":5,"max_reassignments":3}`, 18000, "18000"},
		{"deadline-lbry-pi", `{"no_deadline":false,"deadline_multiplier":3,"max_reassignments":3}`, 10800, "10800"},
		{"deadline-fraction", `{"deadline_multiplier":0.5,"max_reassignments":3}`, 1800, "1800"},
		{"deadline-zero-explicit", `{"deadline_multiplier":2.5,"deadline_seconds":0,"max_reassignments":3}`, 9000, "9000"},
		{"deadline-no-deadline-with-seconds", `{"no_deadline":true,"deadline_multiplier":3,"deadline_seconds":7200,"max_reassignments":3}`, 0, "21600 (ceiling; deadline_seconds ignored)"},
		{"deadline-never-configured", `{"heartbeat_interval_seconds":0,"deadline_multiplier":0,"no_deadline":false,"max_reassignments":0}`, 0, "none"},
		{"deadline-empty", `{}`, 0, "none"},
		{"deadline-malformed", `{"deadline_multiplier":"three","deadline_seconds":"soon"}`, -1, "unloadable"},
	}
	slugs := make([]string, 0, len(cases))
	for _, c := range cases {
		slugs = append(slugs, c.slug)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM leafs WHERE slug = ANY($1)`, slugs)
	}
	cleanup() // clear leftovers from a previously failed run
	t.Cleanup(cleanup)

	for _, c := range cases {
		_, err := pool.Exec(ctx, `
			INSERT INTO leafs (
				name, slug, description, state, task_pattern,
				fault_tolerance_config, creator_public_key
			) VALUES (
				$1, $2, 'A deadline migration regression leaf', 'ACTIVE', 'PARAMETER_SWEEP',
				$3::jsonb, $4
			)`,
			"Deadline "+c.slug, c.slug, c.stored, []byte("deadline-test-key"),
		)
		if err != nil {
			t.Fatalf("inserting %s: %v", c.slug, err)
		}
	}

	if err := RunMigrations(url); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	for _, c := range cases {
		var deadlineType *string
		var deadline *float64
		err := pool.QueryRow(ctx, `
			SELECT jsonb_typeof(fault_tolerance_config->'deadline_seconds'),
			       CASE WHEN jsonb_typeof(fault_tolerance_config->'deadline_seconds') = 'number'
			            THEN (fault_tolerance_config->>'deadline_seconds')::float8 END
			FROM leafs WHERE slug = $1`, c.slug).Scan(&deadlineType, &deadline)
		if err != nil {
			t.Fatalf("reading %s back: %v", c.slug, err)
		}
		switch {
		case c.wantDeadline < 0:
			// Malformed values are skipped, not cast: the migration must not fail
			// the boot, and the row is left as it was.
			if deadlineType == nil || *deadlineType != "string" {
				t.Errorf("%s: malformed deadline_seconds was rewritten (type %v)", c.slug, deadlineType)
			}
		case c.wantDeadline == 0:
			if deadlineType != nil && *deadlineType != "null" {
				t.Errorf("%s: deadline_seconds = %s after migrations, want none so the head default applies (was %s)", c.slug, describe(deadline), c.before)
			}
		default:
			if deadline == nil || int64(*deadline) != c.wantDeadline {
				t.Errorf("%s: deadline_seconds = %s after migrations, want %d (was %s)", c.slug, describe(deadline), c.wantDeadline, c.before)
			}
		}
	}

	// The retired keys stay, so a head still on the previous release reads the
	// same deadlines during a rolling deploy.
	var kept int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM leafs WHERE slug = ANY($1) AND fault_tolerance_config ? 'deadline_multiplier'`,
		slugs).Scan(&kept); err != nil {
		t.Fatalf("counting retired keys: %v", err)
	}
	if want := len(cases) - 1; kept != want { // every case but deadline-empty stored one
		t.Errorf("%d leafs still carry deadline_multiplier, want %d (the migration must not strip it)", kept, want)
	}
}

func describe(deadline *float64) string {
	if deadline == nil {
		return "none"
	}
	return fmt.Sprintf("%g", *deadline)
}
