package server

import (
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// PB-37 regression: under maintenance-semaphore saturation, StartWork's forced flush
// (the PB-15 Major-3 guard) could not complete before the shed budget expired — the
// pre-fix guard fell through to Assign anyway, the volunteer was told to DROP the
// unit ("work unit no longer reserved for this volunteer"), and the stuck ticker
// batch landed a phantom RESERVED copy row moments later (reaped only by the buffer
// reconciler). The fixed contract is wait-or-fail-atomically: when the flush cannot
// complete, StartWork returns retryable ResourceExhausted, the volunteer keeps the
// unit, and whenever the batch does land the row is the volunteer's own still-held
// reservation — never a denial-plus-phantom.

// saturationWURepo backs the PB-37 saturation test: GetByID serves the staged QUEUED
// unit, and Assign answers the exact conflict the pre-fix guard converted into the
// drop-the-unit denial (the RESERVED copy row is not durable yet — the flush batch
// holding it is stuck on the saturated maintenance semaphore).
type saturationWURepo struct {
	*fakeWURepo
	unitID types.ID
	leafID types.ID
}

func (r *saturationWURepo) GetByID(_ context.Context, id types.ID) (*workunit.WorkUnit, error) {
	return &workunit.WorkUnit{ID: id, LeafID: r.leafID, State: workunit.WorkUnitStateQueued}, nil
}

func (r *saturationWURepo) Assign(_ context.Context, _ types.ID, _ types.ID) (*workunit.WorkUnit, error) {
	return nil, apierror.Conflict("no reserved copy to start for this volunteer",
		map[string]string{"code": "ASSIGNMENT_CONFLICT"})
}

func TestStartWork_SaturatedFlush_RetryableNotDroppedPlusPhantom(t *testing.T) {
	leafID := types.NewID()
	unitID := types.NewID()
	vol := types.NewID()

	fake := &fakeWURepo{}
	var landedCount atomic.Int32
	landedCh := make(chan struct{}, 1)
	fake.flushFn = func(recs []workunit.FlushReservation) ([]workunit.FlushedCopy, error) {
		out := make([]workunit.FlushedCopy, 0, len(recs))
		for _, r := range recs {
			out = append(out, workunit.FlushedCopy{WorkUnitID: r.WorkUnitID, VolunteerID: r.VolunteerID})
		}
		landedCount.Add(int32(len(recs)))
		select {
		case landedCh <- struct{}{}:
		default:
		}
		return out, nil
	}
	leafRepo := &fakeLeafRepo{}
	c := newTestCache(fake, leafRepo, &fakeAssignRepo{})
	c.warm(nativeLeaf(leafID, 2, false, 0), leafRepo)
	c.stageUnit(unitID, leafID, 2, 0)

	// Warm-cache hand-out: the reservation is held in memory and queued for the
	// async flush.
	results, _ := c.HandOut(vol, capableOpts(vol, 0), 1)
	if len(results) != 1 {
		t.Fatalf("hand-out failed: got %d results", len(results))
	}
	if !c.hasInMemReservation(unitID, vol) {
		t.Fatal("hand-out did not register an in-memory reservation")
	}

	// Saturate the maintenance semaphore (cap admissionCap/4 = 1 for the test cache),
	// then fire the ticker flush: it snapshots the pending write OUT of the queue
	// (flushInFlight = 1) and blocks on the saturated semaphore — the exact PB-37
	// window (the record is in neither the queue nor the DB).
	c.maintenanceAdmission <- struct{}{}
	go c.flushOnce(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		inFlight := c.flushInFlight
		queued := len(c.pendingWrites)
		c.mu.Unlock()
		if inFlight == 1 && queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticker batch never entered the in-flight window (inFlight=%d queued=%d)", inFlight, queued)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// StartWork with a short budget: the forced flush cannot complete (the batch is
	// stuck), so the deterministic outcome must be a RETRYABLE refusal — never the
	// drop-the-unit denial the volunteer acts on by abandoning the work.
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	c.putIdentity(&volunteer.Volunteer{ID: vol, PublicKey: pub, AvailableRuntimes: []string{leaf.RuntimeNative}})
	s := &volunteerService{
		wuRepo:        &saturationWURepo{fakeWURepo: fake, unitID: unitID, leafID: leafID},
		dispatchCache: c,
		logger:        testLogger(),
	}
	shortCtx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	authCtx := context.WithValue(shortCtx, grpcAuthPublicKeyContextKey{}, ed25519.PublicKey(pub))

	resp, err := s.StartWork(authCtx, &lettucev1.StartWorkRequest{
		WorkUnitId:  unitID.String(),
		VolunteerId: vol.String(),
	})
	if err == nil {
		t.Fatalf("StartWork under an incompletable forced flush must fail RETRYABLE; got response %+v — with Ok=false the volunteer DROPS the unit while the stuck flush batch lands a phantom RESERVED copy row afterwards (PB-37)", resp)
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want retryable ResourceExhausted, got %v (%v)", status.Code(err), err)
	}

	// Release the semaphore: the stuck batch now lands. Because the volunteer was
	// told to RETRY (not drop), the row it lands is the volunteer's own still-held
	// reservation — deterministic, no phantom.
	<-c.maintenanceAdmission
	select {
	case <-landedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("released flush batch never landed")
	}
	if got := landedCount.Load(); got != 1 {
		t.Fatalf("expected exactly the one reservation write to land, got %d", got)
	}
	if !c.hasInMemReservation(unitID, vol) {
		t.Fatal("the in-memory hold must survive a retryable StartWork refusal (the volunteer still holds the unit)")
	}
}

// PB-38 regression (in-memory half): a non-PUBLIC leaf's unit staged in the shared
// ready pool (a leaf-scoped refill for a pinned requester puts it there) must not be
// handed to an any-leaf requester; a requester whose leaf filter names the leaf id
// still receives it.
func TestHandOut_NonPublicLeafOnlyToPinnedRequester(t *testing.T) {
	for _, vis := range []leaf.LeafVisibility{leaf.VisibilityUnlisted, leaf.VisibilityPrivate} {
		t.Run(string(vis), func(t *testing.T) {
			wuRepo := &fakeWURepo{}
			leafRepo := &fakeLeafRepo{}
			c := newTestCache(wuRepo, leafRepo, &fakeAssignRepo{})

			leafID := types.NewID()
			lf := nativeLeaf(leafID, 2, false, 0)
			lf.Visibility = vis
			c.warm(lf, leafRepo)
			unitID := types.NewID()
			c.stageUnit(unitID, leafID, 2, 0)

			// Any-leaf requester (no leaf filter — the volunteer fallback): refused.
			anyVol := types.NewID()
			got, _ := c.HandOut(anyVol, capableOpts(anyVol, 0), 1)
			if len(got) != 0 {
				t.Fatalf("any-leaf requester was handed %d unit(s) of a %s leaf it never pinned (PB-38)", len(got), vis)
			}

			// Pin-by-id requester: served.
			pinVol := types.NewID()
			opts := capableOpts(pinVol, 0)
			opts.LeafIDs = []types.ID{leafID}
			got, _ = c.HandOut(pinVol, opts, 1)
			if len(got) != 1 {
				t.Fatalf("pin-by-id requester must be served the %s leaf's unit, got %d", vis, len(got))
			}
		})
	}
}
