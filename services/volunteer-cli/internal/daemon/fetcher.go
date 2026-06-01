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

	// enabledLeafsFunc is called to get enabled leafs for a server.
	// Injected from the daemon to reuse its filtering logic.
	enabledLeafsFunc func(serverName string) []CachedLeafInfo

	// leafPrefsFunc returns leaf ID filter and block list from config.
	leafPrefsFunc func() (leafIDs, blockedIDs []string)

	// shouldFetchFunc checks whether fetching is allowed (disk space, scheduler, etc.).
	// Returns true if fetching should proceed, false to wait.
	shouldFetchFunc func() bool

	// --- THROTTLE (#18 fix 2): minimum inter-request interval ---
	// minInterval is the floor on how often RequestWorkUnit may be issued across
	// the poll cycle. It composes with the per-head backoff and the no-work
	// backoff: it bounds the FAST path (zero-delay success, or a sub-second
	// backoff that resets to f.backoff on each success), which those backoffs do
	// not. Injectable so tests can set it to 0 to disable the gate.
	minInterval time.Duration
	// lastRequest is when the most recent RequestWorkUnit poll cycle began. Only
	// touched from the single Run goroutine, so no lock is required.
	lastRequest time.Time
	// rateLimitBackoff is the dedicated (larger) per-head backoff floor applied
	// when a head answers codes.ResourceExhausted, so we ease pressure on the
	// shared head rate bucket rather than hammering it.
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

// Throttle / escalation defaults. minIntervalFloor is the hard floor a
// configured non-zero interval is clamped to (an explicit 0 disables the gate,
// used by tests). The head's shared rate bucket sustains ~1 req/sec across all
// volunteers behind one proxy IP; a 2s floor caps each volunteer at ~0.5
// req/sec so a small pool stays within the shared budget.
const (
	defaultMinInterval      = 2 * time.Second
	minIntervalFloor        = 1 * time.Second
	defaultRateLimitBackoff = 5 * time.Second
)

// runtimeAbandonPauseThreshold is how many consecutive capability-driven
// abandons for one runtime trip the circuit breaker. It matches the head's
// max_reassignments default (3): after a volunteer has caused a unit to exhaust
// its reassignments once, it stops contributing.
const runtimeAbandonPauseThreshold = 3

// runtimeAbandonCooldown is how long a tripped runtime stays paused before it
// is re-probed once. Container backends recover (Docker/Podman restart) and
// leaf ExecutionSpecs change on head edits, so the pause is time-bounded rather
// than permanent-until-restart; this lines up with the 5-min leaf-cache refresh.
const runtimeAbandonCooldown = 10 * time.Minute

// NewFetcher creates a new fetcher that fills the pre-fetch queue.
func NewFetcher(d *Daemon, queue *PreFetchQueue, selector *WeightedSelector, leafCache *LeafCache) *Fetcher {
	return &Fetcher{
		queue:            queue,
		selector:         selector,
		leafCache:        leafCache,
		registry:         d.runtimeRegistry,
		multiClient:      d.multiClient,
		logger:           d.logger,
		backoff:          d.initialBackoff,
		maxBackoff:       d.maxBackoff,
		cachedHW:         d.cachedHW,
		pubKey:           d.pubKey,
		enabledLeafsFunc: d.enabledLeafs,
		leafPrefsFunc:    d.leafPreferences,
		shouldFetchFunc:  d.shouldFetch,
		minInterval:      resolveMinInterval(d.fetcherMinInterval),
		rateLimitBackoff: defaultRateLimitBackoff,
		runtimeAbandons:  make(map[string]int),
		pausedRuntimes:   make(map[string]time.Time),
		now:              time.Now,
	}
}

// resolveMinInterval maps the Daemon's fetcherMinInterval knob to the gate's
// effective interval:
//   - 0 (the production zero value): use defaultMinInterval (2s).
//   - negative: gate disabled (used by tests so loops stay fast).
//   - positive: use it, but clamp up to the hard floor so the gate can never be
//     configured tighter than minIntervalFloor.
func resolveMinInterval(configured time.Duration) time.Duration {
	switch {
	case configured < 0:
		return 0
	case configured == 0:
		return defaultMinInterval
	case configured < minIntervalFloor:
		return minIntervalFloor
	default:
		return configured
	}
}

// noWorkWarnThreshold is how many consecutive empty polls (no work returned)
// trigger the one-time "connected but getting no work" diagnostic WARN. With
// exponential backoff this is roughly half a minute of genuine idleness, long
// enough to skip a momentarily-empty queue but short enough to be useful.
const noWorkWarnThreshold = 5

// Run is the fetcher's main loop. It fills the queue until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) {
	currentBackoff := f.backoff
	f.logger.Info("fetcher: started", "initial_backoff", f.backoff, "max_backoff", f.maxBackoff)

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
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}

		// Drop expiring items.
		f.queue.DropExpiring(0.1)

		// If queue is full, wait before checking again.
		if f.queue.IsFull() {
			f.logger.Debug("fetcher: queue is full, waiting 5s", "queue_len", f.queue.Len())
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		// THROTTLE (#18 fix 2): enforce a minimum interval between RequestWorkUnit
		// poll cycles. This sits on the poll CYCLE (the same head+leaf only recurs
		// across cycles, never within a single fetchOne's distinct-leaf inner loop)
		// and is independent of which branch (success / NotFound / error) the prior
		// cycle took, so it bounds both the zero-delay success path and any
		// sub-second backoff reset. It composes with the existing per-head and
		// no-work backoffs rather than replacing them.
		if f.minInterval > 0 {
			if wait := f.minInterval - time.Since(f.lastRequest); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}
		f.lastRequest = time.Now()

		f.logger.Debug("fetcher: attempting fetchOne", "queue_len", f.queue.Len())

		// Try to fetch a work unit.
		item, err := f.fetchOne(ctx)
		if err != nil {
			f.logger.Warn("fetcher: fetchOne returned error", "error", err, "backoff", currentBackoff)
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Back off on failure.
			select {
			case <-ctx.Done():
				return
			case <-time.After(currentBackoff):
			}
			currentBackoff = time.Duration(float64(currentBackoff) * 2)
			if currentBackoff > f.maxBackoff {
				currentBackoff = f.maxBackoff
			}
			continue
		}

		if item == nil {
			f.logger.Debug("fetcher: fetchOne returned nil (no work available)", "backoff", currentBackoff)
			emptyPolls++
			if emptyPolls >= noWorkWarnThreshold && !warnedNoWork {
				f.warnNoWork()
				warnedNoWork = true
			}
			// No work available — back off.
			select {
			case <-ctx.Done():
				return
			case <-time.After(currentBackoff):
			}
			currentBackoff = time.Duration(float64(currentBackoff) * 2)
			if currentBackoff > f.maxBackoff {
				currentBackoff = f.maxBackoff
			}
			f.logger.Debug("fetcher: backoff increased", "new_backoff", currentBackoff)
			continue
		}

		// Got work — clear the no-work streak so the diagnostic can fire again if
		// the volunteer later goes idle for a new reason.
		emptyPolls = 0
		warnedNoWork = false

		f.logger.Info("fetcher: got work unit", "work_unit_id", item.WU.ID, "leaf_id", item.WU.LeafID, "server", item.Conn.Name)

		// Push to queue.
		if err := f.queue.Push(item); err != nil {
			f.logger.Warn("fetcher: queue push failed (full between check and push)", "error", err)
			// Queue became full between check and push — return the un-run unit to
			// the head and clean up rather than orphaning it as ASSIGNED.
			item.stopHeartbeat()
			f.abandonWorkUnit(ctx, item.Conn, item.WU, "queue full")
			if item.Runtime != nil && item.Prep != nil {
				item.Runtime.Cleanup(item.Prep)
			}
			continue
		}

		f.logger.Debug("fetcher: pushed to queue", "queue_len", f.queue.Len())

		// Reset backoff on success.
		currentBackoff = f.backoff
	}
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

// fetchOne attempts to fetch and prepare a single work unit.
// Returns nil, nil when no work is available.
func (f *Fetcher) fetchOne(ctx context.Context) (*PreFetchItem, error) {
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
		return nil, nil
	}
	f.logger.Debug("fetcher: available servers", "count", len(available), "servers", serverNames(available))

	// Check if any server has cached leafs. If not, fall back to the
	// legacy round-robin path (needed for servers that don't support GetHeadInfo).
	hasLeafs := false
	for _, srv := range available {
		if leafs := f.leafCache.GetLeafs(srv.Name); len(leafs) > 0 {
			hasLeafs = true
			break
		}
	}

	if !hasLeafs {
		f.logger.Debug("fetcher: no cached leafs, falling back to legacy path")
		return f.fetchOneLegacy(ctx, available)
	}

	// Try heads in deficit order.
	tried := make(map[string]bool)
	for len(tried) < len(available) {
		head := f.selector.SelectHead(filterOut(available, tried))
		if head == nil {
			f.logger.Debug("fetcher: no more heads to try")
			break
		}
		tried[head.Name] = true

		enabled := f.enabledLeafsFunc(head.Name)
		if len(enabled) == 0 {
			f.logger.Debug("fetcher: no enabled leafs for server", "server", head.Name)
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
			f.logger.Debug("fetcher: requesting work unit", "server", head.Name, "leaf_id", leaf.ID, "leaf_slug", leaf.Slug)
			resp, err := head.Client.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{
				VolunteerId:      head.VolunteerID,
				PublicKey:        f.pubKey,
				LeafIds:          []string{leaf.ID},
				CurrentAvailable: f.cachedHW,
			})
			if err != nil {
				st, ok := status.FromError(err)
				if ok && st.Code() == codes.NotFound {
					f.logger.Debug("fetcher: no work for leaf (NotFound)", "server", head.Name, "leaf_slug", leaf.Slug)
					head.Available = true
					head.Backoff = 0
					continue
				}
				if ok && st.Code() == codes.ResourceExhausted {
					// RATE LIMITED (#18 fix 2): the head throttled us. Treat it as a
					// calm, dedicated, larger backoff rather than a hard connection
					// error so we stop adding pressure to the shared head bucket. It
					// is logged at Info (not Warn) since it is a normal flow-control
					// signal, not a fault.
					f.applyRateLimitBackoff(head, leaf.Slug, err)
					break
				}
				// Connection error.
				f.logger.Warn("fetcher: gRPC error requesting work", "server", head.Name, "leaf_slug", leaf.Slug, "error", err, "code", st.Code())
				head.Available = false
				head.LastError = time.Now()
				if head.Backoff == 0 {
					head.Backoff = f.backoff
				} else {
					head.Backoff = time.Duration(float64(head.Backoff) * 2)
					if head.Backoff > f.maxBackoff {
						head.Backoff = f.maxBackoff
					}
				}
				break
			}

			// Got work. Prepare it.
			head.Available = true
			head.Backoff = 0

			wu := runtime.WorkUnitFromProto(resp)
			f.logger.Info("fetcher: received work unit", "work_unit_id", wu.ID, "leaf_id", wu.LeafID, "leaf_slug", leaf.Slug, "runtime", wu.Runtime)

			// SECURITY (H2): the head supplies the work unit ID, which becomes the
			// trailing component of on-disk paths (and container bind-mount sources).
			// Reject anything that is not a canonical UUID before it reaches a
			// runtime, so a malicious head can't use path traversal to write outside
			// the data dir. Treat it like any other unusable unit: abandon and skip.
			if idErr := runtime.ValidateWorkUnitID(wu.ID); idErr != nil {
				f.logger.Warn("fetcher: rejecting work unit with invalid ID", "work_unit_id", wu.ID, "leaf_slug", leaf.Slug, "error", idErr)
				f.abandonWorkUnit(ctx, head, wu, idErr.Error())
				continue
			}

			rt, selErr := f.registry.SelectRuntime(wu)
			if selErr != nil {
				f.logger.Warn("fetcher: no runtime for work unit", "work_unit_id", wu.ID, "runtime", wu.Runtime, "error", selErr)
				// ESCALATION (#15 fix 4): capability category A — no registered or
				// capable runtime for this unit's required runtime.
				f.recordRuntimeAbandon(runtimeKeyForWU(wu), selErr)
				f.abandonWorkUnit(ctx, head, wu, selErr.Error())
				continue
			}
			f.logger.Debug("fetcher: selected runtime", "work_unit_id", wu.ID, "runtime_name", fmt.Sprintf("%T", rt))

			f.logger.Debug("fetcher: preparing work unit", "work_unit_id", wu.ID)
			prep, hbCancel, prepErr := f.prepareWithHeartbeat(ctx, head, wu, resp.HeartbeatIntervalSeconds, rt)
			if prepErr != nil {
				f.logger.Warn("fetcher: prepare FAILED", "work_unit_id", wu.ID, "leaf_slug", leaf.Slug, "runtime", wu.Runtime, "error", prepErr)
				// ESCALATION (#15 fix 4): capability category B — Prepare failed for
				// this runtime (image pull / binary download / setup). Keyed on the
				// runtime that was actually selected.
				f.recordRuntimeAbandon(strings.ToLower(rt.Name()), prepErr)
				f.abandonWorkUnit(ctx, head, wu, prepErr.Error())
				continue
			}
			f.logger.Info("fetcher: prepare succeeded", "work_unit_id", wu.ID, "work_dir", prep.WorkDir)

			// ESCALATION (#15 fix 4): a successful Prepare clears the runtime's
			// abandon streak (and any pause). Reset on prepare-success, not
			// select-success: select can pass while Prepare keeps failing.
			f.resetRuntimeAbandon(strings.ToLower(rt.Name()))

			f.selector.RecordAssignment(head.Name, leaf.Slug)

			return &PreFetchItem{
				WU:        wu,
				WUResp:    resp,
				Prep:      prep,
				Runtime:   rt,
				Conn:      head,
				FetchedAt: time.Now(),
				hbCancel:  hbCancel,
			}, nil
		}
	}

	f.logger.Debug("fetcher: exhausted all servers and leafs, no work found")
	return nil, nil
}

// fetchOneLegacy falls back to round-robin work requests when no leafs are cached.
// This supports servers that don't implement GetHeadInfo.
func (f *Fetcher) fetchOneLegacy(ctx context.Context, available []*ServerConnection) (*PreFetchItem, error) {
	var leafIDs, blockedIDs []string
	if f.leafPrefsFunc != nil {
		leafIDs, blockedIDs = f.leafPrefsFunc()
	}
	f.logger.Debug("fetcher: legacy request", "leaf_ids", leafIDs, "blocked_ids", blockedIDs)

	resp, conn, err := f.multiClient.RequestWork(ctx, f.pubKey, leafIDs, blockedIDs, f.cachedHW)
	if err != nil {
		// RATE LIMITED (#18 fix 2, legacy mirror): if the head throttled us, park
		// every available head with the dedicated rate-limit backoff floor so we
		// stop hammering the shared bucket. The legacy multi-client API doesn't
		// surface which head answered, so apply the floor to all available heads.
		if st, ok := status.FromError(err); ok && st.Code() == codes.ResourceExhausted {
			for _, srv := range f.availableServers() {
				f.applyRateLimitBackoff(srv, "", err)
			}
			return nil, nil
		}
		f.logger.Debug("fetcher: legacy request failed", "error", err)
		return nil, nil // no work available
	}

	wu := runtime.WorkUnitFromProto(resp)
	f.logger.Info("fetcher: legacy got work unit", "work_unit_id", wu.ID, "leaf_id", wu.LeafID)

	// SECURITY (H2): validate the head-supplied work unit ID before it reaches a
	// runtime (see fetchOne). On a bad ID, abandon and report no work available.
	if idErr := runtime.ValidateWorkUnitID(wu.ID); idErr != nil {
		f.logger.Warn("fetcher: rejecting work unit with invalid ID (legacy)", "work_unit_id", wu.ID, "error", idErr)
		f.abandonWorkUnit(ctx, conn, wu, idErr.Error())
		return nil, nil
	}

	rt, selErr := f.registry.SelectRuntime(wu)
	if selErr != nil {
		f.logger.Warn("fetcher: no runtime for work unit (legacy)", "work_unit_id", wu.ID, "error", selErr)
		// ESCALATION (#15 fix 4, legacy mirror): capability category A.
		f.recordRuntimeAbandon(runtimeKeyForWU(wu), selErr)
		f.abandonWorkUnit(ctx, conn, wu, selErr.Error())
		return nil, nil
	}

	prep, hbCancel, prepErr := f.prepareWithHeartbeat(ctx, conn, wu, resp.HeartbeatIntervalSeconds, rt)
	if prepErr != nil {
		f.logger.Warn("fetcher: prepare failed (legacy)", "work_unit_id", wu.ID, "error", prepErr)
		// ESCALATION (#15 fix 4, legacy mirror): capability category B.
		f.recordRuntimeAbandon(strings.ToLower(rt.Name()), prepErr)
		f.abandonWorkUnit(ctx, conn, wu, prepErr.Error())
		return nil, nil
	}

	// ESCALATION (#15 fix 4, legacy mirror): clear the streak on prepare-success.
	f.resetRuntimeAbandon(strings.ToLower(rt.Name()))

	return &PreFetchItem{
		WU:        wu,
		WUResp:    resp,
		Prep:      prep,
		Runtime:   rt,
		Conn:      conn,
		FetchedAt: time.Now(),
		hbCancel:  hbCancel,
	}, nil
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

// prepareHeartbeatInterval caps how often a PREPARING heartbeat is sent while a
// unit is pulling its image or waiting in the prefetch queue. Kept small so
// last_heartbeat_at stays well within the head's abandonment window.
const prepareHeartbeatInterval = 60 * time.Second

// prepareWithHeartbeat runs rt.Prepare while sending PREPARING heartbeats so a
// long image pull doesn't look like a dead volunteer. On success it returns the
// prep result plus the still-running heartbeat's cancel func (the caller stores
// it on the PreFetchItem and cancels it at slot handoff or disposal). On failure
// it stops the heartbeat and returns the error.
func (f *Fetcher) prepareWithHeartbeat(ctx context.Context, conn *ServerConnection, wu *runtime.WorkUnit, intervalSeconds int32, rt runtime.Runtime) (*runtime.PrepareResult, context.CancelFunc, error) {
	hbCtx, hbCancel := context.WithCancel(context.Background())
	go f.runPrepareHeartbeat(hbCtx, conn, wu, intervalSeconds)

	prep, err := rt.Prepare(ctx, wu)
	if err != nil {
		hbCancel()
		return nil, nil, err
	}
	return prep, hbCancel, nil
}

// runPrepareHeartbeat sends PREPARING heartbeats until ctx is cancelled. The head
// refreshes last_heartbeat_at without transitioning the unit to RUNNING.
func (f *Fetcher) runPrepareHeartbeat(ctx context.Context, conn *ServerConnection, wu *runtime.WorkUnit, intervalSeconds int32) {
	interval := time.Duration(intervalSeconds) * time.Second
	if interval <= 0 || interval > prepareHeartbeatInterval {
		interval = prepareHeartbeatInterval
	}

	send := func() {
		hbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := conn.Client.Heartbeat(hbCtx, &lettucev1.HeartbeatRequest{
			WorkUnitId:  wu.ID,
			VolunteerId: conn.VolunteerID,
			Status:      "PREPARING",
		}); err != nil {
			f.logger.Debug("fetcher: prepare heartbeat failed", "work_unit_id", wu.ID, "server", conn.Name, "error", err)
		}
	}

	send() // immediate, so a long pull is covered from assignment onward
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

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

// applyRateLimitBackoff parks a head that answered codes.ResourceExhausted with
// the dedicated, larger rate-limit backoff floor (then exponential, jittered,
// capped growth on repeat). availableServers() honors Backoff via
// time.Since(LastError) >= Backoff, so the rate-limited head is skipped for the
// longer floor. This is deliberately calmer (Info, not Warn) than the generic
// connection-error path because rate limiting is normal flow control. See #18.
func (f *Fetcher) applyRateLimitBackoff(head *ServerConnection, leafSlug string, err error) {
	f.logger.Info("fetcher: head rate-limited request, backing off",
		"server", head.Name, "leaf_slug", leafSlug, "error", err, "floor", f.rateLimitBackoff)
	head.Available = false
	head.LastError = time.Now()
	if head.Backoff < f.rateLimitBackoff {
		head.Backoff = f.rateLimitBackoff
	} else {
		// +/-20% jitter on the doubling de-synchronizes the fleet so volunteers
		// don't all retry on the same tick (gRPC retry etiquette).
		head.Backoff = withBackoffJitter(head.Backoff * 2)
		if head.Backoff > f.maxBackoff {
			head.Backoff = f.maxBackoff
		}
	}
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
// after a successful Prepare for it.
func (f *Fetcher) resetRuntimeAbandon(name string) {
	delete(f.runtimeAbandons, name)
	delete(f.pausedRuntimes, name)
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
