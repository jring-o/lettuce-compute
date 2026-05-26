package custom

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/generate"
	"github.com/lettuce-compute/infrastructure/internal/logging"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
)

const maxBulkWorkUnits = 10000

// BulkUploadRequest is the request body for POST /work-units/bulk.
type BulkUploadRequest struct {
	WorkUnits []WorkUnitInput `json:"work_units"`
}

// WorkUnitInput is a single work unit definition from the project lead.
type WorkUnitInput struct {
	InputData                json.RawMessage `json:"input_data,omitempty"`
	InputDataRef             *string         `json:"input_data_ref,omitempty"`
	Parameters               json.RawMessage `json:"parameters,omitempty"`
	EstimatedDurationSeconds *int            `json:"estimated_duration_seconds,omitempty"`
	OutputSpec               json.RawMessage `json:"output_spec,omitempty"`
}

// BulkUploadResponse is the response body for POST /work-units/bulk.
type BulkUploadResponse struct {
	BatchIDs         []types.ID `json:"batch_ids"`
	WorkUnitsCreated int        `json:"work_units_created"`
	Status           string     `json:"status"`
}

// BulkUploadHandler handles the /work-units/bulk endpoint for custom pattern projects.
type BulkUploadHandler struct {
	wuRepo      workunit.WorkUnitRepository
	batchRepo   workunit.BatchRepository
	leafRepo leaf.Repository
	logger      *slog.Logger
}

// NewBulkUploadHandler creates a new BulkUploadHandler.
func NewBulkUploadHandler(
	wuRepo workunit.WorkUnitRepository,
	batchRepo workunit.BatchRepository,
	leafRepo leaf.Repository,
	logger *slog.Logger,
) *BulkUploadHandler {
	return &BulkUploadHandler{
		wuRepo:      wuRepo,
		batchRepo:   batchRepo,
		leafRepo: leafRepo,
		logger:      logger,
	}
}

// HandleBulkUpload processes the bulk work unit upload request.
func (h *BulkUploadHandler) HandleBulkUpload(w http.ResponseWriter, r *http.Request) {
	l := logging.LoggerFromContext(r.Context(), h.logger)

	leafID, err := types.ParseID(r.PathValue("leaf_id"))
	if err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid leaf_id: must be a valid UUID", nil))
		return
	}

	proj, err := h.leafRepo.GetByID(r.Context(), leafID)
	if err != nil {
		l.Error("failed to get project for bulk upload", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	if proj.State != leaf.StateConfiguring && proj.State != leaf.StateActive {
		apierror.WriteError(w, apierror.Conflict(
			"project must be in CONFIGURING or ACTIVE state to upload work units",
			map[string]string{
				"code":          "INVALID_STATE_TRANSITION",
				"current_state": string(proj.State),
			},
		))
		return
	}

	if proj.TaskPattern != leaf.PatternCustom {
		apierror.WriteError(w, apierror.Forbidden(
			"bulk upload is only available for CUSTOM pattern leafs; this leaf uses "+string(proj.TaskPattern),
		))
		return
	}

	var req BulkUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
		return
	}

	if len(req.WorkUnits) == 0 {
		apierror.WriteError(w, apierror.ValidationError("work_units array is required and must not be empty", nil))
		return
	}
	if len(req.WorkUnits) > maxBulkWorkUnits {
		apierror.WriteError(w, apierror.ValidationError(
			fmt.Sprintf("work_units array exceeds maximum of %d; split into multiple requests", maxBulkWorkUnits), nil))
		return
	}

	for i, wu := range req.WorkUnits {
		if len(wu.InputData) == 0 && wu.InputDataRef == nil && len(wu.Parameters) == 0 {
			apierror.WriteError(w, apierror.ValidationError(
				fmt.Sprintf("work_units[%d]: at least one of input_data, input_data_ref, or parameters must be provided", i), nil))
			return
		}
		if len(wu.InputData) > 0 && int64(len(wu.InputData)) > proj.DataConfig.MaxInputSizeBytes {
			apierror.WriteError(w, apierror.ValidationError(
				fmt.Sprintf("work_units[%d]: input_data size %d exceeds max_input_size_bytes %d",
					i, len(wu.InputData), proj.DataConfig.MaxInputSizeBytes), nil))
			return
		}
		if wu.EstimatedDurationSeconds != nil && *wu.EstimatedDurationSeconds <= 0 {
			apierror.WriteError(w, apierror.ValidationError(
				fmt.Sprintf("work_units[%d]: estimated_duration_seconds must be > 0", i), nil))
			return
		}
	}

	codeArtifactRef := generate.ResolveCodeArtifactRef(proj)
	deadlineSeconds := generate.ResolveDeadlineSeconds(proj)
	maxReassignments := proj.FaultToleranceConfig.MaxReassignments

	nextSeqNum, err := generate.ResolveNextSequenceNumber(r.Context(), proj.ID, h.batchRepo)
	if err != nil {
		l.Error("failed to resolve next sequence number", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.FromError(err))
		return
	}

	batch := &workunit.Batch{
		LeafID:      proj.ID,
		SequenceNumber: nextSeqNum,
		TotalWorkUnits: len(req.WorkUnits),
	}
	if err := h.batchRepo.Create(r.Context(), batch); err != nil {
		l.Error("failed to create batch", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.Internal("create batch", err))
		return
	}

	wus := make([]*workunit.WorkUnit, len(req.WorkUnits))
	for i, input := range req.WorkUnits {
		wu := &workunit.WorkUnit{
			LeafID:                proj.ID,
			BatchID:                  &batch.ID,
			State:                    workunit.WorkUnitStateCreated,
			Priority:                 workunit.WorkUnitPriorityNormal,
			CodeArtifactRef:          codeArtifactRef,
			DeadlineSeconds:          deadlineSeconds,
			MaxReassignments:         maxReassignments,
			InputData:                input.InputData,
			InputDataRef:             input.InputDataRef,
			Parameters:               input.Parameters,
			EstimatedDurationSeconds: input.EstimatedDurationSeconds,
			OutputSpec:               input.OutputSpec,
		}
		wus[i] = wu
	}

	if err := h.wuRepo.BulkCreate(r.Context(), wus); err != nil {
		l.Error("failed to bulk create work units", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.Internal("bulk create work units", err))
		return
	}

	if _, err := h.wuRepo.BulkTransitionByBatch(r.Context(), batch.ID, workunit.WorkUnitStateCreated, workunit.WorkUnitStateQueued); err != nil {
		l.Error("failed to transition work units to queued", "error", err, "leaf_id", leafID)
		apierror.WriteError(w, apierror.Internal("transition work units to queued", err))
		return
	}

	l.Info("custom bulk upload complete",
		"leaf_id", leafID,
		"batch_id", batch.ID,
		"work_units_created", len(req.WorkUnits),
	)

	resp := BulkUploadResponse{
		BatchIDs:         []types.ID{batch.ID},
		WorkUnitsCreated: len(req.WorkUnits),
		Status:           "complete",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}
