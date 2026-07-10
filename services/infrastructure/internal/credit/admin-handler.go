package credit

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

const (
	defaultAdjustmentListLimit = 100
	maxAdjustmentListLimit     = 1000
	maxReasonLength            = 64
)

// AdminHandler serves the operator-only credit-settlement endpoints (manual clawback and the
// per-volunteer adjustment list). Every method re-checks admin authorization via requireAdmin
// against the router-injected Caller, so the operator-only invariant is enforced in the
// handler itself — the router's authAdmin wrapper only authenticates and injects the caller;
// a handler wrapped in it WITHOUT this in-handler check would be fail-open. This mirrors
// trust.Handler verbatim in shape.
type AdminHandler struct {
	adjRepo    AdjustmentsRepository
	ledgerRepo Repository
	logger     *slog.Logger
}

// NewAdminHandler creates a new credit admin Handler.
func NewAdminHandler(adjRepo AdjustmentsRepository, ledgerRepo Repository, logger *slog.Logger) *AdminHandler {
	return &AdminHandler{adjRepo: adjRepo, ledgerRepo: ledgerRepo, logger: logger}
}

// requireAdmin writes a 403 and returns false unless the injected caller is an admin. The
// credit settlement API is operator-only: a plain researcher (non-admin) is rejected, as is
// an anonymous caller (no caller injected → zero value → IsAdmin=false, fails closed).
func (h *AdminHandler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !callerFromContext(r.Context()).IsAdmin {
		apierror.WriteError(w, apierror.Forbidden("admin privileges required"))
		return false
	}
	return true
}

// clawbackRequest is the body of POST /api/v1/admin/credit/adjustments. Exactly one of
// result_id / ledger_entry_id identifies the grant; amount is the POSITIVE magnitude to claw
// back (the repository negates it), and its absence means "the full remaining net".
type clawbackRequest struct {
	ResultID      *string  `json:"result_id"`
	LedgerEntryID *string  `json:"ledger_entry_id"`
	Amount        *float64 `json:"amount"`
	Reason        string   `json:"reason"`
	Note          string   `json:"note"`
}

// HandleClawback handles POST /api/v1/admin/credit/adjustments: append a compensating
// negative adjustment against one credit_ledger entry. Full-cancel (amount omitted) is the
// expected use; a partial magnitude is allowed. The ledger stays append-only.
func (h *AdminHandler) HandleClawback(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req clawbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	// Exactly one target id (reject both / neither).
	hasResult := req.ResultID != nil && strings.TrimSpace(*req.ResultID) != ""
	hasEntry := req.LedgerEntryID != nil && strings.TrimSpace(*req.LedgerEntryID) != ""
	if hasResult == hasEntry {
		apierror.WriteError(w, apierror.ValidationError(
			"exactly one of result_id or ledger_entry_id is required", nil))
		return
	}

	// amount, when present, is the positive magnitude the server negates. Reject
	// non-positive and non-finite values; absent means "full remaining", computed inside the
	// repository transaction.
	if req.Amount != nil {
		a := *req.Amount
		if math.IsNaN(a) || math.IsInf(a, 0) || a <= 0 {
			apierror.WriteError(w, apierror.ValidationError(
				"amount must be a positive, finite number", nil))
			return
		}
	}

	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		apierror.WriteError(w, apierror.ValidationError("reason is required", nil))
		return
	}
	if len(reason) > maxReasonLength {
		apierror.WriteError(w, apierror.ValidationError(
			"reason must be at most 64 characters", nil))
		return
	}

	// Resolve the target ledger entry id. A result_id is resolved to its ledger entry via
	// GetByResultID; a result with no grant returns the repository's NotFound → 404.
	var entryID types.ID
	if hasEntry {
		id, err := types.ParseID(strings.TrimSpace(*req.LedgerEntryID))
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("ledger_entry_id is not a valid id", nil))
			return
		}
		entryID = id
	} else {
		resultID, err := types.ParseID(strings.TrimSpace(*req.ResultID))
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("result_id is not a valid id", nil))
			return
		}
		entry, err := h.ledgerRepo.GetByResultID(r.Context(), resultID)
		if err != nil {
			apierror.WriteError(w, apierror.FromError(err))
			return
		}
		entryID = entry.ID
	}

	adj, err := h.adjRepo.Clawback(r.Context(), entryID, req.Amount, reason, strings.TrimSpace(req.Note), AdjustmentByOperator)
	if err != nil {
		switch {
		case errors.Is(err, ErrAdjustmentExhausted):
			apierror.WriteError(w, apierror.Conflict("ledger entry already fully adjusted", nil))
		case errors.Is(err, ErrAdjustmentOvershoot):
			apierror.WriteError(w, apierror.Conflict("amount exceeds the entry's remaining credit", nil))
		default:
			apierror.WriteError(w, apierror.FromError(err))
		}
		return
	}

	l.Info("credit clawback recorded",
		"ledger_entry_id", adj.LedgerEntryID,
		"volunteer_id", adj.VolunteerID,
		"amount", adj.Amount,
		"reason", adj.Reason,
	)
	writeJSON(w, http.StatusCreated, adj)
}

// HandleListAdjustments handles GET /api/v1/admin/credit/adjustments?volunteer_id=&limit=&offset=:
// list one volunteer's adjustments, newest first. limit defaults to 100 and is capped at 1000.
func (h *AdminHandler) HandleListAdjustments(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	l := logging.LoggerFromContext(r.Context(), h.logger)

	raw := strings.TrimSpace(r.URL.Query().Get("volunteer_id"))
	if raw == "" {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id is required", nil))
		return
	}
	volunteerID, err := types.ParseID(raw)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("volunteer_id is not a valid id", nil))
		return
	}

	limit := defaultAdjustmentListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			apierror.WriteError(w, apierror.ValidationError("limit must be a positive integer", nil))
			return
		}
		if n > maxAdjustmentListLimit {
			n = maxAdjustmentListLimit
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

	adjustments, err := h.adjRepo.ListByVolunteer(r.Context(), volunteerID, limit, offset)
	if err != nil {
		l.Error("failed to list credit adjustments", "error", err, "volunteer_id", volunteerID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}
	if adjustments == nil {
		adjustments = []*Adjustment{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": adjustments})
}
