package daemon

import (
	"sync"
	"time"
)

// Per-leaf execution-failure circuit breaker (TB-10).
//
// The runtime-level breaker in the fetcher (recordRuntimeAbandon) only counts
// CAPABILITY failures — no registered runtime, or a Prepare that could not fetch
// the artifact. A leaf whose binary downloads fine, starts fine and then exits
// non-zero in 200 ms passes every one of those gates, so nothing bounded it: the
// client fetched a unit, failed it, abandoned it, the head requeued it, and the
// loop repeated several times a second for hours. Head-side that read as churn
// across the whole fleet; volunteer-side it was invisible, and the volunteer
// concluded they were simply never sent work of that kind.
//
// This tracker is the missing bound. It counts CONSECUTIVE local failures per
// leaf — non-zero exit or a runtime error while executing — and once a leaf
// crosses the threshold the fetcher stops requesting it until a cooldown
// elapses. It is deliberately per LEAF, not per runtime: one broken artifact
// must not stop this machine from running every other native leaf.

// leafFailurePauseThreshold is how many consecutive local failures of one leaf
// trip the breaker. It matches the runtime breaker's threshold and the head's
// max_reassignments default (3): by the time a volunteer has failed the same
// leaf three times running, it has already cost that leaf's units their whole
// reassignment budget once, and is contributing nothing but churn.
const leafFailurePauseThreshold = 3

// leafFailureCooldown is how long a tripped leaf stays paused before it is
// re-probed once. A broken artifact is usually fixed head-side (a re-uploaded
// binary, an edited execution spec), which the volunteer learns about on the
// 5-minute leaf-cache refresh, so the pause is time-bounded rather than
// permanent-until-restart. Mirrors runtimeAbandonCooldown.
const leafFailureCooldown = 10 * time.Minute

// LeafFailureSummary is one leaf's failure record as reported to the management
// API (and from there to `status` and `leafs list`).
type LeafFailureSummary struct {
	LeafID string `json:"leaf_id"`
	// Consecutive is the current unbroken run of failures. It resets to 0 the
	// moment a unit of this leaf exits 0 on this machine.
	Consecutive int `json:"consecutive_failures"`
	// Total is every failure of this leaf since the daemon started, so a leaf
	// that fails intermittently (and therefore never trips the breaker) is still
	// visible.
	Total        int       `json:"total_failures"`
	LastReason   string    `json:"last_reason,omitempty"`
	LastFailedAt time.Time `json:"last_failed_at"`
	// Paused reports whether the breaker is currently holding this leaf back, and
	// PausedUntil when it will next be re-probed.
	Paused      bool      `json:"paused"`
	PausedUntil time.Time `json:"paused_until,omitempty"`
}

// leafFailureState is the tracker's per-leaf record.
type leafFailureState struct {
	consecutive  int
	total        int
	lastReason   string
	lastFailedAt time.Time
	pausedAt     time.Time // zero when the leaf is not paused
}

// leafFailureTracker records local execution failures per leaf. It is written
// from the daemon's coordinator goroutine (handleSlotResult) and read from the
// fetcher goroutine and the management API's HTTP handlers, so every method
// takes the lock.
type leafFailureTracker struct {
	mu     sync.Mutex
	byLeaf map[string]*leafFailureState
	now    func() time.Time
}

func newLeafFailureTracker(now func() time.Time) *leafFailureTracker {
	if now == nil {
		now = time.Now
	}
	return &leafFailureTracker{
		byLeaf: make(map[string]*leafFailureState),
		now:    now,
	}
}

// RecordFailure notes one local failure of a leaf and reports the new
// consecutive count plus whether this failure is the one that trips the breaker.
// tripped is true exactly once per pause, so the caller can emit a single loud
// WARN rather than one per failure.
func (t *leafFailureTracker) RecordFailure(leafID, reason string) (consecutive int, tripped bool) {
	if leafID == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.byLeaf[leafID]
	if st == nil {
		st = &leafFailureState{}
		t.byLeaf[leafID] = st
	}
	st.consecutive++
	st.total++
	st.lastReason = reason
	st.lastFailedAt = t.now()

	if st.consecutive >= leafFailurePauseThreshold && st.pausedAt.IsZero() {
		st.pausedAt = st.lastFailedAt
		return st.consecutive, true
	}
	return st.consecutive, false
}

// RecordSuccess clears a leaf's failure streak and any pause. It reports whether
// it cleared an active pause, so the caller can mark the recovery in the log —
// the trip is loud, and an un-pause that stayed silent would leave the WARN
// looking permanent.
func (t *leafFailureTracker) RecordSuccess(leafID string) (wasPaused bool) {
	if leafID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.byLeaf[leafID]
	if st == nil {
		return false
	}
	wasPaused = !st.pausedAt.IsZero()
	// Keep the running total: it is the answer to "has this leaf ever failed
	// here?", which stays useful after the streak is broken. Only the streak and
	// the pause are cleared.
	st.consecutive = 0
	st.pausedAt = time.Time{}
	return wasPaused
}

// Paused reports whether the breaker is currently holding this leaf back,
// clearing the pause (and the streak) when the cooldown has elapsed so the leaf
// is re-probed once.
func (t *leafFailureTracker) Paused(leafID string) bool {
	if leafID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	st := t.byLeaf[leafID]
	if st == nil || st.pausedAt.IsZero() {
		return false
	}
	if t.now().Sub(st.pausedAt) >= leafFailureCooldown {
		st.pausedAt = time.Time{}
		st.consecutive = 0
		return false
	}
	return true
}

// Snapshot returns every leaf that has failed at least once since the daemon
// started, newest failure first. Pauses whose cooldown has elapsed are reported
// as not paused (without mutating state — Paused does the clearing, on the
// fetcher's own goroutine).
func (t *leafFailureTracker) Snapshot() []LeafFailureSummary {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	out := make([]LeafFailureSummary, 0, len(t.byLeaf))
	for leafID, st := range t.byLeaf {
		s := LeafFailureSummary{
			LeafID:       leafID,
			Consecutive:  st.consecutive,
			Total:        st.total,
			LastReason:   st.lastReason,
			LastFailedAt: st.lastFailedAt,
		}
		if !st.pausedAt.IsZero() && now.Sub(st.pausedAt) < leafFailureCooldown {
			s.Paused = true
			s.PausedUntil = st.pausedAt.Add(leafFailureCooldown)
		}
		out = append(out, s)
	}
	// Newest failure first, ties broken by leaf id so the order is stable across
	// calls (a map range is not).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.LastFailedAt.After(b.LastFailedAt) ||
				(a.LastFailedAt.Equal(b.LastFailedAt) && a.LeafID <= b.LeafID) {
				break
			}
			out[j-1], out[j] = b, a
		}
	}
	return out
}
