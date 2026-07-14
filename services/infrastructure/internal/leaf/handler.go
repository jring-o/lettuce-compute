package leaf

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// LeafHandler handles HTTP requests for leaf CRUD operations.
type LeafHandler struct {
	repo   Repository
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewLeafHandler creates a new LeafHandler.
func NewLeafHandler(repo Repository, pool *pgxpool.Pool, logger *slog.Logger) *LeafHandler {
	return &LeafHandler{
		repo:   repo,
		pool:   pool,
		logger: logger,
	}
}

// RegisterRoutes registers the leaf read routes on the given mux.
//
// These GET routes are anonymous-friendly (no auth REQUIRED) but visibility is
// enforced per-leaf inside the handlers using the Viewer injected by the server
// package's leafViewer wrapper. The router wraps these handlers with leafViewer
// so the handlers can identify the caller; tests that call RegisterRoutes
// directly run with an anonymous viewer (PUBLIC/UNLISTED only).
func (h *LeafHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/leafs/{leaf_id}", h.handleGet)
	mux.HandleFunc("GET /api/v1/leafs", h.handleList)
}

// HandleGet handles GET /api/v1/leafs/{leaf_id} (exported for viewer wrapping).
func (h *LeafHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	h.handleGet(w, r)
}

// HandleList handles GET /api/v1/leafs (exported for viewer wrapping).
func (h *LeafHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	h.handleList(w, r)
}

// HandleCreate handles POST /api/v1/leafs.
func (h *LeafHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	h.handleCreate(w, r)
}

// HandleUpdate handles PUT /api/v1/leafs/{leaf_id} (exported for auth wrapping).
func (h *LeafHandler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	h.handleUpdate(w, r)
}

// HandleDelete handles DELETE /api/v1/leafs/{leaf_id} (exported for auth wrapping).
func (h *LeafHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	h.handleDelete(w, r)
}

// HandleActivate handles POST /api/v1/leafs/{leaf_id}/activate (exported for auth wrapping).
func (h *LeafHandler) HandleActivate(w http.ResponseWriter, r *http.Request) {
	h.handleActivate(w, r)
}

// HandlePause handles POST /api/v1/leafs/{leaf_id}/pause (exported for auth wrapping).
func (h *LeafHandler) HandlePause(w http.ResponseWriter, r *http.Request) {
	h.handlePause(w, r)
}

// HandleResume handles POST /api/v1/leafs/{leaf_id}/resume (exported for auth wrapping).
func (h *LeafHandler) HandleResume(w http.ResponseWriter, r *http.Request) {
	h.handleResume(w, r)
}

// HandleArchive handles POST /api/v1/leafs/{leaf_id}/archive (exported for auth wrapping).
func (h *LeafHandler) HandleArchive(w http.ResponseWriter, r *http.Request) {
	h.handleArchive(w, r)
}

// HandleConfigure handles POST /api/v1/leafs/{leaf_id}/configure (exported for auth wrapping).
func (h *LeafHandler) HandleConfigure(w http.ResponseWriter, r *http.Request) {
	h.handleConfigure(w, r)
}

// HandleGetDeprecated serves the deprecated GET /api/v1/projects/{leaf_id} route.
func (h *LeafHandler) HandleGetDeprecated(w http.ResponseWriter, r *http.Request) {
	h.handleGet(w, r)
}

// HandleListDeprecated serves the deprecated GET /api/v1/projects route.
func (h *LeafHandler) HandleListDeprecated(w http.ResponseWriter, r *http.Request) {
	h.handleList(w, r)
}

// handleCreate handles POST /api/v1/leafs.
func (h *LeafHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	var req CreateLeafRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	// Default visibility to PUBLIC if not specified.
	if req.Visibility == "" {
		req.Visibility = VisibilityPublic
	}

	// Bind creator_id to the authenticated caller (★BG-11d-write / R1.5). A
	// non-admin caller may NOT set creator_id from the request body — doing so
	// would let a direct USER-key caller mint a leaf owned by any user id,
	// forging the ownership root every authOwner check keys on. Their leaf is
	// always owned by themselves. An ADMIN may still create-on-behalf by
	// supplying an explicit creator_id (falling back to their own id). The
	// caller is read from the Viewer the route wrapper injects; absent it (a
	// misconfigured route), fail closed.
	v, ok := ViewerFromContext(r.Context())
	if !ok || (!v.Authed && !v.IsAdmin) {
		apierror.WriteError(w, apierror.Unauthorized("authentication required"))
		return
	}
	if v.IsAdmin {
		if req.CreatorID == nil {
			callerID := v.UserID
			req.CreatorID = &callerID
		}
	} else {
		callerID := v.UserID
		req.CreatorID = &callerID
	}

	p := &Leaf{
		Name:         req.Name,
		Description:  req.Description,
		ResearchArea: req.ResearchArea,
		State:        StateDraft,
		TaskPattern:  req.TaskPattern,
		IsOngoing:    req.IsOngoing,
		Visibility:   req.Visibility,
		CreatorID:    req.CreatorID,
	}

	if apiErr := ValidateMetadata(p); apiErr != nil {
		apierror.WriteError(w, apiErr)
		return
	}

	if err := h.repo.Create(r.Context(), p); err != nil {
		l.Error("failed to create leaf", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	writeJSON(w, http.StatusCreated, p)
}

// handleGet handles GET /api/v1/leafs/{leaf_id}.
// Accepts either a UUID or a slug.
func (h *LeafHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	raw := r.PathValue("leaf_id")
	id, err := types.ParseID(raw)
	if err != nil {
		// Not a UUID — try slug lookup.
		p, slugErr := h.repo.GetBySlugPublic(r.Context(), raw)
		if slugErr != nil {
			l.Error("failed to get leaf by slug", "error", slugErr, "slug", raw)
			apierror.WriteError(w, apierror.FromError(slugErr))
			return
		}
		// Enforce visibility: a PRIVATE leaf is only readable by its creator or
		// an admin. Return the same NOT_FOUND the repo emits for a missing leaf
		// so existence is not leaked to unauthorized callers.
		v, ok := ViewerFromContext(r.Context())
		if !canViewLeaf(p.Visibility, p.CreatorID, v, ok) {
			apierror.WriteError(w, apierror.NotFound("leaf", raw))
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}

	p, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		l.Error("failed to get leaf", "error", err, "leaf_id", id)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Enforce visibility: a PRIVATE leaf is only readable by its creator or an
	// admin. Return the same NOT_FOUND the repo emits for a missing leaf so
	// existence is not leaked to unauthorized callers.
	v, ok := ViewerFromContext(r.Context())
	if !canViewLeaf(p.Visibility, p.CreatorID, v, ok) {
		apierror.WriteError(w, apierror.NotFound("leaf", id.String()))
		return
	}

	writeJSON(w, http.StatusOK, p)
}

// handleList handles GET /api/v1/leafs.
func (h *LeafHandler) handleList(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)
	q := r.URL.Query()

	filters := LeafListFilters{
		Sort:  SortCreatedAt,
		Order: OrderDesc,
	}

	// Parse state filter.
	if v := q.Get("state"); v != "" {
		state := LeafState(v)
		filters.State = &state
	}

	// Parse creator_id filter.
	if v := q.Get("creator_id"); v != "" {
		id, err := types.ParseID(v)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid creator_id: must be a valid UUID", nil))
			return
		}
		filters.CreatorID = &id
	}

	// Parse research_area filter.
	if v := q.Get("research_area"); v != "" {
		filters.ResearchArea = &v
	}

	// Parse search filter.
	if v := q.Get("search"); v != "" {
		if len(v) > 200 {
			apierror.WriteError(w, apierror.ValidationError("search query must be 200 characters or fewer", nil))
			return
		}
		filters.Search = &v
	}

	// Parse sort.
	if v := q.Get("sort"); v != "" {
		switch v {
		case "created_at":
			filters.Sort = SortCreatedAt
		case "updated_at":
			filters.Sort = SortUpdatedAt
		case "name":
			filters.Sort = SortName
		default:
			apierror.WriteError(w, apierror.ValidationError(
				"invalid sort: must be one of created_at, updated_at, name", nil))
			return
		}
	}

	// Parse order. Accept case-insensitively: the SortOrder constants are
	// uppercase ("ASC"/"DESC") and the proto mirrors them, so a client that
	// copies the canonical form would otherwise be rejected (TODO #5).
	if v := q.Get("order"); v != "" {
		switch strings.ToLower(v) {
		case "asc":
			filters.Order = OrderAsc
		case "desc":
			filters.Order = OrderDesc
		default:
			apierror.WriteError(w, apierror.ValidationError(
				"invalid order: must be one of asc, desc", nil))
			return
		}
	}

	// Visibility scoping. Anyone may list PUBLIC leafs. A leaf's full
	// (all-visibility) list is restricted so that callers cannot enumerate
	// another user's PRIVATE/UNLISTED leafs via ?creator_id=<X>:
	//   - admin                              -> no restriction (sees everything)
	//   - creator_id == own authed UserID    -> no restriction (own leafs)
	//   - otherwise (anon, foreign creator,
	//     or no creator_id)                  -> PUBLIC only
	v, ok := ViewerFromContext(r.Context())
	switch {
	case ok && v.Authed && v.IsAdmin:
		// No visibility restriction for admins.
	case ok && v.Authed && filters.CreatorID != nil && *filters.CreatorID == v.UserID:
		// Owner listing their own leafs — no visibility restriction.
	default:
		vis := VisibilityPublic
		filters.Visibility = &vis
	}

	// Parse pagination.
	page := types.PaginationRequest{
		Cursor: q.Get("cursor"),
	}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil || limit < 1 || limit > types.MaxPageSize {
			apierror.WriteError(w, apierror.ValidationError(
				"invalid limit: must be an integer between 1 and 200", nil))
			return
		}
		page.PageSize = limit
	}

	leafs, pagination, err := h.repo.List(r.Context(), filters, page)
	if err != nil {
		l.Error("failed to list leafs", "error", err)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	summaries := make([]LeafSummary, len(leafs))
	for i, p := range leafs {
		summaries[i] = ToLeafSummary(p)
	}

	// Populate active_volunteers from the live count. ToLeafSummary leaves it at
	// zero because the DTO conversion has no DB access; the count is the same
	// rolling-window metric the head endpoints report, so all surfaces agree.
	// A failure here is non-fatal — the list is still useful without the counts.
	if h.pool != nil && len(leafs) > 0 {
		counts, err := CountActiveVolunteersByLeaf(r.Context(), h.pool)
		if err != nil {
			l.Warn("failed to count active volunteers for leaf list", "error", err)
		} else {
			for i, p := range leafs {
				summaries[i].ActiveVolunteers = counts[p.ID]
			}
		}
		hostCounts, err := CountActiveHostsByLeaf(r.Context(), h.pool)
		if err != nil {
			l.Warn("failed to count active hosts for leaf list", "error", err)
		} else {
			for i, p := range leafs {
				summaries[i].ActiveHosts = hostCounts[p.ID]
			}
		}
	}

	resp := types.NewListResponse(summaries, pagination)
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdate handles PUT /api/v1/leafs/{leaf_id}.
func (h *LeafHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	id, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	var req UpdateLeafRequest
	if err := json.Unmarshal(body, &req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}
	// Also capture the raw top-level keys so config blocks can be MERGED field-by-field
	// (overlay only the keys the caller actually sent) rather than whole-block REPLACED.
	// Whole-block replace (#41) silently zeroed any field the caller omitted.
	var rawReq map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawReq); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	// Fetch the existing leaf.
	p, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		l.Error("failed to get leaf for update", "error", err, "leaf_id", id)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Description != nil {
		p.Description = *req.Description
	}
	if req.ResearchArea != nil {
		p.ResearchArea = *req.ResearchArea
	}
	if req.IsOngoing != nil {
		p.IsOngoing = *req.IsOngoing
	}
	if req.Visibility != nil {
		p.Visibility = *req.Visibility
	}
	if req.StatsCacheSeconds != nil {
		v := *req.StatsCacheSeconds
		if v < 5 || v > 3600 {
			apierror.WriteError(w, apierror.ValidationError(
				"stats_cache_seconds must be between 5 and 3600", nil))
			return
		}
		p.StatsCacheSeconds = v
	}

	// Config updates — MERGE (overlay only the JSON keys the caller sent onto the
	// existing block) and RE-VALIDATE the affected block. This fixes #41: the old code
	// whole-block-REPLACED each config (zeroing omitted fields) and skipped re-validation,
	// so a one-field change required resending the whole block and an invalid config (e.g.
	// redundancy_factor: 0) was accepted silently on an ACTIVE leaf.
	//
	// Gate each block on the TYPED pointer (req.X != nil), matching the metadata fields
	// above — nil means "not provided (no change)" per the UpdateLeafRequest contract,
	// covering both an absent key AND an explicit JSON null. (Gating on rawReq-key
	// presence instead treated a null block — which is what a nil pointer marshals to
	// without omitempty — as "supplied", so a name-only partial update on a not-yet-
	// configured leaf wrongly re-ran full execution-config validation: "runtime is
	// required".) rawReq is still used for the field-by-field merge payload.
	if req.ExecutionConfig != nil {
		raw := rawReq["execution_config"]
		merged := p.ExecutionConfig
		if err := json.Unmarshal(raw, &merged); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid execution_config", nil))
			return
		}
		if p.State == StateActive && merged.Runtime != p.ExecutionConfig.Runtime {
			apierror.WriteError(w, apierror.Conflict(
				"execution_config.runtime cannot be changed while leaf is ACTIVE",
				map[string]string{"field": "execution_config.runtime"}))
			return
		}
		ApplyExecutionConfigDefaults(&merged)
		if apiErr := ValidateExecutionConfig(&merged); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		p.ExecutionConfig = merged
	}
	if req.ValidationConfig != nil {
		raw := rawReq["validation_config"]
		// Refuse keys removed from ValidationConfig on the RAW block before the typed merge
		// drops them silently (E1-C config honesty — e.g. max_success_copies, design §4.9).
		if apiErr := RejectRemovedValidationConfigKeys(raw); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		merged := p.ValidationConfig
		if err := json.Unmarshal(raw, &merged); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid validation_config", nil))
			return
		}
		ApplyValidationConfigDefaults(&merged)
		if apiErr := ValidateValidationConfig(&merged); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		p.ValidationConfig = merged
	}
	if req.FaultToleranceConfig != nil {
		raw := rawReq["fault_tolerance_config"]
		merged := p.FaultToleranceConfig
		if err := json.Unmarshal(raw, &merged); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid fault_tolerance_config", nil))
			return
		}
		ApplyFaultToleranceConfigDefaults(&merged)
		if apiErr := ValidateFaultToleranceConfig(&merged); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		p.FaultToleranceConfig = merged
	}
	if req.DataConfig != nil {
		raw := rawReq["data_config"]
		merged := p.DataConfig
		if err := json.Unmarshal(raw, &merged); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid data_config", nil))
			return
		}
		ApplyDataConfigDefaults(&merged)
		if apiErr := ValidateDataConfig(&merged, p.TaskPattern); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		p.DataConfig = merged
	}
	if req.CreditConfig != nil {
		raw := rawReq["credit_config"]
		merged := p.CreditConfig
		if err := json.Unmarshal(raw, &merged); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid credit_config", nil))
			return
		}
		ApplyCreditConfigDefaults(&merged)
		if apiErr := ValidateCreditConfig(&merged); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		p.CreditConfig = merged
	}
	if req.ResourceRequirements != nil {
		raw := rawReq["resource_requirements"]
		merged := p.ResourceRequirements
		if err := json.Unmarshal(raw, &merged); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid resource_requirements", nil))
			return
		}
		ApplyResourceRequirementsDefaults(&merged)
		if apiErr := ValidateResourceRequirements(&merged); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
		p.ResourceRequirements = merged
	}

	// Validate updated metadata.
	if req.Name != nil || req.Description != nil || req.Visibility != nil || req.ResearchArea != nil {
		if apiErr := ValidateMetadata(p); apiErr != nil {
			apierror.WriteError(w, apiErr)
			return
		}
	}

	if err := h.repo.Update(r.Context(), p); err != nil {
		l.Error("failed to update leaf", "error", err, "leaf_id", id)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	writeJSON(w, http.StatusOK, p)
}

// handleDelete handles DELETE /api/v1/leafs/{leaf_id}.
func (h *LeafHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	id, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	// Fetch the leaf to check state.
	p, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		l.Error("failed to get leaf for deletion", "error", err, "leaf_id", id)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Check deletion guards.
	if err := CanDelete(r.Context(), h.pool, p.ID, p.State); err != nil {
		l.Info("delete rejected by guard", "error", err, "leaf_id", id, "state", p.State)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	if err := h.repo.Delete(r.Context(), id); err != nil {
		l.Error("failed to delete leaf", "error", err, "leaf_id", id)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleActivate handles POST /api/v1/leafs/{leaf_id}/activate.
func (h *LeafHandler) handleActivate(w http.ResponseWriter, r *http.Request) {
	h.handleTransition(w, r, StateActive, "activate")
}

// handlePause handles POST /api/v1/leafs/{leaf_id}/pause.
func (h *LeafHandler) handlePause(w http.ResponseWriter, r *http.Request) {
	h.handleTransition(w, r, StatePaused, "pause")
}

// handleResume handles POST /api/v1/leafs/{leaf_id}/resume.
func (h *LeafHandler) handleResume(w http.ResponseWriter, r *http.Request) {
	h.handleTransition(w, r, StateActive, "resume")
}

// handleArchive handles POST /api/v1/leafs/{leaf_id}/archive.
func (h *LeafHandler) handleArchive(w http.ResponseWriter, r *http.Request) {
	h.handleTransition(w, r, StateArchived, "archive")
}

// handleConfigure handles POST /api/v1/leafs/{leaf_id}/configure.
func (h *LeafHandler) handleConfigure(w http.ResponseWriter, r *http.Request) {
	h.handleTransition(w, r, StateConfiguring, "configure")
}

// handleTransition is the shared implementation for all state transition endpoints.
func (h *LeafHandler) handleTransition(w http.ResponseWriter, r *http.Request, target LeafState, op string) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	id, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	p, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		l.Error("failed to get leaf for "+op, "error", err, "leaf_id", id)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	if err := TransitionLeaf(r.Context(), h.repo, p, target); err != nil {
		l.Info(op+" transition failed", "error", err, "leaf_id", id, "state", p.State)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// On going live, surface the work-unit deadline this leaf's units will carry
	// (otherwise invisible to operators) and warn if it is too short for the work.
	if target == StateActive {
		if p.FaultToleranceConfig.NoDeadline {
			l.Info("leaf activated", "leaf_id", id, "no_deadline", true)
		} else {
			l.Info("leaf activated",
				"leaf_id", id,
				"work_unit_deadline_seconds", p.FaultToleranceConfig.ResolveDeadlineSeconds(),
				"max_cpu_seconds", p.ExecutionConfig.MaxCPUSeconds,
			)
			for _, warning := range DeadlineAdequacyWarnings(p) {
				l.Warn("leaf deadline may be too short for its work", "leaf_id", id, "warning", warning)
			}
		}
	}

	writeJSON(w, http.StatusOK, p)
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

