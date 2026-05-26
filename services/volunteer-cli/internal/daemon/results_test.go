package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Canonical-UUID fixtures. SaveResult/GetResultData now require the work unit ID
// to be a strict UUID (H2 path-traversal fix), so test fixtures use real UUIDs.
const (
	uuidWU001   = "00000000-0000-4000-8000-000000000001"
	uuidWU002   = "00000000-0000-4000-8000-000000000002"
	uuidWU003   = "00000000-0000-4000-8000-000000000003"
	uuidFetch   = "11111111-1111-4111-8111-111111111111"
	uuidOld     = "aaaaaaaa-0000-4000-8000-000000000001"
	uuidMid     = "aaaaaaaa-0000-4000-8000-000000000002"
	uuidNew     = "aaaaaaaa-0000-4000-8000-000000000003"
	uuidOnly    = "bbbbbbbb-0000-4000-8000-000000000001"
	uuidFirst   = "cccccccc-0000-4000-8000-000000000001"
	uuidSecond  = "cccccccc-0000-4000-8000-000000000002"
	uuidDup     = "dddddddd-0000-4000-8000-000000000001"
	uuidLegit   = "eeeeeeee-0000-4000-8000-000000000001"
	uuidNoViz   = "ffffffff-0000-4000-8000-000000000001"
)

func TestSaveResult_WritesFileAndIndex(t *testing.T) {
	dataDir := t.TempDir()

	outputData := []byte(`{"answer": 42}`)
	err := SaveResult(dataDir, uuidWU001, "Prime Gaps", "prime-gaps", "example.com", outputData, "/viz/bundle.html", 0)
	if err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}

	// Check result file exists.
	resultPath := filepath.Join(ResultsDir(dataDir), uuidWU001+".json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result file not written: %v", err)
	}
	if string(data) != string(outputData) {
		t.Errorf("result file content = %q, want %q", string(data), string(outputData))
	}

	// Check index file has one entry.
	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.WorkUnitID != uuidWU001 {
		t.Errorf("WorkUnitID = %q, want %s", e.WorkUnitID, uuidWU001)
	}
	if e.LeafName != "Prime Gaps" {
		t.Errorf("LeafName = %q, want Prime Gaps", e.LeafName)
	}
	if e.LeafSlug != "prime-gaps" {
		t.Errorf("LeafSlug = %q, want prime-gaps", e.LeafSlug)
	}
	if e.HeadName != "example.com" {
		t.Errorf("HeadName = %q, want example.com", e.HeadName)
	}
	if e.VizBundlePath != "/viz/bundle.html" {
		t.Errorf("VizBundlePath = %q, want /viz/bundle.html", e.VizBundlePath)
	}
	if e.SizeBytes != int64(len(outputData)) {
		t.Errorf("SizeBytes = %d, want %d", e.SizeBytes, len(outputData))
	}
	if e.CompletedAt.IsZero() {
		t.Error("CompletedAt should not be zero")
	}
}

func TestSaveResult_MultipleEntries(t *testing.T) {
	dataDir := t.TempDir()

	for i, id := range []string{uuidWU001, uuidWU002, uuidWU003} {
		data := []byte(`{"iteration": ` + string(rune('0'+i)) + `}`)
		err := SaveResult(dataDir, id, "Leaf "+id, "leaf-"+id, "head", data, "", 0)
		if err != nil {
			t.Fatalf("SaveResult(%s) error: %v", id, err)
		}
	}

	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Verify each entry has the correct work unit ID.
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.WorkUnitID] = true
	}
	for _, id := range []string{uuidWU001, uuidWU002, uuidWU003} {
		if !ids[id] {
			t.Errorf("missing entry for %s", id)
		}
	}
}

func TestListResults_EmptyDirectory(t *testing.T) {
	dataDir := t.TempDir()

	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil for missing index, got %v", entries)
	}
}

func TestListResults_MissingIndexFile(t *testing.T) {
	dataDir := t.TempDir()
	// Create results dir but no index file.
	os.MkdirAll(ResultsDir(dataDir), 0700)

	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil for missing index file, got %v", entries)
	}
}

func TestListResults_SkipsMalformedLines(t *testing.T) {
	dataDir := t.TempDir()
	dir := ResultsDir(dataDir)
	os.MkdirAll(dir, 0700)

	// Write an index with one good line, one bad line, and one empty line.
	goodEntry := ResultEntry{
		WorkUnitID:  "wu-good",
		LeafName:    "Good Leaf",
		LeafSlug:    "good-leaf",
		HeadName:    "head",
		CompletedAt: time.Now().UTC(),
		SizeBytes:   10,
	}
	goodJSON, _ := json.Marshal(goodEntry)

	indexContent := string(goodJSON) + "\n" +
		"this is not valid json\n" +
		"\n" +
		"also bad {{\n"

	os.WriteFile(ResultIndexPath(dataDir), []byte(indexContent), 0644)

	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 valid entry, got %d", len(entries))
	}
	if entries[0].WorkUnitID != "wu-good" {
		t.Errorf("WorkUnitID = %q, want wu-good", entries[0].WorkUnitID)
	}
}

func TestGetResultData_ReturnsCorrectData(t *testing.T) {
	dataDir := t.TempDir()
	outputData := []byte(`{"result": "computed"}`)

	err := SaveResult(dataDir, uuidFetch, "Fetch Leaf", "fetch-leaf", "head", outputData, "", 0)
	if err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}

	data, err := GetResultData(dataDir, uuidFetch)
	if err != nil {
		t.Fatalf("GetResultData() error: %v", err)
	}
	if string(data) != string(outputData) {
		t.Errorf("GetResultData() = %q, want %q", string(data), string(outputData))
	}
}

func TestGetResultData_NonExistentID(t *testing.T) {
	dataDir := t.TempDir()
	os.MkdirAll(ResultsDir(dataDir), 0700)

	_, err := GetResultData(dataDir, "99999999-9999-4999-8999-999999999999")
	if err == nil {
		t.Fatal("expected error for non-existent result")
	}
	if !strings.Contains(err.Error(), "result not found") {
		t.Errorf("error = %q, want to contain 'result not found'", err.Error())
	}
}

func TestEvictResults_RemovesOldestWhenOverLimit(t *testing.T) {
	dataDir := t.TempDir()

	// Save 3 results, each 100 bytes.
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte('A' + (i % 26))
	}

	// Save with small time gaps so ordering is deterministic.
	for i, id := range []string{uuidOld, uuidMid, uuidNew} {
		err := SaveResult(dataDir, id, "Leaf", "leaf", "head", payload, "", 0)
		if err != nil {
			t.Fatalf("SaveResult(%s) error: %v", id, err)
		}
		// Hack: rewrite the index to inject specific timestamps so eviction order is deterministic.
		// We'll fix up after the loop.
		_ = i
	}

	// Rewrite index with explicit timestamps for deterministic ordering.
	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	for i := range entries {
		entries[i].CompletedAt = time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC)
	}
	rewriteResultIndex(dataDir, entries)

	// Total size is 300 bytes. Set max to 150 bytes.
	// Should evict "wu-old" (oldest) and "wu-mid" (second oldest), keeping "wu-new".
	err = evictResults(dataDir, 150)
	if err != nil {
		t.Fatalf("evictResults() error: %v", err)
	}

	remaining, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() after eviction error: %v", err)
	}

	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining entry after eviction, got %d", len(remaining))
	}
	if remaining[0].WorkUnitID != uuidNew {
		t.Errorf("remaining entry = %q, want %s", remaining[0].WorkUnitID, uuidNew)
	}

	// Verify the old files are deleted.
	for _, id := range []string{uuidOld, uuidMid} {
		path := filepath.Join(ResultsDir(dataDir), id+".json")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted after eviction", id)
		}
	}

	// Verify the kept file still exists.
	path := filepath.Join(ResultsDir(dataDir), uuidNew+".json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s.json to still exist: %v", uuidNew, err)
	}
}

func TestEvictResults_NoEvictionWhenUnderLimit(t *testing.T) {
	dataDir := t.TempDir()

	payload := []byte(`{"small": true}`)
	err := SaveResult(dataDir, uuidOnly, "Leaf", "leaf", "head", payload, "", 0)
	if err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}

	// Max bytes is much larger than the single entry.
	err = evictResults(dataDir, 1024*1024)
	if err != nil {
		t.Fatalf("evictResults() error: %v", err)
	}

	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (no eviction), got %d", len(entries))
	}
}

func TestEvictResults_EmptyIndex(t *testing.T) {
	dataDir := t.TempDir()

	// Evicting an empty directory should not error.
	err := evictResults(dataDir, 100)
	if err != nil {
		t.Fatalf("evictResults() on empty dir error: %v", err)
	}
}

func TestSaveResult_EvictionTriggeredByMaxBytes(t *testing.T) {
	dataDir := t.TempDir()

	// Save two 100-byte results with a 150-byte max.
	// The second save should trigger eviction of the first.
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = byte('X')
	}

	err := SaveResult(dataDir, uuidFirst, "Leaf", "leaf", "head", payload, "", 0)
	if err != nil {
		t.Fatalf("SaveResult(%s) error: %v", uuidFirst, err)
	}

	// Rewrite index with old timestamp so wu-first is the oldest.
	entries, _ := ListResults(dataDir)
	entries[0].CompletedAt = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rewriteResultIndex(dataDir, entries)

	// Save second result with maxBytes=150 (total would be 200, over limit).
	err = SaveResult(dataDir, uuidSecond, "Leaf", "leaf", "head", payload, "", 150)
	if err != nil {
		t.Fatalf("SaveResult(%s) error: %v", uuidSecond, err)
	}

	remaining, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}

	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining after auto-eviction, got %d", len(remaining))
	}
	if remaining[0].WorkUnitID != uuidSecond {
		t.Errorf("remaining entry = %q, want %s", remaining[0].WorkUnitID, uuidSecond)
	}
}

func TestResultsDir_ReturnsExpectedPath(t *testing.T) {
	got := ResultsDir("/home/user/.lettuce")
	want := filepath.Join("/home/user/.lettuce", "results")
	if got != want {
		t.Errorf("ResultsDir() = %q, want %q", got, want)
	}
}

func TestResultIndexPath_ReturnsExpectedPath(t *testing.T) {
	got := ResultIndexPath("/home/user/.lettuce")
	want := filepath.Join("/home/user/.lettuce", "results", "index.jsonl")
	if got != want {
		t.Errorf("ResultIndexPath() = %q, want %q", got, want)
	}
}

func TestSaveResult_DuplicateWorkUnitID(t *testing.T) {
	dataDir := t.TempDir()

	// Save the same work unit ID twice with different data.
	err := SaveResult(dataDir, uuidDup, "Leaf A", "leaf-a", "head", []byte(`{"v":1}`), "", 0)
	if err != nil {
		t.Fatalf("first SaveResult() error: %v", err)
	}

	err = SaveResult(dataDir, uuidDup, "Leaf A", "leaf-a", "head", []byte(`{"v":2}`), "", 0)
	if err != nil {
		t.Fatalf("second SaveResult() error: %v", err)
	}

	// Verify only one file exists (overwritten).
	resultPath := filepath.Join(ResultsDir(dataDir), uuidDup+".json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result file not found: %v", err)
	}
	if string(data) != `{"v":2}` {
		t.Errorf("result file content = %q, want %q (should be overwritten)", string(data), `{"v":2}`)
	}

	// Verify index has exactly one entry (deduplication).
	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 index entry after duplicate save, got %d", len(entries))
	}
	if entries[0].WorkUnitID != uuidDup {
		t.Errorf("WorkUnitID = %q, want %s", entries[0].WorkUnitID, uuidDup)
	}
	if entries[0].SizeBytes != int64(len(`{"v":2}`)) {
		t.Errorf("SizeBytes = %d, want %d (should reflect second save)", entries[0].SizeBytes, len(`{"v":2}`))
	}
}

func TestGetResultData_PathTraversal(t *testing.T) {
	dataDir := t.TempDir()

	// Create a file outside the results directory to verify it can't be read.
	secretPath := filepath.Join(dataDir, "secret.json")
	os.WriteFile(secretPath, []byte(`{"secret": true}`), 0644)

	// Attempt path traversal and other non-UUID payloads. All must be rejected
	// before any filesystem access (H2). Note the validator now also rejects the
	// old short fixture style (e.g. "wu-legit") since only canonical UUIDs pass.
	traversalIDs := []string{
		"../secret",
		"..\\secret",
		"foo/../../secret",
		"foo\\..\\secret",
		"..%2fsecret", // won't match / but tests the .. check
		"wu-legit",    // not a UUID
	}

	for _, id := range traversalIDs {
		_, err := GetResultData(dataDir, id)
		if err == nil {
			t.Errorf("GetResultData(%q) should have returned error for path traversal", id)
			continue
		}
		if !strings.Contains(err.Error(), "invalid work unit ID") {
			t.Errorf("GetResultData(%q) error = %q, want 'invalid work unit ID'", id, err.Error())
		}
	}

	// Verify legitimate (canonical UUID) IDs still work.
	err := SaveResult(dataDir, uuidLegit, "Leaf", "leaf", "head", []byte(`{"ok":true}`), "", 0)
	if err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}
	data, err := GetResultData(dataDir, uuidLegit)
	if err != nil {
		t.Fatalf("GetResultData(%s) error: %v", uuidLegit, err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("GetResultData(wu-legit) = %q, want %q", string(data), `{"ok":true}`)
	}
}

func TestSaveResult_VizBundlePathEmpty(t *testing.T) {
	dataDir := t.TempDir()

	err := SaveResult(dataDir, uuidNoViz, "Leaf", "leaf", "head", []byte(`{}`), "", 0)
	if err != nil {
		t.Fatalf("SaveResult() error: %v", err)
	}

	entries, err := ListResults(dataDir)
	if err != nil {
		t.Fatalf("ListResults() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].VizBundlePath != "" {
		t.Errorf("VizBundlePath = %q, want empty", entries[0].VizBundlePath)
	}
}
