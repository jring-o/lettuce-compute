//go:build integration

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// PB-16 regression, end to end: a volunteer that pins an UNLISTED leaf by id must
// receive its work on a head that has NO public backlog.
//
// This is the filed shape, and it is the shape every earlier test missed. The
// selection SQL was already correct (internal/workunit/visibility_dispatch_test.go)
// and so was the in-memory hand-out (TestHandOut_NonPublicLeafOnlyToPinnedRequester),
// but gRPC RequestWorkUnit serves ONLY from the in-process dispatch cache, and nothing
// staged the pinned leaf's units into it: the global watermark refill stages PUBLIC
// leafs only (the PB-38 visibility gate), and the on-demand leaf-scoped refill that
// would stage a pinned non-PUBLIC leaf was gated on the ready pool being non-empty. On
// a head whose only ACTIVE leaf is UNLISTED the pool is permanently empty, so the units
// sat QUEUED forever. Any co-resident PUBLIC leaf with queued work fills the pool and
// hides the defect completely — so this test deliberately creates NO public leaf.
func TestPB16_UnlistedOnlyHead_PinnedVolunteerReceivesWork(t *testing.T) {
	env, cleanup := setupHeadsLeafsServerWithCache(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "pb16-unlisted")
	opts := hlDefaultLeafOpts("PB-16 Unlisted Only Leaf")
	opts.Visibility = leaf.VisibilityUnlisted
	lf := createHLLeaf(t, env, ctx, userID, opts)
	generateLeafWUs(t, env, lf.ID, 4)

	// The head's catalog does not advertise the leaf — the volunteer can only reach it
	// by pinning the id it was given out of band (`lettuce-volunteer attach <leaf-id>`).
	for _, advertised := range getHeadInfo(t, env).Leafs {
		if advertised.ID == lf.ID.String() {
			t.Fatalf("an UNLISTED leaf must not appear in the head catalog (PB-38)")
		}
	}

	pubKey := genVolunteerKey(t)
	volID := registerHLVolunteer(t, env, ctx, pubKey, "pb16-vol")

	// Control (PB-38 must keep holding): the any-leaf fallback — a request with NO leaf
	// filter, which is what an un-pinned volunteer sends — is never served this leaf, no
	// matter how long it polls or what the pinned path stages into the shared pool.
	anyKey := genVolunteerKey(t)
	anyVolID := registerHLVolunteer(t, env, ctx, anyKey, "pb16-anyleaf-vol")
	for i := 0; i < 5; i++ {
		requestWUExpectNone(t, env, ctx, anyVolID, anyKey, nil)
		time.Sleep(100 * time.Millisecond)
	}

	// The filed capability: the pinned volunteer polls, and the head must converge on
	// serving it. Pre-fix this loop expires having received nothing — the live repro
	// polled for ~9 minutes across two daemon restarts.
	var asg *lettucev1.WorkUnitAssignment
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{
			VolunteerId:    volID,
			PublicKey:      pubKey,
			LeafIds:        []string{lf.ID.String()},
			MaxAssignments: 2,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit (pinned): %v", err)
		}
		if len(resp.Assignments) > 0 {
			asg = resp.Assignments[0]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if asg == nil {
		t.Fatal("a volunteer pinning the head's ONLY leaf (UNLISTED, with queued work) was never served: nothing stages a pinned non-PUBLIC leaf while the ready pool is empty (PB-16)")
	}
	if asg.LeafId != lf.ID.String() {
		t.Fatalf("served leaf %s, want the pinned UNLISTED leaf %s", asg.LeafId, lf.ID)
	}

	// The reservation lands like any other: the async flush writes the RESERVED copy
	// while the unit stays QUEUED (the per-copy dispatch model).
	if reserved := pollReservedVolunteer(t, ctx, env, asg.WorkUnitId, 10*time.Second); reserved != volID {
		t.Fatalf("flushed RESERVED copy volunteer = %q, want %s", reserved, volID)
	}

	// And the any-leaf requester is STILL refused now that the pinned refill has staged
	// the leaf's units into the SHARED ready pool — the hole PB-38 closed stays closed.
	requestWUExpectNone(t, env, ctx, anyVolID, anyKey, nil)
}
