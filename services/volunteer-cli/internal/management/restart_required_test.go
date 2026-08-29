package management

import (
	"fmt"
	"net/http"
	"testing"
)

// max_concurrent_tasks is fixed when the daemon starts (it is the slot
// count), so a change to it needs a restart just as a trust change does; an
// unchanged value does not.
func TestUpdateConfig_MaxConcurrentTasksRequiresRestart(t *testing.T) {
	env := setupTestEnv(t)

	current := env.daemon.GetConfig().MaxConcurrentTasks
	resp, body := putServers(t, env, fmt.Sprintf(`{"max_concurrent_tasks": %d}`, current))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != false {
		t.Errorf("restart_required = %v, want false when max_concurrent_tasks is unchanged", body["restart_required"])
	}

	resp, body = putServers(t, env, fmt.Sprintf(`{"max_concurrent_tasks": %d}`, current+1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", resp.StatusCode, body)
	}
	if body["restart_required"] != true {
		t.Errorf("restart_required = %v, want true when max_concurrent_tasks changed", body["restart_required"])
	}
	if int(body["max_concurrent_tasks"].(float64)) != current+1 {
		t.Errorf("max_concurrent_tasks = %v, want %d", body["max_concurrent_tasks"], current+1)
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
