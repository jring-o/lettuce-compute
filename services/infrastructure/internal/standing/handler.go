package standing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// Caller carries the minimal authorization fact the standing admin handlers need: whether
// the authenticated principal is a head operator (an ADMIN API key). It is injected into
// the request context by the server router AFTER authentication, so the standing package can
// enforce admin-only access WITHOUT importing the server package (which would be an import
// cycle). This mirrors trust.Caller, and the router injects both from one wrapper.
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
// yields the zero value — IsAdmin=false — so the standing API fails closed.
func callerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(callerContextKey{}).(Caller)
	return c
}

// Handler serves the operator-only account-standing administration endpoints. Every method
// re-checks admin authorization via requireAdmin against the router-injected Caller, so the
// operator-only invariant is enforced in the handler itself (not only at the route), and is
// unit-testable without the server package.
type Handler struct {
	repo   Repository
	logger *slog.Logger
}

// NewHandler creates a new standing admin Handler.
func NewHandler(repo Repository, logger *slog.Logger) *Handler {
	return &Handler{repo: repo, logger: logger}
}

// requireAdmin writes a 403 and returns false unless the injected caller is an admin. The
// standing admin API is operator-only: a plain researcher (non-admin) is rejected.
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

// setRequest is the body of POST /api/v1/admin/standing.
type setRequest struct {
	VolunteerID  string `json:"volunteer_id"`
	Standing     string `json:"standing"`
	BenchedUntil string `json:"benched_until"`
	Reason       string `json:"reason"`
}

// clearRequest is the body of POST /api/v1/admin/standing/clear.
type clearRequest struct {
	VolunteerID string `json:"volunteer_id"`
}

// getResponse is a standing Entry plus the EFFECTIVE standing resolved at read time. The
// stored standing can differ from what enforcement sees (an expired bench reads as
// PROBATION), so the read surface reports both: the raw row for provenance and the
// effective value the dispatch/validation layers act on.
type getResponse struct {
	*Entry
	EffectiveStanding string `json:"effective_standing"`
}

// HandleSet handles POST /api/v1/admin/standing: set a volunteer's standing as an operator
// action. This is the operator's direct lever — the reason BG-24b exists at all: a manually
// identified attacker must be stoppable regardless of what the automatic backpressure
// machine concludes, so the resulting row is OPERATOR-owned and is never auto-changed. A
// benched_until is honored only for BENCHED (indefinite when omitted); reason is optional.
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
	volunteerID, err := types.ParseID(strings.TrimSpace(req.VolunteerID))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id must be a valid UUID", nil))
		return
	}
	switch req.Standing {
	case volunteer.StandingOK, volunteer.StandingProbation, volunteer.StandingBenched:
	default:
		apierror.WriteError(w, apierror.ValidationError("standing must be one of OK, PROBATION, BENCHED", nil))
		return
	}
	var benchedUntil *time.Time
	if s := strings.TrimSpace(req.BenchedUntil); s != "" {
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			apierror.WriteError(w, apierror.ValidationError("benched_until must be an RFC3339 timestamp", nil))
			return
		}
		benchedUntil = &t
	}

	entry, err := h.repo.SetOperator(r.Context(), volunteerID, req.Standing, benchedUntil, req.Reason)
	if err != nil {
		l.Error("failed to set standing", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if entry == nil {
		apierror.WriteError(w, apierror.NotFound("volunteer", volunteerID.String()))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// HandleClear handles POST /api/v1/admin/standing/clear: return a volunteer to OK/AUTO with
// benched_until and reason cleared (the operator release path).
func (h *Handler) HandleClear(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req clearRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	volunteerID, err := types.ParseID(strings.TrimSpace(req.VolunteerID))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id must be a valid UUID", nil))
		return
	}

	entry, err := h.repo.Clear(r.Context(), volunteerID)
	if err != nil {
		l.Error("failed to clear standing", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if entry == nil {
		apierror.WriteError(w, apierror.NotFound("volunteer", volunteerID.String()))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// HandleGet handles GET /api/v1/admin/standing/{volunteer_id}: return the volunteer's
// standing entry (raw row plus the effective standing enforcement sees now), or 404 when
// the volunteer does not exist.
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	volunteerID, err := types.ParseID(strings.TrimSpace(r.PathValue("volunteer_id")))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id must be a valid UUID", nil))
		return
	}

	entry, err := h.repo.Get(r.Context(), volunteerID)
	if err != nil {
		l.Error("failed to get standing", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if entry == nil {
		apierror.WriteError(w, apierror.NotFound("volunteer", volunteerID.String()))
		return
	}
	writeJSON(w, http.StatusOK, getResponse{
		Entry:             entry,
		EffectiveStanding: volunteer.EffectiveStanding(entry.Standing, entry.BenchedUntil, time.Now()),
	})
}

// HandleList handles GET /api/v1/admin/standing?limit=&offset=: list the rows whose stored
// standing is not OK, newest change first. limit defaults to 100 and is capped at 1000.
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

	entries, err := h.repo.ListNonOK(r.Context(), limit, offset)
	if err != nil {
		l.Error("failed to list standing entries", "error", err)
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
