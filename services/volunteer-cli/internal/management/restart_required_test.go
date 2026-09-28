package management

import (
	"net/http"
	"testing"
)

// max_running_tasks is live: the cap bounds the next task admitted, so a
// change to it needs no restart (the retired max_concurrent_tasks it replaces
// was the slot count, fixed when the daemon started). It is saved and
// returned as sent.
func TestUpdateConfig_MaxRunningTasksIsLive(t *testing.T) {
	env := setupTestEnv(t)

	resp, body := putServers(t, env, `{"max_running_tasks": 3}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != false {
		t.Errorf("restart_required = %v, want false: max_running_tasks applies to the next task admitted", body["restart_required"])
	}
	if int(body["max_running_tasks"].(float64)) != 3 {
		t.Errorf("max_running_tasks = %v, want 3", body["max_running_tasks"])
	}
	if got := env.daemon.GetConfig().MaxRunningTasks; got != 3 {
		t.Errorf("daemon's max_running_tasks = %d, want 3 (applied without a restart)", got)
	}

	// An unrelated change alone still needs no restart.
	resp, body = putServers(t, env, `{"log_level": "debug"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != false {
		t.Errorf("restart_required = %v, want false for a log_level change", body["restart_required"])
	}
}
