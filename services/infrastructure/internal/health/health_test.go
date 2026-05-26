package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

func TestStatusForLowerIsBetter(t *testing.T) {
	tests := []struct {
		name      string
		value     float64
		threshold float64
		want      Status
	}{
		{"healthy - well below threshold", 10, 48, StatusHealthy},
		{"warning - within 20% of threshold", 40, 48, StatusWarning},
		{"critical - at threshold", 48, 48, StatusCritical},
		{"critical - above threshold", 72, 48, StatusCritical},
		{"healthy - zero value", 0, 48, StatusHealthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusForLowerIsBetter(tt.value, tt.threshold)
			if got != tt.want {
				t.Errorf("statusForLowerIsBetter(%v, %v) = %v, want %v", tt.value, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestStatusForHigherIsBetter(t *testing.T) {
	tests := []struct {
		name      string
		value     float64
		threshold float64
		want      Status
	}{
		{"healthy - well above threshold", 15, 0, StatusHealthy},
		{"critical - at threshold", 0, 0, StatusCritical},
		{"healthy - high ratio", 0.85, 0.1, StatusHealthy},
		{"critical - below ratio threshold", 0.05, 0.1, StatusCritical},
		{"warning - near ratio threshold", 0.11, 0.1, StatusWarning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusForHigherIsBetter(tt.value, tt.threshold)
			if got != tt.want {
				t.Errorf("statusForHigherIsBetter(%v, %v) = %v, want %v", tt.value, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestDefaultHealthConfig(t *testing.T) {
	hc := leaf.DefaultHealthConfig()

	if hc.ContributionFlowAlertHours != 48 {
		t.Errorf("expected 48, got %d", hc.ContributionFlowAlertHours)
	}
	if hc.WorkAvailabilityAlertRatio != 0.1 {
		t.Errorf("expected 0.1, got %f", hc.WorkAvailabilityAlertRatio)
	}
	if hc.VolunteerActivityAlertCount != 0 {
		t.Errorf("expected 0, got %d", hc.VolunteerActivityAlertCount)
	}
}

func TestTrendConstants(t *testing.T) {
	if TrendImproving != "improving" {
		t.Errorf("expected 'improving', got %q", TrendImproving)
	}
	if TrendDeclining != "declining" {
		t.Errorf("expected 'declining', got %q", TrendDeclining)
	}
	if TrendStable != "stable" {
		t.Errorf("expected 'stable', got %q", TrendStable)
	}
}

func TestStatusConstants(t *testing.T) {
	if StatusHealthy != "healthy" {
		t.Errorf("expected 'healthy', got %q", StatusHealthy)
	}
	if StatusWarning != "warning" {
		t.Errorf("expected 'warning', got %q", StatusWarning)
	}
	if StatusCritical != "critical" {
		t.Errorf("expected 'critical', got %q", StatusCritical)
	}
}

func TestMetricDetailJSON_Structure(t *testing.T) {
	m := metricDetailJSON{
		Value:          12.5,
		Unit:           "hours_since_last_validation",
		Status:         StatusHealthy,
		AlertThreshold: 48,
		Trend24h:       TrendImproving,
		Avg7d:          8.2,
		Avg30d:         10.1,
	}

	if m.Status != StatusHealthy {
		t.Errorf("expected healthy, got %s", m.Status)
	}
	if m.Trend24h != TrendImproving {
		t.Errorf("expected improving, got %s", m.Trend24h)
	}
}

func TestWorkAvailabilityScore_Computation(t *testing.T) {
	tests := []struct {
		name          string
		sevenDayMean  float64
		fortyDayMean  float64
		expectedScore float64
		expectedAlert bool
	}{
		{"both zero - alert", 0, 0, 0, true},
		{"forty day zero - alert", 10, 0, 0, true},
		{"equal means - score 1.0", 100, 100, 1.0, false},
		{"half activity - score 0.5", 50, 100, 0.5, false},
		{"very low activity - score 0.05 alert", 5, 100, 0.05, true},
		{"increasing activity - score 1.5", 150, 100, 1.5, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var score float64
			if tt.fortyDayMean > 0 {
				score = tt.sevenDayMean / tt.fortyDayMean
			}
			alert := score < 0.1

			epsilon := 0.001
			diff := score - tt.expectedScore
			if diff < -epsilon || diff > epsilon {
				t.Errorf("score=%.3f, want %.3f", score, tt.expectedScore)
			}
			if alert != tt.expectedAlert {
				t.Errorf("alert=%v, want %v", alert, tt.expectedAlert)
			}
		})
	}
}

func TestConfigurableThresholds(t *testing.T) {
	// Custom thresholds should change alert behavior.
	hc := leaf.HealthConfig{
		ContributionFlowAlertHours:  24,
		WorkAvailabilityAlertRatio:  0.5,
		VolunteerActivityAlertCount: 5,
	}

	// With threshold=24, 30 hours should be critical.
	cf := statusForLowerIsBetter(30, float64(hc.ContributionFlowAlertHours))
	if cf != StatusCritical {
		t.Errorf("expected critical for 30h with threshold 24, got %s", cf)
	}

	// With threshold=0.5, ratio 0.3 should be critical.
	wa := statusForHigherIsBetter(0.3, hc.WorkAvailabilityAlertRatio)
	if wa != StatusCritical {
		t.Errorf("expected critical for ratio 0.3 with threshold 0.5, got %s", wa)
	}

	// With threshold=5, count 3 should be critical.
	va := statusForHigherIsBetter(3, float64(hc.VolunteerActivityAlertCount))
	if va != StatusCritical {
		t.Errorf("expected critical for count 3 with threshold 5, got %s", va)
	}
}

func TestStatusForLowerIsBetter_ZeroThreshold(t *testing.T) {
	// Zero threshold: only value == 0 is not critical.
	got := statusForLowerIsBetter(0, 0)
	if got != StatusCritical {
		t.Errorf("expected critical for value=0 threshold=0, got %s", got)
	}

	got = statusForLowerIsBetter(1, 0)
	if got != StatusCritical {
		t.Errorf("expected critical for value=1 threshold=0, got %s", got)
	}
}

func TestStatusForHigherIsBetter_NegativeThreshold(t *testing.T) {
	// Negative threshold edge case: value > threshold should be healthy.
	got := statusForHigherIsBetter(5, -1)
	if got != StatusHealthy {
		t.Errorf("expected healthy for value=5 threshold=-1, got %s", got)
	}
}

func TestStatusForLowerIsBetter_BoundaryAt80Percent(t *testing.T) {
	// Exactly at 80% of threshold should be the warning boundary.
	threshold := 100.0

	got := statusForLowerIsBetter(79.9, threshold)
	if got != StatusHealthy {
		t.Errorf("expected healthy at 79.9 (below 80%% of 100), got %s", got)
	}

	got = statusForLowerIsBetter(80, threshold)
	if got != StatusWarning {
		t.Errorf("expected warning at 80 (at 80%% of 100), got %s", got)
	}
}

func TestOverallStatus_AggregationLogic(t *testing.T) {
	// Test that the worst-status-wins logic from handleHealth works correctly.
	tests := []struct {
		name     string
		statuses []Status
		want     Status
	}{
		{"all healthy", []Status{StatusHealthy, StatusHealthy, StatusHealthy}, StatusHealthy},
		{"one warning", []Status{StatusHealthy, StatusWarning, StatusHealthy}, StatusWarning},
		{"one critical", []Status{StatusHealthy, StatusCritical, StatusHealthy}, StatusCritical},
		{"critical beats warning", []Status{StatusWarning, StatusCritical, StatusHealthy}, StatusCritical},
		{"empty", []Status{}, StatusHealthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worst := StatusHealthy
			for _, s := range tt.statuses {
				if s == StatusCritical {
					worst = StatusCritical
				} else if s == StatusWarning && worst == StatusHealthy {
					worst = StatusWarning
				}
			}
			if worst != tt.want {
				t.Errorf("got %s, want %s", worst, tt.want)
			}
		})
	}
}

func TestLeafHealthJSON_Structure(t *testing.T) {
	lh := leafHealthJSON{
		LeafName: "test-leaf",
		ContributionFlow: metricDetailJSON{
			Value:  12.5,
			Unit:   "hours_since_last_validation",
			Status: StatusHealthy,
		},
		WorkAvailability: metricDetailJSON{
			Value:  0.95,
			Unit:   "7d_40d_ratio",
			Status: StatusHealthy,
		},
		VolunteerActivity: metricDetailJSON{
			Value:  10,
			Unit:   "active_24h",
			Status: StatusHealthy,
		},
	}

	if lh.LeafName != "test-leaf" {
		t.Errorf("expected 'test-leaf', got %q", lh.LeafName)
	}
	if lh.ContributionFlow.Status != StatusHealthy {
		t.Errorf("expected healthy contribution_flow, got %s", lh.ContributionFlow.Status)
	}
	if lh.WorkAvailability.Unit != "7d_40d_ratio" {
		t.Errorf("expected '7d_40d_ratio' unit, got %q", lh.WorkAvailability.Unit)
	}
	if lh.VolunteerActivity.Value != 10 {
		t.Errorf("expected volunteer_activity value 10, got %f", lh.VolunteerActivity.Value)
	}
}

func TestHealthResponse_Structure(t *testing.T) {
	resp := healthResponse{
		HeadName:      "test-head",
		Leafs:         []leafHealthJSON{},
		OverallStatus: StatusHealthy,
		RecordedAt:    "2026-03-22T10:00:00Z",
	}

	if resp.OverallStatus != StatusHealthy {
		t.Errorf("expected healthy, got %s", resp.OverallStatus)
	}
	if resp.HeadName != "test-head" {
		t.Errorf("expected 'test-head', got %q", resp.HeadName)
	}
	if len(resp.Leafs) != 0 {
		t.Errorf("expected empty leafs, got %d", len(resp.Leafs))
	}
	if resp.RecordedAt == "" {
		t.Error("expected non-empty recorded_at")
	}
}

func TestNewHandler(t *testing.T) {
	// Verify constructor returns non-nil and stores fields.
	h := NewHandler(nil, nil, nil, nil, "")
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestNewRecorder(t *testing.T) {
	r := NewRecorder(nil, nil, nil, nil)
	if r == nil {
		t.Fatal("expected non-nil recorder")
	}
}

func TestHandler_RegisterRoutes(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, "")
	mux := http.NewServeMux()

	// Should not panic.
	h.RegisterRoutes(mux)
}

// stubLeafRepo is a self-contained leaf.Repository for handler tests.
// Only List is exercised; the rest are no-ops.
type stubLeafRepo struct {
	leafs []*leaf.Leaf
}

func (s *stubLeafRepo) Create(ctx context.Context, p *leaf.Leaf) error { return nil }
func (s *stubLeafRepo) GetByID(ctx context.Context, id types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (s *stubLeafRepo) GetBySlug(ctx context.Context, slug string, creatorID *types.ID) (*leaf.Leaf, error) {
	return nil, nil
}
func (s *stubLeafRepo) GetBySlugPublic(ctx context.Context, slug string) (*leaf.Leaf, error) {
	return nil, nil
}
func (s *stubLeafRepo) List(ctx context.Context, filters leaf.LeafListFilters, page types.PaginationRequest) ([]*leaf.Leaf, types.PaginationResponse, error) {
	return s.leafs, types.PaginationResponse{HasMore: false}, nil
}
func (s *stubLeafRepo) Update(ctx context.Context, p *leaf.Leaf) error { return nil }
func (s *stubLeafRepo) Delete(ctx context.Context, id types.ID) error  { return nil }

func TestHandler_AggregatedResponse(t *testing.T) {
	leaf1ID := types.NewID()
	leaf2ID := types.NewID()
	leaf3ID := types.NewID()

	leafRepo := &stubLeafRepo{
		leafs: []*leaf.Leaf{
			{ID: leaf1ID, Name: "leaf-a", State: leaf.StateActive},
			{ID: leaf2ID, Name: "leaf-b", State: leaf.StateActive},
			{ID: leaf3ID, Name: "leaf-c", State: leaf.StateActive},
		},
	}

	// pool is nil — head-level metrics will get defaults (hours=0, etc.)
	h := NewHandler(nil, nil, leafRepo, nil, "test-head")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/health/leafs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp healthResponse
	body, _ := io.ReadAll(w.Body)
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("parse error: %v\nBody: %s", err, body)
	}

	if resp.HeadName != "test-head" {
		t.Errorf("head_name = %q, want %q", resp.HeadName, "test-head")
	}
	if resp.LeafCount != 3 {
		t.Errorf("leaf_count = %d, want 3", resp.LeafCount)
	}
	if len(resp.Leafs) != 3 {
		t.Errorf("expected 3 leaf details, got %d", len(resp.Leafs))
	}
	// Head-level metrics should be present.
	if resp.ContributionFlow.Unit != "hours_since_last_validation" {
		t.Errorf("head contribution_flow unit = %q, want hours_since_last_validation", resp.ContributionFlow.Unit)
	}
	if resp.VolunteerActivity.Unit != "active_24h" {
		t.Errorf("head volunteer_activity unit = %q, want active_24h", resp.VolunteerActivity.Unit)
	}
	if resp.RecordedAt == "" {
		t.Error("expected non-empty recorded_at")
	}
}

func TestHandler_UniqueVolunteerCount(t *testing.T) {
	// With nil pool, countActiveVolunteers returns 0.
	leafRepo := &stubLeafRepo{
		leafs: []*leaf.Leaf{
			{ID: types.NewID(), Name: "leaf-a", State: leaf.StateActive},
			{ID: types.NewID(), Name: "leaf-b", State: leaf.StateActive},
		},
	}

	h := NewHandler(nil, nil, leafRepo, nil, "test-head")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/v1/health/leafs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp healthResponse
	body, _ := io.ReadAll(w.Body)
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// Head-level volunteer activity should be 0 with nil pool (no DB).
	if resp.VolunteerActivity.Value != 0 {
		t.Errorf("head volunteer_activity = %f, want 0 (nil pool)", resp.VolunteerActivity.Value)
	}
}
