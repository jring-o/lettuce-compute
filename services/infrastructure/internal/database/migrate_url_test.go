package database

// Unit tests for the BG-28 migration-session URL builder: the migration
// connection (and ONLY it) must carry golang-migrate's x-statement-timeout so
// a wedged or runaway migration file aborts within the bound instead of
// freezing traffic — and it must NOT carry a server-side lock_timeout, which
// would also govern golang-migrate's pg_advisory_lock serialization call and
// crash-loop the non-winning replicas of a multi-replica boot whenever a
// migration outlives it (independent-review finding on the first cut of this
// change, which shipped `options=-c lock_timeout=5s`).

import (
	"net/url"
	"strconv"
	"testing"
)

func mustParseQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Query()
}

func TestMigrationSessionURL_AddsStatementTimeout(t *testing.T) {
	got, err := migrationSessionURL("postgres://user:pass@db.example.com:5432/lettuce?sslmode=require")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)

	if want := strconv.Itoa(migrationStatementTimeoutMs); q.Get("x-statement-timeout") != want {
		t.Errorf("x-statement-timeout = %q, want %q", q.Get("x-statement-timeout"), want)
	}
	// Pre-existing parameters must survive.
	if q.Get("sslmode") != "require" {
		t.Errorf("sslmode = %q, want %q (existing params must be preserved)", q.Get("sslmode"), "require")
	}
}

// TestMigrationSessionURL_NoSessionLockTimeout pins the multi-replica-boot
// fix: the builder must not smuggle a session-level lock_timeout (via the
// PostgreSQL `options` startup parameter or otherwise) onto the migration
// connection. golang-migrate's advisory serialization lock is acquired on that
// same session with a blocking, context-free call; a session lock_timeout
// aborts the non-winning replicas' healthy wait and crash-loops them until the
// winner finishes.
func TestMigrationSessionURL_NoSessionLockTimeout(t *testing.T) {
	got, err := migrationSessionURL("postgres://user:pass@db.example.com:5432/lettuce?sslmode=require")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)
	if opts := q.Get("options"); opts != "" {
		t.Errorf("migration URL sets options = %q; a session-level GUC here also governs the advisory serialization lock", opts)
	}
}

func TestMigrationSessionURL_RespectsExistingOverride(t *testing.T) {
	got, err := migrationSessionURL("postgres://u:p@localhost:5432/db?x-statement-timeout=5")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)
	if q.Get("x-statement-timeout") != "5" {
		t.Errorf("x-statement-timeout = %q, want caller override %q kept", q.Get("x-statement-timeout"), "5")
	}
}

func TestMigrationSessionURL_PreservesCallerOptions(t *testing.T) {
	got, err := migrationSessionURL("postgres://u:p@localhost:5432/db?options=-c%20search_path%3Dpublic")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)
	if q.Get("options") != "-c search_path=public" {
		t.Errorf("options = %q, want caller-supplied value untouched", q.Get("options"))
	}
}
