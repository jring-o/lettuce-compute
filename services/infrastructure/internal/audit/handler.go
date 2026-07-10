package audit

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

const (
	defaultAuditsListLimit = 100
	maxAuditsListLimit     = 1000
	maxRunnerLabelLength   = 128
)

// AdminHandler serves the operator-only result-audit endpoints: the trusted-runner registry
// (register / deactivate / list) and the observe-only verdict read surface. Every method
// re-checks admin authorization via requireAdmin against the router-injected Caller, so the
// operator-only invariant is enforced in the handler itself — the router's authAdmin wrapper
// only authenticates and injects the caller; a handler wrapped in it WITHOUT this in-handler
// check would be fail-open. This mirrors credit.AdminHandler / trust.Handler verbatim in shape.
type AdminHandler struct {
	runnersRepo RunnersRepository
	auditsRepo  AuditsRepository
	// flaggedRepo is the flagged-leaves read view, derived from auditsRepo at construction
	// (the concrete PgxAuditsRepository satisfies both). Kept off the AuditsRepository
	// interface so the gRPC AuditService consumer is unaffected; nil only if a caller wired
	// an audits repo that does not implement the read (never in production).
	flaggedRepo FlaggedLeavesReader
	logger      *slog.Logger
}

// NewAdminHandler creates a new audit admin Handler.
func NewAdminHandler(runnersRepo RunnersRepository, auditsRepo AuditsRepository, logger *slog.Logger) *AdminHandler {
	flaggedRepo, _ := auditsRepo.(FlaggedLeavesReader)
	return &AdminHandler{
		runnersRepo: runnersRepo,
		auditsRepo:  auditsRepo,
		flaggedRepo: flaggedRepo,
		logger:      logger,
	}
}

// requireAdmin writes a 403 and returns false unless the injected caller is an admin. The
// audit registry/verdict API is operator-only: a plain researcher (non-admin) is rejected, as
// is an anonymous caller (no caller injected → zero value → IsAdmin=false, fails closed).
func (h *AdminHandler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !callerFromContext(r.Context()).IsAdmin {
		apierror.WriteError(w, apierror.Forbidden("admin privileges required"))
		return false
	}
	return true
}

// registerRunnerRequest is the body of POST /api/v1/admin/audit/runners.
type registerRunnerRequest struct {
	VolunteerID string `json:"volunteer_id"`
	Label       string `json:"label"`
	Note        string `json:"note"`
}

// HandleRegisterRunner handles POST /api/v1/admin/audit/runners: create (or reactivate +
// relabel) a trusted-runner registry row. An unknown volunteer id is a 400.
func (h *AdminHandler) HandleRegisterRunner(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req registerRunnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	volunteerID, err := types.ParseID(strings.TrimSpace(req.VolunteerID))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id is not a valid id", nil))
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		apierror.WriteError(w, apierror.ValidationError("label is required", nil))
		return
	}
	if len(label) > maxRunnerLabelLength {
		apierror.WriteError(w, apierror.ValidationError("label must be at most 128 characters", nil))
		return
	}

	runner, err := h.runnersRepo.Register(r.Context(), volunteerID, label, strings.TrimSpace(req.Note))
	if err != nil {
		if errors.Is(err, ErrUnknownVolunteer) {
			apierror.WriteError(w, apierror.ValidationError("unknown volunteer id", nil))
			return
		}
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	l.Info("trusted runner registered",
		"runner_id", runner.ID,
		"volunteer_id", runner.VolunteerID,
		"label", runner.Label,
	)
	writeJSON(w, http.StatusCreated, runner)
}

// deactivateRunnerRequest is the body of POST /api/v1/admin/audit/runners/deactivate.
type deactivateRunnerRequest struct {
	VolunteerID string `json:"volunteer_id"`
}

// HandleDeactivateRunner handles POST /api/v1/admin/audit/runners/deactivate: set active =
// false. A volunteer with no registry row is a 404. Rows are never deleted (audit trail).
func (h *AdminHandler) HandleDeactivateRunner(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req deactivateRunnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	volunteerID, err := types.ParseID(strings.TrimSpace(req.VolunteerID))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id is not a valid id", nil))
		return
	}

	if err := h.runnersRepo.Deactivate(r.Context(), volunteerID); err != nil {
		if errors.Is(err, ErrNotRegistered) {
			apierror.WriteError(w, apierror.NotFound("trusted_runner", volunteerID.String()))
			return
		}
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	l.Info("trusted runner deactivated", "volunteer_id", volunteerID)
	writeJSON(w, http.StatusOK, map[string]any{"volunteer_id": volunteerID, "active": false})
}

// HandleListRunners handles GET /api/v1/admin/audit/runners: the full registry, newest first.
func (h *AdminHandler) HandleListRunners(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	runners, err := h.runnersRepo.List(r.Context())
	if err != nil {
		l.Error("failed to list trusted runners", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if runners == nil {
		runners = []*Runner{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": runners})
}

// HandleListAudits handles GET /api/v1/admin/audit/results?status=&verdict=&leaf_id=&limit=:
// the observe-only verdict read surface, newest first. Enum params are validated; a garbage
// status / verdict / leaf_id / limit is a 400.
func (h *AdminHandler) HandleListAudits(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var f ListFilter

	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		if !isValidStatus(raw) {
			apierror.WriteError(w, apierror.ValidationError("status is not a valid audit status", nil))
			return
		}
		f.Status = Status(raw)
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("verdict")); raw != "" {
		if !isValidVerdict(raw) {
			apierror.WriteError(w, apierror.ValidationError("verdict is not a valid audit verdict", nil))
			return
		}
		f.Verdict = Verdict(raw)
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("leaf_id")); raw != "" {
		leafID, err := types.ParseID(raw)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("leaf_id is not a valid id", nil))
			return
		}
		f.LeafID = &leafID
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("enforcement_state")); raw != "" {
		if !isValidEnforcementState(raw) {
			apierror.WriteError(w, apierror.ValidationError("enforcement_state is not a valid value", nil))
			return
		}
		f.EnforcementState = EnforcementState(raw)
	}

	f.Limit = defaultAuditsListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			apierror.WriteError(w, apierror.ValidationError("limit must be a positive integer", nil))
			return
		}
		if n > maxAuditsListLimit {
			n = maxAuditsListLimit
		}
		f.Limit = n
	}

	audits, err := h.auditsRepo.List(r.Context(), f)
	if err != nil {
		l.Error("failed to list result audits", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if audits == nil {
		audits = []*Audit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": audits})
}

// HandleFlaggedLeaves handles GET /api/v1/admin/audit/flagged-leaves: leaves with at least
// one enforced/contradicted/stalled ROOT audit, with per-state counts, the newest
// enforced_at, and the owner id (design doc §9.8). The flag is derived on read — no persisted
// column. Same operator-only dual-auth as the other audit admin routes.
func (h *AdminHandler) HandleFlaggedLeaves(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	if h.flaggedRepo == nil {
		l.Error("audits repo does not implement the flagged-leaves read")
		apierror.WriteError(w, apierror.Internal("flagged-leaves surface unavailable", nil))
		return
	}
	leaves, err := h.flaggedRepo.FlaggedLeaves(r.Context())
	if err != nil {
		l.Error("failed to list flagged leaves", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if leaves == nil {
		leaves = []FlaggedLeaf{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": leaves})
}

func isValidStatus(s string) bool {
	switch Status(s) {
	case StatusQueued, StatusClaimed, StatusCompleted, StatusExpired:
		return true
	default:
		return false
	}
}

func isValidEnforcementState(s string) bool {
	switch EnforcementState(s) {
	case EnforcementNone, EnforcementAwaitingConfirmation, EnforcementEnforced,
		EnforcementContradicted, EnforcementStalled:
		return true
	default:
		return false
	}
}

func isValidVerdict(v string) bool {
	switch Verdict(v) {
	case VerdictMatch, VerdictMismatch, VerdictInconclusive:
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
