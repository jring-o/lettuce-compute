package daemon

import (
	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// The volunteer's CPU override for a leaf on this machine.
//
// A leaf declares the cores its units can use (min_cpu_cores–max_cpu_cores)
// and each task is granted as many of those as are free when it starts
// (grantCPU). A volunteer may fix the figure instead — leaf_preferences.cores,
// kept inside the declared range — and cap how many of the leaf's tasks run
// at once — leaf_preferences.max_running. Both are keyed by the leaf's slug
// on the head that serves it, like the weights, and are read live, so a
// change applies to the next task that starts and the next buffer decision.
//
// The override counts cores, not tasks: a leaf's tasks are booked at the
// cores they are given, so a cap on a wide leaf cannot overload the machine.
// And the work buffer never holds more of a leaf than the override lets run
// (leafSlotsFor, the per-leaf buffer class): a cap that the fetcher did not
// plan for is how a running-task limit makes a client fetch far more work
// than it can finish.

// leafCPUOverride is what the volunteer set for one leaf.
type leafCPUOverride struct {
	cores      int // the cores each task is given; 0 = the leaf's own range
	maxRunning int // the most of its tasks that run at once; 0 = no cap of its own
}

// leafOverride finds the volunteer's override for a leaf: the head that
// serves it (the leaf cache, or a head that pins the leaf by id), that head's
// leaf preferences, and their entries under the leaf's slug or, for a leaf
// the catalog does not list, its id. The zero value when there is none.
func (d *Daemon) leafOverride(leafID string) leafCPUOverride {
	cfg := d.cfg
	if cfg == nil || leafID == "" {
		return leafCPUOverride{}
	}
	var head, slug string
	if d.leafCache != nil {
		if name, l, ok := d.leafCache.HeadOfLeaf(leafID); ok {
			head, slug = name, l.Slug
		}
	}
	for _, srv := range cfg.Servers {
		if head != "" && srv.DisplayName() != head {
			continue
		}
		if head == "" && !contains(srv.PinnedLeafIDs, leafID) {
			continue
		}
		return overrideFrom(srv.LeafPreferences, slug, leafID)
	}
	return leafCPUOverride{}
}

// overrideFrom reads a leaf's entries from one head's preferences, by slug
// and then by id.
func overrideFrom(lp config.LeafPreferences, slug, id string) leafCPUOverride {
	lookup := func(m map[string]int) int {
		if slug != "" {
			if n, ok := m[slug]; ok && n > 0 {
				return n
			}
		}
		if n, ok := m[id]; ok && n > 0 {
			return n
		}
		return 0
	}
	return leafCPUOverride{cores: lookup(lp.Cores), maxRunning: lookup(lp.MaxRunning)}
}

// leafMaxRunning is the volunteer's cap on how many of the leaf's tasks run
// at once; 0 when there is none.
func (d *Daemon) leafMaxRunning(leafID string) int {
	return d.leafOverride(leafID).maxRunning
}

// applyCoresOverride narrows a leaf's declared range to the volunteer's
// cores figure, kept inside the range; the range itself when none is set.
func applyCoresOverride(minCores, maxCores, cores int) (int, int) {
	if cores <= 0 {
		return minCores, maxCores
	}
	if cores < minCores {
		cores = minCores
	}
	if cores > maxCores {
		cores = maxCores
	}
	return cores, cores
}

// contains reports whether list holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// LeafCPUStatus is this machine's CPU arrangement for one leaf: the
// volunteer's override for it (0 = none set) and what it comes to.
type LeafCPUStatus struct {
	// CoresOverride and MaxRunningOverride are what the volunteer set for the
	// leaf (leaf_preferences.cores and .max_running); 0 when unset.
	CoresOverride      int
	MaxRunningOverride int
	// TaskCoresMin and TaskCoresMax are the range a task of the leaf is given
	// cores from here: its declared range, narrowed by the override and kept
	// within the CPU budget of its runtime.
	TaskCoresMin, TaskCoresMax int
	// RunsAtOnce is the most of the leaf's tasks that run together here
	// (leafSlotsFor) — also how many tasks' worth of it the work buffer holds.
	RunsAtOnce int
}

// LeafCPUStatus reports the leaf's CPU arrangement on this machine.
func (d *Daemon) LeafCPUStatus(leaf CachedLeafInfo) LeafCPUStatus {
	o := d.leafOverride(leaf.ID)
	minCores, maxCores := d.unitCoreRange(leafShapeUnit(leaf))
	return LeafCPUStatus{
		CoresOverride:      o.cores,
		MaxRunningOverride: o.maxRunning,
		TaskCoresMin:       minCores,
		TaskCoresMax:       maxCores,
		RunsAtOnce:         d.leafSlotsFor(leaf),
	}
}
