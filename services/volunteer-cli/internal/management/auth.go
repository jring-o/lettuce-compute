package management

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// authMiddleware validates the Bearer token on every request.
func authMiddleware(token string, next http.Handler) http.Handler {
	tokenBytes := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid or missing authorization token")
			return
		}

		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid or missing authorization token")
			return
		}

		provided := []byte(parts[1])
		if subtle.ConstantTimeCompare(provided, tokenBytes) != 1 {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid or missing authorization token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// writeError writes a standard error JSON response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// writeJSON writes a JSON response with 200 OK.
func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
