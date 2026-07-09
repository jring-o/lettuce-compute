package server

import (
	"context"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Server-issued host identity (BG-25, design §4.6): the head is the sole minter of
// per-machine host ids. RegisterVolunteer mints (empty request id, under the
// per-account cap), echoes (a known id of the requesting account), or answers empty
// (unknown/foreign id — the client discards its stored id and re-registers empty to
// mint explicitly). RequestWorkUnit REFUSES a non-empty id the head did not issue to
// the requesting account, with the pinned message below.

// HostCapPolicy is the per-account host cap (BG-25), a plain struct (no config-package
// dependency) filled from HeadConfig.Effective* values via SetHostCapPolicy. The zero
// value disables the cap.
type HostCapPolicy struct {
	// PerAccount is the hard bound on one account's TOTAL issued host ids; <= 0
	// disables the cap.
	PerAccount int
	// ActiveWindow is the staleness threshold: a host unseen for longer is evictable
	// at mint time (its slot is reclaimed for a new machine).
	ActiveWindow time.Duration
}

// HostUnknownMessagePrefix is the machine-readable contract solver-era clients match
// on a FailedPrecondition from RequestWorkUnit to trigger their
// discard-id-and-re-register flow. Pinned: changing it orphans shipped clients. Clients
// MUST check this prefix BEFORE their too-old classifier — the full message also
// carries the word "outdated" so PRE-issuance builds (which echo self-generated ids
// the head never issued) classify it via IsVolunteerTooOldError and print the
// actionable update hint instead of a generic error.
const HostUnknownMessagePrefix = "unknown or revoked host id"

// HostUnknownMessage is the full client-facing text of the host-unknown refusal.
const HostUnknownMessage = HostUnknownMessagePrefix +
	": this volunteer build is outdated — run 'lettuce-volunteer update' (updated builds re-register and acquire a fresh id automatically)"

const (
	// hostOwnerTTL bounds how long a cached ownership fact (positive or negative) is
	// trusted. It is the revocation latency: an operator's DELETE FROM hosts (or a
	// mint-time eviction on another replica) takes effect on this process's hot path
	// within one TTL. Deliberately the trust/standing-snapshot staleness class.
	hostOwnerTTL = 30 * time.Second

	// hostSeenBumpInterval throttles the work-path hosts.last_seen_at bump. The bump
	// is what keeps a continuously WORKING machine out of the cap's stale-eviction
	// window (audit F-A — load-bearing for "working machines are never evictable"),
	// but it must not cost one UPDATE per work request; once per interval per host is
	// plenty against a 30-day staleness window.
	hostSeenBumpInterval = 5 * time.Minute
)

// hostOwnerEntry is one cached ownership fact. found=false is a cached NEGATIVE (the
// row definitively did not exist when checked) so a client hammering an unknown id
// costs at most one DB read per TTL, not one per request.
type hostOwnerEntry struct {
	owner    types.ID
	found    bool
	expires  time.Time
	lastBump time.Time
}

// putHostOwner warms (or refreshes) the ownership fact for an issued host id. Called at
// RegisterVolunteer (mint and echo paths both know the owner) so the machine's first
// work request validates entirely in memory.
func (c *dispatchCache) putHostOwner(hostID, owner types.ID) {
	now := time.Now()
	c.hostOwnerMu.Lock()
	prev := c.hostOwnerCache[hostID]
	e := &hostOwnerEntry{owner: owner, found: true, expires: now.Add(hostOwnerTTL)}
	if prev != nil {
		e.lastBump = prev.lastBump
	}
	c.hostOwnerCache[hostID] = e
	c.hostOwnerMu.Unlock()
}

// resolveHostOwner answers "who owns this host id" for work-path validation.
// Returns:
//
//	owner, true,  true  — the row exists and is owned by `owner` (validate against it)
//	_,     false, true  — the row DEFINITIVELY does not exist (refuse the request)
//	_,     false, false — could not determine (no repo / admission shed / DB error):
//	                      the caller MUST fold to the per-account bucket for this
//	                      request and NEVER refuse (audit F-C: a post-deploy cold
//	                      cache under reconnect load must not trigger a fleet-wide
//	                      discard-and-re-mint storm).
//
// A cold miss reads the authoritative hosts row once under the dispatch admission
// semaphore + short timeout (the resolveIdentity idiom) and caches the outcome —
// positive or negative — for hostOwnerTTL.
func (c *dispatchCache) resolveHostOwner(hostID types.ID) (types.ID, bool, bool) {
	now := time.Now()
	c.hostOwnerMu.Lock()
	if e, ok := c.hostOwnerCache[hostID]; ok && now.Before(e.expires) {
		owner, found := e.owner, e.found
		c.hostOwnerMu.Unlock()
		return owner, found, true
	}
	c.hostOwnerMu.Unlock()

	if c.deps.hostRepo == nil {
		return types.ID{}, false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchDBTimeout)
	defer cancel()
	release, ok := c.acquire(ctx)
	if !ok {
		return types.ID{}, false, false // admission saturated: fold, never refuse
	}
	defer release()
	h, err := c.deps.hostRepo.GetByID(ctx, hostID)
	if err != nil {
		if isNotFound(err) {
			// Definitive negative: cache it and refuse upstream.
			c.hostOwnerMu.Lock()
			c.hostOwnerCache[hostID] = &hostOwnerEntry{found: false, expires: now.Add(hostOwnerTTL)}
			c.hostOwnerMu.Unlock()
			return types.ID{}, false, true
		}
		c.logger.Warn("dispatch cache: host owner resolve failed", "host_id", hostID, "error", err)
		return types.ID{}, false, false
	}
	c.putHostOwner(hostID, h.VolunteerID)
	return h.VolunteerID, true, true
}

// shouldBumpHostSeen reports whether the work path should bump hosts.last_seen_at for
// this host now, and stamps the throttle when it says yes. Memory-only; the caller does
// the (best-effort) DB write.
func (c *dispatchCache) shouldBumpHostSeen(hostID types.ID) bool {
	now := time.Now()
	c.hostOwnerMu.Lock()
	defer c.hostOwnerMu.Unlock()
	e, ok := c.hostOwnerCache[hostID]
	if !ok {
		// No entry (validation just resolved via a racing eviction of the map, or the
		// entry expired): create a bump-tracking entry with no ownership claim.
		c.hostOwnerCache[hostID] = &hostOwnerEntry{lastBump: now}
		return true
	}
	if now.Sub(e.lastBump) < hostSeenBumpInterval {
		return false
	}
	e.lastBump = now
	return true
}
