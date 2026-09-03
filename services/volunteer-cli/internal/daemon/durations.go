package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DurationTracker learns how long one work unit of each leaf takes on THIS
// machine, from the units it has actually completed here (TB-58).
//
// The per-unit estimate feeds everything that books time: the hours-based work
// buffer (how many units to hold and how many to ask for) and the static half
// of the remaining-time figure the status API and the desktop app show. Before
// this tracker the estimate was rsc_fpops_est ÷ a three-second single-threaded
// CPU benchmark, corrected by a per-leaf factor that ramped up 80/20 when a
// unit over-ran but decayed only 10 % per completion when units were faster.
// One CPU core is the wrong yardstick for a GPU leaf or a container that fans
// out over every allowed core, so such leaves carried estimates hours too high
// and the factor crawled toward the truth over dozens of completions — the
// buffer latched full at two or three units, and the app showed "80 % done,
// 3 h left".
//
// This tracker keeps the last durationSampleWindow completions per leaf — each
// the unit's FP-ops estimate and the seconds it actually computed (suspended
// time excluded, TB-18) — and answers with MEDIANS: the median seconds per
// FP-op scales each unit by its own size, the median unit seconds sizes a
// leaf's first request. A median converges on the very first completion and
// is not moved by one slow or fast outlier once three samples exist. The
// benchmark formula survives only as the fallback before a leaf's first
// completion here.
type DurationTracker struct {
	mu      sync.RWMutex
	samples map[string][]durationSample // leaf ID -> oldest first, at most durationSampleWindow
	dataDir string
}

// durationSample is one completed unit of a leaf on this machine.
type durationSample struct {
	RscFpopsEst   float64 `json:"rsc_fpops_est"`
	ActiveSeconds float64 `json:"active_seconds"`
}

const durationsFile = "durations.json"

// durationSampleWindow is how many recent completions per leaf the medians are
// taken over. Five is enough that a single outlier cannot move the median and
// small enough that a real change in the machine's speed (a cores setting,
// TB-47) shows within three completions.
const durationSampleWindow = 5

// legacyDCFFile is the per-leaf correction-factor file this tracker replaces. A
// leftover one is removed on load so a triage read of the data directory does
// not find a figure nothing consults.
const legacyDCFFile = "dcf.json"

// LoadDurationTracker loads persisted samples or creates an empty tracker.
func LoadDurationTracker(dataDir string) *DurationTracker {
	t := &DurationTracker{
		samples: make(map[string][]durationSample),
		dataDir: dataDir,
	}
	_ = os.Remove(filepath.Join(dataDir, legacyDCFFile))
	data, err := os.ReadFile(filepath.Join(dataDir, durationsFile))
	if err != nil {
		return t
	}
	if err := json.Unmarshal(data, &t.samples); err != nil || t.samples == nil {
		t.samples = make(map[string][]durationSample)
	}
	return t
}

// Record adds one completed unit of the leaf. activeSeconds is the time it
// spent computing (wall clock less suspended time); a non-positive value is
// ignored. rscFpopsEst may be zero — the sample still counts toward the leaf's
// unit seconds, just not toward its seconds per FP-op.
func (t *DurationTracker) Record(leafID string, rscFpopsEst, activeSeconds float64) {
	if leafID == "" || activeSeconds <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := append(t.samples[leafID], durationSample{RscFpopsEst: rscFpopsEst, ActiveSeconds: activeSeconds})
	if len(s) > durationSampleWindow {
		s = s[len(s)-durationSampleWindow:]
	}
	t.samples[leafID] = s
	t.save()
}

// SecondsPerFpop is the median seconds this machine needs per estimated FP-op
// of the leaf, over the recorded completions that carried an FP-ops estimate.
// ok is false until one such completion exists.
func (t *DurationTracker) SecondsPerFpop(leafID string) (rate float64, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var rates []float64
	for _, s := range t.samples[leafID] {
		if s.RscFpopsEst > 0 {
			rates = append(rates, s.ActiveSeconds/s.RscFpopsEst)
		}
	}
	return median(rates)
}

// UnitSeconds is the median seconds one unit of the leaf took on this machine.
// ok is false until the leaf has completed here.
func (t *DurationTracker) UnitSeconds(leafID string) (sec float64, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var secs []float64
	for _, s := range t.samples[leafID] {
		secs = append(secs, s.ActiveSeconds)
	}
	return median(secs)
}

// Completions is how many of the leaf's completions the tracker holds (at most
// durationSampleWindow).
func (t *DurationTracker) Completions(leafID string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.samples[leafID])
}

// median returns the middle value of v (the mean of the two middle values for
// an even count) and false for an empty slice.
func median(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], true
	}
	return (s[n/2-1] + s[n/2]) / 2, true
}

func (t *DurationTracker) save() {
	data, err := json.MarshalIndent(t.samples, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(t.dataDir, durationsFile), data, 0600)
}
