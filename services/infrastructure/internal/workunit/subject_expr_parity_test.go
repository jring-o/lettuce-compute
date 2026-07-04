//go:build integration

package workunit

import (
	"context"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// TestSubjectExprSQL_MatchesSubjectForVolunteer pins the two twins of the account-level
// trust subject against each other: the Go source of truth trust.SubjectForVolunteer,
// which validation and the in-memory dispatch predicate use, and subjectExprSQL, the SQL
// expression every dispatch site in this package embeds. For each binding state it
// creates a volunteer row, fetches the Go model, computes the Go subject, then asks the
// DB for the subject the shared SQL expression yields for the same row and asserts they
// are byte-identical. A change to EITHER side that drifts from the other fails here, so
// the SQL dispatch distinctness can never silently diverge from the validation-side
// subject the two must agree on.
func TestSubjectExprSQL_MatchesSubjectForVolunteer(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	did := func(s string) *string { return &s }

	cases := []struct {
		name   string
		did    *string // nil => did column NULL (unbound)
		status *string // nil => did_binding_status NULL
	}{
		{"unbound_did_null", nil, nil},
		{"bound_ok", did("did:plc:golden-ok"), did(volunteer.DIDBindingStatusOK)},
		{"bound_stale", did("did:plc:golden-stale"), did(volunteer.DIDBindingStatusStale)},
		{"bound_revoked", did("did:plc:golden-revoked"), did(volunteer.DIDBindingStatusRevoked)},
		// did = '' with a live status is the empty-string edge: the subject must fall
		// back to the sentinel, NOT the empty DID, on both sides.
		{"empty_did_ok", did(""), did(volunteer.DIDBindingStatusOK)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cleanParityTables(t, pool)
			volID := createTestVolunteer(t, pool)
			if _, err := pool.Exec(ctx,
				`UPDATE volunteers SET did = $2, did_binding_status = $3 WHERE id = $1`,
				volID, tc.did, tc.status); err != nil {
				t.Fatalf("set DID columns: %v", err)
			}

			// The fetched Go model, carrying exactly the fields trust.SubjectForVolunteer reads.
			v := volunteer.Volunteer{ID: volID}
			if err := pool.QueryRow(ctx,
				`SELECT did, did_binding_status FROM volunteers WHERE id = $1`, volID).
				Scan(&v.DID, &v.DIDBindingStatus); err != nil {
				t.Fatalf("fetch volunteer: %v", err)
			}
			goSubject := trust.SubjectForVolunteer(&v)

			// The subject the DB computes via the shared expression for the same row.
			var sqlSubject string
			if err := pool.QueryRow(ctx,
				`SELECT `+subjectExprSQL("v")+` FROM volunteers v WHERE v.id = $1`, volID).
				Scan(&sqlSubject); err != nil {
				t.Fatalf("compute SQL subject: %v", err)
			}

			if goSubject != sqlSubject {
				t.Fatalf("subject twin drift (%s): trust.SubjectForVolunteer=%q, subjectExprSQL=%q\n"+
					"The Go rule and the SQL expression must stay identical — update both together.",
					tc.name, goSubject, sqlSubject)
			}
		})
	}
}
