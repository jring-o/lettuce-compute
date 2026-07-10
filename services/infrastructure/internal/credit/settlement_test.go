package credit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// Unit tests (no DB) for the export settlement gates and the pure anomaly arithmetic. The
// DB-backed maturation netting and the AnomalyChecker's SQL are covered by the integration
// suite (settlement_integration_test.go).

func unitLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubAnomalyChecker is an AnomalyCheck the handler can consult without a database.
type stubAnomalyChecker struct {
	verdict AnomalyVerdict
	err     error
	calls   int
}

func (s *stubAnomalyChecker) Check(_ context.Context) (AnomalyVerdict, error) {
	s.calls++
	return s.verdict, s.err
}

// --- Pure anomaly arithmetic (anomalyVerdict) ---

func TestAnomalyVerdict_BaselineDenominatorIsFixed30(t *testing.T) {
	// windowSum spread over any number of active days is always divided by the FIXED
	// 30-day calendar denominator, so a sparse window yields a deliberately low baseline.
	v := anomalyVerdict(0, 300, 10, 3.0)
	if v.Baseline != 10 { // 300 / 30
		t.Fatalf("Baseline = %v, want 10 (windowSum 300 / fixed 30 days)", v.Baseline)
	}
}

func TestAnomalyVerdict_ColdStartDoesNotArmBelow7Days(t *testing.T) {
	// Only 6 distinct baseline days: the breaker is inert regardless of how large today is.
	v := anomalyVerdict(1_000_000, 30, 6, 3.0)
	if v.Armed {
		t.Fatalf("Armed = true with 6 distinct days, want false (arming needs >= %d)", anomalyMinDistinctDays)
	}
	if v.Halted {
		t.Fatalf("Halted = true while unarmed, want false — a young/sparse head must never trip")
	}
}

func TestAnomalyVerdict_ArmsAtExactly7Days(t *testing.T) {
	v := anomalyVerdict(0, 30, anomalyMinDistinctDays, 3.0)
	if !v.Armed {
		t.Fatalf("Armed = false at exactly %d distinct days, want true", anomalyMinDistinctDays)
	}
}

func TestAnomalyVerdict_FactorEdge(t *testing.T) {
	// baseline = 30/30 = 1; factor 3 => threshold 3.0. Halt is strictly greater-than.
	if v := anomalyVerdict(3.0, 30, 7, 3.0); v.Halted {
		t.Fatalf("Halted = true at today == factor*baseline (3.0), want false (strict >)")
	}
	if v := anomalyVerdict(3.0001, 30, 7, 3.0); !v.Halted {
		t.Fatalf("Halted = false just above factor*baseline, want true")
	}
}

// --- exportGate: kill switch, anomaly halt, fail-open, inert default ---

func gateHandler(cfg *SettlementExportConfig) *VolunteerStatsHandler {
	return &VolunteerStatsHandler{logger: unitLogger(), settlement: cfg}
}

func TestExportGate_NilConfigIsInert(t *testing.T) {
	h := gateHandler(nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil)
	if h.exportGate(rr, req) {
		t.Fatal("exportGate returned true with nil config, want false (inert default)")
	}
	if rr.Header().Get(exportStatusHeader) != "" {
		t.Fatalf("nil config set %s header = %q, want empty", exportStatusHeader, rr.Header().Get(exportStatusHeader))
	}
}

func TestExportGate_KillSwitch(t *testing.T) {
	h := gateHandler(&SettlementExportConfig{ExportEnabled: false})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil)

	if !h.exportGate(rr, req) {
		t.Fatal("exportGate returned false with the kill switch off, want true")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if got := rr.Header().Get(exportStatusHeader); got != exportStatusKilled {
		t.Fatalf("%s = %q, want %q", exportStatusHeader, got, exportStatusKilled)
	}
	if got := rr.Header().Get("Retry-After"); got != exportRetryAfterSeconds {
		t.Fatalf("Retry-After = %q, want %q", got, exportRetryAfterSeconds)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "credit stats export is disabled by the operator" {
		t.Fatalf("error body = %q, unexpected", body.Error)
	}
}

func TestExportGate_AnomalyHalt(t *testing.T) {
	checker := &stubAnomalyChecker{verdict: AnomalyVerdict{Halted: true, Today: 900, Baseline: 100, Armed: true}}
	h := gateHandler(&SettlementExportConfig{
		ExportEnabled:      true,
		AnomalyHaltEnabled: true,
		AnomalyFactor:      3.0,
		AnomalyChecker:     checker,
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil)

	if !h.exportGate(rr, req) {
		t.Fatal("exportGate returned false on a halted verdict, want true")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if got := rr.Header().Get(exportStatusHeader); got != exportStatusAnomaly {
		t.Fatalf("%s = %q, want %q", exportStatusHeader, got, exportStatusAnomaly)
	}
	if got := rr.Header().Get("Retry-After"); got != exportRetryAfterSeconds {
		t.Fatalf("Retry-After = %q, want %q", got, exportRetryAfterSeconds)
	}
	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// The body names today's total, the baseline, and the factor so the "page" is actionable.
	for _, k := range []string{"error", "today", "baseline", "factor"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("anomaly body missing %q key: %v", k, body)
		}
	}
	if body["factor"].(float64) != 3.0 {
		t.Fatalf("factor = %v, want 3.0", body["factor"])
	}
}

func TestExportGate_AnomalyCheckerErrorFailsOpen(t *testing.T) {
	checker := &stubAnomalyChecker{err: errors.New("db down")}
	h := gateHandler(&SettlementExportConfig{
		ExportEnabled:      true,
		AnomalyHaltEnabled: true,
		AnomalyChecker:     checker,
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil)

	// A checker outage must NOT freeze the export: the gate fails open (serves).
	if h.exportGate(rr, req) {
		t.Fatal("exportGate returned true on a checker error, want false (fail open)")
	}
	if rr.Header().Get(exportStatusHeader) != "" {
		t.Fatalf("fail-open path set %s header, want none", exportStatusHeader)
	}
}

func TestExportGate_AnomalyNotHaltedServes(t *testing.T) {
	checker := &stubAnomalyChecker{verdict: AnomalyVerdict{Halted: false, Armed: true}}
	h := gateHandler(&SettlementExportConfig{
		ExportEnabled:      true,
		AnomalyHaltEnabled: true,
		AnomalyChecker:     checker,
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil)

	if h.exportGate(rr, req) {
		t.Fatal("exportGate returned true on a non-halted verdict, want false")
	}
	if checker.calls != 1 {
		t.Fatalf("checker consulted %d times, want 1", checker.calls)
	}
}

func TestExportGate_AnomalyDisabledSkipsChecker(t *testing.T) {
	checker := &stubAnomalyChecker{verdict: AnomalyVerdict{Halted: true, Armed: true}}
	h := gateHandler(&SettlementExportConfig{
		ExportEnabled:      true,
		AnomalyHaltEnabled: false, // halt not armed: the checker must not be consulted
		AnomalyChecker:     checker,
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/volunteers/stats", nil)

	if h.exportGate(rr, req) {
		t.Fatal("exportGate returned true with the anomaly halt disabled, want false")
	}
	if checker.calls != 0 {
		t.Fatalf("checker consulted %d times with halt disabled, want 0", checker.calls)
	}
}

// --- Kill switch wired through the routed handlers, on BOTH gated endpoints ---

func TestKillSwitch_BothEndpoints503(t *testing.T) {
	// The gate short-circuits before any query, so a nil pool/repos is safe here — the
	// point is to prove the wiring gates BOTH public credit-stats surfaces (audit F5).
	h := NewVolunteerStatsHandler(nil, nil, nil, nil, nil, unitLogger()).
		WithSettlement(&SettlementExportConfig{ExportEnabled: false})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	paths := []string{
		"/api/v1/volunteers/stats",
		"/api/v1/volunteers/" + types.NewID().String() + "/stats",
	}
	for _, p := range paths {
		req := httptest.NewRequest("GET", p, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", p, rr.Code)
		}
		if got := rr.Header().Get(exportStatusHeader); got != exportStatusKilled {
			t.Errorf("%s: %s = %q, want %q", p, exportStatusHeader, got, exportStatusKilled)
		}
		if got := rr.Header().Get("Retry-After"); got != exportRetryAfterSeconds {
			t.Errorf("%s: Retry-After = %q, want %q", p, got, exportRetryAfterSeconds)
		}
	}
}
