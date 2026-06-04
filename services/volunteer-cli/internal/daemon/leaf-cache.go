package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// LeafCache stores head info fetched from each connected server.
// Thread-safe. The daemon drives the refresh cycle; the cache does not
// run its own goroutine.
type LeafCache struct {
	mu      sync.RWMutex
	heads   map[string]*CachedHeadInfo // server name -> cached head info
	refresh time.Duration              // default 5 minutes
	logger  *slog.Logger
}

// CachedHeadInfo holds the cached result of a GetHeadInfo RPC call.
type CachedHeadInfo struct {
	Name           string
	Description    string
	URL            string
	Leafs          []CachedLeafInfo
	DefaultWeights map[string]int // slug -> weight
	LastRefreshed  time.Time
}

// CachedExecutionSpec holds cached execution spec for runtime badge rendering.
type CachedExecutionSpec struct {
	Binaries      map[string]string
	Image         string
	GPURequired   bool
	GPUType       string
	MaxMemoryMB   int32
	MaxDiskMB     int32
	NetworkAccess bool
}

// CachedLeafInfo holds cached info about a single leaf.
type CachedLeafInfo struct {
	ID               string
	Slug             string
	Name             string
	Description      string
	ResearchArea     []string
	TaskPattern      string
	State            string // "ACTIVE", "PAUSED", etc.
	QueuedWorkUnits  int
	ActiveVolunteers int
	ExecutionSpec    *CachedExecutionSpec

	// EstimatedDurationSeconds is a per-leaf, benchmark-INDEPENDENT estimate of
	// wall-clock seconds for one unit of this leaf (#29). The head derives it in
	// GetHeadInfo (from an explicit leaf estimate or rsc_fpops_est against the
	// head's reference benchmark) so the volunteer can size its FIRST batch
	// request to a leaf before it has seen any of that leaf's units — without
	// needing a local CPU benchmark. 0 means "no estimate available".
	EstimatedDurationSeconds float64
}

// NewLeafCache creates a new leaf cache with the given refresh interval.
func NewLeafCache(refreshInterval time.Duration, logger *slog.Logger) *LeafCache {
	return &LeafCache{
		heads:   make(map[string]*CachedHeadInfo),
		refresh: refreshInterval,
		logger:  logger,
	}
}

// Refresh calls GetHeadInfo on the given client and stores the result.
func (lc *LeafCache) Refresh(ctx context.Context, serverName string, client WorkClient) error {
	resp, err := client.GetHeadInfo(ctx, &lettucev1.GetHeadInfoRequest{})
	if err != nil {
		return err
	}

	cached := &CachedHeadInfo{
		Name:           resp.Name,
		Description:    resp.Description,
		URL:            resp.Url,
		DefaultWeights: make(map[string]int),
		LastRefreshed:  time.Now(),
	}

	for _, l := range resp.Leafs {
		cli := CachedLeafInfo{
			ID:                       l.Id,
			Slug:                     l.Slug,
			Name:                     l.Name,
			Description:              l.Description,
			ResearchArea:             l.ResearchArea,
			TaskPattern:              l.TaskPattern,
			State:                    l.State,
			QueuedWorkUnits:          int(l.QueuedWorkUnits),
			ActiveVolunteers:         int(l.ActiveVolunteers),
			EstimatedDurationSeconds: l.GetEstimatedDurationSeconds(),
		}
		if es := l.GetExecutionSpec(); es != nil {
			cli.ExecutionSpec = &CachedExecutionSpec{
				Binaries:      es.GetBinaries(),
				Image:         es.GetImage(),
				GPURequired:   es.GetGpuRequired(),
				GPUType:       es.GetGpuType(),
				MaxMemoryMB:   es.GetMaxMemoryMb(),
				MaxDiskMB:     es.GetMaxDiskMb(),
				NetworkAccess: es.GetNetworkAccess(),
			}
		}
		cached.Leafs = append(cached.Leafs, cli)
	}

	for slug, w := range resp.DefaultLeafWeights {
		cached.DefaultWeights[slug] = int(w)
	}

	lc.mu.Lock()
	lc.heads[serverName] = cached
	lc.mu.Unlock()

	return nil
}

// RefreshAll refreshes all servers. Logs warnings on failures but does not
// fail if some servers are unreachable.
func (lc *LeafCache) RefreshAll(ctx context.Context, servers []*ServerConnection) error {
	for _, srv := range servers {
		if err := lc.Refresh(ctx, srv.Name, srv.Client); err != nil {
			lc.logger.Warn("failed to refresh head info",
				"server", srv.Name,
				"error", err,
			)
		}
	}
	return nil
}

// NeedsRefresh returns true if the server's cached data is stale or missing.
func (lc *LeafCache) NeedsRefresh(serverName string) bool {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	info, ok := lc.heads[serverName]
	if !ok {
		return true
	}
	return time.Since(info.LastRefreshed) > lc.refresh
}

// GetHeadInfo returns cached head info for a server, or nil if not cached.
func (lc *LeafCache) GetHeadInfo(serverName string) *CachedHeadInfo {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	return lc.heads[serverName]
}

// GetLeafs returns cached leafs for a server, or nil if not cached.
func (lc *LeafCache) GetLeafs(serverName string) []CachedLeafInfo {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	info, ok := lc.heads[serverName]
	if !ok {
		return nil
	}
	return info.Leafs
}

// GetDefaultWeights returns the default weights map for a server, or nil.
func (lc *LeafCache) GetDefaultWeights(serverName string) map[string]int {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	info, ok := lc.heads[serverName]
	if !ok {
		return nil
	}
	return info.DefaultWeights
}

// AllLeafs returns all cached leafs keyed by server name.
func (lc *LeafCache) AllLeafs() map[string][]CachedLeafInfo {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	result := make(map[string][]CachedLeafInfo, len(lc.heads))
	for name, info := range lc.heads {
		result[name] = info.Leafs
	}
	return result
}

// PopulateForTest directly sets cached head info for a server.
// Intended for use by tests in other packages that need to inject cache state.
func (lc *LeafCache) PopulateForTest(serverName string, info *CachedHeadInfo) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if info.LastRefreshed.IsZero() {
		info.LastRefreshed = time.Now()
	}
	lc.heads[serverName] = info
}
