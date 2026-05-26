package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// DCFTracker maintains per-leaf duration correction factors.
// DCF corrects for systematic error in runtime estimates.
// Starts at 1.0. Ramps up aggressively (tasks took longer), down slowly (tasks were faster).
type DCFTracker struct {
	mu      sync.RWMutex
	factors map[string]float64 // leaf ID -> DCF
	dataDir string
}

const dcfFile = "dcf.json"

// LoadDCFTracker loads persisted DCF values or creates a new empty tracker.
func LoadDCFTracker(dataDir string) *DCFTracker {
	t := &DCFTracker{
		factors: make(map[string]float64),
		dataDir: dataDir,
	}
	data, err := os.ReadFile(filepath.Join(dataDir, dcfFile))
	if err != nil {
		return t
	}
	_ = json.Unmarshal(data, &t.factors)
	return t
}

// Get returns the DCF for a leaf. Returns 1.0 if unknown.
func (t *DCFTracker) Get(leafID string) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if dcf, ok := t.factors[leafID]; ok {
		return dcf
	}
	return 1.0
}

// Update adjusts the DCF for a leaf after a task completes.
// estimatedSec is the benchmark-based estimate; actualSec is the real wall-clock time.
// Ramp up aggressively, ramp down slowly.
func (t *DCFTracker) Update(leafID string, estimatedSec, actualSec float64) {
	if estimatedSec <= 0 || actualSec <= 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	oldDCF := t.factors[leafID]
	if oldDCF <= 0 {
		oldDCF = 1.0
	}

	ratio := actualSec / estimatedSec

	var newDCF float64
	if ratio > oldDCF {
		// Tasks taking longer than expected: ramp up aggressively.
		// Weight new observation heavily (80/20).
		newDCF = 0.2*oldDCF + 0.8*ratio
	} else {
		// Tasks finishing faster than expected: ramp down slowly.
		// Weight old value heavily (90/10).
		newDCF = 0.9*oldDCF + 0.1*ratio
	}

	// Clamp to reasonable range.
	if newDCF < 0.01 {
		newDCF = 0.01
	}
	if newDCF > 100 {
		newDCF = 100
	}

	t.factors[leafID] = newDCF
	t.save()
}

func (t *DCFTracker) save() {
	data, err := json.MarshalIndent(t.factors, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(t.dataDir, dcfFile), data, 0600)
}
