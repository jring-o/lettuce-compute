package workunit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/types"
)

// GenerateFunc is the signature for pattern-specific work unit generators.
// It breaks the import cycle between workunit and generator packages. Generators persist
// each batch through the BatchSink (not raw repos), so a batch's rows — and, on the lazy
// path, its cursor advance — commit atomically (design §4.8, invariant E1-G).
type GenerateFunc func(
	ctx context.Context,
	proj *leaf.Leaf,
	parameterSpace map[string]interface{},
	batchSize int,
	sink BatchSink,
) (*GenerateResult, error)

// GenerationCursorAdvance carries a lazy-generation cursor advance that MUST commit atomically
// with the batch it accounts for (design §4.8, BG-22c / E1-3). The production BatchSink applies
// it as a guarded UPDATE inside the same transaction as the batch's units. The guard key is
// ExpectedPrevTotalGenerated: the cursor's total_generated must advance monotonically on every
// batch, whatever pattern-specific fields the cursor also carries, so a concurrent second writer
// (a leadership-failover overlap) matches zero rows and its whole transaction aborts instead of
// double-emitting. Cursor is the full replacement cursor JSON, not a delta.
type GenerationCursorAdvance struct {
	LeafID                     types.ID
	Cursor                     []byte
	ExpectedPrevTotalGenerated int64
}

// BatchSink persists one generated batch — the batch row, its work units, and their
// CREATED->QUEUED transition — as a single atomic unit, optionally advancing the leaf's
// generation cursor in the same transaction (cursor != nil, the lazy path). It replaces the
// three raw per-batch repo calls the generators used to make separately, so no batch can leave
// stranded CREATED units (E1-G withholding half) and no committed units can lack their cursor
// advance (E1-G duplication half). NextSequenceNumber resolves the leaf's next batch sequence
// number (the once-per-generation read the three writes never covered).
type BatchSink interface {
	// NextSequenceNumber returns the next batch sequence_number for the leaf (max existing + 1).
	NextSequenceNumber(ctx context.Context, leafID types.ID) (int, error)
	// PersistBatch creates batch, wires each work unit to it, bulk-inserts them, transitions
	// them CREATED->QUEUED, and (when cursor != nil) advances the leaf's generation cursor —
	// all atomically. On return batch.ID is populated. A non-nil cursor whose guard fails
	// (a concurrent writer advanced first) aborts the whole batch.
	PersistBatch(ctx context.Context, batch *Batch, wus []*WorkUnit, cursor *GenerationCursorAdvance) error
}

// GenerateResult is returned after successfully generating work units.
type GenerateResult struct {
	BatchIDs         []types.ID `json:"batch_ids"`
	WorkUnitsCreated int        `json:"work_units_created"`
	Status           string     `json:"status"`
}

// WorkUnitHandler handles HTTP requests for work unit operations.
type WorkUnitHandler struct {
	wuRepo     WorkUnitRepository
	batchRepo  BatchRepository
	leafRepo   leaf.Repository
	assignRepo assignment.Repository // optional; enables closing assignment outcomes on requeue
	generate   GenerateFunc
	sink       BatchSink // eager-generation persistence seam (design §4.8); per-batch atomic
	logger     *slog.Logger
}

// SetAssignmentRepo wires the assignment-history repository so operator requeue
// can close the active assignment row (mirrors the fault monitor). Without it,
// a requeued unit's open assignment-history row keeps the prior volunteer
// permanently excluded from reassignment (see FindNextAssignable's outcome
// IS NULL exclusion). Optional so existing constructor call sites are unchanged.
func (h *WorkUnitHandler) SetAssignmentRepo(r assignment.Repository) {
	h.assignRepo = r
}

// NewWorkUnitHandler creates a new WorkUnitHandler. sink is the per-batch atomic persistence
// seam the eager /generate path drives (design §4.8): each generated batch and its work units
// commit together, so a crashed multi-batch eager run leaves earlier complete batches QUEUED
// (resumable) rather than stranded CREATED.
func NewWorkUnitHandler(
	wuRepo WorkUnitRepository,
	batchRepo BatchRepository,
	leafRepo leaf.Repository,
	generate GenerateFunc,
	sink BatchSink,
	logger *slog.Logger,
) *WorkUnitHandler {
	return &WorkUnitHandler{
		wuRepo:    wuRepo,
		batchRepo: batchRepo,
		leafRepo:  leafRepo,
		generate:  generate,
		sink:      sink,
		logger:    logger,
	}
}

// RegisterRoutes is a no-op; all work unit routes require auth and are
// registered in the router with middleware wrappers.
func (h *WorkUnitHandler) RegisterRoutes(mux *http.ServeMux) {}

// HandleList handles GET /api/v1/projects/{leaf_id}/work-units (exported for auth wrapping).
func (h *WorkUnitHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	h.handleList(w, r)
}

// HandleGet handles GET /api/v1/projects/{leaf_id}/work-units/{work_unit_id} (exported for auth wrapping).
func (h *WorkUnitHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	h.handleGet(w, r)
}

// HandleGenerate handles POST /api/v1/projects/{leaf_id}/work-units/generate (exported for auth wrapping).
func (h *WorkUnitHandler) HandleGenerate(w http.ResponseWriter, r *http.Request) {
	h.handleGenerate(w, r)
}

// HandleRequeue handles POST /api/v1/leafs/{leaf_id}/work-units/{work_unit_id}/requeue (exported for auth wrapping).
func (h *WorkUnitHandler) HandleRequeue(w http.ResponseWriter, r *http.Request) {
	h.handleRequeue(w, r)
}

// handleList handles GET /api/v1/projects/{leaf_id}/work-units.
func (h *WorkUnitHandler) handleList(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	// Verify leaf exists.
	if _, err := h.leafRepo.GetByID(r.Context(), leafID); err != nil {
		l.Error("failed to get leaf for work unit list", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	q := r.URL.Query()
	filters := WorkUnitListFilters{
		LeafID: &leafID,
	}

	// Parse state filter.
	if v := q.Get("state"); v != "" {
		state := WorkUnitState(v)
		filters.State = &state
	}

	// Parse batch_id filter.
	if v := q.Get("batch_id"); v != "" {
		batchID, err := types.ParseID(v)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid batch_id: must be a valid UUID", nil))
			return
		}
		filters.BatchID = &batchID
	}

	// Parse priority filter.
	if v := q.Get("priority"); v != "" {
		priority := WorkUnitPriority(v)
		filters.Priority = &priority
	}

	// Parse flagged_for_review filter.
	if v := q.Get("flagged_for_review"); v != "" {
		flagged, err := strconv.ParseBool(v)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid flagged_for_review: must be true or false", nil))
			return
		}
		filters.FlaggedForReview = &flagged
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

	workUnits, pagination, err := h.wuRepo.List(r.Context(), filters, page)
	if err != nil {
		l.Error("failed to list work units", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	summaries := make([]WorkUnitSummary, len(workUnits))
	for i, wu := range workUnits {
		summaries[i] = ToWorkUnitSummary(wu)
	}

	resp := types.NewListResponse(summaries, pagination)
	writeJSON(w, http.StatusOK, resp)
}

// handleGet handles GET /api/v1/projects/{leaf_id}/work-units/{work_unit_id}.
func (h *WorkUnitHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	workUnitID, err := types.ParseID(r.PathValue("work_unit_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid work_unit_id: must be a valid UUID", nil))
		return
	}

	wu, err := h.wuRepo.GetByID(r.Context(), workUnitID)
	if err != nil {
		l.Error("failed to get work unit", "error", err, "work_unit_id", workUnitID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Verify work unit belongs to the specified leaf.
	if wu.LeafID != leafID {
		apierror.WriteError(w, apierror.NotFound("work_unit", workUnitID.String()))
		return
	}

	writeJSON(w, http.StatusOK, wu)
}

// handleRequeue resets a stuck work unit back to QUEUED so it can be reassigned.
// Operator-authed (the router wraps it with requireAuth + requireLeafOwnership).
// It exists for units stranded in ASSIGNED/RUNNING — e.g. a volunteer that
// vanished mid-pull or mid-run — which for no_deadline leaves are never
// auto-expired and would otherwise be orphaned with no way to reset them.
// It reuses the same transition path as volunteer abandonment
// (TransitionToExpired → Reassign).
func (h *WorkUnitHandler) handleRequeue(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	workUnitID, err := types.ParseID(r.PathValue("work_unit_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid work_unit_id: must be a valid UUID", nil))
		return
	}

	wu, err := h.wuRepo.GetByID(r.Context(), workUnitID)
	if err != nil {
		l.Error("requeue: failed to get work unit", "error", err, "work_unit_id", workUnitID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Scope to the leaf in the path (mirrors handleGet).
	if wu.LeafID != leafID {
		apierror.WriteError(w, apierror.NotFound("work_unit", workUnitID.String()))
		return
	}

	// Per-copy model: abandon every in-flight copy of the unit (so a fresh set of
	// copies dispatches), then ensure the unit is QUEUED. Closing the live copies
	// also stops them counting toward redundancy and frees their volunteers.
	switch wu.State {
	case WorkUnitStateQueued:
		if _, err := h.wuRepo.ExpireLiveCopies(r.Context(), workUnitID, string(assignment.OutcomeAbandoned)); err != nil {
			l.Error("requeue: failed to abandon live copies", "error", err, "work_unit_id", workUnitID)
			apierror.WriteError(w, apierror.FromError(err))
			return
		}
		// Already QUEUED; dispatchable now that its live copies are closed.
		l.Info("work unit requeued by operator (live copies abandoned)",
			"work_unit_id", workUnitID, "leaf_id", leafID)
		writeJSON(w, http.StatusOK, map[string]any{
			"work_unit_id": workUnitID.String(),
			"requeued":     true,
			"state":        string(WorkUnitStateQueued),
		})
		return
	case WorkUnitStateExpired, WorkUnitStateRejected:
		_, _ = h.wuRepo.ExpireLiveCopies(r.Context(), workUnitID, string(assignment.OutcomeAbandoned))
		updated, requeued, err := h.wuRepo.Reassign(r.Context(), workUnitID)
		if err != nil {
			l.Error("requeue: failed to reassign work unit", "error", err, "work_unit_id", workUnitID)
			apierror.WriteError(w, apierror.FromError(err))
			return
		}
		l.Info("work unit requeued by operator",
			"work_unit_id", workUnitID, "leaf_id", leafID, "requeued", requeued, "state", updated.State)
		writeJSON(w, http.StatusOK, map[string]any{
			"work_unit_id": workUnitID.String(),
			"requeued":     requeued,
			"state":        string(updated.State),
		})
		return
	default:
		apierror.WriteError(w, apierror.Conflict(
			"work unit cannot be requeued from its current state",
			map[string]string{"code": "INVALID_REQUEUE_STATE", "current_state": string(wu.State)},
		))
		return
	}
}

// closeActiveAssignment sets the outcome on the prior volunteer's open
// assignment-history row so FindNextAssignable no longer excludes them.
// Best-effort: the work unit is already requeued regardless. No-op when the
// assignment repo isn't wired or the unit had no assigned volunteer.
func (h *WorkUnitHandler) closeActiveAssignment(ctx context.Context, l *slog.Logger, wu *WorkUnit, outcome assignment.AssignmentOutcome) {
	if h.assignRepo == nil || wu.AssignedVolunteerID == nil {
		return
	}
	active, err := h.assignRepo.FindActiveByWorkUnitAndVolunteer(ctx, wu.ID, *wu.AssignedVolunteerID)
	if err != nil {
		l.Error("requeue: failed to find active assignment to close",
			"work_unit_id", wu.ID, "volunteer_id", wu.AssignedVolunteerID, "error", err)
		return
	}
	if err := h.assignRepo.UpdateOutcome(ctx, active.ID, outcome, nil); err != nil {
		l.Error("requeue: failed to close assignment outcome",
			"assignment_id", active.ID, "outcome", outcome, "error", err)
	}
}

// handleGenerate handles POST /api/v1/projects/{leaf_id}/work-units/generate.
func (h *WorkUnitHandler) handleGenerate(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	// Verify leaf exists.
	proj, err := h.leafRepo.GetByID(r.Context(), leafID)
	if err != nil {
		l.Error("failed to get leaf for generation", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	// Check leaf state — must be CONFIGURING or ACTIVE.
	if proj.State != leaf.StateConfiguring && proj.State != leaf.StateActive {
		apierror.WriteError(w, apierror.Conflict(
			"leaf must be in CONFIGURING or ACTIVE state to generate work units",
			map[string]string{
				"code":          "INVALID_STATE_TRANSITION",
				"current_state": string(proj.State),
			},
		))
		return
	}

	// Lazy leaves are generated ONLY by the head's lazy generation manager (★BG-22d): its
	// durable cursor is what guarantees each ordinal is emitted exactly once. This endpoint
	// generates from the request/splitting_config with NO cursor — on a lazy leaf that has
	// already emitted [0, cursor) it would re-emit those ordinals from offset 0 with
	// byte-identical seeds and trial indices (there is no unique constraint on trial
	// identity, so the duplicates insert cleanly and burn real volunteer compute).
	if proj.DataConfig.GenerationMode == leaf.GenerationModeLazy {
		apierror.WriteError(w, apierror.Conflict(
			"work units for a lazy leaf are generated automatically by the head's lazy generation manager; manual generation would re-emit already-generated trials",
			map[string]string{
				"code":            "LAZY_GENERATION_MANAGED",
				"generation_mode": proj.DataConfig.GenerationMode,
			},
		))
		return
	}

	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	// Build parameter space for the generator. For map-reduce, input_data and
	// input_data_ref are passed through parameterSpace since GenerateFunc uses
	// a single map for all pattern-specific inputs.
	parameterSpace := req.ParameterSpace
	if parameterSpace == nil {
		parameterSpace = make(map[string]interface{})
	}

	// Pass input_data / input_data_ref through parameterSpace for map-reduce.
	if req.InputData != nil {
		parameterSpace["input_data"] = req.InputData
	}
	if req.InputDataRef != nil {
		parameterSpace["input_data_ref"] = *req.InputDataRef
	}

	// For parameter_sweep, fall back to leaf splitting_config if no parameter_space provided.
	if len(parameterSpace) == 0 {
		if proj.DataConfig.SplittingConfig != nil {
			parameterSpace = proj.DataConfig.SplittingConfig
		}
	}
	if len(parameterSpace) == 0 {
		apierror.WriteError(w, apierror.ValidationError(
			"parameter_space is required: not provided in request and leaf has no splitting_config", nil))
		return
	}

	result, err := h.generate(r.Context(), proj, parameterSpace, req.BatchSize, h.sink)
	if err != nil {
		l.Error("failed to generate work units", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	if len(result.BatchIDs) == 0 {
		l.Error("generate returned no batches", "leaf_id", leafID)
		apierror.WriteError(w, apierror.Internal("generation produced no batches", nil))
		return
	}

	resp := GenerateResponse{
		BatchIDs:         result.BatchIDs,
		WorkUnitsCreated: result.WorkUnitsCreated,
		Status:           result.Status,
	}

	writeJSON(w, http.StatusAccepted, resp)
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
