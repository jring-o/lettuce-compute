package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidationError(t *testing.T) {
	details := map[string]string{"field": "name", "reason": "too_short"}
	err := ValidationError("name is too short", details)

	if err.Code != "VALIDATION_ERROR" {
		t.Errorf("expected code VALIDATION_ERROR, got %s", err.Code)
	}
	if err.Message != "name is too short" {
		t.Errorf("expected message 'name is too short', got %s", err.Message)
	}
	if err.HTTPStatus != 400 {
		t.Errorf("expected status 400, got %d", err.HTTPStatus)
	}
	if err.Details == nil {
		t.Error("expected details to be set")
	}
}

func TestUnauthorized(t *testing.T) {
	err := Unauthorized("missing token")
	if err.Code != "UNAUTHORIZED" || err.HTTPStatus != 401 {
		t.Errorf("unexpected: code=%s, status=%d", err.Code, err.HTTPStatus)
	}
}

func TestForbidden(t *testing.T) {
	err := Forbidden("not allowed")
	if err.Code != "FORBIDDEN" || err.HTTPStatus != 403 {
		t.Errorf("unexpected: code=%s, status=%d", err.Code, err.HTTPStatus)
	}
}

func TestNotFound(t *testing.T) {
	err := NotFound("project", "abc-123")
	if err.Code != "NOT_FOUND" || err.HTTPStatus != 404 {
		t.Errorf("unexpected: code=%s, status=%d", err.Code, err.HTTPStatus)
	}
	if err.Message != "project not found: abc-123" {
		t.Errorf("unexpected message: %s", err.Message)
	}
}

func TestConflict(t *testing.T) {
	err := Conflict("invalid transition", map[string]string{"from": "active", "to": "draft"})
	if err.Code != "CONFLICT" || err.HTTPStatus != 409 {
		t.Errorf("unexpected: code=%s, status=%d", err.Code, err.HTTPStatus)
	}
}

func TestRateLimited(t *testing.T) {
	err := RateLimited(30)
	if err.Code != "RATE_LIMITED" || err.HTTPStatus != 429 {
		t.Errorf("unexpected: code=%s, status=%d", err.Code, err.HTTPStatus)
	}
	details, ok := err.Details.(map[string]int)
	if !ok {
		t.Fatal("expected details to be map[string]int")
	}
	if details["retry_after"] != 30 {
		t.Errorf("expected retry_after 30, got %d", details["retry_after"])
	}
}

func TestInternal(t *testing.T) {
	cause := fmt.Errorf("db connection failed")
	err := Internal("internal server error", cause)
	if err.Code != "INTERNAL_ERROR" || err.HTTPStatus != 500 {
		t.Errorf("unexpected: code=%s, status=%d", err.Code, err.HTTPStatus)
	}
	if err.Err != cause {
		t.Error("expected wrapped error to be the cause")
	}
}

func TestAPIErrorImplementsError(t *testing.T) {
	var err error = ValidationError("test", nil)
	if err.Error() == "" {
		t.Error("expected non-empty error string")
	}
}

func TestAPIErrorErrorStringWithWrappedError(t *testing.T) {
	cause := fmt.Errorf("connection refused")
	apiErr := Internal("database unavailable", cause)

	got := apiErr.Error()
	expected := "INTERNAL_ERROR: database unavailable: connection refused"
	if got != expected {
		t.Errorf("Error() = %q, want %q", got, expected)
	}
}

func TestAPIErrorErrorStringWithoutWrappedError(t *testing.T) {
	apiErr := NotFound("project", "abc")

	got := apiErr.Error()
	expected := "NOT_FOUND: project not found: abc"
	if got != expected {
		t.Errorf("Error() = %q, want %q", got, expected)
	}
}

func TestAPIErrorUnwrap(t *testing.T) {
	cause := fmt.Errorf("root cause")
	apiErr := Internal("failed", cause)

	if apiErr.Unwrap() != cause {
		t.Error("expected Unwrap to return the wrapped error")
	}
}

func TestErrorsIsAs(t *testing.T) {
	cause := fmt.Errorf("root cause")
	apiErr := Internal("failed", cause)

	// errors.Is should find the wrapped error
	if !errors.Is(apiErr, cause) {
		t.Error("expected errors.Is to find the wrapped cause")
	}

	// errors.As should work with APIError
	var target *APIError
	if !errors.As(apiErr, &target) {
		t.Error("expected errors.As to find *APIError")
	}
	if target.Code != "INTERNAL_ERROR" {
		t.Errorf("expected code INTERNAL_ERROR, got %s", target.Code)
	}
}

func TestWriteErrorJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	apiErr := NotFound("project", "xyz")

	WriteError(rec, apiErr)

	if rec.Code != 404 {
		t.Errorf("expected status 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}

	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected valid JSON response, got error: %v", err)
	}
	if resp.Error.Code != "NOT_FOUND" {
		t.Errorf("expected error code NOT_FOUND, got %s", resp.Error.Code)
	}
	if resp.Error.Message != "project not found: xyz" {
		t.Errorf("expected message 'project not found: xyz', got %s", resp.Error.Message)
	}
}

func TestWriteErrorSetsStatusCode(t *testing.T) {
	tests := []struct {
		name   string
		err    *APIError
		status int
	}{
		{"validation", ValidationError("bad", nil), 400},
		{"unauthorized", Unauthorized("no token"), 401},
		{"forbidden", Forbidden("denied"), 403},
		{"not found", NotFound("item", "1"), 404},
		{"conflict", Conflict("clash", nil), 409},
		{"rate limited", RateLimited(60), 429},
		{"internal", Internal("oops", nil), 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, tt.err)
			if rec.Code != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, rec.Code)
			}
		})
	}
}

func TestWriteErrorSetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, ValidationError("test", nil))

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got '%s'", ct)
	}
}

func TestFromErrorWithAPIError(t *testing.T) {
	original := NotFound("user", "42")
	result := FromError(original)
	if result != original {
		t.Error("expected FromError to return the same *APIError")
	}
}

func TestFromErrorWithGenericError(t *testing.T) {
	generic := fmt.Errorf("something broke")
	result := FromError(generic)

	if result.Code != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR, got %s", result.Code)
	}
	if result.HTTPStatus != 500 {
		t.Errorf("expected status 500, got %d", result.HTTPStatus)
	}
	if result.Err != generic {
		t.Error("expected wrapped error to be the original")
	}
}

func TestInternalDoesNotExposeWrappedError(t *testing.T) {
	cause := fmt.Errorf("secret database password leak")
	apiErr := Internal("internal server error", cause)

	rec := httptest.NewRecorder()
	WriteError(rec, apiErr)

	body := rec.Body.String()
	if strings.Contains(body, "secret database password leak") {
		t.Error("expected internal error details to NOT appear in serialized response")
	}

	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if resp.Error.Code != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR code in response")
	}
}

func TestWriteErrorMatchesAPIErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	details := map[string]string{"field": "email", "reason": "invalid"}
	WriteError(rec, ValidationError("invalid email", details))

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Must have top-level "error" key
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatal("expected top-level 'error' object")
	}

	// Must have code, message, details
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected code VALIDATION_ERROR, got %v", errObj["code"])
	}
	if errObj["message"] != "invalid email" {
		t.Errorf("expected message 'invalid email', got %v", errObj["message"])
	}
	detailsMap, ok := errObj["details"].(map[string]any)
	if !ok {
		t.Fatal("expected details object")
	}
	if detailsMap["field"] != "email" {
		t.Errorf("expected field 'email', got %v", detailsMap["field"])
	}

	// Must NOT have http_status or err in serialized response
	if _, exists := errObj["http_status"]; exists {
		t.Error("http_status should not be serialized")
	}
}
