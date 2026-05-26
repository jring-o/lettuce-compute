//go:build integration

package leaf

import (
	"io"
	"net/http"
	"testing"
)

// TestF03ProjectLifecycleE2E exercises the full F03 project management lifecycle:
// create → configure → update configs → activate → pause → resume → archive (fail) →
// pause → archive → verify → delete → verify gone.
func TestF03ProjectLifecycleE2E(t *testing.T) {
	ts, pool, cleanup := setupHandlerServer(t)
	defer cleanup()

	userID := createTestUser(t, pool, "e2e-lifecycle")
	baseURL := ts.URL + "/api/v1/leafs"

	// Step 1: Create leaf → 201 (DRAFT).
	req := validCreateRequest(&userID)
	req.Name = "E2E Lifecycle Project"
	resp := doRequest(t, "POST", baseURL, req)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 1: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var p Leaf
	decodeJSON(t, resp, &p)
	if p.State != StateDraft {
		t.Fatalf("step 1: state = %q, want DRAFT", p.State)
	}
	leafURL := baseURL + "/" + p.ID.String()

	// Step 2: Configure → 200 (CONFIGURING).
	resp = doRequest(t, "POST", leafURL+"/configure", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 2: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)
	if p.State != StateConfiguring {
		t.Fatalf("step 2: state = %q, want CONFIGURING", p.State)
	}

	// Step 3: Update with full configs → 200.
	updateReq := fullConfigUpdate()
	resp = doRequest(t, "PUT", leafURL, updateReq)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 3: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)

	// Step 4: Activate → 200 (ACTIVE).
	resp = doRequest(t, "POST", leafURL+"/activate", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 4: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)
	if p.State != StateActive {
		t.Fatalf("step 4: state = %q, want ACTIVE", p.State)
	}

	// Step 5: Pause → 200 (PAUSED).
	resp = doRequest(t, "POST", leafURL+"/pause", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 5: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)
	if p.State != StatePaused {
		t.Fatalf("step 5: state = %q, want PAUSED", p.State)
	}

	// Step 6: Resume → 200 (ACTIVE).
	resp = doRequest(t, "POST", leafURL+"/resume", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 6: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)
	if p.State != StateActive {
		t.Fatalf("step 6: state = %q, want ACTIVE", p.State)
	}

	// Step 7: Archive from ACTIVE → 409 (can't archive active).
	resp = doRequest(t, "POST", leafURL+"/archive", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("step 7: expected 409, got %d", resp.StatusCode)
	}

	// Step 8: Pause → 200 (PAUSED).
	resp = doRequest(t, "POST", leafURL+"/pause", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 8: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)
	if p.State != StatePaused {
		t.Fatalf("step 8: state = %q, want PAUSED", p.State)
	}

	// Step 9: Archive → 200 (ARCHIVED).
	resp = doRequest(t, "POST", leafURL+"/archive", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 9: expected 200, got %d: %s", resp.StatusCode, body)
	}
	decodeJSON(t, resp, &p)
	if p.State != StateArchived {
		t.Fatalf("step 9: state = %q, want ARCHIVED", p.State)
	}

	// Step 10: GET → 200 (verify ARCHIVED state persisted).
	resp = doRequest(t, "GET", leafURL, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("step 10: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var fetched Leaf
	decodeJSON(t, resp, &fetched)
	if fetched.State != StateArchived {
		t.Fatalf("step 10: state = %q, want ARCHIVED", fetched.State)
	}

	// Step 11: DELETE → 204 (archived leaf can be deleted).
	resp = doRequest(t, "DELETE", leafURL, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("step 11: expected 204, got %d", resp.StatusCode)
	}

	// Step 12: GET → 404 (project is gone).
	resp = doRequest(t, "GET", leafURL, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("step 12: expected 404, got %d", resp.StatusCode)
	}
}
