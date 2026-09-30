package daemon

import (
	"sort"
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
	// NoWork is why the head is sending no work: the reason it gave on its most
	// recent explained empty work reply or, when that reason is one of the
	// informational ones (already contributed, bench cooldown), one line for
	// every leaf the head currently answers that way. The zero value when there
	// is none, or once the head sends work again.
	NoWork HeadNoWork
}

// HeadNoWork is a head's stated reason for sending no work: the notice code,
// its volunteer-facing wording, the leaf it concerned (its label and id; empty
// for a reason about the machine or the account, a request that named no leaf,
// or a line covering several leaves), the labels of every leaf the line covers
// (the informational reasons only), and when the head said it.
type HeadNoWork struct {
	Reason  string
	Message string
	Leaf    string
	LeafID  string
	Leaves  []string
	At      time.Time
}

// waitingLeaf is one leaf a head last answered with an informational reason:
// every task it has ready for the leaf already has this account's result or
// copy, or a recent failed copy of this account's keeps it off one for a while.
// head is the head's name as the volunteer knows it, for the line's wording.
type waitingLeaf struct {
	code  string
	label string
	head  string
	at    time.Time
}

// HeadStatusTracker holds HeadStatus per head gRPC address. It is written from
// daemon start-up and the fetcher goroutine and read from the management API's
// HTTP handlers, so every method takes the lock.
type HeadStatusTracker struct {
	mu     sync.Mutex
	byAddr map[string]HeadStatus
	// waiting holds, per head address, the leaves (by id; "" for a request that
	// named none) whose latest answer was an informational reason.
	waiting map[string]map[string]waitingLeaf
}

// NewHeadStatusTracker returns an empty tracker.
func NewHeadStatusTracker() *HeadStatusTracker {
	return &HeadStatusTracker{byAddr: make(map[string]HeadStatus), waiting: make(map[string]map[string]waitingLeaf)}
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

// Get returns the head's status; the zero value for a head never recorded. While
// the head's latest reason is informational, or it has none of its own (its
// leaf answered without one) and other leaves are still waiting, NoWork is the
// one line for all its waiting leaves.
func (t *HeadStatusTracker) Get(addr string) HeadStatus {
	if t == nil {
		return HeadStatus{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byAddr[addr]
	if w := t.waiting[addr]; len(w) > 0 && (st.NoWork.Reason == "" || informationalNoWork(st.NoWork.Reason)) {
		st.NoWork = waitingLine(w)
	}
	return st
}

// WaitingMessage is the head's line for one informational reason alone, naming
// every leaf it currently answers with that reason; "" when there is none.
func (t *HeadStatusTracker) WaitingMessage(addr, code string) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var of []waitingLeaf
	for _, wl := range t.waiting[addr] {
		if wl.code == code {
			of = append(of, wl)
		}
	}
	return waitingSentence(code, of)
}

// SetNoWork records the head's latest stated reason for sending no work. head is
// the head's name as the volunteer knows it. An informational reason also puts
// its leaf among the head's waiting leaves; any other takes the leaf out, since
// its latest answer is no longer that.
func (t *HeadStatusTracker) SetNoWork(addr, head string, nw HeadNoWork) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.byAddr[addr]
	st.NoWork = nw
	t.byAddr[addr] = st
	if !informationalNoWork(nw.Reason) {
		delete(t.waiting[addr], nw.LeafID)
		return
	}
	if t.waiting[addr] == nil {
		t.waiting[addr] = make(map[string]waitingLeaf)
	}
	t.waiting[addr][nw.LeafID] = waitingLeaf{code: nw.Reason, label: nw.Leaf, head: head, at: nw.At}
}

// ClearNoWork forgets everything the head said about sending no work, its
// waiting leaves included: it sent work.
func (t *HeadStatusTracker) ClearNoWork(addr string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiting, addr)
	st, ok := t.byAddr[addr]
	if !ok || st.NoWork.Reason == "" {
		return
	}
	st.NoWork = HeadNoWork{}
	t.byAddr[addr] = st
}

// ClearNoWorkFor forgets what the head said about leafID: the head has since
// answered for that leaf without a reason, so it no longer applies.
func (t *HeadStatusTracker) ClearNoWorkFor(addr, leafID string) {
	if t == nil || addr == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiting[addr], leafID)
	st, ok := t.byAddr[addr]
	if !ok || st.NoWork.Reason == "" || st.NoWork.LeafID != leafID {
		return
	}
	st.NoWork = HeadNoWork{}
	t.byAddr[addr] = st
}

// informationalNoWork reports the reasons that ask nothing of the volunteer and
// are shown as one line per head: this account has done everything the head has
// ready for a leaf, or a recent failed copy of its own keeps it off a task for a
// while.
func informationalNoWork(reason string) bool {
	return reason == noticeAlreadyContributed || reason == noticeBenchCooldown
}

// waitingLine is the head's one line for its waiting leaves: the leaves already
// contributed to first, then those held back by a failed copy. Its reason is
// already_contributed only when that covers every waiting leaf, because a leaf
// held back by a failed copy is not one the account has finished.
func waitingLine(w map[string]waitingLeaf) HeadNoWork {
	var line HeadNoWork
	byCode := map[string][]waitingLeaf{}
	for id, wl := range w {
		byCode[wl.code] = append(byCode[wl.code], wl)
		if wl.at.After(line.At) {
			line.At = wl.at
		}
		if wl.label != "" {
			line.Leaves = append(line.Leaves, wl.label)
		}
		if len(w) == 1 {
			line.Leaf, line.LeafID = wl.label, id
		}
	}
	sort.Strings(line.Leaves)
	line.Reason = noticeAlreadyContributed
	if len(byCode[noticeBenchCooldown]) > 0 {
		line.Reason = noticeBenchCooldown
	}
	for _, code := range []string{noticeAlreadyContributed, noticeBenchCooldown} {
		if s := waitingSentence(code, byCode[code]); s != "" {
			if line.Message != "" {
				line.Message += " "
			}
			line.Message += s
		}
	}
	return line
}
