package daemon

import (
	"sync"
	"time"
)

// Volunteer-facing notices.
//
// The daemon already knows when something needs a person's attention — a head
// rejecting this build as too old, a leaf that fails on every attempt, a disk
// allowance too small for any attached leaf — and says so at WARN in its log.
// A volunteer running the desktop app never reads that log. The notice ring
// is the same set of escalations, kept in memory in a form the management API
// can hand to a client: bounded, de-duplicated, and pollable by id.
//
// Notices are emitted only from the log's existing WARN/escalation sites; the
// ring adds no conditions of its own. It is deliberately in-memory only —
// a notice describes a condition observed by THIS daemon run, and a restart
// re-observes anything still true.
//
// A notice is a condition, not an event, so it also ends: the site that
// observes the condition ending (work arriving after a no-work streak, the
// disk gate clearing, a leaf recovering, the thermal throttle releasing)
// calls Resolve, which stamps the notice resolved. Resolved notices stay in
// the ring, so a client that has already shown one learns that it is over
// instead of displaying a 12-hour-old warning beside a machine that is
// working (TB-50).

// noticeRingCapacity bounds the ring. Notices past it are dropped oldest-first.
const noticeRingCapacity = 100

// noticeDedupeWindow is how recently a notice with the same code, head and
// leaf must have been updated for a new emission to refresh it (count, time,
// message) rather than append a duplicate. A condition that re-fires every
// poll therefore occupies one entry with a rising count, not the whole ring.
const noticeDedupeWindow = 10 * time.Minute

// Notice levels, mirroring the log level of the site that emitted the notice.
const (
	NoticeInfo  = "info"
	NoticeWarn  = "warn"
	NoticeError = "error"
)

// Notice is one volunteer-facing escalation as reported by GET /api/v1/notices.
type Notice struct {
	// ID is a monotonically increasing sequence number, assigned once at
	// creation and stable across refreshes, so a client can poll with
	// ?since=<id> and receive only notices created after it.
	ID    uint64 `json:"id"`
	Level string `json:"level"`
	// Code is the machine-readable condition (e.g. "update_required",
	// "disk_gate_blocked"); Message is the human-readable explanation.
	Code    string `json:"code"`
	Message string `json:"message"`
	// Head and Leaf name what the notice concerns, when it concerns one head or
	// one leaf specifically. Empty for daemon-wide conditions.
	Head string `json:"head,omitempty"`
	Leaf string `json:"leaf,omitempty"`
	// Count is how many times this condition was emitted while the notice was
	// live (see noticeDedupeWindow). FirstAt is the first emission; At is the
	// most recent.
	Count   int       `json:"count"`
	FirstAt time.Time `json:"first_at"`
	At      time.Time `json:"at"`
	// ResolvedAt is when the condition was observed to have ended (see
	// Resolve); nil while it is still live. A client should stop showing a
	// resolved notice as needing attention.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// NoticeLog is the bounded, de-duplicating notice ring. It is written from
// the fetcher, the coordinator, the thermal monitor and daemon start-up, and
// read from the management API's HTTP handlers, so every method takes the
// lock.
type NoticeLog struct {
	mu      sync.Mutex
	entries []Notice // oldest first
	nextID  uint64
	now     func() time.Time
}

// NewNoticeLog returns an empty notice ring.
func NewNoticeLog() *NoticeLog {
	return &NoticeLog{nextID: 1, now: time.Now}
}

// Notify records one emission of a condition. If a notice with the same code,
// head and leaf was updated within noticeDedupeWindow it is refreshed in place
// (Count incremented, At and Message replaced) instead of a new entry being
// appended — and if that notice had been resolved in the meantime it is
// reopened, so a condition that flaps (no work, a unit, no work again) stays
// one notice with a rising count rather than a stream of new ones. Otherwise a
// new notice is appended, dropping the oldest entry if the ring is full. A nil
// NoticeLog is a no-op, so call sites need no guard.
func (l *NoticeLog) Notify(level, code, message, head, leaf string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	// Search newest-first: the most recently updated match is the one to refresh.
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := &l.entries[i]
		if e.Code != code || e.Head != head || e.Leaf != leaf {
			continue
		}
		// The window runs from the notice's last change — its latest emission
		// or, if later, its resolution — so a condition that returns within
		// ten minutes of ending is the same episode.
		last := e.At
		if e.ResolvedAt != nil && e.ResolvedAt.After(last) {
			last = *e.ResolvedAt
		}
		if now.Sub(last) <= noticeDedupeWindow {
			e.Count++
			e.At = now
			e.Message = message
			e.Level = level
			e.ResolvedAt = nil
			return
		}
		break
	}

	if len(l.entries) >= noticeRingCapacity {
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, Notice{
		ID:      l.nextID,
		Level:   level,
		Code:    code,
		Message: message,
		Head:    head,
		Leaf:    leaf,
		Count:   1,
		FirstAt: now,
		At:      now,
	})
	l.nextID++
}

// Resolve marks every live notice with the given code — and, when head or
// leaf is non-empty, that head or leaf too — as resolved now, returning how
// many it resolved. An empty head or leaf matches any: the disk gate clears
// for every leaf at once, while a recovering leaf resolves only its own
// leaf_failing notice. A resolved notice keeps its id and stays in the ring;
// a later emission of the same condition within noticeDedupeWindow reopens it
// (see Notify), and one after the window starts a new notice. Resolving a
// condition with no live notice is a harmless no-op, as is a nil NoticeLog.
func (l *NoticeLog) Resolve(code, head, leaf string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	resolved := 0
	for i := range l.entries {
		e := &l.entries[i]
		if e.ResolvedAt != nil || e.Code != code {
			continue
		}
		if head != "" && e.Head != head {
			continue
		}
		if leaf != "" && e.Leaf != leaf {
			continue
		}
		at := now
		e.ResolvedAt = &at
		resolved++
	}
	return resolved
}

// ResolveDaemonWide marks resolved every live notice with the given code that
// names neither a head nor a leaf — the daemon-wide form of a condition that
// also comes in per-leaf form, which Resolve's wildcard would sweep up too.
// The disk gate needs the distinction: the data-dir floor recovering ends the
// machine-wide stall while a leaf whose own gate still refuses stays blocked,
// and its notice must outlive the floor's (TB-70).
func (l *NoticeLog) ResolveDaemonWide(code string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	resolved := 0
	for i := range l.entries {
		e := &l.entries[i]
		if e.ResolvedAt != nil || e.Code != code || e.Head != "" || e.Leaf != "" {
			continue
		}
		at := now
		e.ResolvedAt = &at
		resolved++
	}
	return resolved
}

// Since returns the notices created after the given id — every notice when
// since is 0 — most recently updated first, plus the highest id ever
// assigned (0 when nothing has been emitted). A refreshed notice keeps its
// id, so a client that only ever polls with ?since=latest_id sees each
// condition once; one that wants updated counts, and resolutions, fetches
// without since. Resolved notices are included, carrying resolved_at.
func (l *NoticeLog) Since(since uint64) (notices []Notice, latestID uint64) {
	notices = []Notice{}
	if l == nil {
		return notices, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, e := range l.entries {
		if e.ID > since {
			notices = append(notices, e)
		}
	}
	// Most recently updated first; ties broken by newest id first so the order
	// is stable.
	for i := 1; i < len(notices); i++ {
		for j := i; j > 0; j-- {
			a, b := notices[j-1], notices[j]
			if a.At.After(b.At) || (a.At.Equal(b.At) && a.ID > b.ID) {
				break
			}
			notices[j-1], notices[j] = b, a
		}
	}
	return notices, l.nextID - 1
}
