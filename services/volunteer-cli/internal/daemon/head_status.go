package daemon

import (
	"sync"
	"time"
)

// Per-head version and update-required state.
//
// A head (server) and the volunteer builds that talk to it are coupled by
// protocol version: a head rejects a build that is too old for it, fleet-wide,
// until the volunteer runs `lettuce-volunteer update`. The daemon already
// detects that rejection at both places it can happen — registration at
// start-up and the work request path — and logs it. This tracker keeps the
// same fact per head, together with the head's own build version, so the
// management API can tell a client "this head needs you to update" rather
// than leaving it to be inferred from an empty work queue.
//
// State is keyed by the head's gRPC address, because a head that rejected the
// registration itself never becomes a live connection — there is no
// ServerConnection to hang the flag on — while the configured server entry
// (which carries the address) still exists for the API to report. It is
// in-memory only: a restart re-observes the rejection if it still applies.

// HeadStatus is one head's version and update-required state.
type HeadStatus struct {
	// HeadVersion is the head's build version as it reported it over
	// GetServerStatus at start-up; empty when it could not be read.
	HeadVersion string
	// UpdateRequired is true once this head has rejected this volunteer build
	// as too old, until a later RPC to the head succeeds.
	UpdateRequired bool
	// NoWork is the reason the head gave on its most recent explained empty
	// work reply; the zero value when there is none, or once the head sends
	// work again.
	NoWork HeadNoWork
}

// HeadNoWork is a head's stated reason for sending no work: the notice code,
// its volunteer-facing wording, the leaf it concerned (its label and id; empty
// for a reason about the machine or the account, or a request that named no
// leaf), and when the head said it.
type HeadNoWork struct {
	Reason  string
	Message string
	Leaf    string
	LeafID  string
	At      time.Time
}

// HeadStatusTracker holds HeadStatus per head gRPC address. It is written from
// daemon start-up and the fetcher goroutine and read from the management API's
// HTTP handlers, so every method takes the lock.
type HeadStatusTracker struct {
	mu     sync.Mutex
	byAddr map[string]HeadStatus
}

// NewHeadStatusTracker returns an empty tracker.
func NewHeadStatusTracker() *HeadStatusTracker {
	return &HeadStatusTracker{byAddr: make(map[string]HeadStatus)}
}

// SetVersion records the head's reported build version. A nil tracker is a
// no-op, so call sites need no guard.
func (t *HeadStatusTracker) SetVersion(addr, version string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byAddr[addr]
	st.HeadVersion = version
	t.byAddr[addr] = st
}

// MarkUpdateRequired records that the head rejected this volunteer build as
// too old.
func (t *HeadStatusTracker) MarkUpdateRequired(addr string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byAddr[addr]
	st.UpdateRequired = true
	t.byAddr[addr] = st
}

// MarkContactOK records a successful RPC to the head, clearing any
// update-required flag: a head that serves this build is not rejecting it.
func (t *HeadStatusTracker) MarkContactOK(addr string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.byAddr[addr]
	if !ok || !st.UpdateRequired {
		return
	}
	st.UpdateRequired = false
	t.byAddr[addr] = st
}

// Get returns the head's status; the zero value for a head never recorded.
func (t *HeadStatusTracker) Get(addr string) HeadStatus {
	if t == nil {
		return HeadStatus{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byAddr[addr]
}

// SetNoWork records the head's latest stated reason for sending no work.
func (t *HeadStatusTracker) SetNoWork(addr string, nw HeadNoWork) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byAddr[addr]
	st.NoWork = nw
	t.byAddr[addr] = st
}

// ClearNoWork forgets the head's stated reason: it sent work.
func (t *HeadStatusTracker) ClearNoWork(addr string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.byAddr[addr]
	if !ok || st.NoWork.Reason == "" {
		return
	}
	st.NoWork = HeadNoWork{}
	t.byAddr[addr] = st
}

// ClearNoWorkFor forgets the head's stated reason if it concerned leafID: the head
// has since answered for that leaf without one, so it no longer applies.
func (t *HeadStatusTracker) ClearNoWorkFor(addr, leafID string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.byAddr[addr]
	if !ok || st.NoWork.Reason == "" || st.NoWork.LeafID != leafID {
		return
	}
	st.NoWork = HeadNoWork{}
	t.byAddr[addr] = st
}
