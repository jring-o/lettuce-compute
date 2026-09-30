package server

import (
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// The machine's in-flight cap on every work reply.
//
// The head bounds how many copies one machine may hold at once, running and buffered
// together (the per-machine in-flight cap, which the reliability quota ramps and the
// machine's cores and GPUs scale). A client that does not know the bound cannot size its
// ask to it: it asks for whatever its own buffer wants, and learns the bound only from an
// empty INFLIGHT_CAP reply once it is already there. On a large machine that meant asking
// for the whole batch ceiling every round, and taking more units than its own buffer could
// hold. Every reply therefore states the cap this hand-out enforced and how many copies the
// machine holds once the reply's units are counted, so the client can ask for the room left.

// handOutReply is what a hand-out's reply states besides its units: the requesting
// machine's in-flight cap and count, on every reply, and, when nothing was handed out, the
// reason (noWorkReply, the zero value when there is none).
type handOutReply struct {
	noWorkReply
	// inflightCap is the cap this hand-out enforced for the machine (0 when the head sets
	// none); inflightHeld is how many copies the machine holds on this head afterwards.
	inflightCap  int
	inflightHeld int
}

// apply stamps the reply's figures, and the reason when there is one, on a work reply.
func (r handOutReply) apply(resp *lettucev1.RequestWorkUnitResponse) {
	r.noWorkReply.apply(resp)
	resp.InflightCap = int32(r.inflightCap)
	resp.InflightHeld = int32(r.inflightHeld)
}
