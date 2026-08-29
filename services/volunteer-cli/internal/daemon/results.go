package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// ResultEntry is an index record for a persisted result JSON file.
type ResultEntry struct {
	WorkUnitID  string    `json:"work_unit_id"`
	LeafName    string    `json:"leaf_name"`
	LeafSlug    string    `json:"leaf_slug"`
	HeadName    string    `json:"head_name"`
	CompletedAt time.Time `json:"completed_at"`
	ResultPath  string    `json:"result_path"`
	// VizBundlePath is the root (the directory holding index.html) of the
	// daemon's persistent extracted copy of the leaf's visualization bundle,
	// under VizBundlesDir — never a path inside a work directory, which is
	// deleted when the unit completes. Empty when the leaf has no bundle.
	VizBundlePath string `json:"viz_bundle_path"`
	// VizBundleKey identifies that bundle (see runtime.VizBundleKey). Several
	// results of the same leaf share one extracted copy; eviction removes the
	// copy only when no remaining entry carries its key.
	VizBundleKey string `json:"viz_bundle_key,omitempty"`
	SizeBytes    int64  `json:"size_bytes"`
}

// VizBundleSource identifies a leaf's visualization bundle as the execution
// spec describes it: the tarball URL (ExecutionSpec.Binaries["viz"]) and its
// expected lowercase hex SHA-256 (ExecutionSpec.BinaryChecksums["viz"], empty
// when the head did not publish one). A zero value means the leaf has no
// bundle.
type VizBundleSource struct {
	URL      string
	Checksum string
}

// ResultsDir returns the directory for persisted result files.
func ResultsDir(dataDir string) string {
	return filepath.Join(dataDir, "results")
}

// ResultIndexPath returns the path to the results index JSONL file.
func ResultIndexPath(dataDir string) string {
	return filepath.Join(ResultsDir(dataDir), "index.jsonl")
}

// VizBundlesDir returns the directory holding the persistent extracted
// visualization bundles, one subdirectory per bundle key:
// {dataDir}/results/viz/<key>/.
func VizBundlesDir(dataDir string) string {
	return filepath.Join(ResultsDir(dataDir), "viz")
}

// SaveResult persists result output JSON and appends an index entry, keeping
// a persistent extracted copy of the leaf's visualization bundle so the
// result can be replayed later. If a result with the same work unit ID
// already exists in the index, the old entry is replaced (no duplicates). It
// also runs LRU eviction if the total stored size — result files plus
// extracted bundles — exceeds maxBytes.
//
// The bundle the runtime extracted into the work directory is gone by the
// time a completed unit reaches this function (the slot removes the work
// directory on completion, and the start-up sweep removes any leftovers), so
// the persistent copy is re-extracted from the tarball EnsureVizBundle cached
// under {dataDir}/cache/viz/ — once per bundle key, shared by every result of
// that leaf. A result whose bundle cannot be persisted is not indexed: it
// could never be replayed, and the runtime never reads the bundle, so nothing
// else is lost.
func SaveResult(dataDir string, workUnitID, leafName, leafSlug, headName string, outputData []byte, viz VizBundleSource, maxBytes int64) error {
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

	// Persist the bundle first: if that fails there is nothing to index.
	var vizKey, vizPath string
	if viz.URL != "" {
		var err error
		vizKey, vizPath, err = persistVizBundle(dataDir, viz)
		if err != nil {
			return fmt.Errorf("persisting visualization bundle: %w", err)
		}
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
		VizBundlePath: vizPath,
		VizBundleKey:  vizKey,
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

// persistVizBundle ensures {dataDir}/results/viz/<key>/ holds an extracted
// copy of the bundle and returns the key and the bundle root path. An
// existing copy is reused as is; otherwise the bundle is extracted from the
// cached tarball into a staging directory and renamed into place, so a crash
// mid-extraction can never leave a torn copy under the final name.
func persistVizBundle(dataDir string, viz VizBundleSource) (key, root string, err error) {
	key = runtime.VizBundleKey(viz.URL, strings.ToLower(viz.Checksum))
	finalDir := filepath.Join(VizBundlesDir(dataDir), key)

	if _, statErr := os.Stat(finalDir); statErr == nil {
		root, err = vizBundleRoot(finalDir)
		if err == nil {
			return key, root, nil
		}
		// An existing copy with no index.html is unusable; replace it.
		if rmErr := os.RemoveAll(finalDir); rmErr != nil {
			return "", "", fmt.Errorf("removing unusable bundle copy: %w", rmErr)
		}
	}

	tarball := runtime.VizTarballPath(dataDir, key)
	if _, statErr := os.Stat(tarball); statErr != nil {
		return "", "", fmt.Errorf("cached bundle tarball %s: %w", tarball, statErr)
	}

	if mkErr := os.MkdirAll(VizBundlesDir(dataDir), 0700); mkErr != nil {
		return "", "", fmt.Errorf("creating bundle directory: %w", mkErr)
	}
	stageDir, tmpErr := os.MkdirTemp(VizBundlesDir(dataDir), ".stage-"+key+"-")
	if tmpErr != nil {
		return "", "", fmt.Errorf("creating staging directory: %w", tmpErr)
	}
	stagedRoot, exErr := runtime.ExtractVizBundleTo(tarball, stageDir)
	if exErr != nil {
		os.RemoveAll(stageDir)
		return "", "", exErr
	}
	rel, relErr := filepath.Rel(stageDir, stagedRoot)
	if relErr != nil {
		os.RemoveAll(stageDir)
		return "", "", fmt.Errorf("resolving bundle root: %w", relErr)
	}
	if renErr := os.Rename(stageDir, finalDir); renErr != nil {
		os.RemoveAll(stageDir)
		// A concurrent persist of the same key may have won the rename; use it.
		if r, rootErr := vizBundleRoot(finalDir); rootErr == nil {
			return key, r, nil
		}
		return "", "", fmt.Errorf("moving bundle into place: %w", renErr)
	}
	return key, filepath.Join(finalDir, rel), nil
}

// vizBundleRoot locates index.html in a persisted bundle copy: at the copy's
// root, or inside its single wrapper directory (the two layouts
// runtime.ExtractVizBundle accepts).
func vizBundleRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var subDirs []string
	for _, e := range entries {
		if e.IsDir() {
			subDirs = append(subDirs, e.Name())
		}
	}
	if len(subDirs) == 1 {
		nested := filepath.Join(dir, subDirs[0])
		if _, err := os.Stat(filepath.Join(nested, "index.html")); err == nil {
			return nested, nil
		}
	}
	return "", fmt.Errorf("bundle copy %s has no index.html", dir)
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

// evictResults removes the oldest results (by CompletedAt) until the total
// stored size — result files plus the persistent extracted bundles they
// reference, each bundle counted once — is under maxBytes. A bundle directory
// is removed only when no remaining entry references it; bundle directories
// no entry references at all (a crash between extraction and indexing) are
// removed too. The most recent entry is always kept, so a single result whose
// bundle alone exceeds maxBytes does not evict itself the moment it is saved.
// Rewrites the index file after eviction.
func evictResults(dataDir string, maxBytes int64) error {
	entries, err := ListResults(dataDir)
	if err != nil {
		return err
	}

	bundleDirs, _ := os.ReadDir(VizBundlesDir(dataDir))
	bundleSize := make(map[string]int64, len(bundleDirs))
	for _, d := range bundleDirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue // staging directories are in flight, not evictable
		}
		bundleSize[d.Name()] = dirSizeBytes(filepath.Join(VizBundlesDir(dataDir), d.Name()))
	}

	refs := make(map[string]int, len(bundleSize))
	var totalSize int64
	for _, e := range entries {
		totalSize += e.SizeBytes
		if e.VizBundleKey != "" {
			refs[e.VizBundleKey]++
		}
	}
	for key, size := range bundleSize {
		if refs[key] > 0 {
			totalSize += size
		}
	}

	// Sort oldest first for eviction.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CompletedAt.Before(entries[j].CompletedAt)
	})

	evicted := false
	var remaining []ResultEntry
	for i, e := range entries {
		if totalSize > maxBytes && i < len(entries)-1 {
			// Evict this entry.
			os.Remove(e.ResultPath)
			totalSize -= e.SizeBytes
			evicted = true
			if e.VizBundleKey != "" {
				refs[e.VizBundleKey]--
				if refs[e.VizBundleKey] == 0 {
					os.RemoveAll(filepath.Join(VizBundlesDir(dataDir), e.VizBundleKey))
					totalSize -= bundleSize[e.VizBundleKey]
					delete(bundleSize, e.VizBundleKey)
				}
			}
		} else {
			remaining = append(remaining, e)
		}
	}

	// Bundle copies nothing references: not counted above, never replayable.
	for key := range bundleSize {
		if refs[key] == 0 {
			os.RemoveAll(filepath.Join(VizBundlesDir(dataDir), key))
		}
	}

	if !evicted {
		return nil
	}
	// Rewrite the index file.
	return rewriteResultIndex(dataDir, remaining)
}

// dirSizeBytes sums the sizes of the regular files under root; unreadable
// entries are skipped rather than failing the measurement.
func dirSizeBytes(root string) int64 {
	var bytes int64
	filepath.WalkDir(root, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return fs.SkipDir
		}
		if de.Type().IsRegular() {
			if info, ierr := de.Info(); ierr == nil {
				bytes += info.Size()
			}
		}
		return nil
	})
	return bytes
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
