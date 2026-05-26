package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// ResultEntry is an index record for a persisted result JSON file.
type ResultEntry struct {
	WorkUnitID   string    `json:"work_unit_id"`
	LeafName     string    `json:"leaf_name"`
	LeafSlug     string    `json:"leaf_slug"`
	HeadName     string    `json:"head_name"`
	CompletedAt  time.Time `json:"completed_at"`
	ResultPath   string    `json:"result_path"`
	VizBundlePath string  `json:"viz_bundle_path"`
	SizeBytes    int64     `json:"size_bytes"`
}

// ResultsDir returns the directory for persisted result files.
func ResultsDir(dataDir string) string {
	return filepath.Join(dataDir, "results")
}

// ResultIndexPath returns the path to the results index JSONL file.
func ResultIndexPath(dataDir string) string {
	return filepath.Join(ResultsDir(dataDir), "index.jsonl")
}

// SaveResult persists result output JSON and appends an index entry.
// If a result with the same work unit ID already exists in the index,
// the old entry is replaced (no duplicates). It also runs LRU eviction
// if the total stored size exceeds maxBytes.
func SaveResult(dataDir string, workUnitID, leafName, leafSlug, headName string, outputData []byte, vizBundlePath string, maxBytes int64) error {
	// SECURITY (H2): defense-in-depth — workUnitID becomes the result file name
	// below. Reject non-UUID IDs so a malicious head can't write outside the
	// results dir via path traversal.
	if err := runtime.ValidateWorkUnitID(workUnitID); err != nil {
		return err
	}

	dir := ResultsDir(dataDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating results directory: %w", err)
	}

	// Write result JSON file.
	resultPath := filepath.Join(dir, workUnitID+".json")
	if err := os.WriteFile(resultPath, outputData, 0644); err != nil {
		return fmt.Errorf("writing result file: %w", err)
	}

	entry := ResultEntry{
		WorkUnitID:    workUnitID,
		LeafName:      leafName,
		LeafSlug:      leafSlug,
		HeadName:      headName,
		CompletedAt:   time.Now().UTC(),
		ResultPath:    resultPath,
		VizBundlePath: vizBundlePath,
		SizeBytes:     int64(len(outputData)),
	}

	// Remove any existing index entry for this work unit ID before appending.
	if err := removeDuplicateIndexEntry(dataDir, workUnitID); err != nil {
		// Non-fatal: if we can't deduplicate, still save the new entry.
		// This only fails if the index file can't be read/written.
	}

	// Append to index.
	if err := appendResultIndex(dataDir, entry); err != nil {
		// Clean up the result file if index append fails.
		os.Remove(resultPath)
		return fmt.Errorf("appending result index: %w", err)
	}

	// Run eviction if needed. Failure is non-fatal — the result is already saved.
	if maxBytes > 0 {
		_ = evictResults(dataDir, maxBytes)
	}

	return nil
}

// removeDuplicateIndexEntry removes any existing entry for a work unit ID from the index.
// If no entry exists, this is a no-op.
func removeDuplicateIndexEntry(dataDir string, workUnitID string) error {
	entries, err := ListResults(dataDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	// Check if the work unit ID exists in the index.
	found := false
	for _, e := range entries {
		if e.WorkUnitID == workUnitID {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	// Filter out the duplicate entry and rewrite.
	var filtered []ResultEntry
	for _, e := range entries {
		if e.WorkUnitID != workUnitID {
			filtered = append(filtered, e)
		}
	}
	return rewriteResultIndex(dataDir, filtered)
}

// appendResultIndex appends a single entry to the results index JSONL file.
func appendResultIndex(dataDir string, entry ResultEntry) error {
	path := ResultIndexPath(dataDir)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening result index: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling result entry: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing result index entry: %w", err)
	}
	return nil
}

// ListResults reads all entries from the results index.
func ListResults(dataDir string) ([]ResultEntry, error) {
	path := ResultIndexPath(dataDir)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening result index: %w", err)
	}
	defer f.Close()

	var entries []ResultEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e ResultEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip malformed lines
		}
		entries = append(entries, e)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading result index: %w", err)
	}
	return entries, nil
}

// GetResultData reads the raw result JSON for a given work unit ID.
func GetResultData(dataDir string, workUnitID string) ([]byte, error) {
	// SECURITY (H2): reject path traversal attempts. Centralized on the shared
	// validator so the rejection rule is identical to SaveResult and the runtimes.
	if err := runtime.ValidateWorkUnitID(workUnitID); err != nil {
		return nil, err
	}

	resultPath := filepath.Join(ResultsDir(dataDir), workUnitID+".json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("result not found: %s", workUnitID)
		}
		return nil, fmt.Errorf("reading result file: %w", err)
	}
	return data, nil
}

// evictResults removes the oldest results (by CompletedAt) until total size
// is under maxBytes. Rewrites the index file after eviction.
func evictResults(dataDir string, maxBytes int64) error {
	entries, err := ListResults(dataDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	var totalSize int64
	for _, e := range entries {
		totalSize += e.SizeBytes
	}

	if totalSize <= maxBytes {
		return nil
	}

	// Sort oldest first for eviction.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CompletedAt.Before(entries[j].CompletedAt)
	})

	var remaining []ResultEntry
	for _, e := range entries {
		if totalSize > maxBytes {
			// Evict this entry.
			os.Remove(e.ResultPath)
			totalSize -= e.SizeBytes
		} else {
			remaining = append(remaining, e)
		}
	}

	// Rewrite the index file.
	return rewriteResultIndex(dataDir, remaining)
}

// rewriteResultIndex overwrites the index file with the given entries.
func rewriteResultIndex(dataDir string, entries []ResultEntry) error {
	path := ResultIndexPath(dataDir)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating result index: %w", err)
	}
	defer f.Close()

	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("writing result index: %w", err)
		}
	}
	return nil
}
