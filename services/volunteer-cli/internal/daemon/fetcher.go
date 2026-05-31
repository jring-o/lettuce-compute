package daemon

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
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
}

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
				f.abandonWorkUnit(ctx, head, wu, selErr.Error())
				continue
			}
			f.logger.Debug("fetcher: selected runtime", "work_unit_id", wu.ID, "runtime_name", fmt.Sprintf("%T", rt))

			f.logger.Debug("fetcher: preparing work unit", "work_unit_id", wu.ID)
			prep, hbCancel, prepErr := f.prepareWithHeartbeat(ctx, head, wu, resp.HeartbeatIntervalSeconds, rt)
			if prepErr != nil {
				f.logger.Warn("fetcher: prepare FAILED", "work_unit_id", wu.ID, "leaf_slug", leaf.Slug, "runtime", wu.Runtime, "error", prepErr)
				f.abandonWorkUnit(ctx, head, wu, prepErr.Error())
				continue
			}
			f.logger.Info("fetcher: prepare succeeded", "work_unit_id", wu.ID, "work_dir", prep.WorkDir)

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
		f.abandonWorkUnit(ctx, conn, wu, selErr.Error())
		return nil, nil
	}

	prep, hbCancel, prepErr := f.prepareWithHeartbeat(ctx, conn, wu, resp.HeartbeatIntervalSeconds, rt)
	if prepErr != nil {
		f.logger.Warn("fetcher: prepare failed (legacy)", "work_unit_id", wu.ID, "error", prepErr)
		f.abandonWorkUnit(ctx, conn, wu, prepErr.Error())
		return nil, nil
	}

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
