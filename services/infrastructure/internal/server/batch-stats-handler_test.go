package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

func TestBatchStatsHandlerValidation(t *testing.T) {
	// handler with nil engine — validation errors should fire before engine is called
	handler := handleBatchStats(nil)

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "missing ids parameter",
			query:      "",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "ids parameter is required",
		},
		{
			name:       "empty ids value",
			query:      "ids=",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "ids parameter is required",
		},
		{
			name:       "only commas",
			query:      "ids=,,,",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "ids parameter must contain at least one valid UUID",
		},
		{
			name:       "invalid UUID format",
			query:      "ids=not-a-uuid",
			wantStatus: http.StatusBadRequest,
			wantMsg:    "invalid UUID",
		},
		{
			name:       "too many IDs",
			query:      "ids=" + strings.Repeat(types.NewID().String()+",", 201) + types.NewID().String(),
			wantStatus: http.StatusBadRequest,
			wantMsg:    "maximum 200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/api/v1/leafs/stats/batch"
			if tt.query != "" {
				url += "?" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			var errResp struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if !strings.Contains(errResp.Error.Message, tt.wantMsg) {
				t.Errorf("error message %q should contain %q", errResp.Error.Message, tt.wantMsg)
			}
		})
	}
}
