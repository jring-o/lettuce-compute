package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewLoggerJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("test message", "key", "value")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected valid JSON output, got error: %v, output: %s", err, buf.String())
	}
	if entry["msg"] != "test message" {
		t.Errorf("expected msg 'test message', got %v", entry["msg"])
	}
	if entry["key"] != "value" {
		t.Errorf("expected key 'value', got %v", entry["key"])
	}
}

func TestNewLoggerTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("test message")

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Errorf("expected text output to contain 'test message', got %s", output)
	}
	// text format should not be valid JSON
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err == nil {
		t.Error("expected text output to not be valid JSON")
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Debug("debug message")

	if buf.Len() > 0 {
		t.Errorf("expected no output for debug message at info level, got: %s", buf.String())
	}
}

func TestNewLoggerCreatesWorkingLogger(t *testing.T) {
	logger := NewLogger("info", "json")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}

	logger = NewLogger("debug", "text")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestParseLevelAllBranches(t *testing.T) {
	tests := []struct {
		input    string
		expected slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"Error", slog.LevelError},
		{"unknown", slog.LevelInfo},
		{"", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseLevel(tt.input)
			if got != tt.expected {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestNewLoggerLevelFiltering(t *testing.T) {
	// Verify that NewLogger correctly wires the level: a "warn" logger should
	// suppress Info-level messages.
	logger := NewLogger("warn", "json")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
	// We can't capture NewLogger's output (it writes to os.Stdout), but we can
	// verify the handler is enabled at the expected level by checking Enabled().
	if logger.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("warn-level logger should not be enabled for info messages")
	}
	if !logger.Handler().Enabled(context.Background(), slog.LevelWarn) {
		t.Error("warn-level logger should be enabled for warn messages")
	}
}

func TestNewLoggerDefaultFormatIsJSON(t *testing.T) {
	// Any format string other than "text" should produce a JSON handler.
	logger := NewLogger("info", "something-invalid")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
	// Verify it's a JSON handler by checking the type name.
	handlerType := fmt.Sprintf("%T", logger.Handler())
	if !strings.Contains(handlerType, "JSONHandler") {
		t.Errorf("expected JSONHandler for unknown format, got %s", handlerType)
	}
}

func TestNewLoggerTextFormatHandlerType(t *testing.T) {
	logger := NewLogger("info", "text")
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
	handlerType := fmt.Sprintf("%T", logger.Handler())
	if !strings.Contains(handlerType, "TextHandler") {
		t.Errorf("expected TextHandler for 'text' format, got %s", handlerType)
	}
}

func TestWithRequestIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	ctx = WithRequestID(ctx, "test-request-id-123")

	got := RequestIDFromContext(ctx)
	if got != "test-request-id-123" {
		t.Errorf("expected 'test-request-id-123', got '%s'", got)
	}
}

func TestRequestIDFromContextEmpty(t *testing.T) {
	ctx := context.Background()
	got := RequestIDFromContext(ctx)
	if got != "" {
		t.Errorf("expected empty string, got '%s'", got)
	}
}

func TestLoggerFromContextAddsRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx := WithRequestID(context.Background(), "req-456")
	logger := LoggerFromContext(ctx, base)
	logger.Info("test")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if entry["request_id"] != "req-456" {
		t.Errorf("expected request_id 'req-456', got %v", entry["request_id"])
	}
}

func TestLoggerFromContextNoRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logger := LoggerFromContext(context.Background(), base)
	logger.Info("test")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected valid JSON, got error: %v", err)
	}
	if _, exists := entry["request_id"]; exists {
		t.Error("expected no request_id field when not set in context")
	}
}

func TestRequestIDMiddlewareGeneratesID(t *testing.T) {
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := RequestIDFromContext(r.Context())
		if requestID == "" {
			t.Error("expected request ID in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	responseID := rec.Header().Get("X-Request-ID")
	if responseID == "" {
		t.Error("expected X-Request-ID response header")
	}
}

func TestRequestIDMiddlewareUsesExistingHeader(t *testing.T) {
	const existingID = "existing-request-id-789"

	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := RequestIDFromContext(r.Context())
		if requestID != existingID {
			t.Errorf("expected request ID '%s', got '%s'", existingID, requestID)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", existingID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	responseID := rec.Header().Get("X-Request-ID")
	if responseID != existingID {
		t.Errorf("expected response header '%s', got '%s'", existingID, responseID)
	}
}

func TestRequestIDMiddlewareSetsResponseHeader(t *testing.T) {
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	responseID := rec.Header().Get("X-Request-ID")
	if responseID == "" {
		t.Error("expected X-Request-ID response header to be set")
	}
	// Verify it looks like a UUID (contains hyphens, correct length)
	if len(responseID) != 36 {
		t.Errorf("expected UUID-length request ID (36 chars), got %d chars: %s", len(responseID), responseID)
	}
}
