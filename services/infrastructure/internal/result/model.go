package result

import (
	"encoding/json"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/types"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// ValidationStatus represents the validation state of a result.
type ValidationStatus string

const (
	ValidationPending   ValidationStatus = "PENDING"
	ValidationAgreed    ValidationStatus = "AGREED"
	ValidationDisagreed ValidationStatus = "DISAGREED"
)

// ExecutionMetadata holds self-reported resource metrics from the volunteer.
type ExecutionMetadata struct {
	WallClockSeconds int64   `json:"wall_clock_seconds"`
	CPUSecondsUser   float64 `json:"cpu_seconds_user"`
	CPUSecondsSystem float64 `json:"cpu_seconds_system"`
	CPUCoresUsed     int     `json:"cpu_cores_used"`
	GPUSeconds       float64 `json:"gpu_seconds"`
	GPUModel         string  `json:"gpu_model,omitempty"`
	GPUVRAMUsedMB    int     `json:"gpu_vram_used_mb"`
	PeakMemoryMB     int     `json:"peak_memory_mb"`
	DiskReadMB       int64   `json:"disk_read_mb"`
	DiskWriteMB      int64   `json:"disk_write_mb"`
	NetworkRxMB      int64   `json:"network_rx_mb"`
	NetworkTxMB      int64   `json:"network_tx_mb"`
}

// ExecutionMetadataFromProto converts a protobuf ExecutionMetadata to a Go struct.
func ExecutionMetadataFromProto(pb *lettucev1.ExecutionMetadata) ExecutionMetadata {
	if pb == nil {
		return ExecutionMetadata{}
	}
	return ExecutionMetadata{
		WallClockSeconds: pb.WallClockSeconds,
		CPUSecondsUser:   pb.CpuSecondsUser,
		CPUSecondsSystem: pb.CpuSecondsSystem,
		CPUCoresUsed:     int(pb.CpuCoresUsed),
		GPUSeconds:       pb.GpuSeconds,
		GPUModel:         pb.GpuModel,
		GPUVRAMUsedMB:    int(pb.GpuVramUsedMb),
		PeakMemoryMB:     int(pb.PeakMemoryMb),
		DiskReadMB:       pb.DiskReadMb,
		DiskWriteMB:      pb.DiskWriteMb,
		NetworkRxMB:      pb.NetworkRxMb,
		NetworkTxMB:      pb.NetworkTxMb,
	}
}

// Result is the output of a completed work unit submitted by a volunteer.
type Result struct {
	ID                types.ID          `json:"id"`
	WorkUnitID        types.ID          `json:"work_unit_id"`
	VolunteerID       types.ID          `json:"volunteer_id"`
	OutputData        json.RawMessage   `json:"output_data,omitempty"`
	OutputDataRef     *string           `json:"output_data_ref,omitempty"`
	OutputChecksum    string            `json:"output_checksum"`
	ExecutionMetadata ExecutionMetadata `json:"execution_metadata"`
	ValidationStatus  ValidationStatus  `json:"validation_status"`
	// ArtifactVersionID records which leaf_artifact_versions row produced this result
	// (TODO #38): the unit's pinned version, else the leaf's current version at submit.
	// nil = legacy / unversioned leaf. Lets validation refuse to compare results from
	// different artifact versions and gives per-result version provenance.
	ArtifactVersionID *types.ID `json:"artifact_version_id,omitempty"`
	// HostID attributes the result to the MACHINE that produced it (TODO #19), copied
	// from the volunteer's live copy row at submit. nil = a volunteer that reported no
	// host (per-account fallback).
	HostID *types.ID `json:"host_id,omitempty"`
	// TrustSubject / TrustScoreAtSubmit are the account-level trust snapshot stamped at
	// submission (see internal/trust): the subject resolved for the submitting volunteer
	// and that subject's score AT SUBMIT time. Acceptance decisions must use the
	// submission-time score (not a later, possibly slashed or re-accrued value), so it is
	// snapshotted per result rather than re-read at validation. Both nil = a legacy row
	// created before the trust feature.
	TrustSubject       *string    `json:"trust_subject,omitempty"`
	TrustScoreAtSubmit *int       `json:"trust_score_at_submit,omitempty"`
	SubmittedAt        time.Time  `json:"submitted_at"`
	ValidatedAt        *time.Time `json:"validated_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}
