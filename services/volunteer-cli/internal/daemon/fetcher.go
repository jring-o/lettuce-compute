package daemon

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Fetcher is a background goroutine that fills the pre-fetch queue.
// It is controlled entirely via context cancellation — no Stop/Start methods.
// To pause: cancel the context (goroutine exits). To resume: create a new Fetcher.
type Fetcher struct {
	queue       *PreFetchQueue
	selector    *WeightedSelector
	leafCache   *LeafCache
	registry    *RuntimeRegistry
	multiClient *MultiServerClient
	logger      *slog.Logger
	backoff     time.Duration
	maxBackoff  time.Duration
	cachedHW    *lettucev1.HardwareCapabilities
	pubKey      ed25519.PublicKey

	// reRegisterFn re-registers this machine against one head after a host-unknown
	// work-path refusal (BG-25 self-heal): it discards the refused id, registers with an
	// empty host id (so the head mints a fresh one), persists and returns the head's
	// new id (possibly empty). Injected from the daemon; nil disables self-heal (the
	// refusal then falls through to the normal reconnect backoff).
	reRegisterFn func(ctx context.Context, head *ServerConnection) (string, error)

	// enabledLeafsFunc is called to get enabled leafs for a server.
	// Injected from the daemon to reuse its filtering logic.
	enabledLeafsFunc func(serverName string) []CachedLeafInfo

	// leafPrefsFunc returns leaf ID filter and block list from config.
	leafPrefsFunc func() (leafIDs, blockedIDs []string)

	// serverBlockedLeafIDsFunc returns the leaf IDs a server's per-server
	// leaf_preferences exclude, so the any-leaf fallback can tell the head not to
	// dispatch them. Injected from the daemon.
	serverBlockedLeafIDsFunc func(serverName string) []string

	// shouldFetchFunc checks whether fetching is allowed (disk space, scheduler, etc.).
	// Returns true if fetching should proceed, false to wait.
	shouldFetchFunc func() bool

	// --- CLIENT WORK BUFFER (Layer 1) ---
	// workBufferFullFn reports whether the hours-based client work buffer is full.
	// When it returns true the fetcher issues ZERO RequestWorkUnit calls (DoD #2).
	workBufferFullFn func() bool
	// batchSizeFn returns how many assignments to request for a leaf given an
	// estimate of seconds-per-unit, clamped to [1, maxBatchPerRequest].
	batchSizeFn func(estSecondsPerUnit float64) int32
	// leafEstSecondsFn estimates wall-clock seconds for ONE unit of the given leaf
	// (0 = unknown), used to size the per-leaf batch request BEFORE any of that
	// leaf's units have been buffered (#29). It prefers the leaf-level,
	// benchmark-independent estimate carried in CachedLeafInfo
	// (EstimatedDurationSeconds), so it stays non-zero even on a host with no CPU
	// benchmark — the exact case the old FP-ops-only path tripped to 0.
	leafEstSecondsFn func(leaf CachedLeafInfo) float64

	// heldWorkUnitIDsFn returns the ids of every work unit the volunteer currently
	// holds — its prefetch buffer (buffered, not yet started) plus its active slots
	// (in-transit and running) — reported on every RequestWorkUnit so the head can
	// release reservations the volunteer no longer holds. nil disables reporting.
	heldWorkUnitIDsFn func() []string

	// rateLimitBackoff is the fixed local backoff floor applied when a head
	// answers codes.ResourceExhausted. ResourceExhausted carries NO
	// server-directed value (the head is shedding load and wants the caller gone
	// now), so this is a pure jittered LOCAL backoff, distinct from the
	// authoritative server-directed retry delay carried in the response body.
	rateLimitBackoff time.Duration

	// --- ESCALATION (#15 fix 4): per-runtime consecutive-abandon breaker ---
	// runtimeAbandons counts consecutive capability-driven abandons (missing or
	// incapable runtime, or a failed Prepare), keyed on the normalized runtime
	// name. pausedRuntimes records when a runtime tripped the breaker so its
	// leafs are skipped until the cooldown elapses. Both are owned by the single
	// Run goroutine — no lock needed.
	runtimeAbandons map[string]int
	pausedRuntimes  map[string]time.Time

	// now is the clock seam (defaults to time.Now). Tests advance it to exercise
	// the pause cooldown without real sleeps.
	now func() time.Time
}

// Cadence defaults.
//
// The single authoritative cadence is the head-provided server-directed retry
// delay (RequestWorkUnitResponse.RetryAfterSeconds), obeyed verbatim via
// ServerConnection.NextContactAt. There is no client-side minimum-interval
// throttle anymore: the head decides when each volunteer checks back.
//
// defaultRateLimitBackoff is the fixed LOCAL backoff applied on
// codes.ResourceExhausted (which carries no server-directed value). It is
// jittered ±20% and capped at maxResourceExhaustedBackoff.
const (
	defaultRateLimitBackoff     = 30 * time.Second
	maxResourceExhaustedBackoff = 900 * time.Second
)

// maxBatchPerRequest is the SAFETY CEILING on how many assignments the volunteer
// requests in one RequestWorkUnit — not the primary limiter (#29). The primary
// limiter is the hours-deficit / per-unit-seconds math in requestBatchSize; this
// ceiling only stops a pathologically short-unit leaf from asking for an
// unbounded batch. Raised from 8 to 64 so short-unit leafs can actually fill
// work_buffer_hours in one request instead of idling between polls. Mirrors the
// head's max_batch_per_request default (also 64) so a volunteer never asks for
// more than the head will hand out in a batch.
const maxBatchPerRequest = 64

// runtimeAbandonPauseThreshold is how many consecutive capability-driven
// abandons for one runtime trip the circuit breaker. It matches the head's
// max_reassignments default (3): after a volunteer has caused a unit to exhaust
// its reassignments once, it stops contributing.
const runtimeAbandonPauseThreshold = 3

// reservationDropMargin is the safety window before a buffered unit's reservation
// window (reserved_until, sized once at hand-out and never renewed) lapses, at
// which point the fetcher drops it from the work buffer rather than wasting a
// run-start (StartWork) on a unit the head may have already re-staged via its
// lapsed-reservation sweep. It is the deadline-based-leasing analogue of the old
// prepare-heartbeat renewal interval: large enough to absorb a slow image pull
// before the unit reaches a slot, small enough that we don't race the head's
// reclaim. See PreFetchQueue.DropLapsedReservations.
const reservationDropMargin = 60 * time.Second

// runtimeAbandonCooldown is how long a tripped runtime stays paused before it
// is re-probed once. Container backends recover (Docker/Podman restart) and
// leaf ExecutionSpecs change on head edits, so the pause is time-bounded rather
// than permanent-until-restart; this lines up with the 5-min leaf-cache refresh.
const runtimeAbandonCooldown = 10 * time.Minute

// NewFetcher creates a new fetcher that fills the pre-fetch queue.
func NewFetcher(d *Daemon, queue *PreFetchQueue, selector *WeightedSelector, leafCache *LeafCache) *Fetcher {
	return &Fetcher{
		queue:                    queue,
		selector:                 selector,
		leafCache:                leafCache,
		registry:                 d.runtimeRegistry,
		multiClient:              d.multiClient,
		logger:                   d.logger,
		backoff:                  d.initialBackoff,
		maxBackoff:               d.maxBackoff,
		cachedHW:                 d.cachedHW,
		pubKey:                   d.pubKey,
		reRegisterFn:             d.reRegisterHost,
		enabledLeafsFunc:         d.enabledLeafs,
		leafPrefsFunc:            d.leafPreferences,
		serverBlockedLeafIDsFunc: d.serverBlockedLeafIDs,
		shouldFetchFunc:          d.shouldFetch,
		workBufferFullFn:         d.workBufferFull,
		batchSizeFn:              d.requestBatchSize,
		leafEstSecondsFn:         d.leafEstSeconds,
		heldWorkUnitIDsFn:        d.heldWorkUnitIDs,
		rateLimitBackoff:         defaultRateLimitBackoff,
		runtimeAbandons:          make(map[string]int),
		pausedRuntimes:           make(map[string]time.Time),
		now:                      time.Now,
	}
}

// noWorkWarnThreshold is how many consecutive empty polls (no work returned)
// trigger the one-time "connected but getting no work" diagnostic WARN. With
// exponential backoff this is roughly half a minute of genuine idleness, long
// enough to skip a momentarily-empty queue but short enough to be useful.
const noWorkWarnThreshold = 5

// idleWait is how long the fetcher sleeps when there is nothing to do right now
// (buffer full, every head waiting out its server-directed delay with no
// computable wake time, or no work served). The cadence between actual
// RequestWorkUnit calls is governed by the head's retry delay, not this value;
// this is only the loop's poll granularity.
const idleWait = 5 * time.Second

// Run is the fetcher's main loop. It fills the client work buffer until ctx is
// cancelled. Cadence is entirely server-directed: each head stamps a
// retry_after_seconds the volunteer obeys via ServerConnection.NextContactAt.
func (f *Fetcher) Run(ctx context.Context) {
	f.logger.Info("fetcher: started", "max_batch_per_request", maxBatchPerRequest)

	// Track a run of empty polls so we can surface "connected but no work — here's
	// why" exactly once, instead of leaving the operator staring at a silent,
	// idle daemon.
	emptyPolls := 0
	warnedNoWork := false

	for {
		select {
		case <-ctx.Done():
			f.logger.Info("fetcher: context cancelled, exiting")
			return
		default:
		}

		// Check if fetching is allowed (disk space, scheduler, etc.).
		if f.shouldFetchFunc != nil && !f.shouldFetchFunc() {
			f.logger.Debug("fetcher: shouldFetch returned false, waiting 1s")
			if !f.sleep(ctx, 1*time.Second) {
				return
			}
			continue
		}

		// Drop expiring items (deadline safety).
		f.queue.DropExpiring(0.1)

		// Drop buffered items whose head-side reservation window has (nearly) lapsed.
		// With per-task heartbeats removed, the reservation window (reserved_until) is
		// sized ONCE at hand-out and is NOT renewed, so a unit held in the buffer past
		// its window is re-staged by the head's lapsed-reservation sweep. This guard
		// drops such a unit `reservationDropMargin` ahead of lapse, before we waste a
		// run-start (StartWork) on a unit the head no longer believes is ours.
		f.queue.DropLapsedReservations(reservationDropMargin, f.now())

		// CLIENT WORK BUFFER (DoD #2): when the hours-based buffer is full, issue
		// ZERO RequestWorkUnit calls. Re-check on a short cadence (f.backoff, not
		// the longer idleWait) so the fetcher refills promptly the moment a running
		// slot completes and frees buffer capacity, without polling the head.
		if f.workBufferFullFn != nil && f.workBufferFullFn() {
			f.logger.Debug("fetcher: work buffer full, not requesting", "queue_len", f.queue.Len())
			recheck := f.backoff
			if recheck <= 0 {
				recheck = time.Millisecond
			}
			if !f.sleep(ctx, recheck) {
				return
			}
			continue
		}

		// SERVER-DIRECTED DELAY (DoD #1): if every available head is still waiting
		// out its retry delay, sleep until the earliest NextContactAt (or a poll
		// tick), issuing no requests in the meantime.
		if wait, ok := f.waitUntilHeadEligible(); ok {
			f.logger.Debug("fetcher: all heads waiting out retry delay", "wait", wait)
			if !f.sleep(ctx, wait) {
				return
			}
			continue
		}

		f.logger.Debug("fetcher: attempting fetchOne", "queue_len", f.queue.Len())

		// Try to fetch a batch of work units.
		got, err := f.fetchOne(ctx)
		if err != nil {
			f.logger.Warn("fetcher: fetchOne returned error", "error", err)
			if !f.sleep(ctx, idleWait) {
				return
			}
			continue
		}

		if got == 0 {
			f.logger.Debug("fetcher: fetchOne buffered no work")
			emptyPolls++
			if emptyPolls >= noWorkWarnThreshold && !warnedNoWork {
				f.warnNoWork()
				warnedNoWork = true
			}
			// No work was buffered this cycle (no assignments, or every assignment
			// was abandoned as unusable). The authoritative cadence is the head's
			// retry delay, obeyed via NextContactAt by the head-eligibility gate at
			// the top of the loop. This small no-work floor (f.backoff) only prevents
			// a busy-spin when a head answers with a zero delay and no work; it does
			// NOT grow (cadence is server-directed, not client exponential backoff).
			if !f.sleep(ctx, f.backoff) {
				return
			}
			continue
		}

		// Got work — clear the no-work streak so the diagnostic can fire again if
		// the volunteer later goes idle for a new reason.
		emptyPolls = 0
		warnedNoWork = false
	}
}

// sleep waits for d or until ctx is cancelled. Returns false if ctx was
// cancelled (the caller should return).
func (f *Fetcher) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// waitUntilHeadEligible reports whether EVERY available head is still waiting out
// its server-directed retry delay. If so it returns the duration until the
// earliest head becomes eligible (capped at idleWait so a far-future delay still
// re-checks periodically) and true. If at least one head is contactable now, it
// returns (0, false).
func (f *Fetcher) waitUntilHeadEligible() (time.Duration, bool) {
	now := f.now()
	var earliest time.Time
	any := false
	for _, srv := range f.availableServers() {
		if !now.Before(srv.NextContactAt) {
			// Contactable now.
			return 0, false
		}
		any = true
		if earliest.IsZero() || srv.NextContactAt.Before(earliest) {
			earliest = srv.NextContactAt
		}
	}
	if !any {
		// No available heads at all (all in connection backoff) — let the caller
		// fall through to fetchOne, which returns no work and sleeps.
		return 0, false
	}
	wait := earliest.Sub(now)
	if wait > idleWait {
		wait = idleWait
	}
	return wait, true
}

// warnNoWork emits the one-time "connected but getting no work" diagnostic. It
// compares the leafs the volunteer is attached to against the runtimes it can
// actually run: if every attached leaf needs a container runtime this box lacks,
// that's the (fixable) reason and it says so; otherwise the queue is most likely
// just empty, which it reports without crying wolf.
func (f *Fetcher) warnNoWork() {
	hasContainer := f.registry != nil && f.registry.GetRuntime("container") != nil
	var runtimes []string
	if f.registry != nil {
		runtimes = f.registry.AvailableRuntimes()
	}

	var totalLeafs, containerBlocked int
	if f.enabledLeafsFunc != nil && f.multiClient != nil {
		for _, srv := range f.multiClient.Servers() {
			for _, lf := range f.enabledLeafsFunc(srv.Name) {
				totalLeafs++
				if lf.ExecutionSpec != nil && lf.ExecutionSpec.Image != "" && !hasContainer {
					containerBlocked++
				}
			}
		}
	}

	if totalLeafs > 0 && containerBlocked == totalLeafs {
		f.logger.Warn("connected but getting no work: every attached leaf needs a container runtime this volunteer doesn't have — install Docker or Podman (see the volunteer setup docs), or attach a head with native leafs",
			"runtimes", runtimes, "leafs", totalLeafs)
		return
	}
	f.logger.Warn("connected but getting no work after repeated polls — the head has no matching units for this volunteer right now",
		"runtimes", runtimes, "attached_leafs", totalLeafs,
		"hint", "normal if the queue is just empty; if it persists, check that your runtimes match the leafs and that disk/scheduling aren't pausing fetches")
}

// fetchOne issues at most one RequestWorkUnit (to the first eligible head/leaf
// in deficit order whose retry delay has elapsed), then prepares and buffers
// every assignment in the returned batch. It returns the number of units pushed
// into the client work buffer (0 = no work served). Cadence is server-directed:
// on every reply it stamps head.NextContactAt from the head's authoritative
// retry_after_seconds.
func (f *Fetcher) fetchOne(ctx context.Context) (int, error) {
	// ESCALATION (#15 fix 4): expire any paused runtimes whose cooldown has
	// elapsed so they are re-probed once. Done here (not only inside runtimePaused)
	// so a runtime no longer referenced by any leaf still clears eventually.
	f.expirePausedRuntimes()

	// Refresh leaf cache for servers that need it.
	for _, srv := range f.multiClient.Servers() {
		if f.leafCache.NeedsRefresh(srv.Name) {
			f.logger.Debug("fetcher: refreshing leaf cache", "server", srv.Name)
			if err := f.leafCache.Refresh(ctx, srv.Name, srv.Client); err != nil {
				f.logger.Warn("fetcher: leaf cache refresh failed", "server", srv.Name, "error", err)
			}
		}
	}

	available := f.availableServers()
	if len(available) == 0 {
		f.logger.Debug("fetcher: no available servers")
		return 0, nil
	}
	f.logger.Debug("fetcher: available servers", "count", len(available), "servers", serverNames(available))

	// Try heads in deficit order, skipping any still waiting out their
	// server-directed retry delay.
	tried := make(map[string]bool)
	for len(tried) < len(available) {
		head := f.selector.SelectHead(filterOut(available, tried))
		if head == nil {
			f.logger.Debug("fetcher: no more heads to try")
			break
		}
		tried[head.Name] = true

		// Obey the head's server-directed retry delay.
		if f.now().Before(head.NextContactAt) {
			f.logger.Debug("fetcher: head waiting out retry delay", "server", head.Name, "next_contact_at", head.NextContactAt)
			continue
		}

		enabled := f.enabledLeafsFunc(head.Name)
		if len(enabled) == 0 {
			// No cached/enabled leafs for this head (e.g. a head that doesn't
			// surface GetHeadInfo). Fall back to a single any-leaf request through
			// the SAME batched path: the head picks any matching leaf for us,
			// honoring the volunteer's config-level leaf filters.
			var leafIDs, blockedIDs []string
			if f.leafPrefsFunc != nil {
				leafIDs, blockedIDs = f.leafPrefsFunc()
			}
			// Merge the per-server leaf_preferences blocklist so an any-leaf request
			// can't be served a leaf the user disabled for this head (the bug where a
			// per-server BLOCKLIST was respected in steady state but ignored here).
			if f.serverBlockedLeafIDsFunc != nil {
				blockedIDs = mergeUnique(blockedIDs, f.serverBlockedLeafIDsFunc(head.Name))
			}
			f.logger.Debug("fetcher: no cached leafs, requesting any-leaf", "server", head.Name, "leaf_ids", leafIDs, "blocked_ids", blockedIDs)
			pushed, _ := f.requestAndBuffer(ctx, head, anyLeafInfo, leafIDs, blockedIDs)
			if pushed > 0 {
				return pushed, nil
			}
			continue
		}
		f.logger.Debug("fetcher: trying server", "server", head.Name, "enabled_leafs", len(enabled), "leaf_slugs", leafSlugs(enabled))

		orderedLeafs := f.selector.SelectLeafByDeficitOrder(head.Name, enabled)
		for _, leaf := range orderedLeafs {
			// ESCALATION (#15 fix 4): if this leaf needs a runtime we've paused
			// after repeated abandons, skip it BEFORE issuing RequestWorkUnit.
			// This pre-request skip is the load-bearing stop for the
			// grab->abandon churn and the head-side reassignment burn.
			if reqRt := requiredRuntimeForLeaf(leaf); f.runtimePaused(reqRt) {
				f.logger.Debug("fetcher: skipping leaf for paused runtime", "server", head.Name, "leaf_slug", leaf.Slug, "runtime", reqRt)
				continue
			}

			pushed, stop := f.requestAndBuffer(ctx, head, leaf, []string{leaf.ID}, nil)
			if pushed > 0 {
				return pushed, nil
			}
			if stop {
				// Transport error or rate-limit on this head: stop trying its leafs.
				break
			}
			// No work / all-abandoned for this leaf: try the next leaf.
		}
	}

	f.logger.Debug("fetcher: exhausted all eligible servers and leafs, no work buffered")
	return 0, nil
}

// anyLeafInfo is the placeholder leaf descriptor used for the no-cached-leafs
// any-leaf request path. Its slug labels the assignment-recording bucket.
var anyLeafInfo = CachedLeafInfo{ID: "", Slug: "any"}

// mergeUnique returns a∪b preserving order and dropping duplicates and empties.
func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// requestAndBuffer issues one RequestWorkUnit to head for the given leaf filter,
// stamps the server-directed retry delay, and buffers every assignment in the
// reply. It returns the number of units buffered and whether the caller should
// stop trying further leafs on this head (true on transport error or rate-limit).
func (f *Fetcher) requestAndBuffer(ctx context.Context, head *ServerConnection, leaf CachedLeafInfo, leafIDs, blockedIDs []string) (pushed int, stop bool) {
	// Size the batch request from the remaining hours deficit. The per-unit
	// seconds estimate comes from the leaf-level, benchmark-independent estimate
	// (#29) so short-unit leafs fill work_buffer_hours on the FIRST request rather
	// than idling at the flat ceiling. The hours-deficit math in batchSizeFn binds;
	// maxBatchPerRequest is only a safety ceiling.
	var estSec float64
	if f.leafEstSecondsFn != nil {
		estSec = f.leafEstSecondsFn(leaf)
	}
	maxAssignments := int32(1)
	if f.batchSizeFn != nil {
		maxAssignments = f.batchSizeFn(estSec)
	}

	// Report the work units this volunteer currently holds so the head can release
	// any reservations it no longer holds (e.g. dropped across a restart). The set
	// spans buffer + running slots across all heads; ids are globally unique, so a
	// head only ever matches its own units — over-reporting is safe, under-reporting
	// is what must be avoided.
	var heldIDs []string
	if f.heldWorkUnitIDsFn != nil {
		heldIDs = f.heldWorkUnitIDsFn()
	}

	f.logger.Debug("fetcher: requesting work unit", "server", head.Name, "leaf_id", leaf.ID, "leaf_slug", leaf.Slug, "max_assignments", maxAssignments, "held", len(heldIDs))
	resp, err := head.Client.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{
		VolunteerId:      head.VolunteerID,
		PublicKey:        f.pubKey,
		HostId:           head.HostID,
		LeafIds:          leafIDs,
		BlockedLeafIds:   blockedIDs,
		MaxAssignments:   maxAssignments,
		CurrentAvailable: f.cachedHW,
		HeldWorkUnitIds:  heldIDs,
	})
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.ResourceExhausted {
			// RATE LIMITED: the head is shedding load. ResourceExhausted carries NO
			// server-directed value (blocker #2 resolution), so we apply a fixed
			// jittered LOCAL backoff to this head's NextContactAt. Logged at Info
			// (not Warn) since it is normal flow control.
			f.applyResourceExhaustedBackoff(head, leaf.Slug, err)
			return 0, true
		}
		if ok && st.Code() == codes.NotFound {
			// DEFENSIVE: the no-work NotFound sentinel was removed from the protocol
			// (no-work is now an OK reply with empty assignments). A current head
			// never returns NotFound here, but if one does we treat it as no-work —
			// the head is healthy and just empty — rather than a transport fault, so
			// it stays available and is not parked in reconnect backoff.
			f.logger.Debug("fetcher: head returned NotFound (treated as no-work)", "server", head.Name, "leaf_slug", leaf.Slug)
			head.Available = true
			head.Backoff = 0
			return 0, false
		}
		// HOST-UNKNOWN self-heal (BG-25): a non-empty host id this head no longer
		// recognizes (a head reset, a mint-time eviction, or an operator revocation).
		// Checked BEFORE IsVolunteerTooOldError — the refusal text also carries the word
		// "outdated" (so PRE-issuance builds classify it as too-old and print the update
		// hint), so this ordering is load-bearing (design audit F-G): an issuance-aware
		// build MUST re-register here instead of degrading to the no-work update path.
		// One re-register attempt per refusal, then the normal reconnect backoff.
		if client.IsHostUnknownError(err) {
			f.logger.Warn("fetcher: head refused our host id (unknown or revoked); discarding it and re-registering",
				"server", head.Name, "leaf_slug", leaf.Slug, "error", err)
			if f.reRegisterFn != nil {
				newID, rerr := f.reRegisterFn(ctx, head)
				if rerr == nil {
					// Re-registered: adopt the fresh id (possibly empty = host-less) and
					// keep the head available so the next loop tick retries it with the
					// new id — no backoff growth. Stop trying this head's other leafs
					// this cycle.
					head.HostID = newID
					f.logger.Info("fetcher: re-registered after host-unknown refusal",
						"server", head.Name, "host_id_issued", newID != "")
					return 0, true
				}
				f.logger.Warn("fetcher: re-register after host-unknown refusal failed; backing off",
					"server", head.Name, "error", rerr)
			}
			// No self-heal hook, or the re-register failed: fall through to the reconnect
			// backoff and try again later.
			head.Available = false
			head.LastError = f.now()
			if head.Backoff == 0 {
				head.Backoff = f.backoff
			} else {
				head.Backoff = time.Duration(float64(head.Backoff) * 2)
				if head.Backoff > f.maxBackoff {
					head.Backoff = f.maxBackoff
				}
			}
			return 0, true
		}
		// "Volunteer too old": the head's version-coupling rejection. Surface a
		// distinct, actionable WARN rather than burying it in the generic transport
		// error, so the (fleet-wide) "update your build" fix is obvious at a glance.
		if client.IsVolunteerTooOldError(err) {
			f.logger.Warn("fetcher: this volunteer build is too old for the head; run 'lettuce-volunteer update'",
				"server", head.Name, "leaf_slug", leaf.Slug, "error", err, "code", st.Code())
		} else {
			// Connection error (Unavailable/Internal/etc.): no delay to obey, so fall
			// back to the per-head exponential reconnect backoff.
			f.logger.Warn("fetcher: gRPC error requesting work", "server", head.Name, "leaf_slug", leaf.Slug, "error", err, "code", st.Code())
		}
		head.Available = false
		head.LastError = f.now()
		if head.Backoff == 0 {
			head.Backoff = f.backoff
		} else {
			head.Backoff = time.Duration(float64(head.Backoff) * 2)
			if head.Backoff > f.maxBackoff {
				head.Backoff = f.maxBackoff
			}
		}
		return 0, true
	}

	// Success: obey the server-directed retry delay on EVERY reply, including the
	// no-work path.
	head.Available = true
	head.Backoff = 0
	f.applyServerRetryDelay(head, resp.RetryAfterSeconds)

	// No-work is an OK response carrying an empty assignments list (the
	// codes.NotFound sentinel was removed from the protocol).
	if len(resp.Assignments) == 0 {
		f.logger.Debug("fetcher: no work for leaf (empty assignments)", "server", head.Name, "leaf_slug", leaf.Slug, "retry_after_s", resp.RetryAfterSeconds)
		return 0, false
	}

	// Prepare and buffer every assignment in the batch.
	return f.bufferBatch(ctx, head, leaf, resp.Assignments), false
}

// bufferBatch prepares each assignment in a batch and pushes it into the client
// work buffer, recording the assignment with the selector once per buffered
// unit. Unusable units (bad ID, no runtime, prepare failure) are abandoned back
// to the head. Returns the count actually buffered.
func (f *Fetcher) bufferBatch(ctx context.Context, head *ServerConnection, leaf CachedLeafInfo, assignments []*lettucev1.WorkUnitAssignment) int {
	pushed := 0
	// Dedup against what this volunteer already holds (prefetch buffer + active slots)
	// and against earlier units in this same batch. A head should never hand a unit a
	// volunteer already holds, but if one slips through (e.g. a re-stage that raced the
	// volunteer's held-set report), running the duplicate is pure waste — its result is
	// rejected as a duplicate from the same volunteer. Skip it WITHOUT abandoning: an
	// abandon is keyed on (unit, volunteer) and would close the legitimate copy we
	// already hold; the redundant reservation instead lapses and the head reclaims it.
	held := make(map[string]struct{})
	if f.heldWorkUnitIDsFn != nil {
		for _, id := range f.heldWorkUnitIDsFn() {
			held[id] = struct{}{}
		}
	}
	for _, asg := range assignments {
		wu := runtime.WorkUnitFromProto(asg)
		// Stamp the dispatching head (display name) on the unit: the artifact
		// netguard opt-in (runtime/artifact_exemption.go) is scoped per head, and
		// this is where units and heads meet.
		wu.SourceHead = head.Name
		f.logger.Info("fetcher: received work unit", "work_unit_id", wu.ID, "leaf_id", wu.LeafID, "leaf_slug", leaf.Slug, "runtime", wu.Runtime)

		// SECURITY (H2): the head supplies the work unit ID, which becomes the
		// trailing component of on-disk paths (and container bind-mount sources).
		// Reject anything that is not a canonical UUID before it reaches a runtime.
		if idErr := runtime.ValidateWorkUnitID(wu.ID); idErr != nil {
			f.logger.Warn("fetcher: rejecting work unit with invalid ID", "work_unit_id", wu.ID, "leaf_slug", leaf.Slug, "error", idErr)
			f.abandonWorkUnit(ctx, head, wu, idErr.Error())
			continue
		}

		// Skip a unit already held (buffered, running, or seen earlier in this batch).
		if _, dup := held[wu.ID]; dup {
			f.logger.Debug("fetcher: skipping duplicate work unit already held", "work_unit_id", wu.ID, "leaf_slug", leaf.Slug)
			continue
		}
		held[wu.ID] = struct{}{}

		rt, selErr := f.registry.SelectRuntime(wu)
		if selErr != nil {
			f.logger.Warn("fetcher: no runtime for work unit", "work_unit_id", wu.ID, "runtime", wu.Runtime, "error", selErr)
			// ESCALATION (#15 fix 4): capability category A — no registered or
			// capable runtime for this unit's required runtime.
			f.recordRuntimeAbandon(runtimeKeyForWU(wu), selErr)
			f.abandonWorkUnit(ctx, head, wu, selErr.Error())
			continue
		}

		// Per-head runtime trust (execute-side backstop): even if this machine CAN run the
		// selected runtime, only run it for a head the volunteer trusted for that runtime
		// kind at attach — WASM is always trusted. An honest head never dispatches an
		// untrusted runtime (we don't advertise it to that head; see advertisedForServer),
		// so this only fires for a malicious/compromised head. Abandon WITHOUT tripping the
		// per-runtime circuit breaker, which is global and would otherwise wrongly pause the
		// runtime for OTHER attached heads that do trust it.
		if !head.Config.TrustsRuntime(rt.Name()) {
			f.logger.Warn("fetcher: head not trusted for this runtime; abandoning unit",
				"work_unit_id", wu.ID, "runtime", rt.Name(), "head", head.Name)
			f.abandonWorkUnit(ctx, head, wu, "runtime not trusted for this head")
			continue
		}
		f.logger.Debug("fetcher: selected runtime", "work_unit_id", wu.ID, "runtime_type", fmt.Sprintf("%T", rt))

		f.logger.Debug("fetcher: preparing work unit", "work_unit_id", wu.ID)
		// A buffered (reserved) unit is leased purely via its reservation window
		// (reserved_until), not a per-task heartbeat, so a long image pull no longer
		// needs a keep-alive to look alive. Prepare directly.
		prep, prepErr := rt.Prepare(ctx, wu)
		if prepErr != nil {
			f.logger.Warn("fetcher: prepare FAILED", "work_unit_id", wu.ID, "leaf_slug", leaf.Slug, "runtime", wu.Runtime, "error", prepErr)
			// ESCALATION (#15 fix 4): capability category B — Prepare failed for
			// this runtime (image pull / binary download / setup).
			f.recordRuntimeAbandon(strings.ToLower(rt.Name()), prepErr)
			f.abandonWorkUnit(ctx, head, wu, prepErr.Error())
			continue
		}
		f.logger.Info("fetcher: prepare succeeded", "work_unit_id", wu.ID, "work_dir", prep.WorkDir)

		// ESCALATION (#15 fix 4): a successful Prepare clears the runtime's
		// abandon streak (and any pause).
		f.resetRuntimeAbandon(strings.ToLower(rt.Name()))

		item := &PreFetchItem{
			WU:        wu,
			WUResp:    asg,
			Prep:      prep,
			Runtime:   rt,
			Conn:      head,
			FetchedAt: f.now(),
		}
		if err := f.queue.Push(item); err != nil {
			// Buffer filled between the fullness check and now — return the un-run
			// unit to the head and clean up rather than orphaning it as reserved.
			f.logger.Warn("fetcher: queue push failed (full between check and push)", "error", err)
			f.abandonWorkUnit(ctx, head, wu, "buffer full")
			if rt != nil && prep != nil {
				rt.Cleanup(prep)
			}
			break
		}

		f.logger.Debug("fetcher: buffered work unit", "work_unit_id", wu.ID, "leaf_id", wu.LeafID)

		// RecordAssignment is called once per buffered unit.
		f.selector.RecordAssignment(head.Name, leaf.Slug)
		pushed++
	}
	return pushed
}

// abandonWorkUnit tells the server to release a work unit the volunteer can't execute.
// It detaches from the caller's context so abandonment still reaches the head even
// when the caller's context was cancelled (e.g. mid-shutdown). See item 4.
func (f *Fetcher) abandonWorkUnit(ctx context.Context, conn *ServerConnection, wu *runtime.WorkUnit, reason string) {
	abCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	resp, err := conn.Client.AbandonWorkUnit(abCtx, &lettucev1.AbandonWorkUnitRequest{
		WorkUnitId:  wu.ID,
		VolunteerId: conn.VolunteerID,
		PublicKey:   f.pubKey,
		Reason:      reason,
	})
	if err != nil {
		f.logger.Warn("fetcher: abandon work unit failed", "work_unit_id", wu.ID, "error", err)
		return
	}
	f.logger.Info("fetcher: abandoned work unit", "work_unit_id", wu.ID, "requeued", resp.Requeued)
}

// The PREPARING-heartbeat helpers (prepareWithHeartbeat / runPrepareHeartbeat) are
// removed: per-task heartbeats no longer exist. A buffered unit is leased via its
// reservation window (reserved_until) and reclaimed by the head's lapsed-reservation
// sweep if never run-started, so rt.Prepare runs without a keep-alive heartbeat.

// availableServers returns servers not in backoff.
func (f *Fetcher) availableServers() []*ServerConnection {
	var available []*ServerConnection
	for _, srv := range f.multiClient.Servers() {
		if srv.Available || time.Since(srv.LastError) >= srv.Backoff {
			available = append(available, srv)
		} else {
			f.logger.Debug("fetcher: server in backoff", "server", srv.Name, "backoff", srv.Backoff, "last_error", srv.LastError)
		}
	}
	return available
}

// applyServerRetryDelay obeys the head's authoritative server-directed retry
// delay: it stamps head.NextContactAt so the fetcher will not contact this head
// again until at least retryAfterSeconds have elapsed. The head already applied
// its own jitter, so the volunteer obeys the value verbatim. A zero/negative
// value means "no enforced delay" (contact again as soon as other gates allow).
func (f *Fetcher) applyServerRetryDelay(head *ServerConnection, retryAfterSeconds int32) {
	if retryAfterSeconds <= 0 {
		head.NextContactAt = time.Time{}
		return
	}
	head.NextContactAt = f.now().Add(time.Duration(retryAfterSeconds) * time.Second)
	f.logger.Debug("fetcher: obeying server-directed retry delay",
		"server", head.Name, "retry_after_s", retryAfterSeconds, "next_contact_at", head.NextContactAt)
}

// applyResourceExhaustedBackoff applies a fixed, jittered LOCAL backoff to a head
// that answered codes.ResourceExhausted. Unlike the server-directed retry delay,
// ResourceExhausted carries no authoritative value (the head returned no body and
// just wants the caller gone), so the volunteer chooses the delay locally:
// rateLimitBackoff (default 30s) ±20%, capped at maxResourceExhaustedBackoff
// (900s). It is parked via NextContactAt, the same per-head gate as the
// server-directed delay, so the two mechanisms compose cleanly.
func (f *Fetcher) applyResourceExhaustedBackoff(head *ServerConnection, leafSlug string, err error) {
	backoff := withBackoffJitter(f.rateLimitBackoff)
	if backoff > maxResourceExhaustedBackoff {
		backoff = maxResourceExhaustedBackoff
	}
	head.NextContactAt = f.now().Add(backoff)
	f.logger.Info("fetcher: head shed load (ResourceExhausted), backing off locally",
		"server", head.Name, "leaf_slug", leafSlug, "error", err, "backoff", backoff)
}

// withBackoffJitter applies +/-20% jitter to a backoff duration.
func withBackoffJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	jittered := float64(d) * (1 + (rand.Float64()*2-1)*0.2)
	if jittered < 0 {
		jittered = 0
	}
	return time.Duration(jittered)
}

// runtimeKeyForWU normalizes a work unit's runtime hint for the abandon counter,
// mirroring RuntimeRegistry.SelectRuntime (empty -> "native").
func runtimeKeyForWU(wu *runtime.WorkUnit) string {
	name := strings.ToLower(wu.Runtime)
	if name == "" {
		return "native"
	}
	return name
}

// requiredRuntimeForLeaf derives the runtime a leaf needs, mirroring the
// SelectRuntime/CanHandle precedence: container if an image is set; else wasm if
// a wasm binary is present and no image; else native. Used to decide whether a
// leaf must be skipped because its runtime is paused.
func requiredRuntimeForLeaf(leaf CachedLeafInfo) string {
	spec := leaf.ExecutionSpec
	if spec == nil {
		return "native"
	}
	if spec.Image != "" {
		return "container"
	}
	if _, ok := spec.Binaries["wasm"]; ok {
		return "wasm"
	}
	return "native"
}

// recordRuntimeAbandon increments the consecutive-abandon counter for a
// capability-driven abandon (missing/incapable runtime, or a failed Prepare).
// When the count first reaches the threshold (and the runtime is not already
// paused) it emits exactly one loud WARN naming the runtime, the count, the last
// error and a runtime-specific remedy, then pauses the runtime.
func (f *Fetcher) recordRuntimeAbandon(name string, err error) {
	f.runtimeAbandons[name]++
	count := f.runtimeAbandons[name]
	if count != runtimeAbandonPauseThreshold {
		return
	}
	if _, already := f.pausedRuntimes[name]; already {
		return
	}
	f.pausedRuntimes[name] = f.now()
	f.logger.Warn("fetcher: runtime repeatedly failing — pausing leaf requests that need it",
		"runtime", name,
		"consecutive_abandons", count,
		"last_error", err,
		"remedy", remedyForRuntime(name),
		"cooldown", runtimeAbandonCooldown)
}

// resetRuntimeAbandon clears the abandon counter and any pause for a runtime
// after a successful Prepare for it. When this clears an active pause it emits a
// single Info marking the recovery (the trip is logged loudly in
// recordRuntimeAbandon; without this the un-pause was silent).
func (f *Fetcher) resetRuntimeAbandon(name string) {
	delete(f.runtimeAbandons, name)
	if _, wasPaused := f.pausedRuntimes[name]; wasPaused {
		delete(f.pausedRuntimes, name)
		f.logger.Info("fetcher: runtime recovered, resuming requests", "runtime", name)
	}
}

// runtimePaused reports whether the named runtime is currently paused, clearing
// the pause (and zeroing its count) if the cooldown has elapsed so it is
// re-probed once.
func (f *Fetcher) runtimePaused(name string) bool {
	pausedAt, ok := f.pausedRuntimes[name]
	if !ok {
		return false
	}
	if f.now().Sub(pausedAt) >= runtimeAbandonCooldown {
		delete(f.pausedRuntimes, name)
		delete(f.runtimeAbandons, name)
		return false
	}
	return true
}

// expirePausedRuntimes sweeps all paused runtimes and clears any whose cooldown
// has elapsed, so a runtime no longer referenced by any leaf is still re-probed.
func (f *Fetcher) expirePausedRuntimes() {
	now := f.now()
	for name, pausedAt := range f.pausedRuntimes {
		if now.Sub(pausedAt) >= runtimeAbandonCooldown {
			delete(f.pausedRuntimes, name)
			delete(f.runtimeAbandons, name)
		}
	}
}

// remedyForRuntime returns a runtime-specific operator hint for the loud WARN.
func remedyForRuntime(name string) string {
	switch name {
	case "container":
		return "install or repair the container backend (Docker/Podman) and make sure it is running"
	case "wasm":
		return "verify the wasm runtime is available and the wasm binary URL/checksum is correct"
	default:
		return "check this leaf's native binary URLs/checksums and that this host can run the work unit"
	}
}

// serverNames extracts names from server connections for logging.
func serverNames(servers []*ServerConnection) []string {
	names := make([]string, len(servers))
	for i, s := range servers {
		names[i] = s.Name
	}
	return names
}

// leafSlugs extracts slugs from cached leaf info for logging.
func leafSlugs(leafs []CachedLeafInfo) []string {
	slugs := make([]string, len(leafs))
	for i, l := range leafs {
		slugs[i] = l.Slug
	}
	return slugs
}
