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
// appended. Otherwise a new notice is appended, dropping the oldest entry if
// the ring is full. A nil NoticeLog is a no-op, so call sites need no guard.
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
		if now.Sub(e.At) <= noticeDedupeWindow {
			e.Count++
			e.At = now
			e.Message = message
			e.Level = level
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

// Since returns the notices created after the given id — every notice when
// since is 0 — most recently updated first, plus the highest id ever
// assigned (0 when nothing has been emitted). A refreshed notice keeps its
// id, so a client that only ever polls with ?since=latest_id sees each
// condition once; one that wants updated counts fetches without since.
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
