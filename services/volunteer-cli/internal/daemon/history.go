package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HistoryEntry records a completed work unit.
type HistoryEntry struct {
	WorkUnitID string `json:"work_unit_id"`
	LeafID     string `json:"leaf_id"`
	// LeafName is the leaf's display name as the daemon's head cache knew it when
	// the unit completed. LeafID is the head's internal UUID, which no volunteer
	// can read; recording the name here lets `history` stay legible with the
	// daemon stopped and long after the cache has moved on (TB-46). Empty in
	// entries written before the field existed or when the cache did not know
	// the leaf — readers fall back to LeafID.
	LeafName         string    `json:"leaf_name,omitempty"`
	ServerName       string    `json:"server_name,omitempty"`
	CompletedAt      time.Time `json:"completed_at"`
	WallClockSeconds int64     `json:"wall_clock_seconds"`
	CPUSeconds       int64     `json:"cpu_seconds"`
	ResultAccepted   bool      `json:"result_accepted"`
}

// HistoryFilePath returns the path to the history JSONL file.
func HistoryFilePath(dataDir string) string {
	return filepath.Join(dataDir, "history.jsonl")
}

// AppendHistory writes a history entry to the JSONL file.
func AppendHistory(dataDir string, entry HistoryEntry) error {
	path := HistoryFilePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating history directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening history file: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling history entry: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing history entry: %w", err)
	}
	return nil
}

// ReadHistory reads the most recent entries from the history file, newest first,
// at most limit of them. A limit of zero or less reads 50.
func ReadHistory(dataDir string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	entries, err := ReadAllHistory(dataDir)
	if err != nil {
		return nil, err
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// ReadAllHistory reads every entry in the history file, newest first. A missing
// file is an empty history, not an error; malformed lines are skipped. Callers
// that show a page of history should count from this slice rather than from the
// page, so a "showing N of M" line can tell the volunteer how much is not shown.
func ReadAllHistory(dataDir string) ([]HistoryEntry, error) {
	path := HistoryFilePath(dataDir)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening history file: %w", err)
	}
	defer f.Close()

	var entries []HistoryEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip malformed lines
		}
		entries = append(entries, e)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading history: %w", err)
	}

	// The file is append-only, so reversing it yields newest first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// DisplayLeafName is the name a person should see for this entry: the recorded
// leaf name when the daemon knew it, else the raw leaf id.
func (e HistoryEntry) DisplayLeafName() string {
	if e.LeafName != "" {
		return e.LeafName
	}
	return e.LeafID
}
