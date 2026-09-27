//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// insertWorkStatusUnit inserts a QUEUED work unit on leafID with no copy.
func insertWorkStatusUnit(t *testing.T, pool *pgxpool.Pool, leafID types.ID) types.ID {
	t.Helper()
	id := types.NewID()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO work_units (id, leaf_id, state, priority, input_data, code_artifact_ref,
			parameters, estimated_duration_seconds, deadline_seconds, reassignment_count,
			max_reassignments, flagged_for_review)
		VALUES ($1, $2, 'QUEUED', 'NORMAL', '{"x": 1}', 'ref://test', '{}', 300, 3600, 0, 3, false)`,
		id, leafID,
	)
	if err != nil {
		t.Fatalf("insert work unit: %v", err)
	}
	return id
}

// insertWorkStatusResult records volID's result on a new unit of leafID in the
// given validation state.
func insertWorkStatusResult(t *testing.T, pool *pgxpool.Pool, leafID, volID types.ID, state string) {
	t.Helper()
	wuID := insertWorkStatusUnit(t, pool, leafID)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO results (work_unit_id, volunteer_id, output_data, output_checksum,
			execution_metadata, validation_status)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		wuID, volID, json.RawMessage(`{"x":1}`),
		"abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		json.RawMessage(`{"wall_clock_seconds":1}`), state,
	)
	if err != nil {
		t.Fatalf("insert %s result: %v", state, err)
	}
}

// insertWorkStatusCopy gives volID a copy of a new unit of leafID. outcome "" leaves
// the copy open; started sets started_at, as the head does at run start.
func insertWorkStatusCopy(t *testing.T, pool *pgxpool.Pool, leafID, volID types.ID, outcome string, started bool) {
	t.Helper()
	wuID := insertWorkStatusUnit(t, pool, leafID)
	now := time.Now().UTC()
	var outcomeArg, outcomeAt, startedAt any
	if outcome != "" {
		outcomeArg, outcomeAt = outcome, now
	}
	if started {
		startedAt = now.Add(-time.Minute)
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO work_unit_assignment_history (work_unit_id, volunteer_id, assigned_at,
			reserved_until, started_at, deadline_seconds, outcome, outcome_at)
		VALUES ($1, $2, $3, $4, $5, 3600, $6, $7)`,
		wuID, volID, now.Add(-2*time.Minute), now.Add(time.Hour), startedAt, outcomeArg, outcomeAt,
	)
	if err != nil {
		t.Fatalf("insert copy (outcome %q, started %v): %v", outcome, started, err)
	}
}

func workStatusByLeaf(t *testing.T, resp *lettucev1.GetMyContributionResponse) map[string]*lettucev1.LeafWorkStatus {
	t.Helper()
	if resp.GetWorkStatus() == nil {
		t.Fatal("work_status is unset: the head did not report the account's results by state")
	}
	out := map[string]*lettucev1.LeafWorkStatus{}
	for _, ls := range resp.GetWorkStatus().GetByLeaf() {
		out[ls.GetLeafId()] = ls
	}
	return out
}

// wantLeafWorkStatus compares every count on one leaf.
func wantLeafWorkStatus(t *testing.T, who string, got, want *lettucev1.LeafWorkStatus) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: leaf %s missing from work_status", who, want.GetLeafId())
		return
	}
	checks := []struct {
		name      string
		got, want int32
	}{
		{"results_pending", got.GetResultsPending(), want.GetResultsPending()},
		{"results_agreed", got.GetResultsAgreed(), want.GetResultsAgreed()},
		{"results_disagreed", got.GetResultsDisagreed(), want.GetResultsDisagreed()},
		{"results_awaiting_content_verification", got.GetResultsAwaitingContentVerification(), want.GetResultsAwaitingContentVerification()},
		{"results_content_verification_failed", got.GetResultsContentVerificationFailed(), want.GetResultsContentVerificationFailed()},
		{"results_superseded", got.GetResultsSuperseded(), want.GetResultsSuperseded()},
		{"runs_stopped", got.GetRunsStopped(), want.GetRunsStopped()},
		{"copies_running", got.GetCopiesRunning(), want.GetCopiesRunning()},
		{"copies_waiting_to_start", got.GetCopiesWaitingToStart(), want.GetCopiesWaitingToStart()},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s, leaf %s: %s = %d, want %d", who, want.GetLeafId(), c.name, c.got, c.want)
		}
	}
	if got.GetLeafName() == "" {
		t.Errorf("%s, leaf %s: leaf_name is empty", who, want.GetLeafId())
	}
}

// TestGetMyContributionReportsResultsByStateAndCopiesInProgress gives one account
// results in every validation state and copies in every relevant stage, on a
// public and a PRIVATE leaf, and a second account work of its own. Each account
// sees exactly its own counts, per leaf, PRIVATE leaf included; a copy stopped
// before it started is not a run; an unregistered key gets a reported, empty
// status; and an unsigned call is refused.
func TestGetMyContributionReportsResultsByStateAndCopiesInProgress(t *testing.T) {
	pool, client, cleanup := setupCheckpointServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	userID := createCPTestUser(t, pool)
	public := createCPTestProject(t, pool, &userID, false)
	private := createCPTestProject(t, pool, &userID, false)
	otherLeaf := createCPTestProject(t, pool, &userID, false)
	if _, err := pool.Exec(ctx, `UPDATE leafs SET visibility = 'PRIVATE' WHERE id = $1`, private); err != nil {
		t.Fatalf("make leaf private: %v", err)
	}

	kA := newCPTestKeyPair(t)
	kB := newCPTestKeyPair(t)
	volA := insertContribVolunteer(t, pool, kA.pub)
	volB := insertContribVolunteer(t, pool, kB.pub)

	// Account A on the public leaf: one or more results in every state ...
	for state, n := range map[string]int{
		"PENDING": 3, "AGREED": 2, "DISAGREED": 1, "AWAITING_CONTENT_VERIFICATION": 1,
		"CONTENT_VERIFICATION_FAILED": 1, "SUPERSEDED": 1,
	} {
		for i := 0; i < n; i++ {
			insertWorkStatusResult(t, pool, public, volA, state)
		}
	}
	// ... and copies: two stopped mid-run, one stopped before it started (never
	// ran), one running, two waiting to start, and closed copies that are none of
	// these.
	insertWorkStatusCopy(t, pool, public, volA, "SUPERSEDED", true)
	insertWorkStatusCopy(t, pool, public, volA, "SUPERSEDED", true)
	insertWorkStatusCopy(t, pool, public, volA, "SUPERSEDED", false)
	insertWorkStatusCopy(t, pool, public, volA, "", true)
	insertWorkStatusCopy(t, pool, public, volA, "", false)
	insertWorkStatusCopy(t, pool, public, volA, "", false)
	insertWorkStatusCopy(t, pool, public, volA, "COMPLETED", true)
	insertWorkStatusCopy(t, pool, public, volA, "EXPIRED", true)
	insertWorkStatusCopy(t, pool, public, volA, "RETURNED", false)

	// Account A on the PRIVATE leaf.
	insertWorkStatusResult(t, pool, private, volA, "PENDING")
	insertWorkStatusCopy(t, pool, private, volA, "", true)

	// Account B: work on the same public leaf and on a leaf A never touched.
	insertWorkStatusResult(t, pool, public, volB, "PENDING")
	insertWorkStatusResult(t, pool, public, volB, "AGREED")
	insertWorkStatusCopy(t, pool, public, volB, "SUPERSEDED", true)
	insertWorkStatusCopy(t, pool, public, volB, "", true)
	insertWorkStatusResult(t, pool, otherLeaf, volB, "DISAGREED")
	insertWorkStatusCopy(t, pool, otherLeaf, volB, "", false)

	respA, err := client.GetMyContribution(signCP(ctx, kA), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution(A): %v", err)
	}
	gotA := workStatusByLeaf(t, respA)
	if len(gotA) != 2 {
		t.Errorf("A: work_status has %d leaves, want 2 (public and PRIVATE; not B's other leaf)", len(gotA))
	}
	wantLeafWorkStatus(t, "A", gotA[public.String()], &lettucev1.LeafWorkStatus{
		LeafId: public.String(), ResultsPending: 3, ResultsAgreed: 2, ResultsDisagreed: 1,
		ResultsAwaitingContentVerification: 1, ResultsContentVerificationFailed: 1, ResultsSuperseded: 1,
		RunsStopped: 2, CopiesRunning: 1, CopiesWaitingToStart: 2,
	})
	wantLeafWorkStatus(t, "A", gotA[private.String()], &lettucev1.LeafWorkStatus{
		LeafId: private.String(), ResultsPending: 1, CopiesRunning: 1,
	})

	respB, err := client.GetMyContribution(signCP(ctx, kB), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution(B): %v", err)
	}
	gotB := workStatusByLeaf(t, respB)
	if len(gotB) != 2 {
		t.Errorf("B: work_status has %d leaves, want 2", len(gotB))
	}
	wantLeafWorkStatus(t, "B", gotB[public.String()], &lettucev1.LeafWorkStatus{
		LeafId: public.String(), ResultsPending: 1, ResultsAgreed: 1, RunsStopped: 1, CopiesRunning: 1,
	})
	wantLeafWorkStatus(t, "B", gotB[otherLeaf.String()], &lettucev1.LeafWorkStatus{
		LeafId: otherLeaf.String(), ResultsDisagreed: 1, CopiesWaitingToStart: 1,
	})

	// A key with no volunteer row on this head: reported, and empty.
	respU, err := client.GetMyContribution(signCP(ctx, newCPTestKeyPair(t)), &lettucev1.GetMyContributionRequest{})
	if err != nil {
		t.Fatalf("GetMyContribution(unregistered): %v", err)
	}
	if got := workStatusByLeaf(t, respU); len(got) != 0 {
		t.Errorf("unregistered key: work_status has %d leaves, want 0", len(got))
	}

	// The RPC stays authenticated: an unsigned call is refused.
	if _, err := client.GetMyContribution(ctx, &lettucev1.GetMyContributionRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("unsigned GetMyContribution: err = %v, want Unauthenticated", err)
	}
}
