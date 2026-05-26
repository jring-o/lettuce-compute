package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apikey"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// AuthUser represents an authenticated user extracted from an API key.
type AuthUser struct {
	ID       types.ID
	Email    string
	Username string
	Role     string // "USER" or "ADMIN"
}

type authUserContextKey struct{}

// ContextWithUser injects an authenticated user into the request context.
func ContextWithUser(ctx context.Context, user *AuthUser) context.Context {
	return context.WithValue(ctx, authUserContextKey{}, user)
}

// UserFromContext extracts the authenticated user from the request context.
// Returns nil if no user is authenticated (anonymous request).
func UserFromContext(ctx context.Context) *AuthUser {
	user, _ := ctx.Value(authUserContextKey{}).(*AuthUser)
	return user
}

// authMiddleware extracts and validates Bearer tokens from the Authorization header.
// If no header is present, the request continues as anonymous.
// If a token is present but invalid, returns 401.
func authMiddleware(next http.Handler, apiKeyRepo apikey.Repository, adminKey string, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			// Anonymous request — continue without user.
			next.ServeHTTP(w, r)
			return
		}

		token, ok := parseBearerToken(authHeader)
		if !ok {
			// Non-Bearer scheme (e.g., Ed25519) — pass through as anonymous.
			// Handler-level auth (ed25519AuthRequired) will handle these.
			next.ServeHTTP(w, r)
			return
		}

		l := logging.LoggerFromContext(r.Context(), logger)

		// Check database API keys first.
		keyHash := apikey.HashKey(token)
		ak, err := apiKeyRepo.GetByHash(r.Context(), keyHash)
		if err != nil {
			l.Error("failed to look up API key", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		if ak != nil {
			// Found a valid DB key — resolve user.
			// Fire-and-forget last_used_at update.
			go func() {
				if updateErr := apiKeyRepo.UpdateLastUsed(context.Background(), ak.ID); updateErr != nil {
					l.Warn("failed to update API key last_used_at", "error", updateErr, "key_id", ak.ID)
				}
			}()

			user := &AuthUser{
				ID:     ak.UserID,
				Email:  "", // Not stored on API key; would need a user lookup.
				Role:   "USER",
			}
			ctx := ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Not in DB — check admin env var key (constant-time comparison).
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminKey)) == 1 {
			user := &AuthUser{
				ID:       types.NilID(),
				Email:    "admin@localhost",
				Username: "admin",
				Role:     "ADMIN",
			}
			ctx := ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// No match anywhere.
		apierror.WriteError(w, apierror.Unauthorized("invalid API key"))
	})
}

// leafViewer wraps an anonymous-friendly leaf READ handler. When the global
// authMiddleware resolved a user, it injects a leaf.Viewer (carrying the user
// id, admin flag, and authed marker) into the request context so the leaf
// handlers can enforce per-leaf visibility WITHOUT importing the server package
// (which would create an import cycle). No user -> no viewer -> the handler
// treats the request as anonymous (PUBLIC/UNLISTED only).
func leafViewer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := UserFromContext(r.Context()); u != nil {
			ctx := leaf.WithViewer(r.Context(), leaf.Viewer{
				UserID:  u.ID,
				IsAdmin: u.Role == "ADMIN",
				Authed:  true,
			})
			r = r.WithContext(ctx)
		}
		next(w, r)
	}
}

// requireAuth wraps a handler to require an authenticated user in the context.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if UserFromContext(r.Context()) == nil {
			apierror.WriteError(w, apierror.Unauthorized("authentication required"))
			return
		}
		next(w, r)
	}
}

// requireLeafOwnership wraps a handler to require the authenticated user
// to be the leaf creator or an admin. Assumes requireAuth already ran.
func requireLeafOwnership(next http.HandlerFunc, leafRepo leaf.Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		// Admins bypass ownership check.
		if user.Role == "ADMIN" {
			next(w, r)
			return
		}

		raw := r.PathValue("leaf_id")
		leafID, err := types.ParseID(raw)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
			return
		}

		l, err := leafRepo.GetByID(r.Context(), leafID)
		if err != nil {
			apierror.WriteError(w, apierror.FromError(err))
			return
		}

		// Compare creator ID with authenticated user.
		if l.CreatorID == nil || *l.CreatorID != user.ID {
			apierror.WriteError(w, apierror.Forbidden("you do not have permission to modify this leaf"))
			return
		}

		next(w, r)
	}
}

// parseBearerToken extracts the token from "Bearer <token>".
func parseBearerToken(header string) (string, bool) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}
