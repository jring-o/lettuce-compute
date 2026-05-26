package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PendingResult is a completed work unit result whose submission to the head
// failed (e.g. a network blip at completion) and is awaiting retry. The full
// SubmitResultRequest is stored as marshaled proto so it can be replayed
// verbatim — the request carries only identity (public key + IDs) and the
// payload, with no timestamp/nonce, so a delayed resubmit is valid. Without this,
// a leaf with no checkpointing loses the entire run on a blip.
type PendingResult struct {
	WorkUnitID       string    `json:"work_unit_id"`
	LeafID           string    `json:"leaf_id"`
	ServerName       string    `json:"server_name"`
	RequestProto     []byte    `json:"request_proto"` // marshaled lettucev1.SubmitResultRequest
	WallClockSeconds int64     `json:"wall_clock_seconds"`
	CPUSeconds       int64     `json:"cpu_seconds"`
	CreatedAt        time.Time `json:"created_at"`
}

// PendingResultsDir returns the directory holding pending (un-submitted) results.
func PendingResultsDir(dataDir string) string {
	return filepath.Join(dataDir, "pending-results")
}

// safeWorkUnitID rejects work unit IDs that could escape the pending dir.
func safeWorkUnitID(workUnitID string) bool {
	return workUnitID != "" &&
		!strings.ContainsAny(workUnitID, `/\`) &&
		!strings.Contains(workUnitID, "..")
}

// SavePendingResult writes a pending result to disk (one JSON file per work unit,
// overwriting any existing entry for the same work unit).
func SavePendingResult(dataDir string, pr PendingResult) error {
	if !safeWorkUnitID(pr.WorkUnitID) {
		return fmt.Errorf("invalid work unit ID: %q", pr.WorkUnitID)
	}
	dir := PendingResultsDir(dataDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating pending-results directory: %w", err)
	}
	data, err := json.Marshal(pr)
	if err != nil {
		return fmt.Errorf("marshaling pending result: %w", err)
	}
	// Atomic write: temp file then rename, so a crash can't leave a torn file.
	path := filepath.Join(dir, pr.WorkUnitID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing pending result: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming pending result: %w", err)
	}
	return nil
}

// ListPendingResults reads all pending results, oldest first. Malformed files
// are skipped rather than failing the whole scan.
func ListPendingResults(dataDir string) ([]PendingResult, error) {
	dir := PendingResultsDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading pending-results directory: %w", err)
	}

	var results []PendingResult
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var pr PendingResult
		if err := json.Unmarshal(data, &pr); err != nil {
			continue
		}
		results = append(results, pr)
	}

	sortPendingByCreatedAt(results)
	return results, nil
}

// DeletePendingResult removes a pending result file (no-op if absent).
func DeletePendingResult(dataDir, workUnitID string) error {
	if !safeWorkUnitID(workUnitID) {
		return fmt.Errorf("invalid work unit ID: %q", workUnitID)
	}
	err := os.Remove(filepath.Join(PendingResultsDir(dataDir), workUnitID+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// sortPendingByCreatedAt orders results oldest-first (stable insertion sort —
// the set is tiny, at most a handful of un-submitted results).
func sortPendingByCreatedAt(results []PendingResult) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].CreatedAt.Before(results[j-1].CreatedAt); j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}
