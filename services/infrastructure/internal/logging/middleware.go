package logging

import (
	"net/http"

	"github.com/google/uuid"
)

// RequestIDMiddleware generates a UUID v4 request ID (or reads from the
// X-Request-ID header if present), stores it in the request context, and
// sets the X-Request-ID response header.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		ctx := WithRequestID(r.Context(), requestID)
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
