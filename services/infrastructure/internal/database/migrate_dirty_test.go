//go:build integration

package database

// Regression test for BG-28: a schema left dirty by a failed migration used to
// crash-loop the head with only golang-migrate's terse "Dirty database version
// N. Fix and force version." — no recovery procedure, no runbook. RunMigrations
// must now return an actionable operator error naming the dirty version, the
// exact repair SQL, and the guides/migrations.md runbook.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunMigrations_DirtySchemaActionableError(t *testing.T) {
	url := testDBURL(t)
	if err := RunMigrations(url); err != nil {
		t.Fatalf("baseline RunMigrations: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var version int
	if err := pool.QueryRow(ctx, "SELECT version FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}

	// Simulate a previously failed migration attempt.
	if _, err := pool.Exec(ctx, "UPDATE schema_migrations SET dirty = true"); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	// Un-wedge the shared test database for whatever runs after this test.
	defer func() {
		if _, err := pool.Exec(ctx, "UPDATE schema_migrations SET dirty = false"); err != nil {
			t.Errorf("restore dirty=false: %v", err)
		}
	}()

	err = RunMigrations(url)
	if err == nil {
		t.Fatal("RunMigrations succeeded on a dirty schema; want an actionable error")
	}
	msg := err.Error()
	for _, want := range []string{
		fmt.Sprintf("DIRTY at version %d", version),
		"UPDATE schema_migrations",
		"dirty = false",
		"guides/migrations.md",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("dirty-schema error is missing %q; full message:\n%s", want, msg)
		}
	}
}
