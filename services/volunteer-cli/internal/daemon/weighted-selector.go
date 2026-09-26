package daemon

import (
	"math"
	"sort"
	"sync"
	"time"
)

// weightBalanceHalfLife is how quickly booked compute time fades from the
// balance the weights steer: an hour of work booked this long ago counts half
// as much as an hour booked now. The weights are therefore kept over roughly
// the last day, which spans many units of even a slow leaf, while a head or
// leaf that had no work for a while catches up by hours rather than by days,
// and nothing counted outlives the conditions it described.
const weightBalanceHalfLife = 12 * time.Hour

// weightBalanceWindow bounds what can still matter to the balance. Time
// booked longer ago than this counts for less than a sixteenth of its
// seconds, so history older than it is not read at start and a booking older
// than it is no longer kept for correction.
const weightBalanceWindow = 4 * weightBalanceHalfLife

// unknownUnitSeconds is what a unit is booked at when nothing estimates its
// length yet: no completion of its leaf here, no FP-ops figure and no
// leaf-level estimate from the head. When every unit is unknown this books
// them alike, which balances by count; the unit's completion replaces the
// figure with the time it actually took.
const unknownUnitSeconds = 3600.0

// tieEpsilon is how close two deficits must be to count as a tie.
const tieEpsilon = 1e-9

// WeightedSelector implements deficit-based weighted selection for both head
// (server) and leaf levels. The deficit compares each head's and leaf's
// share of this machine's recent compute time, as booked here, with its
// share of the weights: a head at weight 200 is due about twice the hours of
// one at 100, whatever its units' lengths.
//
// Every buffered unit is booked at its estimated seconds and corrected to the
// active seconds it took when it completes. The booked time fades with
// weightBalanceHalfLife, and at start it is seeded from the recent
// history.jsonl, so a restart continues the balance instead of starting a new
// one. Nothing is persisted.
type WeightedSelector struct {
	mu          sync.Mutex
	now         func() time.Time
	headWeights map[string]int                         // server name -> weight
	leafWeights map[string]map[string]int              // server name -> slug -> weight
	headTime    map[string]*decayingSeconds            // server name -> booked seconds
	leafTime    map[string]map[string]*decayingSeconds // server name -> leaf id -> booked seconds
	bookings    map[string]unitBooking                 // work unit id -> its booking, until it completes
	lastAsked   map[string]uint64                      // head, or head and leaf, -> askSeq when last asked
	askSeq      uint64                                 // order of requests (the clock can repeat an instant)
}

// unitBooking is what one buffered unit was booked at, kept so its completion
// can replace the estimate with the time it actually took.
type unitBooking struct {
	server, leaf string
	seconds      float64
	at           time.Time
}

// decayingSeconds is a sum of booked seconds in which each addition fades
// with weightBalanceHalfLife from the moment it is dated.
type decayingSeconds struct {
	sum float64
	at  time.Time // when sum was last brought up to date
}

// decayFactor is how much of a booking of the given age still counts.
func decayFactor(age time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	return math.Exp2(-float64(age) / float64(weightBalanceHalfLife))
}

func (s *decayingSeconds) value(now time.Time) float64 {
	if s == nil || s.at.IsZero() {
		return 0
	}
	return s.sum * decayFactor(now.Sub(s.at))
}

// add books seconds dated when, as seen at now. A negative figure corrects
// an earlier booking; the sum never goes below zero.
func (s *decayingSeconds) add(now, when time.Time, seconds float64) {
	s.sum = math.Max(0, s.value(now)+seconds*decayFactor(now.Sub(when)))
	s.at = now
}

// NewWeightedSelector creates a new selector with empty state.
func NewWeightedSelector() *WeightedSelector {
	return &WeightedSelector{
		now:         time.Now,
		headWeights: make(map[string]int),
		leafWeights: make(map[string]map[string]int),
		headTime:    make(map[string]*decayingSeconds),
		leafTime:    make(map[string]map[string]*decayingSeconds),
		bookings:    make(map[string]unitBooking),
		lastAsked:   make(map[string]uint64),
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

// leafKey is what a leaf's booked time is kept under: its id, which
// history.jsonl records too, or its slug when a leaf has no id.
func leafKey(leaf CachedLeafInfo) string {
	if leaf.ID != "" {
		return leaf.ID
	}
	return leaf.Slug
}

func (w *WeightedSelector) headWeightLocked(serverName string) float64 {
	if v := w.headWeights[serverName]; v > 0 {
		return float64(v)
	}
	return 100
}

func (w *WeightedSelector) leafWeightLocked(serverName, slug string) float64 {
	if v, ok := w.leafWeights[serverName][slug]; ok && v > 0 {
		return float64(v)
	}
	return 100
}

// aheadLocked orders two candidates for the next request: the larger deficit
// first; on a tie, the one asked least recently (never asked comes first), so
// a tie does not always go to the same name; then by name.
func (w *WeightedSelector) aheadLocked(deficitA float64, keyA, nameA string, deficitB float64, keyB, nameB string) bool {
	if math.Abs(deficitA-deficitB) > tieEpsilon {
		return deficitA > deficitB
	}
	if askedA, askedB := w.lastAsked[keyA], w.lastAsked[keyB]; askedA != askedB {
		return askedA < askedB
	}
	return nameA < nameB
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

	// Shares of weight and of booked time among the available servers only.
	now := w.now()
	booked := make([]float64, len(available))
	var totalW, totalT float64
	for i, srv := range available {
		totalW += w.headWeightLocked(srv.Name)
		booked[i] = w.headTime[srv.Name].value(now)
		totalT += booked[i]
	}

	best := -1
	var bestDeficit float64
	for i, srv := range available {
		deficit := w.headWeightLocked(srv.Name) / totalW
		if totalT > 0 {
			deficit -= booked[i] / totalT
		}
		if best < 0 || w.aheadLocked(deficit, srv.Name, srv.Name, bestDeficit, available[best].Name, available[best].Name) {
			best, bestDeficit = i, deficit
		}
	}
	return available[best]
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

	now := w.now()
	times := w.leafTime[serverName]
	type leafDeficit struct {
		leaf    CachedLeafInfo
		key     string
		deficit float64
	}
	items := make([]leafDeficit, len(enabledLeafs))
	booked := make([]float64, len(enabledLeafs))
	var totalW, totalT float64
	for i, leaf := range enabledLeafs {
		totalW += w.leafWeightLocked(serverName, leaf.Slug)
		booked[i] = times[leafKey(leaf)].value(now)
		totalT += booked[i]
	}
	for i, leaf := range enabledLeafs {
		deficit := w.leafWeightLocked(serverName, leaf.Slug) / totalW
		if totalT > 0 {
			deficit -= booked[i] / totalT
		}
		items[i] = leafDeficit{leaf: leaf, key: serverName + "|" + leafKey(leaf), deficit: deficit}
	}

	sort.SliceStable(items, func(i, j int) bool {
		return w.aheadLocked(items[i].deficit, items[i].key, items[i].leaf.Slug, items[j].deficit, items[j].key, items[j].leaf.Slug)
	})

	result := make([]CachedLeafInfo, len(items))
	for i, item := range items {
		result[i] = item.leaf
	}
	return result
}

// HeadShare is the fraction of this machine's time the head's weight is due
// among the heads still in play: every available head the current round has
// not already found without work for this machine.
func (w *WeightedSelector) HeadShare(serverName string, inPlay []*ServerConnection) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	own := w.headWeightLocked(serverName)
	total := 0.0
	counted := false
	for _, srv := range inPlay {
		total += w.headWeightLocked(srv.Name)
		counted = counted || srv.Name == serverName
	}
	if !counted {
		total += own
	}
	return own / total
}

// LeafShare is the fraction of its head's time a leaf's weight is due among
// that head's leaves still in play: those this machine can request, less any
// the current round has already found without work.
func (w *WeightedSelector) LeafShare(serverName string, leaf CachedLeafInfo, inPlay []CachedLeafInfo) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	own := w.leafWeightLocked(serverName, leaf.Slug)
	total := 0.0
	counted := false
	for _, l := range inPlay {
		total += w.leafWeightLocked(serverName, l.Slug)
		counted = counted || l.Slug == leaf.Slug
	}
	if !counted {
		total += own
	}
	return own / total
}

// NoteAsked records that a head was asked for work for a leaf (an empty key
// for a request that named none), for the tie-break.
func (w *WeightedSelector) NoteAsked(serverName, leaf string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.askSeq++
	w.lastAsked[serverName] = w.askSeq
	w.lastAsked[serverName+"|"+leaf] = w.askSeq
}

// addLocked books seconds, dated when, to a head and one of its leaves.
func (w *WeightedSelector) addLocked(serverName, leaf string, now, when time.Time, seconds float64) {
	if w.headTime[serverName] == nil {
		w.headTime[serverName] = &decayingSeconds{}
	}
	w.headTime[serverName].add(now, when, seconds)
	if w.leafTime[serverName] == nil {
		w.leafTime[serverName] = make(map[string]*decayingSeconds)
	}
	if w.leafTime[serverName][leaf] == nil {
		w.leafTime[serverName][leaf] = &decayingSeconds{}
	}
	w.leafTime[serverName][leaf].add(now, when, seconds)
}

// RecordAssignment books one buffered unit to its head and leaf at its
// estimated seconds (unknownUnitSeconds when there is no estimate).
func (w *WeightedSelector) RecordAssignment(serverName, leaf, workUnitID string, seconds float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if seconds <= 0 {
		seconds = unknownUnitSeconds
	}
	now := w.now()
	w.addLocked(serverName, leaf, now, now, seconds)
	if workUnitID != "" {
		w.bookings[workUnitID] = unitBooking{server: serverName, leaf: leaf, seconds: seconds, at: now}
	}
	for id, b := range w.bookings {
		if now.Sub(b.at) > weightBalanceWindow {
			delete(w.bookings, id)
		}
	}
}

// RecordCompletion replaces a unit's booking with the active seconds it
// took, dated as the booking was. A unit booked before this daemon started
// (a result resent after a restart) is booked now, at those seconds.
func (w *WeightedSelector) RecordCompletion(serverName, leaf, workUnitID string, seconds float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if b, ok := w.bookings[workUnitID]; ok {
		delete(w.bookings, workUnitID)
		w.addLocked(b.server, b.leaf, now, b.at, seconds-b.seconds)
		return
	}
	if serverName == "" || leaf == "" || seconds <= 0 {
		return
	}
	w.addLocked(serverName, leaf, now, now, seconds)
}

// SeedFromHistory books the runs history.jsonl recorded within
// weightBalanceWindow, each at its active seconds (its wall-clock seconds
// when no active figure was recorded) and dated when it completed. Every run
// counts, whatever the head made of its result: each used the machine's
// time. Entries that name no head or leaf are skipped. It returns how many
// runs were booked.
func (w *WeightedSelector) SeedFromHistory(entries []HistoryEntry) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	seeded := 0
	for _, e := range entries {
		if e.ServerName == "" || e.LeafID == "" || now.Sub(e.CompletedAt) > weightBalanceWindow {
			continue
		}
		seconds := float64(e.CPUSeconds)
		if seconds <= 0 {
			seconds = float64(e.WallClockSeconds)
		}
		if seconds <= 0 {
			continue
		}
		w.addLocked(e.ServerName, e.LeafID, now, e.CompletedAt, seconds)
		seeded++
	}
	return seeded
}

// BookedSeconds is the head's and leaf's booked time as it counts now, after
// fading.
func (w *WeightedSelector) BookedSeconds(serverName, leaf string) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.leafTime[serverName][leaf].value(w.now())
}

// HeadBookedSeconds is the head's booked time as it counts now, after fading.
func (w *WeightedSelector) HeadBookedSeconds(serverName string) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.headTime[serverName].value(w.now())
}
