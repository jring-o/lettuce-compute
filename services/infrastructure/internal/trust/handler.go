package trust

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
)

// Caller carries the minimal authorization fact the trust admin handlers need: whether
// the authenticated principal is a head operator (an ADMIN API key). It is injected into
// the request context by the server router AFTER authentication, so the trust package can
// enforce admin-only access WITHOUT importing the server package (which would be an import
// cycle). This mirrors how the leaf package receives leaf.Viewer.
type Caller struct {
	IsAdmin bool
}

type callerContextKey struct{}

// WithCaller injects the authenticated caller's authorization facts into ctx. The server
// router calls this after resolving the API key; the handlers read it via requireAdmin.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, c)
}

// callerFromContext extracts the injected caller. Absent caller (no WithCaller upstream)
// yields the zero value — IsAdmin=false — so the trust API fails closed.
func callerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(callerContextKey{}).(Caller)
	return c
}

// Handler serves the operator-only trust administration endpoints. Every method
// re-checks admin authorization via requireAdmin against the router-injected Caller, so
// the operator-only invariant is enforced in the handler itself (not only at the route),
// and is unit-testable without the server package.
type Handler struct {
	repo   Repository
	logger *slog.Logger
}

// NewHandler creates a new trust admin Handler.
func NewHandler(repo Repository, logger *slog.Logger) *Handler {
	return &Handler{repo: repo, logger: logger}
}

// requireAdmin writes a 403 and returns false unless the injected caller is an admin. The
// trust admin API is operator-only: a plain researcher (non-admin) is rejected.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !callerFromContext(r.Context()).IsAdmin {
		apierror.WriteError(w, apierror.Forbidden("admin privileges required"))
		return false
	}
	return true
}

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

// setRequest is the body of POST /api/v1/admin/trust: seed or correct a subject's score.
type setRequest struct {
	Subject string `json:"subject"`
	Score   int    `json:"score"`
}

// slashRequest is the body of POST /api/v1/admin/trust/slash.
type slashRequest struct {
	Subject string `json:"subject"`
}

// HandleSet handles POST /api/v1/admin/trust. It is the operator's trust bootstrap: the
// accrual rule ("+1 only when corroborated by an already-trusted subject") is
// deliberately circular until the operator seeds the first trusted subjects on their own
// head, which this endpoint does. Sets score directly; clean_units is left untouched.
func (h *Handler) HandleSet(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req setRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	if strings.TrimSpace(req.Subject) == "" {
		apierror.WriteError(w, apierror.ValidationError("subject is required", nil))
		return
	}
	if req.Score < 0 {
		apierror.WriteError(w, apierror.ValidationError("score must be >= 0", nil))
		return
	}

	if err := h.repo.SetScore(r.Context(), req.Subject, req.Score); err != nil {
		l.Error("failed to set trust score", "error", err, "subject", req.Subject)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	entry, err := h.repo.Get(r.Context(), req.Subject)
	if err != nil {
		l.Error("failed to read trust entry after set", "error", err, "subject", req.Subject)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// HandleSlash handles POST /api/v1/admin/trust/slash: zero a subject's score and stamp
// slashed_at (clean_units retained for audit).
func (h *Handler) HandleSlash(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req slashRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	if strings.TrimSpace(req.Subject) == "" {
		apierror.WriteError(w, apierror.ValidationError("subject is required", nil))
		return
	}

	if err := h.repo.Slash(r.Context(), req.Subject); err != nil {
		l.Error("failed to slash trust subject", "error", err, "subject", req.Subject)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	entry, err := h.repo.Get(r.Context(), req.Subject)
	if err != nil {
		l.Error("failed to read trust entry after slash", "error", err, "subject", req.Subject)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// HandleGet handles GET /api/v1/admin/trust/{subject}: return the subject's entry, or 404
// when the subject has never been seeded or accrued.
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	subject := r.PathValue("subject")
	if strings.TrimSpace(subject) == "" {
		apierror.WriteError(w, apierror.ValidationError("subject is required", nil))
		return
	}

	entry, err := h.repo.Get(r.Context(), subject)
	if err != nil {
		l.Error("failed to get trust entry", "error", err, "subject", subject)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if entry == nil {
		apierror.WriteError(w, apierror.NotFound("trust subject", subject))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// HandleList handles GET /api/v1/admin/trust?limit=&offset=: list entries ordered by
// score DESC, subject ASC. limit defaults to 100 and is capped at 1000.
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	limit := defaultListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			apierror.WriteError(w, apierror.ValidationError("limit must be a positive integer", nil))
			return
		}
		if n > maxListLimit {
			n = maxListLimit
		}
		limit = n
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			apierror.WriteError(w, apierror.ValidationError("offset must be a non-negative integer", nil))
			return
		}
		offset = n
	}

	entries, err := h.repo.List(r.Context(), limit, offset)
	if err != nil {
		l.Error("failed to list trust entries", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if entries == nil {
		entries = []*Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
