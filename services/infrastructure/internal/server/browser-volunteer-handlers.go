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
	"github.com/lettuce-compute/infrastructure/internal/transition"
	"github.com/lettuce-compute/infrastructure/internal/trust"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"

	"github.com/jackc/pgx/v5/pgxpool"
)

// browserVolunteerDeps holds shared dependencies for browser volunteer REST handlers.
type browserVolunteerDeps struct {
	pool             *pgxpool.Pool
	volunteerRepo    volunteer.Repository
	wuRepo           workunit.WorkUnitRepository
	leafRepo         leaf.Repository
	assignRepo       assignment.Repository
	resultRepo       result.Repository
	batchRepo        workunit.BatchRepository
	validationEngine *validation.Engine
	// trustRepo snapshots the submitting subject's account-level trust score onto each result
	// at submit time (see internal/trust). May be nil (tests / no pool); stamping is nil-safe.
	trustRepo trust.Repository
	// now supplies the current time for trust power-suppression checks (overridable in tests).
	now func() time.Time
	// transitioner is the SINGLE owner of the work-unit redundancy decision (TODO #50/#66):
	// the browser/WASM submit path routes through it (validate / reject / wait / dead-letter /
	// supersede) exactly like the gRPC SubmitResult path, instead of writing COMPLETED via raw
	// SQL + calling the legacy validationEngine.TryValidate. May be nil (tests without an
	// engine), in which case the submit still records the result and the decision is deferred.
	transitioner            *transition.Transitioner
	logger                  *slog.Logger
	headName                string
	defaultWeights          map[string]int32
	maxInflightPerVolunteer int
	// trustDispatch is the head trust-gate dispatch policy the browser/WASM request-work path
	// stamps onto its per-request, tx-scoped work-unit repo so FindNextAssignable resolves the
	// trusted-corroborator reservation identically to the shared repo. Zero value = gate off
	// (the reservation is inert and this path dispatches exactly as before).
	trustDispatch workunit.TrustDispatchPolicy
}

// --- Request/Response types ---

type browserRegisterRequest struct {
	PublicKey   string                 `json:"public_key"`
	DisplayName string                 `json:"display_name"`
	Hardware    browserHardwareRequest `json:"hardware"`
}

type browserHardwareRequest struct {
	CPUCores          int      `json:"cpu_cores"`
	MemoryMB          int      `json:"memory_mb"`
	HasGPU            bool     `json:"has_gpu"`
	GPUVendors        []string `json:"gpu_vendors"`
	AvailableRuntimes []string `json:"available_runtimes"`
}

type browserRegisterResponse struct {
	VolunteerID  string `json:"volunteer_id"`
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
	WorkUnitID      string               `json:"work_unit_id"`
	LeafID          string               `json:"leaf_id"`
	Runtime         string               `json:"runtime"`
	InputData       string               `json:"input_data,omitempty"`
	InputDataURL    string               `json:"input_data_url,omitempty"`
	CodeArtifactURL string               `json:"code_artifact_url,omitempty"`
	ParametersJSON  string               `json:"parameters_json,omitempty"`
	DeadlineSeconds int                  `json:"deadline_seconds"`
	EnvVars         map[string]string    `json:"env_vars,omitempty"`
	ExecutionSpec   browserExecutionSpec `json:"execution_spec"`
	RscFpopsEst     float64              `json:"rsc_fpops_est,omitempty"`
}

type browserSubmitResultRequest struct {
	WorkUnitID     string               `json:"work_unit_id"`
	OutputData     string               `json:"output_data"`
	OutputChecksum string               `json:"output_checksum"`
	ExitCode       int                  `json:"exit_code"`
	Metrics        browserResultMetrics `json:"metrics"`
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
			CPUCores:      req.Hardware.CPUCores,
			MaxCPUCores:   req.Hardware.CPUCores,
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
				DisplayName:          displayName,
				HardwareCapabilities: hw,
				AvailableRuntimes:    req.Hardware.AvailableRuntimes,
				SchedulingMode:       volunteer.ScheduleAlways,
				IsActive:             true,
				LastSeenAt:           &now,
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

		// Carry the head trust-gate policy so FindNextAssignable applies the trusted-
		// corroborator reservation on this browser/WASM dispatch path too (inert with the
		// gate off).
		txWURepo := workunit.NewPgxWorkUnitRepository(tx).WithTrustDispatch(deps.trustDispatch)

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
		// Per-copy dispatch: create this browser volunteer's copy. The unit stays
		// QUEUED (its other redundancy copies keep dispatching in parallel). A
		// spot-check first-placement leaves the copy RESERVED (it waits for a second
		// corroborator); a normal placement immediately run-starts it (RUNNING).
		hold := wu.DeadlineSeconds
		if hold <= 0 {
			hold = 3600
		}
		reservedUntil := now.Add(time.Duration(hold) * time.Second)
		// Browser volunteers run in a tab with no persistent per-machine host id, so the
		// copy is attributed to the account only (host_id NULL) — TODO #19.
		if _, rerr := txWURepo.ReserveCopy(r.Context(), wu.ID, vol.ID, nil, reservedUntil, wu.DeadlineSeconds); rerr != nil {
			deps.logger.Error("failed to reserve copy", "error", rerr)
			apierror.WriteError(w, apierror.Internal("internal server error", rerr))
			return
		}
		if isSpotCheckFirst {
			if err := txWURepo.MarkSpotCheck(r.Context(), wu.ID); err != nil {
				deps.logger.Error("failed to mark spot-check", "error", err)
				apierror.WriteError(w, apierror.Internal("internal server error", err))
				return
			}
			wu.SpotCheck = true
		} else {
			if _, err = txWURepo.Assign(r.Context(), wu.ID, vol.ID); err != nil {
				deps.logger.Error("failed to run-start copy", "error", err)
				apierror.WriteError(w, apierror.Internal("internal server error", err))
				return
			}
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
			WorkUnitID:      wu.ID.String(),
			LeafID:          wu.LeafID.String(),
			Runtime:         lf.ExecutionConfig.Runtime,
			InputData:       inputDataB64,
			InputDataURL:    derefString(wu.InputDataRef),
			CodeArtifactURL: wu.CodeArtifactRef,
			ParametersJSON:  string(wu.Parameters),
			DeadlineSeconds: wu.DeadlineSeconds,
			EnvVars:         safeEnvVars,
			RscFpopsEst:     lf.ExecutionConfig.RscFpopsEst,
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

		// Serialize concurrent submits for the same unit (parallel copies) so the
		// PENDING-result count that decides COMPLETED is accurate.
		if _, lerr := tx.Exec(r.Context(), `SELECT 1 FROM work_units WHERE id = $1 FOR UPDATE`, workUnitID); lerr != nil {
			deps.logger.Error("failed to lock work unit for submit", "error", lerr)
			apierror.WriteError(w, apierror.Internal("internal server error", lerr))
			return
		}

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

		// Stamp the account-level trust snapshot (see internal/trust). vol was loaded above, so
		// no extra read; a suppressed/absent principal is stamped score 0 (fail-closed on power),
		// but stamping never blocks the submission (fail-open on work).
		now := time.Now
		if deps.now != nil {
			now = deps.now
		}
		trustSubject, trustScore := stampTrustSnapshot(r.Context(), deps.trustRepo, vol, vol.ID, now(), deps.logger)

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
			// Account-level trust snapshot (see internal/trust): acceptance reads the
			// submission-time subject + score, not a later re-read.
			TrustSubject:       &trustSubject,
			TrustScoreAtSubmit: &trustScore,
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

		// The COMPLETED threshold is the unit's effective QUORUM (TODO #50) — how many results
		// are needed to attempt validation — resolved through the single source (ResolvePolicy);
		// for any leaf that only sets redundancy_factor this equals redundancy_factor (2 for a
		// spot-check unit), identical to before. It is used here ONLY to drive the batch-completed
		// counter; the actual validate / reject / wait / dead-letter / supersede decision — and
		// the COMPLETED state write itself — is delegated to the transitioner after commit.
		quorum := 1
		completionLeaf, clErr := deps.leafRepo.GetByID(r.Context(), currentWU.LeafID)
		if clErr == nil {
			quorum = transition.ResolvePolicy(completionLeaf, currentWU).MinQuorum
		}
		quorumMet := existingCount+1 >= quorum

		// No raw work_units.state write here (TODO #66): the transitioner is the SOLE owner of
		// work-unit state transitions. This volunteer's copy is closed (UpdateOutcome above) and
		// its PENDING result holds a redundancy slot; the unit stays QUEUED until the transitioner
		// (called after commit) marks it COMPLETED and validates/rejects/supersedes.

		if err := tx.Commit(r.Context()); err != nil {
			deps.logger.Error("failed to commit result", "error", err)
			apierror.WriteError(w, apierror.Internal("internal server error", err))
			return
		}

		// Best-effort post-commit work.
		_ = deps.volunteerRepo.UpdateLastSeen(r.Context(), vol.ID)

		if quorumMet && deps.batchRepo != nil {
			// Reuse currentWU from the transaction — no need for a second DB fetch.
			if currentWU.BatchID != nil {
				_ = deps.batchRepo.IncrementCompleted(r.Context(), *currentWU.BatchID)
			}
		}

		// Delegate the redundancy decision to the SINGLE transitioner (TODO #50/#66): it loads
		// the unit + copies + results + resolved policy, runs the pure Decide, and applies the
		// one outcome (validate-at-quorum / reject / wait / dead-letter) — including marking the
		// unit COMPLETED and superseding any over-dispatch extras — under a per-unit lock. This
		// is the SAME path the gRPC SubmitResult uses; it replaces the inline COMPLETED write +
		// legacy TryValidate call so the browser/WASM path no longer bypasses the transitioner.
		if deps.transitioner != nil {
			if _, tErr := deps.transitioner.Evaluate(r.Context(), workUnitID); tErr != nil {
				deps.logger.Error("transition evaluation failed after result submission",
					"work_unit_id", workUnitID, "error", tErr)
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
		pool:             pool,
		volunteerRepo:    volunteerRepo,
		wuRepo:           wuRepo,
		leafRepo:         leafRepo,
		assignRepo:       assignRepo,
		resultRepo:       resultRepo,
		batchRepo:        batchRepo,
		validationEngine: validationEngine,
		// Trust snapshot store for submit-time stamping (see internal/trust); nil-safe.
		trustRepo: trustRepoFromPool(pool),
		now:       time.Now,
		// Build the same single transitioner the gRPC volunteer service uses so the browser/WASM
		// submit path routes its redundancy decision through it (TODO #66). This helper is used by
		// E2E tests only, so the trust gate is left at its zero (off) default here.
		transitioner:            newTransitioner(pool, wuRepo, leafRepo, resultRepo, validationEngine, transition.TrustPolicy{}, logger),
		logger:                  logger,
		maxInflightPerVolunteer: maxInflight,
	}
	mux.HandleFunc("POST /api/v1/volunteers/register", handleBrowserRegister(deps))
	mux.HandleFunc("POST /api/v1/volunteers/request-work",
		ed25519AuthRequired(handleBrowserRequestWork(deps)))
	mux.HandleFunc("POST /api/v1/volunteers/submit-result",
		ed25519AuthRequired(handleBrowserSubmitResult(deps)))
	// NOTE: the browser REST heartbeat (POST /api/v1/volunteers/heartbeat) is
	// removed. Browser/WASM units run-start at assignment time (immediate Assign in
	// handleBrowserRequestWork) and liveness is deadline-based: a closed-tab unit is
	// reclaimed at its deadline (or the synthetic NoDeadline ceiling) by the fault
	// monitor. The browser submit path keeps its active-assignment precondition.
}
