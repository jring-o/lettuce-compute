package logging

import (
	"context"
	"log/slog"
)

type contextKey int

const requestIDKey contextKey = iota

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

// RequestIDFromContext retrieves the request ID from the context.
// Returns an empty string if no request ID is set.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// LoggerFromContext returns a logger enriched with context fields (request ID).
// If no request ID is in the context, returns the base logger unchanged.
func LoggerFromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	requestID := RequestIDFromContext(ctx)
	if requestID == "" {
		return base
	}
	return base.With("request_id", requestID)
}
