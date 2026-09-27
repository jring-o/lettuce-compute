package management

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// TestCreditAPICarriesEachHeadsWorkStatus: GET /api/v1/credit carries each head's
// results by state and copies in progress. A head that reports them with nothing
// here gets an empty list; a head that answered without them (an older head) and
// an unreachable head get null, so a client never shows zeros a head did not send.
func TestCreditAPICarriesEachHeadsWorkStatus(t *testing.T) {
	env := setupTestEnv(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	reply := func(ws *lettucev1.WorkStatus) *e2eMockWorkClient {
		return &e2eMockWorkClient{
			getMyContributionFn: func(ctx context.Context, req *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
				return &lettucev1.GetMyContributionResponse{VolunteerId: "vol-1", TotalCredit: 1, WorkStatus: ws}, nil
			},
		}
	}
	current := reply(&lettucev1.WorkStatus{ByLeaf: []*lettucev1.LeafWorkStatus{{
		LeafId: "leaf-gpu", LeafName: "GPU leaf",
		ResultsPending: 523, ResultsAgreed: 10, ResultsDisagreed: 1,
		ResultsAwaitingContentVerification: 2, ResultsContentVerificationFailed: 3, ResultsSuperseded: 4,
		RunsStopped: 5, CopiesRunning: 6, CopiesWaitingToStart: 7,
	}}})
	empty := reply(&lettucev1.WorkStatus{})
	older := reply(nil)
	down := &e2eMockWorkClient{
		getMyContributionFn: func(ctx context.Context, req *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
			return nil, errors.New("connection refused")
		},
	}
	env.daemon.SetMultiClientForTest(daemon.NewMultiServerClient([]*daemon.ServerConnection{
		{Name: "head-current", VolunteerID: "vol-1", Available: true, Client: current},
		{Name: "head-empty", VolunteerID: "vol-1", Available: true, Client: empty},
		{Name: "head-older", VolunteerID: "vol-1", Available: true, Client: older},
		{Name: "head-down", VolunteerID: "vol-1", Available: true, Client: down},
	}, logger))

	resp := env.doRequest(t, "GET", "/api/v1/credit", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/credit: status %d", resp.StatusCode)
	}
	body := decodeJSON(t, resp)

	heads := map[string]map[string]any{}
	for _, h := range body["by_head"].([]any) {
		hm := h.(map[string]any)
		heads[hm["head_name"].(string)] = hm
	}
	if len(heads) != 4 {
		t.Fatalf("by_head has %d heads, want 4: %v", len(heads), body["by_head"])
	}

	ws, ok := heads["head-current"]["work_status"].(map[string]any)
	if !ok {
		t.Fatalf("head-current: work_status = %#v, want the head's figures", heads["head-current"]["work_status"])
	}
	leaves, _ := ws["by_leaf"].([]any)
	if len(leaves) != 1 {
		t.Fatalf("head-current: work_status.by_leaf = %#v, want one leaf", ws["by_leaf"])
	}
	leaf := leaves[0].(map[string]any)
	for key, want := range map[string]float64{
		"results_pending": 523, "results_agreed": 10, "results_disagreed": 1,
		"results_awaiting_content_verification": 2, "results_content_verification_failed": 3,
		"results_superseded": 4, "runs_stopped": 5, "copies_running": 6, "copies_waiting_to_start": 7,
	} {
		if got, _ := leaf[key].(float64); got != want {
			t.Errorf("head-current, GPU leaf: %s = %v, want %v", key, leaf[key], want)
		}
	}
	if leaf["leaf_name"] != "GPU leaf" || leaf["leaf_id"] != "leaf-gpu" {
		t.Errorf("head-current: leaf = %v / %v, want leaf-gpu / GPU leaf", leaf["leaf_id"], leaf["leaf_name"])
	}

	wsEmpty, ok := heads["head-empty"]["work_status"].(map[string]any)
	if !ok {
		t.Fatalf("head-empty: work_status = %#v, want a reported, empty status", heads["head-empty"]["work_status"])
	}
	if l, ok := wsEmpty["by_leaf"].([]any); !ok || len(l) != 0 {
		t.Errorf("head-empty: work_status.by_leaf = %#v, want []", wsEmpty["by_leaf"])
	}

	for _, name := range []string{"head-older", "head-down"} {
		v, present := heads[name]["work_status"]
		if !present || v != nil {
			t.Errorf("%s: work_status = %#v (present %v), want null: the head did not report it", name, v, present)
		}
	}
}
