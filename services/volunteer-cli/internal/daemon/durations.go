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

// durationSample is one completed unit of a leaf on this machine: its FP-ops
// estimate, the seconds it computed, the cores it was given (0 when not
// recorded) and its deadline in seconds (0 when it had none, or it was not
// recorded).
type durationSample struct {
	RscFpopsEst     float64 `json:"rsc_fpops_est"`
	ActiveSeconds   float64 `json:"active_seconds"`
	CPUCores        int     `json:"cpu_cores,omitempty"`
	DeadlineSeconds int32   `json:"deadline_seconds,omitempty"`
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
	t.RecordRun(leafID, rscFpopsEst, activeSeconds, 0, 0)
}

// RecordRun is Record with the cores the unit was given and its deadline in
// seconds, which let the deadline check scale the leaf's run time to the
// cores a task would be given and know the leaf's deadline before any of its
// units is held (deadline_skip.go). Zero for either means not known.
func (t *DurationTracker) RecordRun(leafID string, rscFpopsEst, activeSeconds float64, cpuCores int, deadlineSeconds int32) {
	if leafID == "" || activeSeconds <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := append(t.samples[leafID], durationSample{RscFpopsEst: rscFpopsEst, ActiveSeconds: activeSeconds,
		CPUCores: cpuCores, DeadlineSeconds: deadlineSeconds})
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

// SecondsAt is the leaf's median unit seconds for a task given cores: each
// recorded run's seconds scaled by the cores it ran on over cores — a unit
// that took 5 h on 2 cores counts as 2.5 h on 4 — so a leaf that uses the
// cores it declares is estimated at the cores a task would now be given. A
// run that did not record its cores counts as run on cores. ok is false until
// the leaf has completed here.
func (t *DurationTracker) SecondsAt(leafID string, cores int) (sec float64, ok bool) {
	if cores < 1 {
		cores = 1
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	var secs []float64
	for _, s := range t.samples[leafID] {
		ran := s.CPUCores
		if ran <= 0 {
			ran = cores
		}
		secs = append(secs, s.ActiveSeconds*float64(ran)/float64(cores))
	}
	return median(secs)
}

// RunCores is the median cores the leaf's recorded runs were given; 0 when
// none recorded them.
func (t *DurationTracker) RunCores(leafID string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var cores []float64
	for _, s := range t.samples[leafID] {
		if s.CPUCores > 0 {
			cores = append(cores, float64(s.CPUCores))
		}
	}
	m, _ := median(cores)
	return int(m + 0.5)
}

// Deadline is the deadline, in seconds, of the leaf's most recent recorded
// run that carried one; false when none did.
func (t *DurationTracker) Deadline(leafID string) (int32, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.samples[leafID]
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].DeadlineSeconds > 0 {
			return s[i].DeadlineSeconds, true
		}
	}
	return 0, false
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
