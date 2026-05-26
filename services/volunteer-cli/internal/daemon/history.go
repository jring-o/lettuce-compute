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
	WorkUnitID       string    `json:"work_unit_id"`
	LeafID           string    `json:"leaf_id"`
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

// ReadHistory reads the most recent entries from the history file.
func ReadHistory(dataDir string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}

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

	// Return newest first, limited.
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	// Reverse for newest first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	return entries, nil
}
