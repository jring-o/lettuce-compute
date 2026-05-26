package management

// Tests for v0.9.5 task visibility API endpoints (S106):
// - Task detail endpoint with all fields (scenario 3)
// - Task suspend/resume/abort endpoints with error codes (scenario 2)
// - History endpoint with cpu_seconds and head_name (scenario 5)
// - computeTaskStatus covered in daemon_bridge_test.go (scenarios 1+2)

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// TestTaskDetailEndpoint verifies that GET /api/v1/tasks/{id}/details returns
// all fields including per-process metrics from the mocked reader.
func TestTaskDetailEndpoint(t *testing.T) {
	env, wuID, blockCh := setupTestEnvWithActiveTask(t)
	_ = blockCh // cleaned up by t.Cleanup

	resp := env.doRequest(t, "GET", "/api/v1/tasks/"+wuID+"/details", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET details: expected 200, got %d", resp.StatusCode)
	}
	detail := decodeJSON(t, resp)

	// Core identity fields.
	requiredFields := []string{
		"work_unit_id", "leaf_name", "task_status", "head_name",
		"runtime_type", "work_dir", "elapsed_seconds", "cpu_seconds",
		"deadline_seconds", "progress_pct", "fraction_done",
	}
	for _, field := range requiredFields {
		if _, ok := detail[field]; !ok {
			t.Errorf("missing required field: %s", field)
		}
	}

	// Verify specific values from the test fixture.
	if detail["work_unit_id"] != wuID {
		t.Errorf("work_unit_id = %v, want %s", detail["work_unit_id"], wuID)
	}
	if detail["task_status"] != "running" {
		t.Errorf("task_status = %v, want 'running'", detail["task_status"])
	}
	if detail["head_name"] != "test-server" {
		t.Errorf("head_name = %v, want 'test-server'", detail["head_name"])
	}
	if detail["runtime_type"] != "native" {
		t.Errorf("runtime_type = %v, want 'native'", detail["runtime_type"])
	}

	// Timing fields should be non-negative.
	if elapsed, ok := detail["elapsed_seconds"].(float64); !ok || elapsed < 0 {
		t.Errorf("elapsed_seconds = %v, want >= 0", detail["elapsed_seconds"])
	}
	if cpuSec, ok := detail["cpu_seconds"].(float64); !ok || cpuSec < 0 {
		t.Errorf("cpu_seconds = %v, want >= 0", detail["cpu_seconds"])
	}

	// Deadline should be close to 3600 (configured in setupTestEnvWithActiveTask).
	if deadline, ok := detail["deadline_seconds"].(float64); !ok || deadline < 3590 {
		t.Errorf("deadline_seconds = %v, want ~3600", detail["deadline_seconds"])
	}

	// Process ID from mock.
	if pid, ok := detail["process_id"].(float64); !ok || int(pid) != 12345 {
		t.Errorf("process_id = %v, want 12345", detail["process_id"])
	}

	// Per-process metrics from the mocked readProcessMetrics.
	if rss, ok := detail["memory_rss_mb"].(float64); !ok || rss != 128.5 {
		t.Errorf("memory_rss_mb = %v, want 128.5", detail["memory_rss_mb"])
	}
	if vmem, ok := detail["virtual_memory_mb"].(float64); !ok || vmem != 512.0 {
		t.Errorf("virtual_memory_mb = %v, want 512.0", detail["virtual_memory_mb"])
	}
	if cpuPct, ok := detail["cpu_usage_pct"].(float64); !ok || cpuPct != 42.0 {
		t.Errorf("cpu_usage_pct = %v, want 42.0", detail["cpu_usage_pct"])
	}
	if dr, ok := detail["disk_read_mb"].(float64); !ok || dr != 10.0 {
		t.Errorf("disk_read_mb = %v, want 10.0", detail["disk_read_mb"])
	}
	if dw, ok := detail["disk_written_mb"].(float64); !ok || dw != 5.0 {
		t.Errorf("disk_written_mb = %v, want 5.0", detail["disk_written_mb"])
	}

	// fraction_done is 0 (no progress file in test fixture).
	if fd, ok := detail["fraction_done"].(float64); !ok || fd != 0 {
		t.Errorf("fraction_done = %v, want 0", detail["fraction_done"])
	}
}

// TestTaskDetailEndpoint_NotFound verifies 404 for a non-existent task.
func TestTaskDetailEndpoint_NotFound(t *testing.T) {
	env, _, blockCh := setupTestEnvWithActiveTask(t)
	_ = blockCh

	resp := env.doRequest(t, "GET", "/api/v1/tasks/nonexistent/details", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET nonexistent: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestTaskSuspendResumeAbortEndpoints verifies HTTP status codes for the
// per-task lifecycle endpoints: 200 on success, 409 on conflict, 404 on not found.
func TestTaskSuspendResumeAbortEndpoints(t *testing.T) {
	env, wuID, blockCh := setupTestEnvWithActiveTask(t)
	_ = blockCh
	taskPath := "/api/v1/tasks/" + wuID

	// Suspend → 200.
	resp := env.doRequest(t, "POST", taskPath+"/suspend", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: expected 200, got %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)
	if body["status"] != "suspended" {
		t.Errorf("suspend status = %v, want 'suspended'", body["status"])
	}

	// Double suspend → 409.
	resp = env.doRequest(t, "POST", taskPath+"/suspend", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double suspend: expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Resume → 200.
	resp = env.doRequest(t, "POST", taskPath+"/resume", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if body["status"] != "resumed" {
		t.Errorf("resume status = %v, want 'resumed'", body["status"])
	}

	// Double resume → 409.
	resp = env.doRequest(t, "POST", taskPath+"/resume", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double resume: expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Resume non-existent → 404.
	resp = env.doRequest(t, "POST", "/api/v1/tasks/nonexistent/resume", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("resume nonexistent: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Abort → 200.
	resp = env.doRequest(t, "POST", taskPath+"/abort", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abort: expected 200, got %d", resp.StatusCode)
	}
	body = decodeJSON(t, resp)
	if body["status"] != "aborted" {
		t.Errorf("abort status = %v, want 'aborted'", body["status"])
	}

	// Wait for slot cleanup.
	time.Sleep(100 * time.Millisecond)

	// Abort non-existent → 404.
	resp = env.doRequest(t, "POST", "/api/v1/tasks/nonexistent/abort", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("abort nonexistent: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestHistoryEndpointCPUSeconds verifies that GET /api/v1/history returns
// cpu_seconds and head_name fields for completed work units.
func TestHistoryEndpointCPUSeconds(t *testing.T) {
	env := setupTestEnv(t)

	// Write history entries with known cpu_seconds values.
	entry1 := daemon.HistoryEntry{
		WorkUnitID:       "wu-hist-1",
		LeafID:           "leaf-prime-gaps",
		ServerName:       "test-head",
		CompletedAt:      time.Now().Add(-2 * time.Hour),
		WallClockSeconds: 3600,
		CPUSeconds:       3000, // 600s paused
		ResultAccepted:   true,
	}
	entry2 := daemon.HistoryEntry{
		WorkUnitID:       "wu-hist-2",
		LeafID:           "leaf-climate",
		ServerName:       "other-head",
		CompletedAt:      time.Now().Add(-1 * time.Hour),
		WallClockSeconds: 7200,
		CPUSeconds:       7000, // 200s paused
		ResultAccepted:   false,
	}

	if err := daemon.AppendHistory(env.dataDir, entry1); err != nil {
		t.Fatalf("writing history entry 1: %v", err)
	}
	if err := daemon.AppendHistory(env.dataDir, entry2); err != nil {
		t.Fatalf("writing history entry 2: %v", err)
	}

	// GET /api/v1/history
	resp := env.doRequest(t, "GET", "/api/v1/history", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET history: expected 200, got %d", resp.StatusCode)
	}

	var histResp struct {
		Entries []struct {
			WorkUnitID       string `json:"work_unit_id"`
			CPUSeconds       int64  `json:"cpu_seconds"`
			HeadName         string `json:"head_name"`
			DurationSeconds  int64  `json:"duration_seconds"`
			ValidationStatus string `json:"validation_status"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&histResp); err != nil {
		t.Fatalf("decoding history: %v", err)
	}
	resp.Body.Close()

	if len(histResp.Entries) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(histResp.Entries))
	}

	// Entries are newest first: wu-hist-2, wu-hist-1.
	e2 := histResp.Entries[0]
	e1 := histResp.Entries[1]

	// Verify cpu_seconds.
	if e1.CPUSeconds != 3000 {
		t.Errorf("entry 1 cpu_seconds = %d, want 3000", e1.CPUSeconds)
	}
	if e2.CPUSeconds != 7000 {
		t.Errorf("entry 2 cpu_seconds = %d, want 7000", e2.CPUSeconds)
	}

	// Verify head_name.
	if e1.HeadName != "test-head" {
		t.Errorf("entry 1 head_name = %q, want 'test-head'", e1.HeadName)
	}
	if e2.HeadName != "other-head" {
		t.Errorf("entry 2 head_name = %q, want 'other-head'", e2.HeadName)
	}

	// Verify duration_seconds (wall clock).
	if e1.DurationSeconds != 3600 {
		t.Errorf("entry 1 duration_seconds = %d, want 3600", e1.DurationSeconds)
	}
	if e2.DurationSeconds != 7200 {
		t.Errorf("entry 2 duration_seconds = %d, want 7200", e2.DurationSeconds)
	}

	// Verify validation_status.
	if e1.ValidationStatus != "accepted" {
		t.Errorf("entry 1 validation_status = %q, want 'accepted'", e1.ValidationStatus)
	}
	if e2.ValidationStatus != "rejected" {
		t.Errorf("entry 2 validation_status = %q, want 'rejected'", e2.ValidationStatus)
	}

	// Verify paused time can be derived: duration - cpu_seconds.
	paused1 := e1.DurationSeconds - e1.CPUSeconds
	if paused1 != 600 {
		t.Errorf("entry 1 paused = %d, want 600", paused1)
	}
	paused2 := e2.DurationSeconds - e2.CPUSeconds
	if paused2 != 200 {
		t.Errorf("entry 2 paused = %d, want 200", paused2)
	}
}

// Note: computeTaskStatus is tested in daemon_bridge_test.go with individual
// test functions for each status variant (Running, SuspendedUser, SuspendedThermal,
// SuspendedScheduled, PerSlotSuspendedNoDaemonPause, NotSuspendedIgnoresPauseReason).
