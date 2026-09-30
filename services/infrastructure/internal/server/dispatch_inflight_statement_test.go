package server

import (
	"log/slog"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Every work reply states the requesting machine's in-flight cap on this head and how
// many copies it holds, not only an empty INFLIGHT_CAP reply. A client that knows the
// room left under the cap asks for no more than that, so it never asks for units the
// head would refuse, and never learns the cap only by being refused. Without the figures
// a large machine asked for the whole batch ceiling every round, whatever room it had
// left.

// A reply that carries work states the cap the hand-out enforced and the machine's count
// once the reply's units are counted.
func TestHandOut_StatesCapAndHeldOnAReplyWithWork(t *testing.T) {
	c, leafRepo := reasonCache(t)
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
	for i := 0; i < 3; i++ {
		c.stageUnit(types.NewID(), lf, 2, 0)
	}
	vol, host, other := types.NewID(), types.NewID(), types.NewID()
	holdInflight(c, host, 2)
	holdInflight(c, other, 7) // another machine's count must not leak into this reply

	res, _, reply := c.HandOutWithReason(vol, hostOpts(vol, host, 10), 3)
	if len(res) != 3 {
		t.Fatalf("hand-out = %d units, want 3", len(res))
	}
	if reply.inflightCap != 10 || reply.inflightHeld != 5 {
		t.Errorf("cap/held = %d/%d, want 10/5 (this machine's cap, and its 2 + the 3 just handed out)", reply.inflightCap, reply.inflightHeld)
	}
	resp := &lettucev1.RequestWorkUnitResponse{}
	reply.apply(resp)
	if resp.NoWorkReason != reasonUnspecified || resp.InflightCap != 10 || resp.InflightHeld != 5 {
		t.Errorf("wire reply = %v, want no reason and 10/5", resp)
	}
}

// An empty reply for a cause the head does not name still states the cap and count.
func TestHandOut_StatesCapAndHeldOnAnEmptyReplyWithNoReason(t *testing.T) {
	c, leafRepo := reasonCache(t)
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo) // no units staged
	vol, host := types.NewID(), types.NewID()
	holdInflight(c, host, 4)

	opts := hostOpts(vol, host, 10)
	opts.LeafIDs = []types.ID{lf}
	reply := emptyHandOut(t, c, vol, opts)
	if reply.reason != reasonUnspecified {
		t.Fatalf("reason = %v, want UNSPECIFIED (the leaf is empty)", reply.reason)
	}
	if reply.inflightCap != 10 || reply.inflightHeld != 4 {
		t.Errorf("cap/held = %d/%d, want 10/4", reply.inflightCap, reply.inflightHeld)
	}
}

// The per-machine send-interval refusal answers before any unit is looked at; it states
// the cap and count all the same.
func TestHandOut_StatesCapAndHeldWhenTheSendIntervalRefuses(t *testing.T) {
	c, leafRepo := reasonCache(t)
	c.cfg.minSendInterval = time.Minute
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
	for i := 0; i < 2; i++ {
		c.stageUnit(types.NewID(), lf, 2, 0)
	}
	vol, host := types.NewID(), types.NewID()
	opts := hostOpts(vol, host, 10)
	if res, _, _ := c.HandOutWithReason(vol, opts, 1); len(res) != 1 {
		t.Fatalf("first hand-out = %d units, want 1", len(res))
	}

	reply := emptyHandOut(t, c, vol, opts) // within the interval
	if reply.inflightCap != 10 || reply.inflightHeld != 1 {
		t.Errorf("cap/held = %d/%d, want 10/1", reply.inflightCap, reply.inflightHeld)
	}
}

// The cap stated is the one the hand-out enforced: under the reliability quota a machine
// with no record is held to the floor, and that floor is what the reply says, not the
// configured flat cap.
func TestHandOut_StatedCapIsTheEnforcedOne(t *testing.T) {
	c, leafRepo := reasonCache(t)
	c.cfg.reliabilityQuotaEnabled = true
	c.cfg.reliabilityFloor = 2
	lf := types.NewID()
	c.warm(nativeLeaf(lf, 2, false, 0), leafRepo)
	for i := 0; i < 5; i++ {
		c.stageUnit(types.NewID(), lf, 2, 0)
	}
	vol, host := types.NewID(), types.NewID()

	res, _, reply := c.HandOutWithReason(vol, hostOpts(vol, host, 10), 5)
	if len(res) != 2 {
		t.Fatalf("hand-out = %d units, want 2 (a new machine starts at the floor)", len(res))
	}
	if reply.inflightCap != 2 || reply.inflightHeld != 2 {
		t.Errorf("cap/held = %d/%d, want 2/2: the floor the hand-out enforced, not the flat cap of 10", reply.inflightCap, reply.inflightHeld)
	}
}

// Through the service: a RequestWorkUnit reply that carries work states the cap and count
// on the wire.
func TestRequestWorkUnit_ReplyWithWorkStatesCapAndHeld(t *testing.T) {
	svc, pub, volID, _ := reasonService(t, slog.LevelInfo, 0, 0, 0)
	svc.maxInflightPerVolunteer = 10

	resp, err := reasonRequest(t, svc, pub, volID)
	if err != nil {
		t.Fatalf("RequestWorkUnit: %v", err)
	}
	if len(resp.Assignments) != 1 {
		t.Fatalf("assignments = %d, want 1", len(resp.Assignments))
	}
	if resp.InflightCap != 10 || resp.InflightHeld != 1 {
		t.Errorf("reply cap/held = %d/%d, want 10/1", resp.InflightCap, resp.InflightHeld)
	}
}
