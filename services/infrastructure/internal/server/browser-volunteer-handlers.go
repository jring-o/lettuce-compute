package server

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"

	"github.com/jackc/pgx/v5/pgxpool"
)

// browserVolunteerDeps holds shared dependencies for browser volunteer REST handlers.
type browserVolunteerDeps struct {
	pool                    *pgxpool.Pool
	volunteerRepo           volunteer.Repository
	wuRepo                  workunit.WorkUnitRepository
	leafRepo                leaf.Repository
	assignRepo              assignment.Repository
	resultRepo              result.Repository
	batchRepo               workunit.BatchRepository
	validationEngine        *validation.Engine
	logger                  *slog.Logger
	headName                string
	defaultWeights          map[string]int32
	maxInflightPerVolunteer int
}

// --- Request/Response types ---

type browserRegisterRequest struct {
	PublicKey   string                  `json:"public_key"`
	DisplayName string                 `json:"display_name"`
	Hardware   browserHardwareRequest  `json:"hardware"`
}

type browserHardwareRequest struct {
	CPUCores          int      `json:"cpu_cores"`
	MemoryMB          int      `json:"memory_mb"`
	HasGPU            bool     `json:"has_gpu"`
	GPUVendors        []string `json:"gpu_vendors"`
	AvailableRuntimes []string `json:"available_runtimes"`
}

type browserRegisterResponse struct {
	VolunteerID string `json:"volunteer_id"`
	RegisteredAt string `json:"registered_at"`
}

type browserRequestWorkRequest struct {
	LeafIDs     []string `json:"leaf_ids"`
	MaxMemoryMB int      `json:"max_memory_mb"`
	MaxDiskMB   int64    `json:"max_disk_mb"`
	HasGPU      bool     `json:"has_gpu"`
	GPUVendors  []string `json:"gpu_vendors"`
}

type browserExecutionSpec struct {
	Binaries      map[string]string `json:"binaries,omitempty"`
	GPURequired   bool              `json:"gpu_required"`
	GPUType       string            `json:"gpu_type,omitempty"`
	MaxMemoryMB   int               `json:"max_memory_mb"`
	MaxDiskMB     int               `json:"max_disk_mb"`
	NetworkAccess bool              `json:"network_access"`
}

type browserRequestWorkResponse struct {
	WorkUnitID               string               `json:"work_unit_id"`
	LeafID                   string               `json:"leaf_id"`
	Runtime                  string               `json:"runtime"`
	InputData                string               `json:"input_data,omitempty"`
	InputDataURL             string               `json:"input_data_url,omitempty"`
	CodeArtifactURL          string               `json:"code_artifact_url,omitempty"`
	ParametersJSON           string               `json:"parameters_json,omitempty"`
	DeadlineSeconds          int                  `json:"deadline_seconds"`
	HeartbeatIntervalSeconds int                  `json:"heartbeat_interval_seconds"`
	EnvVars                  map[string]string    `json:"env_vars,omitempty"`
	ExecutionSpec            browserExecutionSpec `json:"execution_spec"`
	RscFpopsEst              float64              `json:"rsc_fpops_est,omitempty"`
}

type browserSubmitResultRequest struct {
	WorkUnitID     string                 `json:"work_unit_id"`
	OutputData     string                 `json:"output_data"`
	OutputChecksum string                 `json:"output_checksum"`
	ExitCode       int                    `json:"exit_code"`
	Metrics        browserResultMetrics   `json:"metrics"`
}

type browserResultMetrics struct {
	WallClockSeconds int64   `json:"wall_clock_seconds"`
	CPUSecondsUser   float64 `json:"cpu_seconds_user"`
	PeakMemoryMB     int     `json:"peak_memory_mb"`
}

type browserSubmitResultResponse struct {
	Accepted         bool   `json:"accepted"`
	ValidationStatus string `json:"validation_status"`
}

type browserHeartbeatRequest struct {
	WorkUnitID  string               `json:"work_unit_id"`
	ProgressPct int                  `json:"progress_pct"`
	Metrics     browserResultMetrics `json:"metrics"`
}

type browserHeartbeatResponse struct {
	ContinueExecution bool `json:"continue_execution"`
}

// browserRegisterMaxBody limits the request body size for the unauthenticated register endpoint.
const browserRegisterMaxBody = 64 * 1024 // 64 KB — registration payloads are tiny

// --- Handlers ---

// handleBrowserRegister handles POST /api/v1/volunteers/register.
// No Ed25519 auth required — this is the first interaction.
func handleBrowserRegister(deps *browserVolunteerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Limit body size to prevent memory exhaustion on this unauthenticated endpoint.
		r.Body = http.MaxBytesReader(w, r.Body, browserRegisterMaxBody)

		var req browserRegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
			return
		}

		if req.PublicKey == "" {
			apierror.WriteError(w, apierror.ValidationError("public_key is required", nil))
			return
		}

		pubKeyBytes, err := base64.RawURLEncoding.DecodeString(req.PublicKey)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid public_key: must be base64url-encoded", nil))
			return
		}
		if len(pubKeyBytes) != ed25519.PublicKeySize {
			apierror.WriteError(w, apierror.ValidationError(
				fmt.Sprintf("invalid public_key: must be %d bytes, got %d", ed25519.PublicKeySize, len(pubKeyBytes)), nil))
			return
		}

		// Validate display_name length to prevent storage abuse.
		if len(req.DisplayName) > 200 {
			apierror.WriteError(w, apierror.ValidationError("display_name must be at most 200 characters", nil))
			return
		}

		// Limit GPU vendor list to prevent abuse.
		if len(req.Hardware.GPUVendors) > 10 {
			apierror.WriteError(w, apierror.ValidationError("gpu_vendors must have at most 10 entries", nil))
			return
		}

		if req.Hardware.CPUCores <= 0 {
			req.Hardware.CPUCores = 1
		}
		if req.Hardware.MemoryMB <= 0 {
			req.Hardware.MemoryMB = 4096
		}
		if len(req.Hardware.AvailableRuntimes) == 0 {
			req.Hardware.AvailableRuntimes = []string{"WASM"}
		}

		// Build hardware capabilities for browser volunteer.
		var gpus []volunteer.GpuInfo
		if req.Hardware.HasGPU {
			for _, v := range req.Hardware.GPUVendors {
				gpus = append(gpus, volunteer.GpuInfo{
					Vendor:     strings.ToUpper(v),
					VRAMMB:     0,
					MaxVRAMPct: 100,
				})
			}
		}

		hw := volunteer.HardwareCapabilities{
			CPUCores:    req.Hardware.CPUCores,
			MaxCPUCores: req.Hardware.CPUCores,
			MemoryTotalMB: req.Hardware.MemoryMB,
			MaxMemoryMB:   req.Hardware.MemoryMB,
			GPUs:          gpus,
		}

		// Check if volunteer already exists.
		existing, err := deps.volunteerRepo.GetByPublicKey(r.Context(), pubKeyBytes)
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if !ok || apiErr.HTTPStatus != 404 {
				deps.logger.Error("failed to look up volunteer", "error", err)
				apierror.WriteError(w, apierror.Internal("internal server error", err))
				return
			}

			// Not found — create new volunteer.
			now := time.Now().UTC()
			var displayName *string
			if req.DisplayName != "" {
				displayName = &req.DisplayName
			}

			v := &volunteer.Volunteer{
				PublicKey:            pubKeyBytes,
				DisplayName:         displayName,
				HardwareCapabilities: hw,
				AvailableRuntimes:   req.Hardware.AvailableRuntimes,
				SchedulingMode:      volunteer.ScheduleAlways,
				IsActive:            true,
				LastSeenAt:          &now,
			}

			if createErr := deps.volunteerRepo.Create(r.Context(), v); createErr != nil {
				deps.logger.Error("failed to create volunteer", "error", createErr)
				apierror.WriteError(w, apierror.Internal("internal server error", createErr))
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(browserRegisterResponse{
				VolunteerID:  v.ID.String(),
				RegisteredAt: v.RegisteredAt.Format(time.RFC3339),
			})
			return
		}

		// Already registered — return 409 with existing ID.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(browserRegisterResponse{
			VolunteerID:  existing.ID.String(),
			RegisteredAt: existing.RegisteredAt.Format(time.RFC3339),
		})
	}
}

// handleBrowserRequestWork handles POST /api/v1/volunteers/request-work.
func handleBrowserRequestWork(deps *browserVolunteerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubKey, ok := PublicKeyFromContext(r.Context())
		if !ok {
			apierror.WriteError(w, apierror.Unauthorized("missing Ed25519 authentication"))
			return
		}

		var req browserRequestWorkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
			return
		}

		// Look up volunteer by public key.
		vol, err := deps.volunteerRepo.GetByPublicKey(r.Context(), []byte(pubKey))
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				apierror.WriteError(w, apierror.Unauthorized("volunteer not registered"))
				return
			}
			deps.logger.Error("failed to look up volunteer", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Limit leaf IDs to prevent excessively large queries.
		if len(req.LeafIDs) > 50 {
			apierror.WriteError(w, apierror.ValidationError("leaf_ids must have at most 50 entries", nil))
			return
		}

		// Parse leaf IDs.
		var leafIDs []types.ID
		for _, idStr := range req.LeafIDs {
			id, parseErr := types.ParseID(idStr)
			if parseErr != nil {
				apierror.WriteError(w, apierror.ValidationError(
					fmt.Sprintf("invalid leaf_id %q: %v", idStr, parseErr), nil))
				return
			}
			leafIDs = append(leafIDs, id)
		}

		// Build GPU info.
		var gpuVendors []string
		if req.HasGPU {
			for _, v := range req.GPUVendors {
				gpuVendors = append(gpuVendors, strings.ToUpper(v))
			}
		}

		opts := workunit.AssignmentOptions{
			VolunteerID:             vol.ID,
			LeafIDs:                 leafIDs,
			MaxCPUCores:             vol.HardwareCapabilities.MaxCPUCores,
			MaxMemoryMB:             req.MaxMemoryMB,
			MaxDiskMB:               req.MaxDiskMB,
			HasGPU:                  req.HasGPU,
			AvailableRuntimes:       []string{"WASM"},
			GPUVendors:              gpuVendors,
			MaxInflightPerVolunteer: deps.maxInflightPerVolunteer,
		}

		// Begin transaction for atomic find-assign-record.
		tx, err := deps.pool.Begin(r.Context())
		if err != nil {
			deps.logger.Error("failed to begin transaction", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}
		defer tx.Rollback(r.Context())

		txWURepo := workunit.NewPgxWorkUnitRepository(tx)
		txAssignRepo := assignment.NewPgxRepository(tx)

		wu, err := txWURepo.FindNextAssignable(r.Context(), opts)
		if err != nil {
			deps.logger.Error("failed to find assignable work unit", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}
		if wu == nil {
			apierror.WriteError(w, &apierror.APIError{
				Code:       "NO_WORK_AVAILABLE",
				Message:    "no matching work units available",
				HTTPStatus: 404,
			})
			return
		}

		// Fetch leaf once — used for spot-check check and response building.
		lf, err := deps.leafRepo.GetByID(r.Context(), wu.LeafID)
		if err != nil {
			deps.logger.Error("failed to get leaf", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Check spot-check.
		isSpotCheckFirst := false
		if !wu.SpotCheck {
			if lf.ValidationConfig.SpotCheckEnabled &&
				lf.ValidationConfig.RedundancyFactor == 1 &&
				workunit.ShouldSpotCheck(lf.ValidationConfig.SpotCheckPercentage) {
				isSpotCheckFirst = true
			}
		}

		now := time.Now().UTC()
		if isSpotCheckFirst {
			if err := txWURepo.MarkSpotCheck(r.Context(), wu.ID); err != nil {
				deps.logger.Error("failed to mark spot-check", "error", err)
				apierror.WriteError(w, apierror.Internal("internal server error", err))
				return
			}
			wu.SpotCheck = true
		} else {
			wu, err = txWURepo.Assign(r.Context(), wu.ID, vol.ID)
			if err != nil {
				deps.logger.Error("failed to assign work unit", "error", err)
				apierror.WriteError(w, apierror.Internal("internal server error", err))
				return
			}
		}

		historyEntry := &assignment.AssignmentHistoryEntry{
			WorkUnitID:  wu.ID,
			VolunteerID: vol.ID,
			AssignedAt:  now,
		}
		if err := txAssignRepo.Create(r.Context(), historyEntry); err != nil {
			deps.logger.Error("failed to record assignment", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		if err := tx.Commit(r.Context()); err != nil {
			deps.logger.Error("failed to commit assignment", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Best-effort updates.
		_ = deps.volunteerRepo.UpdateLastSeen(r.Context(), vol.ID)
		_ = deps.volunteerRepo.SetActive(r.Context(), vol.ID, true)

		// Encode input data as base64 if present.
		var inputDataB64 string
		if len(wu.InputData) > 0 {
			inputDataB64 = base64.StdEncoding.EncodeToString(wu.InputData)
		}

		// Filter env vars: only send LETTUCE_-prefixed vars to browser volunteers.
		// Env vars may contain researcher secrets (API keys, credentials) that must
		// not be exposed to untrusted browser volunteers.
		var safeEnvVars map[string]string
		if len(lf.ExecutionConfig.EnvVars) > 0 {
			safeEnvVars = make(map[string]string)
			for k, v := range lf.ExecutionConfig.EnvVars {
				if strings.HasPrefix(k, "LETTUCE_") {
					safeEnvVars[k] = v
				}
			}
		}

		resp := browserRequestWorkResponse{
			WorkUnitID:               wu.ID.String(),
			LeafID:                   wu.LeafID.String(),
			Runtime:                  lf.ExecutionConfig.Runtime,
			InputData:                inputDataB64,
			InputDataURL:             derefString(wu.InputDataRef),
			CodeArtifactURL:          wu.CodeArtifactRef,
			ParametersJSON:           string(wu.Parameters),
			DeadlineSeconds:          wu.DeadlineSeconds,
			HeartbeatIntervalSeconds: lf.FaultToleranceConfig.HeartbeatIntervalSeconds,
			EnvVars:                  safeEnvVars,
			RscFpopsEst:              lf.ExecutionConfig.RscFpopsEst,
			ExecutionSpec: browserExecutionSpec{
				Binaries:      lf.ExecutionConfig.Binaries,
				GPURequired:   lf.ExecutionConfig.GPURequired,
				GPUType:       lf.ExecutionConfig.GPUType,
				MaxMemoryMB:   lf.ExecutionConfig.MaxMemoryMB,
				MaxDiskMB:     lf.ExecutionConfig.MaxDiskMB,
				NetworkAccess: lf.ExecutionConfig.NetworkAccess,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// handleBrowserSubmitResult handles POST /api/v1/volunteers/submit-result.
func handleBrowserSubmitResult(deps *browserVolunteerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubKey, ok := PublicKeyFromContext(r.Context())
		if !ok {
			apierror.WriteError(w, apierror.Unauthorized("missing Ed25519 authentication"))
			return
		}

		var req browserSubmitResultRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
			return
		}

		// Validate fields.
		if req.WorkUnitID == "" {
			apierror.WriteError(w, apierror.ValidationError("work_unit_id is required", nil))
			return
		}
		workUnitID, err := types.ParseID(req.WorkUnitID)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid work_unit_id", nil))
			return
		}

		if !sha256HexRegex.MatchString(req.OutputChecksum) {
			apierror.WriteError(w, apierror.ValidationError(
				"output_checksum must be a 64-character lowercase hex SHA-256 digest", nil))
			return
		}

		// Decode output data from base64.
		var outputRaw []byte
		if req.OutputData != "" {
			outputRaw, err = base64.StdEncoding.DecodeString(req.OutputData)
			if err != nil {
				apierror.WriteError(w, apierror.ValidationError("invalid output_data: must be base64-encoded", nil))
				return
			}

			// Verify checksum.
			hash := sha256.Sum256(outputRaw)
			computed := hex.EncodeToString(hash[:])
			if computed != req.OutputChecksum {
				apierror.WriteError(w, apierror.ValidationError(
					fmt.Sprintf("output_checksum mismatch: computed %s, got %s", computed, req.OutputChecksum), nil))
				return
			}
		}

		// Look up volunteer.
		vol, err := deps.volunteerRepo.GetByPublicKey(r.Context(), []byte(pubKey))
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				apierror.WriteError(w, apierror.Unauthorized("volunteer not registered"))
				return
			}
			deps.logger.Error("failed to look up volunteer", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// M3: enforce the leaf's researcher-configured per-result output cap on the
		// decoded INLINE output before storing it. Without this, an authenticated,
		// assigned browser volunteer could submit inline output far larger than the
		// configured maximum (only the global ~10MB body cap applies here),
		// causing unbounded JSONB storage and memory pressure. Browser submissions
		// are always inline, so there is no external output_data_url path to skip.
		if len(outputRaw) > 0 {
			wu, wuErr := deps.wuRepo.GetByID(r.Context(), workUnitID)
			if wuErr != nil {
				apiErr, ok := wuErr.(*apierror.APIError)
				if ok && apiErr.HTTPStatus == 404 {
					apierror.WriteError(w, apierror.NotFound("work unit", req.WorkUnitID))
					return
				}
				deps.logger.Error("failed to load work unit for output size check", "error", wuErr)
				apierror.WriteError(w, apierror.Internal("internal server error", wuErr))
				return
			}
			lf, leafErr := deps.leafRepo.GetByID(r.Context(), wu.LeafID)
			if leafErr != nil {
				deps.logger.Error("failed to load leaf for output size check", "error", leafErr)
				apierror.WriteError(w, apierror.Internal("internal server error", leafErr))
				return
			}
			// MaxOutputSizeBytes is always > 0 for a stored leaf (ValidateDataConfig
			// requires > 0 and ApplyDataConfigDefaults fills 0 with a 100MB default),
			// but we still guard on > 0 so a max of 0 is treated as "unlimited".
			maxOut := lf.DataConfig.MaxOutputSizeBytes
			if maxOut > 0 && int64(len(outputRaw)) > maxOut {
				apierror.WriteError(w, apierror.ValidationError(
					fmt.Sprintf("output_data size %d bytes exceeds leaf max_output_size_bytes %d", len(outputRaw), maxOut),
					nil))
				return
			}
		}

		// Begin transaction.
		tx, err := deps.pool.Begin(r.Context())
		if err != nil {
			deps.logger.Error("failed to begin transaction", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}
		defer tx.Rollback(r.Context())

		txAssignRepo := assignment.NewPgxRepository(tx)
		txResultRepo := result.NewPgxRepository(tx)

		// Verify active assignment.
		activeAssignment, err := txAssignRepo.FindActiveByWorkUnitAndVolunteer(r.Context(), workUnitID, vol.ID)
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				apierror.WriteError(w, apierror.NotFound("work unit assignment", req.WorkUnitID))
				return
			}
			deps.logger.Error("failed to check assignment", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Count existing pending results.
		existingCount, err := txResultRepo.CountPendingByWorkUnit(r.Context(), workUnitID)
		if err != nil {
			deps.logger.Error("failed to count results", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Build result.
		var outputData json.RawMessage
		if len(outputRaw) > 0 {
			outputData = json.RawMessage(outputRaw)
		}

		res := &result.Result{
			WorkUnitID:     workUnitID,
			VolunteerID:    vol.ID,
			OutputData:     outputData,
			OutputChecksum: req.OutputChecksum,
			ExecutionMetadata: result.ExecutionMetadata{
				WallClockSeconds: req.Metrics.WallClockSeconds,
				CPUSecondsUser:   req.Metrics.CPUSecondsUser,
				PeakMemoryMB:     req.Metrics.PeakMemoryMB,
			},
			ValidationStatus: result.ValidationPending,
		}

		if err := txResultRepo.Create(r.Context(), res); err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 409 {
				apierror.WriteError(w, apierror.Conflict("result already submitted for this assignment", nil))
				return
			}
			deps.logger.Error("failed to create result", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Update assignment outcome.
		if err := txAssignRepo.UpdateOutcome(r.Context(), activeAssignment.ID, assignment.OutcomeCompleted, &res.ID); err != nil {
			deps.logger.Error("failed to update assignment outcome", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Transition work unit to COMPLETED if enough results.
		txWURepo := workunit.NewPgxWorkUnitRepository(tx)
		currentWU, err := txWURepo.GetByID(r.Context(), workUnitID)
		if err != nil {
			deps.logger.Error("failed to load work unit", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Determine effective redundancy from the leaf's validation config.
		// For spot-check WUs, always require at least 2 results.
		effectiveRedundancy := 1
		completionLeaf, clErr := deps.leafRepo.GetByID(r.Context(), currentWU.LeafID)
		if clErr == nil {
			effectiveRedundancy = completionLeaf.ValidationConfig.RedundancyFactor
		}
		if currentWU.SpotCheck && effectiveRedundancy < 2 {
			effectiveRedundancy = 2
		}

		if existingCount+1 >= effectiveRedundancy {
			_, err := tx.Exec(r.Context(), `
				UPDATE work_units SET
					state = 'COMPLETED',
					started_at = COALESCE(started_at, NOW()),
					completed_at = NOW()
				WHERE id = $1 AND (state IN ('ASSIGNED', 'RUNNING') OR (state = 'QUEUED' AND spot_check = true))`,
				workUnitID,
			)
			if err != nil {
				deps.logger.Error("failed to transition work unit", "error", err)
				apierror.WriteError(w, apierror.Internal("internal server error", err))
				return
			}
		}

		if err := tx.Commit(r.Context()); err != nil {
			deps.logger.Error("failed to commit result", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Best-effort post-commit work.
		_ = deps.volunteerRepo.UpdateLastSeen(r.Context(), vol.ID)

		if existingCount+1 >= effectiveRedundancy && deps.batchRepo != nil {
			// Reuse currentWU from the transaction — no need for a second DB fetch.
			if currentWU.BatchID != nil {
				_ = deps.batchRepo.IncrementCompleted(r.Context(), *currentWU.BatchID)
			}
		}

		if deps.validationEngine != nil {
			if _, valErr := deps.validationEngine.TryValidate(r.Context(), workUnitID); valErr != nil {
				deps.logger.Error("validation failed after result submission",
					"work_unit_id", workUnitID, "error", valErr)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(browserSubmitResultResponse{
			Accepted:         true,
			ValidationStatus: string(result.ValidationPending),
		})
	}
}

// RegisterBrowserVolunteerRoutes registers all browser volunteer REST endpoints on the given mux.
// This is used by E2E tests that build their own mux rather than going through NewRouter.
func RegisterBrowserVolunteerRoutes(mux *http.ServeMux, pool *pgxpool.Pool, volunteerRepo volunteer.Repository, wuRepo workunit.WorkUnitRepository, leafRepo leaf.Repository, assignRepo assignment.Repository, resultRepo result.Repository, batchRepo workunit.BatchRepository, validationEngine *validation.Engine, logger *slog.Logger, maxInflight int) {
	deps := &browserVolunteerDeps{
		pool:                    pool,
		volunteerRepo:           volunteerRepo,
		wuRepo:                  wuRepo,
		leafRepo:                leafRepo,
		assignRepo:              assignRepo,
		resultRepo:              resultRepo,
		batchRepo:               batchRepo,
		validationEngine:        validationEngine,
		logger:                  logger,
		maxInflightPerVolunteer: maxInflight,
	}
	mux.HandleFunc("POST /api/v1/volunteers/register", handleBrowserRegister(deps))
	mux.HandleFunc("POST /api/v1/volunteers/request-work",
		ed25519AuthRequired(handleBrowserRequestWork(deps)))
	mux.HandleFunc("POST /api/v1/volunteers/submit-result",
		ed25519AuthRequired(handleBrowserSubmitResult(deps)))
	mux.HandleFunc("POST /api/v1/volunteers/heartbeat",
		ed25519AuthRequired(handleBrowserHeartbeat(deps)))
}

// handleBrowserHeartbeat handles POST /api/v1/volunteers/heartbeat.
func handleBrowserHeartbeat(deps *browserVolunteerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pubKey, ok := PublicKeyFromContext(r.Context())
		if !ok {
			apierror.WriteError(w, apierror.Unauthorized("missing Ed25519 authentication"))
			return
		}

		var req browserHeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid request body", nil))
			return
		}

		if req.WorkUnitID == "" {
			apierror.WriteError(w, apierror.ValidationError("work_unit_id is required", nil))
			return
		}
		workUnitID, err := types.ParseID(req.WorkUnitID)
		if err != nil {
			apierror.WriteError(w, apierror.ValidationError("invalid work_unit_id", nil))
			return
		}

		// Look up volunteer.
		vol, err := deps.volunteerRepo.GetByPublicKey(r.Context(), []byte(pubKey))
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				apierror.WriteError(w, apierror.Unauthorized("volunteer not registered"))
				return
			}
			deps.logger.Error("failed to look up volunteer", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Load work unit.
		wu, err := deps.wuRepo.GetByID(r.Context(), workUnitID)
		if err != nil {
			apiErr, ok := err.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				apierror.WriteError(w, apierror.NotFound("work unit", req.WorkUnitID))
				return
			}
			deps.logger.Error("failed to load work unit", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Verify assignment.
		if wu.SpotCheck {
			entry, assignErr := deps.assignRepo.FindActiveByWorkUnitAndVolunteer(r.Context(), workUnitID, vol.ID)
			if assignErr != nil || entry == nil {
				apierror.WriteError(w, apierror.NotFound("work unit assignment", req.WorkUnitID))
				return
			}
		} else if wu.AssignedVolunteerID == nil || *wu.AssignedVolunteerID != vol.ID {
			apierror.WriteError(w, apierror.NotFound("work unit assignment", req.WorkUnitID))
			return
		}

		// Check work unit state.
		switch wu.State {
		case workunit.WorkUnitStateAssigned, workunit.WorkUnitStateRunning:
			// OK
		case workunit.WorkUnitStateQueued:
			if !wu.SpotCheck {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(browserHeartbeatResponse{ContinueExecution: false})
				return
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(browserHeartbeatResponse{ContinueExecution: false})
			return
		}

		// Transaction for atomic updates.
		tx, err := deps.pool.Begin(r.Context())
		if err != nil {
			deps.logger.Error("failed to begin transaction", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}
		defer tx.Rollback(r.Context())

		if wu.State == workunit.WorkUnitStateAssigned {
			_, err = tx.Exec(r.Context(), `
				UPDATE work_units SET
					state = 'RUNNING',
					started_at = NOW(),
					last_heartbeat_at = NOW()
				WHERE id = $1 AND state = 'ASSIGNED'`, workUnitID)
		} else {
			_, err = tx.Exec(r.Context(),
				"UPDATE work_units SET last_heartbeat_at = NOW() WHERE id = $1", workUnitID)
		}
		if err != nil {
			deps.logger.Error("failed to update heartbeat", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		_, err = tx.Exec(r.Context(),
			"UPDATE volunteers SET last_seen_at = NOW(), is_active = true WHERE id = $1", vol.ID)
		if err != nil {
			deps.logger.Error("failed to update volunteer", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		if err := tx.Commit(r.Context()); err != nil {
			deps.logger.Error("failed to commit heartbeat", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Check leaf state.
		lf, err := deps.leafRepo.GetByID(r.Context(), wu.LeafID)
		if err != nil {
			deps.logger.Error("failed to load leaf", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		continueExec := true
		switch lf.State {
		case leaf.StatePaused, leaf.StateCompleted, leaf.StateArchived:
			continueExec = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(browserHeartbeatResponse{ContinueExecution: continueExec})
	}
}

