package database

// Unit tests for the BG-28 migration-session URL builder: the migration
// connection (and ONLY it) must carry fail-fast session timeouts so boot-time
// DDL cannot wedge behind traffic — lock_timeout via the PostgreSQL `options`
// startup parameter and golang-migrate's x-statement-timeout.

import (
	"fmt"
	"net/url"
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

func TestMigrationSessionURL_AddsTimeouts(t *testing.T) {
	got, err := migrationSessionURL("postgres://user:pass@db.example.com:5432/lettuce?sslmode=require")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)

	if want := fmt.Sprintf("%d", migrationStatementTimeoutMs); q.Get("x-statement-timeout") != want {
		t.Errorf("x-statement-timeout = %q, want %q", q.Get("x-statement-timeout"), want)
	}
	if want := "-c lock_timeout=" + migrationLockTimeout; q.Get("options") != want {
		t.Errorf("options = %q, want %q", q.Get("options"), want)
	}
	// Pre-existing parameters must survive.
	if q.Get("sslmode") != "require" {
		t.Errorf("sslmode = %q, want %q (existing params must be preserved)", q.Get("sslmode"), "require")
	}
}

func TestMigrationSessionURL_MergesExistingOptions(t *testing.T) {
	got, err := migrationSessionURL("postgres://u:p@localhost:5432/db?options=-c%20search_path%3Dpublic")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)
	if want := "-c search_path=public -c lock_timeout=" + migrationLockTimeout; q.Get("options") != want {
		t.Errorf("options = %q, want %q (existing options must be kept)", q.Get("options"), want)
	}
}

func TestMigrationSessionURL_RespectsExistingOverrides(t *testing.T) {
	got, err := migrationSessionURL("postgres://u:p@localhost:5432/db?x-statement-timeout=5&options=-c%20lock_timeout%3D1s")
	if err != nil {
		t.Fatalf("migrationSessionURL: %v", err)
	}
	q := mustParseQuery(t, got)
	if q.Get("x-statement-timeout") != "5" {
		t.Errorf("x-statement-timeout = %q, want caller override %q kept", q.Get("x-statement-timeout"), "5")
	}
	if q.Get("options") != "-c lock_timeout=1s" {
		t.Errorf("options = %q, want caller override %q kept", q.Get("options"), "-c lock_timeout=1s")
	}
}
