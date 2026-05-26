package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/assignment"
	"github.com/lettuce-compute/infrastructure/internal/checkpoint"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/validation"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type volunteerService struct {
	lettucev1.UnimplementedVolunteerServiceServer
	pool             *pgxpool.Pool
	version          string
	startTime        time.Time
	volunteerRepo    volunteer.Repository
	wuRepo           workunit.WorkUnitRepository
	leafRepo      leaf.Repository
	assignRepo       assignment.Repository
	resultRepo       result.Repository
	batchRepo        workunit.BatchRepository
	checkpointRepo   checkpoint.Repository
	validationEngine *validation.Engine
	logger           *slog.Logger
	headName                string
	headDescription         string
	headURL                 string
	defaultWeights          map[string]int32
	maxInflightPerVolunteer int
}

// NewVolunteerService creates a new VolunteerService gRPC implementation.
func NewVolunteerService(
	pool *pgxpool.Pool,
	version string,
	startTime time.Time,
	volunteerRepo volunteer.Repository,
	wuRepo workunit.WorkUnitRepository,
	leafRepo leaf.Repository,
	assignRepo assignment.Repository,
	resultRepo result.Repository,
	batchRepo workunit.BatchRepository,
	checkpointRepo checkpoint.Repository,
	validationEngine *validation.Engine,
	logger *slog.Logger,
) lettucev1.VolunteerServiceServer {
	return &volunteerService{
		pool:             pool,
		version:          version,
		startTime:        startTime,
		volunteerRepo:    volunteerRepo,
		wuRepo:           wuRepo,
		leafRepo:      leafRepo,
		assignRepo:       assignRepo,
		resultRepo:       resultRepo,
		batchRepo:        batchRepo,
		checkpointRepo:   checkpointRepo,
		validationEngine: validationEngine,
		logger:           logger,
	}
}

// SetHeadConfig sets the head identity for GetHeadInfo gRPC responses.
func SetHeadConfig(svc lettucev1.VolunteerServiceServer, name, description, url string, weights map[string]int32, maxInflight int) {
	if vs, ok := svc.(*volunteerService); ok {
		vs.headName = name
		vs.headDescription = description
		vs.headURL = url
		vs.defaultWeights = weights
		vs.maxInflightPerVolunteer = maxInflight
	}
}

func (s *volunteerService) GetHeadInfo(ctx context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
	// Uses LEFT JOINs with pre-aggregated subqueries instead of correlated subqueries to avoid
	// O(N) sequential scans per leaf when work_units table is large.
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.slug, l.name, l.description, l.research_area, l.task_pattern, l.state,
			COALESCE(q.cnt, 0),
			COALESCE(a.cnt, 0),
			l.execution_config
		FROM leafs l
		LEFT JOIN (
			SELECT leaf_id, COUNT(*) AS cnt
			FROM work_units
			WHERE state IN ('QUEUED', 'CREATED')
			GROUP BY leaf_id
		) q ON q.leaf_id = l.id
		LEFT JOIN (
			SELECT leaf_id, COUNT(DISTINCT assigned_volunteer_id) AS cnt
			FROM work_units
			WHERE state IN ('ASSIGNED', 'RUNNING')
			AND assigned_volunteer_id IS NOT NULL
			GROUP BY leaf_id
		) a ON a.leaf_id = l.id
		WHERE l.state = 'ACTIVE' AND l.visibility = 'PUBLIC'
		ORDER BY l.name ASC`)
	if err != nil {
		s.logger.Error("query leafs", "method", "GetHeadInfo", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer rows.Close()

	var leafs []*lettucev1.LeafInfo
	for rows.Next() {
		var li lettucev1.LeafInfo
		var researchArea []string
		var execConfig leaf.ExecutionConfig
		if err := rows.Scan(&li.Id, &li.Slug, &li.Name, &li.Description, &researchArea,
			&li.TaskPattern, &li.State, &li.QueuedWorkUnits, &li.ActiveVolunteers, &execConfig); err != nil {
			s.logger.Error("scan leaf", "method", "GetHeadInfo", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		li.ResearchArea = researchArea
		if li.ResearchArea == nil {
			li.ResearchArea = []string{}
		}
		li.ExecutionSpec = &lettucev1.ExecutionSpec{
			Binaries:        execConfig.Binaries,
			BinaryChecksums: execConfig.BinaryChecksums,
			Image:           derefString(execConfig.Image),
			GpuRequired:     execConfig.GPURequired,
			GpuType:         execConfig.GPUType,
			MaxMemoryMb:     int32(execConfig.MaxMemoryMB),
			MaxDiskMb:       int32(execConfig.MaxDiskMB),
			NetworkAccess:   execConfig.NetworkAccess,
		}
		leafs = append(leafs, &li)
	}
	if leafs == nil {
		leafs = []*lettucev1.LeafInfo{}
	}

	weights := s.defaultWeights
	if weights == nil {
		weights = map[string]int32{}
	}

	return &lettucev1.GetHeadInfoResponse{
		Name:               s.headName,
		Description:        s.headDescription,
		Url:                s.headURL,
		Leafs:              leafs,
		DefaultLeafWeights: weights,
	}, nil
}

func (s *volunteerService) GetServerStatus(ctx context.Context, _ *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	st, dbStatus := checkDBHealth(ctx, s.pool)

	return &lettucev1.GetServerStatusResponse{
		Status:         st,
		UptimeSeconds:  int64(time.Since(s.startTime).Seconds()),
		DatabaseStatus: dbStatus,
	}, nil
}

// validRuntimes is the set of accepted runtime values.
var validRuntimes = map[string]bool{
	leaf.RuntimeNative:    true,
	leaf.RuntimeContainer: true,
	leaf.RuntimeWasm:      true,
}

// validSchedulingModes is the set of accepted scheduling mode values.
var validSchedulingModes = map[string]bool{
	"ALWAYS":    true,
	"WHEN_IDLE": true,
	"SCHEDULED": true,
}

func (s *volunteerService) RegisterVolunteer(ctx context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	// Validate public_key: must be exactly 32 bytes.
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "public_key must be exactly 32 bytes (Ed25519), got %d", len(req.PublicKey))
	}

	// The request signature proves possession of the private key for the public key
	// being registered. Bind the verified identity to req.PublicKey so a caller can
	// only register a public key they actually control.
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	if !bytes.Equal(authedKey, req.PublicKey) {
		return nil, status.Errorf(codes.PermissionDenied, "authenticated key does not match public_key being registered")
	}

	// Validate hardware: required.
	if req.Hardware == nil {
		return nil, status.Errorf(codes.InvalidArgument, "hardware capabilities are required")
	}
	if req.Hardware.CpuCores <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.cpu_cores must be > 0")
	}
	if req.Hardware.MaxCpuCores <= 0 || req.Hardware.MaxCpuCores > req.Hardware.CpuCores {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.max_cpu_cores must be > 0 and <= cpu_cores")
	}
	if req.Hardware.MemoryTotalMb <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.memory_total_mb must be > 0")
	}
	if req.Hardware.MaxMemoryMb <= 0 || req.Hardware.MaxMemoryMb > req.Hardware.MemoryTotalMb {
		return nil, status.Errorf(codes.InvalidArgument, "hardware.max_memory_mb must be > 0 and <= memory_total_mb")
	}

	// Validate available_runtimes: at least one, all valid.
	if len(req.AvailableRuntimes) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "available_runtimes must contain at least one value")
	}
	for _, rt := range req.AvailableRuntimes {
		if !validRuntimes[rt] {
			return nil, status.Errorf(codes.InvalidArgument, "invalid runtime %q: must be one of [NATIVE, CONTAINER, WASM]", rt)
		}
	}

	// Validate scheduling_mode: default to ALWAYS if empty.
	schedulingMode := req.SchedulingMode
	if schedulingMode == "" {
		schedulingMode = "ALWAYS"
	}
	if !validSchedulingModes[schedulingMode] {
		return nil, status.Errorf(codes.InvalidArgument, "invalid scheduling_mode %q: must be one of [ALWAYS, WHEN_IDLE, SCHEDULED]", schedulingMode)
	}

	// Convert proto hardware to Go struct.
	hw := volunteer.HardwareCapabilitiesFromProto(req.Hardware)

	now := time.Now().UTC()
	var displayName *string
	if req.DisplayName != "" {
		displayName = &req.DisplayName
	}

	// Upsert by public key.
	existing, err := s.volunteerRepo.GetByPublicKey(ctx, req.PublicKey)
	if err != nil {
		// Check if it's a not-found error.
		apiErr, ok := err.(*apierror.APIError)
		if !ok || apiErr.HTTPStatus != 404 {
			s.logger.Error("failed to look up volunteer", "method", "RegisterVolunteer", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		// Not found — create new volunteer.
		v := &volunteer.Volunteer{
			PublicKey:            req.PublicKey,
			DisplayName:         displayName,
			HardwareCapabilities: hw,
			AvailableRuntimes:   req.AvailableRuntimes,
			SchedulingMode:      volunteer.SchedulingMode(schedulingMode),
			IsActive:            true,
			LastSeenAt:          &now,
		}

		if createErr := s.volunteerRepo.Create(ctx, v); createErr != nil {
			s.logger.Error("failed to create volunteer", "method", "RegisterVolunteer", "error", createErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}

		return &lettucev1.RegisterVolunteerResponse{
			VolunteerId: v.ID.String(),
			Registered:  true,
		}, nil
	}

	// Found — update existing volunteer.
	existing.DisplayName = displayName
	existing.HardwareCapabilities = hw
	existing.AvailableRuntimes = req.AvailableRuntimes
	existing.SchedulingMode = volunteer.SchedulingMode(schedulingMode)
	existing.IsActive = true
	existing.LastSeenAt = &now

	if updateErr := s.volunteerRepo.Update(ctx, existing); updateErr != nil {
		s.logger.Error("failed to update volunteer", "method", "RegisterVolunteer", "error", updateErr)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	return &lettucev1.RegisterVolunteerResponse{
		VolunteerId: existing.ID.String(),
		Registered:  false,
	}, nil
}

func (s *volunteerService) RequestWorkUnit(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Validate public_key shape (not used as the auth credential — see below).
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "public_key must be exactly 32 bytes, got %d", len(req.PublicKey))
	}

	// Authenticated identity (cryptographically proven by the request signature).
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}

	// Get volunteer and bind the proven identity to the volunteer being acted on.
	vol, err := s.volunteerRepo.GetByID(ctx, volunteerID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "volunteer not found")
		}
		s.logger.Error("failed to look up volunteer", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if !bytes.Equal(vol.PublicKey, authedKey) {
		return nil, status.Errorf(codes.PermissionDenied, "authenticated key does not match volunteer record")
	}

	// Determine capabilities: use current_available if provided, else registered.
	hw := vol.HardwareCapabilities
	if req.CurrentAvailable != nil {
		hw = volunteer.HardwareCapabilitiesFromProto(req.CurrentAvailable)
	}

	// Parse leaf_ids (new) with fallback to project_ids (deprecated).
	leafIDStrings := req.GetLeafIds()
	if len(leafIDStrings) == 0 {
		leafIDStrings = req.GetProjectIds() // deprecated fallback
	}
	leafIDs, err := parseIDSlice(leafIDStrings)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid leaf_id: %v", err)
	}
	blockedStrings := req.GetBlockedLeafIds()
	if len(blockedStrings) == 0 {
		blockedStrings = req.GetBlockedProjectIds() // deprecated fallback
	}
	blockedIDs, err := parseIDSlice(blockedStrings)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid blocked_leaf_id: %v", err)
	}

	// Compute GPU capabilities.
	hasGPU := len(hw.GPUs) > 0
	maxGPUVRAM := 0
	var gpuVendors []string
	var gpuCapabilities []string
	for _, gpu := range hw.GPUs {
		effective := gpu.VRAMMB * gpu.MaxVRAMPct / 100
		if effective > maxGPUVRAM {
			maxGPUVRAM = effective
		}
		vendor := strings.ToUpper(gpu.Vendor)
		gpuVendors = append(gpuVendors, vendor)
		if gpu.ComputeCapability != "" {
			gpuCapabilities = append(gpuCapabilities, gpu.ComputeCapability)
		}
	}

	opts := workunit.AssignmentOptions{
		VolunteerID:             volunteerID,
		LeafIDs:                 leafIDs,
		BlockedLeafIDs:          blockedIDs,
		MaxCPUCores:             hw.MaxCPUCores,
		MaxMemoryMB:             hw.MaxMemoryMB,
		MaxDiskMB:               hw.MaxDiskMB,
		HasGPU:                  hasGPU,
		MaxGPUVRAMMB:            maxGPUVRAM,
		AvailableRuntimes:       vol.AvailableRuntimes,
		GPUVendors:              gpuVendors,
		GPUComputeCapabilities:  gpuCapabilities,
		MaxInflightPerVolunteer: s.maxInflightPerVolunteer,
	}

	// Begin transaction for atomic find-assign-record.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logger.Error("failed to begin transaction", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer tx.Rollback(ctx)

	txWURepo := workunit.NewPgxWorkUnitRepository(tx)
	txAssignRepo := assignment.NewPgxRepository(tx)

	// Find next assignable work unit.
	wu, err := txWURepo.FindNextAssignable(ctx, opts)
	if err != nil {
		s.logger.Error("failed to find assignable work unit", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if wu == nil {
		return nil, status.Errorf(codes.NotFound, "no matching work units available")
	}

	// Check if this should be a spot-check first assignment.
	isSpotCheckFirst := false
	if !wu.SpotCheck {
		scLeaf, scErr := s.leafRepo.GetByID(ctx, wu.LeafID)
		if scErr != nil {
			s.logger.Warn("failed to check spot-check config, proceeding with normal assignment",
				"leaf_id", wu.LeafID, "error", scErr)
		} else if scLeaf.ValidationConfig.SpotCheckEnabled &&
			scLeaf.ValidationConfig.RedundancyFactor == 1 &&
			workunit.ShouldSpotCheck(scLeaf.ValidationConfig.SpotCheckPercentage) {
			isSpotCheckFirst = true
		}
	}

	now := time.Now().UTC()
	if isSpotCheckFirst {
		// Spot-check first assignment: mark spot_check=true, keep state QUEUED.
		// The WU remains findable so a second volunteer can be assigned.
		if err := txWURepo.MarkSpotCheck(ctx, wu.ID); err != nil {
			s.logger.Error("failed to mark spot-check", "method", "RequestWorkUnit", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		wu.SpotCheck = true
	} else {
		// Normal assignment: QUEUED → ASSIGNED.
		wu, err = txWURepo.Assign(ctx, wu.ID, volunteerID)
		if err != nil {
			s.logger.Error("failed to assign work unit", "method", "RequestWorkUnit", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
	}

	// Record assignment history.
	historyEntry := &assignment.AssignmentHistoryEntry{
		WorkUnitID:  wu.ID,
		VolunteerID: volunteerID,
		AssignedAt:  now,
	}
	if err := txAssignRepo.Create(ctx, historyEntry); err != nil {
		s.logger.Error("failed to record assignment history", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Commit transaction.
	if err := tx.Commit(ctx); err != nil {
		s.logger.Error("failed to commit assignment", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Update volunteer last_seen (best effort, outside transaction).
	_ = s.volunteerRepo.UpdateLastSeen(ctx, volunteerID)
	_ = s.volunteerRepo.SetActive(ctx, volunteerID, true)

	// Get leaf for response building.
	lf, err := s.leafRepo.GetByID(ctx, wu.LeafID)
	if err != nil {
		s.logger.Error("failed to get leaf for response", "method", "RequestWorkUnit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Build response (populate both leaf_id and deprecated project_id).
	resp := &lettucev1.RequestWorkUnitResponse{
		WorkUnitId:               wu.ID.String(),
		ProjectId:                wu.LeafID.String(), // deprecated
		LeafId:                   wu.LeafID.String(),
		Runtime:                  lf.ExecutionConfig.Runtime,
		InputData:                wu.InputData,
		InputDataUrl:             derefString(wu.InputDataRef),
		CodeArtifactUrl:          wu.CodeArtifactRef,
		ParametersJson:           string(wu.Parameters),
		DeadlineSeconds:          int32(wu.DeadlineSeconds),
		HeartbeatIntervalSeconds: int32(lf.FaultToleranceConfig.HeartbeatIntervalSeconds),
		EnvVars:                  lf.ExecutionConfig.EnvVars,
		RscFpopsEst:              lf.ExecutionConfig.RscFpopsEst,
		ExecutionSpec: &lettucev1.ExecutionSpec{
			Binaries:        lf.ExecutionConfig.Binaries,
			BinaryChecksums: lf.ExecutionConfig.BinaryChecksums,
			Image:           derefString(lf.ExecutionConfig.Image),
			GpuRequired:     lf.ExecutionConfig.GPURequired,
			GpuType:         lf.ExecutionConfig.GPUType,
			MaxMemoryMb:     int32(lf.ExecutionConfig.MaxMemoryMB),
			MaxDiskMb:       int32(lf.ExecutionConfig.MaxDiskMB),
			NetworkAccess:   lf.ExecutionConfig.NetworkAccess,
		},
	}

	// Include checkpoint info for reassigned work units with checkpoints.
	if wu.LastCheckpointSequence > 0 {
		resp.HasCheckpoint = true
		resp.CheckpointSequence = int32(wu.LastCheckpointSequence)
	}
	if lf.FaultToleranceConfig.CheckpointingEnabled && lf.FaultToleranceConfig.CheckpointIntervalSeconds != nil {
		resp.CheckpointIntervalSeconds = int32(*lf.FaultToleranceConfig.CheckpointIntervalSeconds)
	}

	return resp, nil
}

// parseIDSlice parses a slice of UUID strings into a slice of types.ID.
func parseIDSlice(ids []string) ([]types.ID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	result := make([]types.ID, len(ids))
	for i, s := range ids {
		id, err := types.ParseID(s)
		if err != nil {
			return nil, err
		}
		result[i] = id
	}
	return result, nil
}

// sha256HexRegex validates a 64-character lowercase hex SHA-256 digest.
var sha256HexRegex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *volunteerService) SubmitResult(ctx context.Context, req *lettucev1.SubmitResultRequest) (*lettucev1.SubmitResultResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Validate public_key shape (not used as the auth credential — see below).
	if len(req.PublicKey) != 32 {
		return nil, status.Errorf(codes.InvalidArgument, "public_key must be exactly 32 bytes, got %d", len(req.PublicKey))
	}

	// Authenticated identity (cryptographically proven by the request signature).
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}

	// Verify volunteer exists and bind the proven identity to it.
	vol, err := s.volunteerRepo.GetByID(ctx, volunteerID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "volunteer not found")
		}
		s.logger.Error("failed to look up volunteer", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if !bytes.Equal(vol.PublicKey, authedKey) {
		return nil, status.Errorf(codes.PermissionDenied, "authenticated key does not match volunteer record")
	}

	// Validate output data: at least one of output_data or output_data_url required.
	if len(req.OutputData) == 0 && req.OutputDataUrl == "" {
		return nil, status.Errorf(codes.InvalidArgument, "either output_data or output_data_url must be provided")
	}

	// Validate checksum format.
	if !sha256HexRegex.MatchString(req.OutputChecksumSha256) {
		return nil, status.Errorf(codes.InvalidArgument, "output_checksum_sha256 must be a 64-character lowercase hex string")
	}

	// If inline output_data, verify SHA-256 matches.
	if len(req.OutputData) > 0 {
		hash := sha256.Sum256(req.OutputData)
		computed := hex.EncodeToString(hash[:])
		if computed != req.OutputChecksumSha256 {
			return nil, status.Errorf(codes.InvalidArgument, "output_checksum_sha256 mismatch: computed %s, got %s", computed, req.OutputChecksumSha256)
		}
	}

	// Validate metadata.
	if req.Metadata == nil {
		return nil, status.Errorf(codes.InvalidArgument, "metadata is required")
	}
	if req.Metadata.WallClockSeconds <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.wall_clock_seconds must be > 0")
	}

	// M3: enforce the leaf's researcher-configured per-result output cap on the
	// INLINE output before storing it (and before opening a transaction). Without
	// this, an authenticated, assigned volunteer could submit inline output far
	// larger than the configured maximum, causing unbounded JSONB storage and
	// memory pressure (the aggregation engine later loads all agreed outputs into
	// memory). This applies only to inline output_data; the external
	// output_data_url path carries no inline bytes here.
	if len(req.OutputData) > 0 {
		submitWU, wuErr := s.wuRepo.GetByID(ctx, workUnitID)
		if wuErr != nil {
			apiErr, ok := wuErr.(*apierror.APIError)
			if ok && apiErr.HTTPStatus == 404 {
				return nil, status.Errorf(codes.NotFound, "work unit not found")
			}
			s.logger.Error("failed to load work unit for output size check", "error", wuErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		submitLeaf, leafErr := s.leafRepo.GetByID(ctx, submitWU.LeafID)
		if leafErr != nil {
			s.logger.Error("failed to load leaf for output size check", "error", leafErr)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
		// MaxOutputSizeBytes is always > 0 for a stored leaf (ValidateDataConfig
		// requires > 0 and ApplyDataConfigDefaults fills 0 with a 100MB default),
		// but we still guard on > 0 so a max of 0 is treated as "unlimited" and
		// never rejects a legitimate submission.
		maxOut := submitLeaf.DataConfig.MaxOutputSizeBytes
		if maxOut > 0 && int64(len(req.OutputData)) > maxOut {
			return nil, status.Errorf(codes.InvalidArgument,
				"output_data size %d bytes exceeds leaf max_output_size_bytes %d", len(req.OutputData), maxOut)
		}
	}

	// Begin transaction.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logger.Error("failed to begin transaction", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer tx.Rollback(ctx)

	txAssignRepo := assignment.NewPgxRepository(tx)
	txResultRepo := result.NewPgxRepository(tx)

	// Verify active assignment exists.
	activeAssignment, err := txAssignRepo.FindActiveByWorkUnitAndVolunteer(ctx, workUnitID, volunteerID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.FailedPrecondition, "no active assignment for this volunteer and work unit")
		}
		s.logger.Error("failed to check assignment", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Count existing PENDING results to determine if work unit should transition to COMPLETED.
	// Must count only PENDING (not DISAGREED from prior rounds) so reassigned work units
	// still transition on their first new result.
	existingCount, err := txResultRepo.CountPendingByWorkUnit(ctx, workUnitID)
	if err != nil {
		s.logger.Error("failed to check existing results", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Build result.
	var outputData json.RawMessage
	if len(req.OutputData) > 0 {
		outputData = json.RawMessage(req.OutputData)
	}
	var outputDataRef *string
	if req.OutputDataUrl != "" {
		outputDataRef = &req.OutputDataUrl
	}

	r := &result.Result{
		WorkUnitID:        workUnitID,
		VolunteerID:       volunteerID,
		OutputData:        outputData,
		OutputDataRef:     outputDataRef,
		OutputChecksum:    req.OutputChecksumSha256,
		ExecutionMetadata: result.ExecutionMetadataFromProto(req.Metadata),
		ValidationStatus:  result.ValidationPending,
	}

	// Insert result.
	if err := txResultRepo.Create(ctx, r); err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 409 {
			return &lettucev1.SubmitResultResponse{
				Accepted: false,
				Message:  "duplicate submission",
			}, nil
		}
		s.logger.Error("failed to create result", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Update assignment outcome to COMPLETED with result_id.
	if err := txAssignRepo.UpdateOutcome(ctx, activeAssignment.ID, assignment.OutcomeCompleted, &r.ID); err != nil {
		s.logger.Error("failed to update assignment outcome", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Determine when to transition to COMPLETED.
	// Read the leaf's redundancy_factor so WUs with redundancy > 1 wait for all results.
	// For spot-check WUs, always require at least 2 results.
	txWURepo := workunit.NewPgxWorkUnitRepository(tx)
	currentWU, err := txWURepo.GetByID(ctx, workUnitID)
	if err != nil {
		s.logger.Error("failed to load work unit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	effectiveRedundancy := 1
	completionLeaf, clErr := s.leafRepo.GetByID(ctx, currentWU.LeafID)
	if clErr == nil {
		effectiveRedundancy = completionLeaf.ValidationConfig.RedundancyFactor
	}
	if currentWU.SpotCheck && effectiveRedundancy < 2 {
		effectiveRedundancy = 2
	}

	if existingCount+1 >= effectiveRedundancy {
		_, err := tx.Exec(ctx, `
			UPDATE work_units SET
				state = 'COMPLETED',
				started_at = COALESCE(started_at, NOW()),
				completed_at = NOW()
			WHERE id = $1 AND (state IN ('ASSIGNED', 'RUNNING') OR (state = 'QUEUED' AND spot_check = true))`,
			workUnitID,
		)
		if err != nil {
			s.logger.Error("failed to transition work unit to COMPLETED", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
	}

	// Commit transaction.
	if err := tx.Commit(ctx); err != nil {
		s.logger.Error("failed to commit result submission", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Best-effort updates outside the transaction.
	_ = s.volunteerRepo.UpdateLastSeen(ctx, volunteerID)

	// Increment batch completed counter when the work unit transitioned to COMPLETED.
	// Reuse currentWU from the transaction — no need for a second DB fetch.
	if existingCount+1 >= effectiveRedundancy && s.batchRepo != nil {
		if currentWU.BatchID != nil {
			_ = s.batchRepo.IncrementCompleted(ctx, *currentWU.BatchID)
		}
	}

	// Try validation — runs if enough results have been submitted.
	if s.validationEngine != nil {
		if _, valErr := s.validationEngine.TryValidate(ctx, workUnitID); valErr != nil {
			s.logger.Error("validation failed after result submission",
				"work_unit_id", workUnitID,
				"error", valErr,
			)
		}
	}

	// Clean up checkpoints for completed work units (VALIDATED or FAILED).
	if s.checkpointRepo != nil {
		postWU, postErr := s.wuRepo.GetByID(ctx, workUnitID)
		if postErr == nil && (postWU.State == workunit.WorkUnitStateValidated || postWU.State == workunit.WorkUnitStateFailed) {
			if postWU.LastCheckpointSequence > 0 {
				if cpErr := s.checkpointRepo.Delete(ctx, workUnitID); cpErr != nil {
					s.logger.Error("failed to clean up checkpoint after completion",
						"work_unit_id", workUnitID,
						"state", postWU.State,
						"error", cpErr,
					)
				}
			}
		}
	}

	return &lettucev1.SubmitResultResponse{
		ResultId: r.ID.String(),
		Accepted: true,
	}, nil
}

func (s *volunteerService) Heartbeat(ctx context.Context, req *lettucev1.HeartbeatRequest) (*lettucev1.HeartbeatResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Bind the authenticated identity to the volunteer being acted on.
	if err := s.requireAuthedVolunteer(ctx, volunteerID, "Heartbeat"); err != nil {
		return nil, err
	}

	// Default status to RUNNING if empty.
	hbStatus := req.Status
	if hbStatus == "" {
		hbStatus = "RUNNING"
	}
	// PREPARING: the volunteer holds the unit but hasn't started executing yet
	// (pulling the image, or waiting in its local prefetch queue). It refreshes
	// last_heartbeat_at so the fault monitor doesn't reclaim a live unit, but it
	// must NOT transition ASSIGNED -> RUNNING (no work has started).
	if hbStatus != "RUNNING" && hbStatus != "CHECKPOINT_SAVED" && hbStatus != "PREPARING" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid status %q: must be RUNNING, PREPARING, or CHECKPOINT_SAVED", hbStatus)
	}

	// Load work unit.
	wu, err := s.wuRepo.GetByID(ctx, workUnitID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "work unit not found")
		}
		s.logger.Error("failed to load work unit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Verify assignment.
	if wu.SpotCheck {
		// For spot-check WUs, verify via assignment history (multiple volunteers may be assigned).
		entry, assignErr := s.assignRepo.FindActiveByWorkUnitAndVolunteer(ctx, workUnitID, volunteerID)
		if assignErr != nil || entry == nil {
			return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
		}
	} else if wu.AssignedVolunteerID == nil || *wu.AssignedVolunteerID != volunteerID {
		return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
	}

	// Check work unit state.
	switch wu.State {
	case workunit.WorkUnitStateAssigned, workunit.WorkUnitStateRunning:
		// OK — proceed.
	case workunit.WorkUnitStateQueued:
		if !wu.SpotCheck {
			return &lettucev1.HeartbeatResponse{
				ContinueExecution: false,
				Message:           "work unit no longer active",
			}, nil
		}
		// Spot-check WUs in QUEUED state are reclaimed by FindExpiredWorkUnits
		// after 1 hour if a second volunteer is never assigned.
	default:
		return &lettucev1.HeartbeatResponse{
			ContinueExecution: false,
			Message:           "work unit no longer active",
		}, nil
	}

	// Begin transaction for atomic updates.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.logger.Error("failed to begin transaction", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	defer tx.Rollback(ctx)

	// If first execution heartbeat (ASSIGNED), transition to RUNNING.
	// PREPARING heartbeats refresh the timestamp only — the unit hasn't started.
	// Skip for spot-check WUs in QUEUED state — they must stay QUEUED for second assignment.
	if wu.State == workunit.WorkUnitStateAssigned && hbStatus != "PREPARING" {
		_, err := tx.Exec(ctx, `
			UPDATE work_units SET
				state = 'RUNNING',
				started_at = NOW(),
				last_heartbeat_at = NOW()
			WHERE id = $1 AND state = 'ASSIGNED'`, workUnitID)
		if err != nil {
			s.logger.Error("failed to transition to RUNNING", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
	} else {
		// Normal heartbeat — just update timestamp.
		_, err := tx.Exec(ctx,
			"UPDATE work_units SET last_heartbeat_at = NOW() WHERE id = $1", workUnitID)
		if err != nil {
			s.logger.Error("failed to update heartbeat", "error", err)
			return nil, status.Errorf(codes.Internal, "internal error")
		}
	}

	// Update volunteer last_seen and is_active.
	_, err = tx.Exec(ctx,
		"UPDATE volunteers SET last_seen_at = NOW(), is_active = true WHERE id = $1", volunteerID)
	if err != nil {
		s.logger.Error("failed to update volunteer", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	if err := tx.Commit(ctx); err != nil {
		s.logger.Error("failed to commit heartbeat", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Log checkpoint-saved status.
	if hbStatus == "CHECKPOINT_SAVED" {
		s.logger.Info("checkpoint saved for work unit",
			"work_unit_id", workUnitID,
			"volunteer_id", volunteerID,
			"last_checkpoint_sequence", wu.LastCheckpointSequence,
		)
	}

	// Check leaf state.
	lf, err := s.leafRepo.GetByID(ctx, wu.LeafID)
	if err != nil {
		s.logger.Error("failed to load leaf", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	switch lf.State {
	case leaf.StatePaused, leaf.StateCompleted, leaf.StateArchived:
		return &lettucev1.HeartbeatResponse{
			ContinueExecution: false,
			Message:           fmt.Sprintf("leaf is %s", lf.State),
		}, nil
	}

	return &lettucev1.HeartbeatResponse{
		ContinueExecution: true,
	}, nil
}

func (s *volunteerService) SaveCheckpoint(ctx context.Context, req *lettucev1.SaveCheckpointRequest) (*lettucev1.SaveCheckpointResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// Validate volunteer_id.
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Bind the authenticated identity to the volunteer being acted on.
	if err := s.requireAuthedVolunteer(ctx, volunteerID, "SaveCheckpoint"); err != nil {
		return nil, err
	}

	// Load work unit.
	wu, err := s.wuRepo.GetByID(ctx, workUnitID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.NotFound, "work unit not found")
		}
		s.logger.Error("failed to load work unit", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Verify volunteer is assigned.
	if wu.AssignedVolunteerID == nil || *wu.AssignedVolunteerID != volunteerID {
		return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
	}

	// Load leaf and check checkpointing is enabled.
	lf, err := s.leafRepo.GetByID(ctx, wu.LeafID)
	if err != nil {
		s.logger.Error("failed to load leaf", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if !lf.FaultToleranceConfig.CheckpointingEnabled {
		return nil, status.Errorf(codes.FailedPrecondition, "checkpointing is not enabled for this leaf")
	}

	// Validate sequence is advancing.
	if int(req.CheckpointSequence) <= wu.LastCheckpointSequence {
		return nil, status.Errorf(codes.AlreadyExists,
			"checkpoint sequence must be greater than %d", wu.LastCheckpointSequence)
	}

	// Validate data size.
	maxSize := lf.FaultToleranceConfig.MaxCheckpointSizeBytes
	if maxSize == 0 {
		maxSize = 104857600 // 100 MB default
	}
	if int64(len(req.CheckpointData)) > maxSize {
		return nil, status.Errorf(codes.ResourceExhausted,
			"checkpoint data size %d exceeds maximum %d bytes", len(req.CheckpointData), maxSize)
	}

	// Compute SHA-256 checksum.
	hash := sha256.Sum256(req.CheckpointData)
	checksum := hex.EncodeToString(hash[:])

	// Build and save checkpoint.
	cp := &checkpoint.Checkpoint{
		LeafID:             wu.LeafID,
		WorkUnitID:         workUnitID,
		VolunteerID:        volunteerID,
		CheckpointSequence: int(req.CheckpointSequence),
		SizeBytes:          int64(len(req.CheckpointData)),
		ChecksumSHA256:     checksum,
	}

	if err := s.checkpointRepo.Save(ctx, cp, req.CheckpointData); err != nil {
		s.logger.Error("failed to save checkpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	s.logger.Info("checkpoint saved",
		"work_unit_id", workUnitID,
		"volunteer_id", volunteerID,
		"sequence", req.CheckpointSequence,
		"size_bytes", len(req.CheckpointData),
	)

	return &lettucev1.SaveCheckpointResponse{
		Accepted: true,
		Message:  "checkpoint saved",
	}, nil
}

func (s *volunteerService) GetCheckpoint(ctx context.Context, req *lettucev1.GetCheckpointRequest) (*lettucev1.GetCheckpointResponse, error) {
	// Validate work_unit_id.
	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}

	// GetCheckpointRequest carries no volunteer_id, so we resolve the caller from the
	// authenticated public key and require that they are (or were) assigned to this
	// work unit before returning checkpoint data. This prevents an authenticated
	// volunteer from reading another volunteer's in-progress checkpoint.
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	caller, err := s.volunteerRepo.GetByPublicKey(ctx, authedKey)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return nil, status.Errorf(codes.PermissionDenied, "authenticated volunteer not found")
		}
		s.logger.Error("failed to look up authenticated volunteer", "method", "GetCheckpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	// Verify the caller is/was assigned to this work unit (mirrors the assignment
	// check SubmitResult uses, but accepts any assignment in history — including a
	// completed one — so a reassigned volunteer can still recover its checkpoint).
	assignments, err := s.assignRepo.ListByWorkUnit(ctx, workUnitID)
	if err != nil {
		s.logger.Error("failed to list assignments", "method", "GetCheckpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	assigned := false
	for _, a := range assignments {
		if a.VolunteerID == caller.ID {
			assigned = true
			break
		}
	}
	if !assigned {
		return nil, status.Errorf(codes.PermissionDenied, "volunteer is not assigned to this work unit")
	}

	cp, data, err := s.checkpointRepo.GetLatest(ctx, workUnitID)
	if err != nil {
		s.logger.Error("failed to get checkpoint", "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	if cp == nil {
		return &lettucev1.GetCheckpointResponse{
			HasCheckpoint: false,
		}, nil
	}

	return &lettucev1.GetCheckpointResponse{
		HasCheckpoint:        true,
		CheckpointData:       data,
		CheckpointSequence:   int32(cp.CheckpointSequence),
		CreatedByVolunteerId: cp.VolunteerID.String(),
		CreatedAt:            cp.CreatedAt.Format(time.RFC3339),
	}, nil
}

func (s *volunteerService) AbandonWorkUnit(ctx context.Context, req *lettucev1.AbandonWorkUnitRequest) (*lettucev1.AbandonWorkUnitResponse, error) {
	if req.WorkUnitId == "" || req.VolunteerId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "work_unit_id and volunteer_id are required")
	}

	workUnitID, err := types.ParseID(req.WorkUnitId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid work_unit_id: %v", err)
	}
	volunteerID, err := types.ParseID(req.VolunteerId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volunteer_id: %v", err)
	}

	// Bind the authenticated identity to the volunteer being acted on.
	if err := s.requireAuthedVolunteer(ctx, volunteerID, "AbandonWorkUnit"); err != nil {
		return nil, err
	}

	// Find the active assignment for this volunteer + work unit.
	activeAssignment, err := s.assignRepo.FindActiveByWorkUnitAndVolunteer(ctx, workUnitID, volunteerID)
	if err != nil {
		s.logger.Error("abandon: failed to find active assignment", "work_unit_id", req.WorkUnitId, "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}
	if activeAssignment == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "no active assignment found for this volunteer and work unit")
	}

	// Mark assignment as ABANDONED.
	if err := s.assignRepo.UpdateOutcome(ctx, activeAssignment.ID, assignment.OutcomeAbandoned, nil); err != nil {
		s.logger.Error("abandon: failed to update assignment outcome", "work_unit_id", req.WorkUnitId, "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Transition work unit to EXPIRED so it can be reassigned.
	if _, err := s.wuRepo.TransitionToExpired(ctx, workUnitID); err != nil {
		s.logger.Error("abandon: failed to transition work unit", "work_unit_id", req.WorkUnitId, "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	// Reassign (requeue or fail if max reassignments exceeded).
	_, requeued, err := s.wuRepo.Reassign(ctx, workUnitID)
	if err != nil {
		s.logger.Error("abandon: failed to reassign work unit", "work_unit_id", req.WorkUnitId, "error", err)
		return nil, status.Errorf(codes.Internal, "internal error")
	}

	s.logger.Info("work unit abandoned by volunteer",
		"work_unit_id", req.WorkUnitId,
		"volunteer_id", req.VolunteerId,
		"reason", req.Reason,
		"requeued", requeued,
	)

	msg := "work unit requeued"
	if !requeued {
		msg = "work unit failed (max reassignments exceeded)"
	}
	return &lettucev1.AbandonWorkUnitResponse{
		Requeued: requeued,
		Message:  msg,
	}, nil
}

// requireAuthedVolunteer verifies that the request was authenticated and that the
// cryptographically proven public key (set by the gRPC auth interceptor) matches the
// public key on record for the volunteer identified by volunteerID. This binds the
// proven identity to the volunteer being acted on. method is used only for logging.
func (s *volunteerService) requireAuthedVolunteer(ctx context.Context, volunteerID types.ID, method string) error {
	authedKey, ok := GRPCAuthPublicKeyFromContext(ctx)
	if !ok {
		return status.Errorf(codes.Unauthenticated, "request is not authenticated")
	}
	vol, err := s.volunteerRepo.GetByID(ctx, volunteerID)
	if err != nil {
		apiErr, ok := err.(*apierror.APIError)
		if ok && apiErr.HTTPStatus == 404 {
			return status.Errorf(codes.NotFound, "volunteer not found")
		}
		s.logger.Error("failed to look up volunteer", "method", method, "error", err)
		return status.Errorf(codes.Internal, "internal error")
	}
	if !bytes.Equal(vol.PublicKey, authedKey) {
		return status.Errorf(codes.PermissionDenied, "authenticated key does not match volunteer record")
	}
	return nil
}

// derefString returns the dereferenced string or empty string if nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
