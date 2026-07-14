//go:build integration

package server

// Submit-door terminal-state check (design §4.1, review #2b, closes ★E1-6's second orphan
// window). A submit that arrives after its unit has already finalized (VALIDATED/FAILED) must
// NOT insert a PENDING result that would sit unadjudicated under a terminal unit forever.
// Instead the door closes the caller's still-live copy SUPERSEDED (non-punitive), commits so
// the supersede persists, and refuses the submit. These tests seed a finalized unit with a
// live copy and assert all three: refusal, copy SUPERSEDED, and no result row. Both the accept
// transaction and this submit transaction hold the unit row lock, so whichever commits second
// sees the truth — the door no longer relies on the post-commit ExpireLiveCopies supersede.
//
// The path runs inside the pool transaction, so it is only reachable with a real database:
// these are integration-tagged and SKIP without LETTUCE_TEST_DB_URL.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
)

// finalizeUnit flips a seeded unit to VALIDATED (a terminal state) so the submit door must
// refuse a late submit against it.
func finalizeUnit(t *testing.T, pool *pgxpool.Pool, wuID types.ID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE work_units SET state='VALIDATED', started_at=COALESCE(started_at, NOW()), completed_at=NOW() WHERE id=$1`,
		wuID); err != nil {
		t.Fatalf("finalize unit: %v", err)
	}
}

// assertCopySupersededNoResult asserts the door's two side effects: the caller's live copy is
// closed SUPERSEDED and NO result row was inserted for the unit.
func assertCopySupersededNoResult(t *testing.T, pool *pgxpool.Pool, wuID, volID types.ID) {
	t.Helper()
	ctx := context.Background()

	var outcome *string
	if err := pool.QueryRow(ctx,
		`SELECT outcome FROM work_unit_assignment_history WHERE work_unit_id=$1 AND volunteer_id=$2`,
		wuID, volID).Scan(&outcome); err != nil {
		t.Fatalf("query copy outcome: %v", err)
	}
	if outcome == nil || *outcome != string(assignment.OutcomeSuperseded) {
		t.Fatalf("live copy outcome = %v, want SUPERSEDED (the door must close the copy)", outcome)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM results WHERE work_unit_id=$1`, wuID).Scan(&n); err != nil {
		t.Fatalf("count results: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected NO result row for a submit against a finalized unit, got %d", n)
	}
}

// TestSubmitDoor_TerminalUnitSupersedesCopy is the closeout refutation target for the gRPC
// submit door: a submit against a VALIDATED unit with a live copy is refused, the copy is
// closed SUPERSEDED, and no result row is written.
func TestSubmitDoor_TerminalUnitSupersedesCopy(t *testing.T) {
	pool := newBG17SingleConnPool(t)
	_, wuID, vol, pub := bg17Seed(t, pool)
	bg17OpenCopy(t, pool, wuID, vol.ID) // a still-live copy for this volunteer
	finalizeUnit(t, pool, wuID)         // the unit finalized while the copy was still in flight

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc := NewVolunteerService(pool, "door-test", time.Now(),
		volunteer.NewPgxRepository(pool),
		workunit.NewPgxWorkUnitRepository(pool),
		leaf.NewPgxRepository(pool),
		assignment.NewPgxRepository(pool),
		result.NewPgxRepository(pool),
		workunit.NewPgxBatchRepository(pool),
		nil, // checkpointRepo: unused by this path
		nil, // validationEngine: the door returns before any post-commit evaluate
		logger, transition.TrustPolicy{})

	output := []byte(`{"answer":42}`)
	sum := sha256.Sum256(output)
	req := &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID.String(),
		VolunteerId:          vol.ID.String(),
		PublicKey:            pub,
		OutputData:           output,
		OutputChecksumSha256: hex.EncodeToString(sum[:]),
		Metadata:             &lettucev1.ExecutionMetadata{WallClockSeconds: 1},
	}

	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)
	if _, err := svc.SubmitResult(ctx, req); err == nil {
		t.Fatal("expected a submit against a finalized unit to be refused, got nil error")
	} else if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", codeOf(err), err)
	} else if !strings.Contains(err.Error(), "already finalized") {
		t.Fatalf("expected an 'already finalized' refusal, got: %v", err)
	}

	assertCopySupersededNoResult(t, pool, wuID, vol.ID)
}

// TestSubmitDoor_Browser_TerminalUnitSupersedesCopy is the same refutation on the browser/WASM
// submit surface: a 409 Conflict, the live copy SUPERSEDED, and no result row.
func TestSubmitDoor_Browser_TerminalUnitSupersedesCopy(t *testing.T) {
	pool := newBG17SingleConnPool(t)
	_, wuID, vol, pub := bg17Seed(t, pool)
	bg17OpenCopy(t, pool, wuID, vol.ID)
	finalizeUnit(t, pool, wuID)

	handler := handleBrowserSubmitResult(bg17BrowserDeps(pool))

	output := []byte(`{"answer":42}`)
	sum := sha256.Sum256(output)
	body := `{"work_unit_id":"` + wuID.String() + `","output_data":"` +
		base64.StdEncoding.EncodeToString(output) + `","output_checksum":"` +
		hex.EncodeToString(sum[:]) + `","metrics":{"wall_clock_seconds":1}}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/submit-result", strings.NewReader(body))
	req = req.WithContext(ContextWithEd25519PubKey(req.Context(), pub))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for a submit against a finalized unit, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already finalized") {
		t.Fatalf("expected an 'already finalized' refusal, got: %s", rec.Body.String())
	}

	assertCopySupersededNoResult(t, pool, wuID, vol.ID)
}
