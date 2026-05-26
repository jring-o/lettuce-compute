package daemon

import (
	"sort"
	"sync"
)

// WeightedSelector implements deficit-based weighted selection for both
// head (server) and leaf levels.
type WeightedSelector struct {
	mu          sync.Mutex
	headWeights map[string]int            // server name -> weight
	leafWeights map[string]map[string]int // server name -> slug -> weight
	headCounts  map[string]int            // cumulative WU count per head
	leafCounts  map[string]map[string]int // cumulative WU count per head per leaf
}

// NewWeightedSelector creates a new selector with empty state.
func NewWeightedSelector() *WeightedSelector {
	return &WeightedSelector{
		headWeights: make(map[string]int),
		leafWeights: make(map[string]map[string]int),
		headCounts:  make(map[string]int),
		leafCounts:  make(map[string]map[string]int),
	}
}

// SetHeadWeights sets weights for all heads.
func (w *WeightedSelector) SetHeadWeights(weights map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.headWeights = make(map[string]int, len(weights))
	for k, v := range weights {
		w.headWeights[k] = v
	}
}

// SetLeafWeights sets effective leaf weights for a specific server.
func (w *WeightedSelector) SetLeafWeights(serverName string, weights map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.leafWeights[serverName] = make(map[string]int, len(weights))
	for k, v := range weights {
		w.leafWeights[serverName][k] = v
	}
}

// SelectHead picks the server with the largest deficit among available servers.
// Returns nil if no servers are available.
func (w *WeightedSelector) SelectHead(available []*ServerConnection) *ServerConnection {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(available) == 0 {
		return nil
	}
	if len(available) == 1 {
		return available[0]
	}

	// Compute total weight and total count for available servers only.
	var totalW, totalC float64
	for _, srv := range available {
		weight := w.headWeights[srv.Name]
		if weight <= 0 {
			weight = 100
		}
		totalW += float64(weight)
		totalC += float64(w.headCounts[srv.Name])
	}

	// Find item with largest deficit.
	var best *ServerConnection
	bestDeficit := -1e18

	// Sort for deterministic tie-breaking.
	sorted := make([]*ServerConnection, len(available))
	copy(sorted, available)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	for _, srv := range sorted {
		weight := w.headWeights[srv.Name]
		if weight <= 0 {
			weight = 100
		}
		targetRatio := float64(weight) / totalW
		actualRatio := float64(0)
		if totalC > 0 {
			actualRatio = float64(w.headCounts[srv.Name]) / totalC
		}
		deficit := targetRatio - actualRatio
		if deficit > bestDeficit {
			bestDeficit = deficit
			best = srv
		}
	}

	return best
}

// SelectLeaf picks the leaf ID with the largest deficit among the enabled leafs
// for a given server. Returns empty string if no leafs available.
func (w *WeightedSelector) SelectLeaf(serverName string, enabledLeafs []CachedLeafInfo) string {
	ordered := w.SelectLeafByDeficitOrder(serverName, enabledLeafs)
	if len(ordered) == 0 {
		return ""
	}
	return ordered[0].ID
}

// SelectLeafByDeficitOrder returns all enabled leafs sorted by deficit (highest first).
// Used for fallback when the top-choice leaf has no work.
func (w *WeightedSelector) SelectLeafByDeficitOrder(serverName string, enabledLeafs []CachedLeafInfo) []CachedLeafInfo {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(enabledLeafs) == 0 {
		return nil
	}

	weights := w.leafWeights[serverName]
	counts := w.leafCounts[serverName]
	if counts == nil {
		counts = make(map[string]int)
	}

	var totalW, totalC float64
	for _, leaf := range enabledLeafs {
		lw := 100
		if weights != nil {
			if v, ok := weights[leaf.Slug]; ok && v > 0 {
				lw = v
			}
		}
		totalW += float64(lw)
		totalC += float64(counts[leaf.Slug])
	}

	type leafDeficit struct {
		leaf    CachedLeafInfo
		deficit float64
	}

	var items []leafDeficit
	for _, leaf := range enabledLeafs {
		lw := 100
		if weights != nil {
			if v, ok := weights[leaf.Slug]; ok && v > 0 {
				lw = v
			}
		}
		targetRatio := float64(lw) / totalW
		actualRatio := float64(0)
		if totalC > 0 {
			actualRatio = float64(counts[leaf.Slug]) / totalC
		}
		items = append(items, leafDeficit{leaf: leaf, deficit: targetRatio - actualRatio})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].deficit != items[j].deficit {
			return items[i].deficit > items[j].deficit
		}
		return items[i].leaf.Slug < items[j].leaf.Slug
	})

	result := make([]CachedLeafInfo, len(items))
	for i, item := range items {
		result[i] = item.leaf
	}
	return result
}

// RecordAssignment increments the cumulative count for a head and leaf.
func (w *WeightedSelector) RecordAssignment(serverName, leafSlug string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.headCounts[serverName]++
	if w.leafCounts[serverName] == nil {
		w.leafCounts[serverName] = make(map[string]int)
	}
	w.leafCounts[serverName][leafSlug]++
}

// Reset zeroes all cumulative counts.
func (w *WeightedSelector) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.headCounts = make(map[string]int)
	w.leafCounts = make(map[string]map[string]int)
}
