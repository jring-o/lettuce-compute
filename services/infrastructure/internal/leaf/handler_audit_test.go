package leaf

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// TB-38 (4): every successful leaf mutation must leave an audit line at production
// log level — for an update, a field-level before→after diff with the actor. The
// scios leaf-overwrite forensics (2026-07-30/31) had only the generic access line
// to work from: WHAT changed and the prior values took a day to reconstruct from
// updated_at timestamps, public reads, and the operator's memory.

// auditTestLogger returns an Info-level JSON logger writing into buf.
func auditTestLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})), buf
}

// auditLines returns the log records in buf whose msg equals msg exactly.
func auditLines(buf *bytes.Buffer, msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if m, _ := rec["msg"].(string); m == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestHandleUpdate_EmitsAuditDiffLine(t *testing.T) {
	lf := newUpdateTestLeaf()
	actor := types.NewID()
	// Metadata re-validation runs on a name/visibility update and requires a creator
	// identity and a valid visibility on the stored leaf.
	lf.CreatorID = &actor
	lf.Visibility = VisibilityPublic
	logger, buf := auditTestLogger()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: logger}
	body := `{"name":"Renamed Arena","visibility":"UNLISTED"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/leafs/"+lf.ID.String(), bytes.NewBufferString(body))
	req.SetPathValue("leaf_id", lf.ID.String())
	req = req.WithContext(WithViewer(req.Context(), Viewer{UserID: actor, Authed: true}))
	rec := httptest.NewRecorder()
	h.handleUpdate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	lines := auditLines(buf, "leaf updated")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1 — a successful PUT must be reconstructable from the "+
			"head log alone; got log:\n%s", len(lines), buf.String())
	}
	line := lines[0]
	if got, _ := line["leaf_id"].(string); got != lf.ID.String() {
		t.Errorf("leaf_id = %q, want %q", got, lf.ID.String())
	}

	// The diff: both changed fields named, each with its before and after values.
	changed, _ := line["changed"].([]any)
	want := map[string]bool{"name": false, "visibility": false}
	for _, c := range changed {
		if s, ok := c.(string); ok {
			want[s] = true
		}
	}
	for field, seen := range want {
		if !seen {
			t.Errorf("changed = %v, must include %q", line["changed"], field)
		}
	}
	if got, _ := line["name_before"].(string); got != `"Beyblade Arena"` {
		t.Errorf("name_before = %q, want the prior value — the diff must preserve what was overwritten", got)
	}
	if got, _ := line["name_after"].(string); got != `"Renamed Arena"` {
		t.Errorf("name_after = %q, want the new value", got)
	}

	// The actor and the connection.
	if got, _ := line["actor_user_id"].(string); got != actor.String() {
		t.Errorf("actor_user_id = %q, want %q", got, actor.String())
	}
	if got, _ := line["remote_addr"].(string); got == "" {
		t.Errorf("line must carry remote_addr: %v", line)
	}
}

// TestHandleUpdate_NoOpChangesNothingInDiff: an update that changes nothing still
// audits (the PUT happened) but must not claim fields changed.
func TestHandleUpdate_NoOpChangesNothingInDiff(t *testing.T) {
	lf := newUpdateTestLeaf()
	creator := types.NewID()
	lf.CreatorID = &creator
	lf.Visibility = VisibilityUnlisted // PUBLIC would additionally demand research_area
	logger, buf := auditTestLogger()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: logger}

	rec := doUpdate(t, h, lf.ID, `{"name":"Beyblade Arena"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	lines := auditLines(buf, "leaf updated")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1; got log:\n%s", len(lines), buf.String())
	}
	if changed, _ := lines[0]["changed"].([]any); len(changed) != 0 {
		t.Errorf("changed = %v, want empty for a no-op update", changed)
	}
}

func TestHandleTransition_EmitsAuditLine(t *testing.T) {
	lf := newUpdateTestLeaf() // StateActive
	logger, buf := auditTestLogger()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: logger}

	actor := types.NewID()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/leafs/"+lf.ID.String()+"/pause", nil)
	req.SetPathValue("leaf_id", lf.ID.String())
	req = req.WithContext(WithViewer(req.Context(), Viewer{UserID: actor, Authed: true}))
	rec := httptest.NewRecorder()
	h.handlePause(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	lines := auditLines(buf, "leaf state changed")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1 — pause/resume/archive/configure logged nothing on "+
			"success; got log:\n%s", len(lines), buf.String())
	}
	line := lines[0]
	if got, _ := line["from"].(string); got != string(StateActive) {
		t.Errorf("from = %q, want %q", got, StateActive)
	}
	if got, _ := line["to"].(string); got != string(StatePaused) {
		t.Errorf("to = %q, want %q", got, StatePaused)
	}
	if got, _ := line["actor_user_id"].(string); got != actor.String() {
		t.Errorf("actor_user_id = %q, want %q", got, actor.String())
	}
}

// TB-38 × design §4.7: flipping results_visibility must land in the audit diff
// with its before/after values and the actor — the reason the public-viz
// opt-in lives on the leaf row rather than in deployment config is precisely
// that an edit leaves this reconstructable line.
func TestHandleUpdate_AuditsResultsVisibilityFlip(t *testing.T) {
	lf := newUpdateTestLeaf()
	lf.ResultsVisibility = ResultsVisibilityOwnerOnly
	actor := types.NewID()
	logger, buf := auditTestLogger()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: logger}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/leafs/"+lf.ID.String(),
		bytes.NewBufferString(`{"results_visibility":"PUBLIC"}`))
	req.SetPathValue("leaf_id", lf.ID.String())
	req = req.WithContext(WithViewer(req.Context(), Viewer{UserID: actor, Authed: true}))
	rec := httptest.NewRecorder()
	h.handleUpdate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	lines := auditLines(buf, "leaf updated")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1; got log:\n%s", len(lines), buf.String())
	}
	line := lines[0]
	changed, _ := line["changed"].([]any)
	found := false
	for _, c := range changed {
		if s, _ := c.(string); s == "results_visibility" {
			found = true
		}
	}
	if !found {
		t.Errorf("changed = %v, must include results_visibility", changed)
	}
	if got, _ := line["results_visibility_before"].(string); got != `"OWNER_ONLY"` {
		t.Errorf("results_visibility_before = %q, want the prior value", got)
	}
	if got, _ := line["results_visibility_after"].(string); got != `"PUBLIC"` {
		t.Errorf("results_visibility_after = %q, want the new value", got)
	}
	if got, _ := line["actor_user_id"].(string); got != actor.String() {
		t.Errorf("actor_user_id = %q, want %q", got, actor.String())
	}
}
